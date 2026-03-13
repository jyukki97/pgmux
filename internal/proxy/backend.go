package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
)

// acquireWriterConn returns the bound transaction connection or acquires a new one from the pool.
// The bool return indicates whether the connection was newly acquired (true) or was already bound (false).
func (s *Server) acquireWriterConn(ctx context.Context, bound *pool.Conn, dbg *DatabaseGroup) (*pool.Conn, bool, error) {
	if bound != nil {
		return bound, false, nil
	}
	// Circuit breaker check
	if dbg.writerCB != nil {
		if err := dbg.writerCB.Allow(); err != nil {
			return nil, false, fmt.Errorf("writer circuit breaker open: %w", err)
		}
	}
	acquireStart := time.Now()
	conn, err := dbg.writerPool.Acquire(ctx)
	if err != nil {
		if dbg.writerCB != nil {
			dbg.writerCB.RecordFailure()
		}
		return nil, false, fmt.Errorf("acquire writer: %w", err)
	}
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("writer", dbg.writerAddr).Inc()
		s.metrics.PoolAcquireDur.WithLabelValues("writer", dbg.writerAddr).Observe(time.Since(acquireStart).Seconds())
	}
	return conn, true, nil
}

// resetAndReleaseWriter sends a reset query (DISCARD ALL) and returns the connection to the pool.
// If the reset fails, the connection is discarded instead.
func (s *Server) resetAndReleaseWriter(conn *pool.Conn, dbg *DatabaseGroup) {
	if err := s.resetConn(conn); err != nil {
		slog.Warn("reset writer conn failed, discarding", "error", err)
		dbg.writerPool.Discard(conn)
		return
	}
	dbg.writerPool.Release(conn)
}

// resetConn sends the configured reset query (e.g. DISCARD ALL) to clean up session state
// before returning a connection to the pool.
func (s *Server) resetConn(conn net.Conn) error {
	resetQuery := s.getConfig().Pool.ResetQuery
	if resetQuery == "" {
		return nil
	}
	payload := append([]byte(resetQuery), 0)
	if err := protocol.WriteMessage(conn, protocol.MsgQuery, payload); err != nil {
		return fmt.Errorf("send reset query: %w", err)
	}
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return fmt.Errorf("read reset response: %w", err)
		}
		if msg.Type == protocol.MsgErrorResponse {
			return fmt.Errorf("reset query error")
		}
		if msg.Type == protocol.MsgReadyForQuery {
			return nil
		}
	}
}

// fallbackToWriter acquires a writer connection from the pool and forwards the query.
func (s *Server) fallbackToWriter(ctx context.Context, clientConn net.Conn, msg *protocol.Message, ct *cancelTarget, dbg *DatabaseGroup) error {
	wConn, err := dbg.writerPool.Acquire(ctx)
	if err != nil {
		s.sendError(clientConn, "no available backend connections")
		return fmt.Errorf("acquire writer for fallback: %w", err)
	}
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("writer", dbg.writerAddr).Inc()
	}
	ct.setFromConn(dbg.writerAddr, wConn)
	err = s.forwardAndRelay(clientConn, wConn, msg)
	ct.clear()
	s.resetAndReleaseWriter(wConn, dbg)
	return err
}

// handleWriteQuery forwards a write query to the writer and invalidates cache.
func (s *Server) handleWriteQuery(clientConn net.Conn, writerConn net.Conn, msg *protocol.Message, query string, session *router.Session, pq *router.ParsedQuery, dbg *DatabaseGroup) {
	if err := s.forwardAndRelay(clientConn, writerConn, msg); err != nil {
		slog.Error("forward write to writer", "error", err)
		if dbg.writerCB != nil {
			dbg.writerCB.RecordFailure()
		}
		return
	}
	if dbg.writerCB != nil {
		dbg.writerCB.RecordSuccess()
	}

	// Track WAL LSN for causal consistency
	if s.getConfig().Routing.CausalConsistency && s.classifyQueryParsed(query, pq) == router.QueryWrite {
		if lsn, err := s.queryCurrentLSN(writerConn); err != nil {
			slog.Warn("query WAL LSN after write", "error", err)
		} else {
			session.SetLastWriteLSN(lsn)
			slog.Debug("write LSN tracked", "lsn", lsn)
		}
	}

	// Invalidate cache for affected tables
	if s.queryCache != nil && s.classifyQueryParsed(query, pq) == router.QueryWrite {
		tables := s.extractQueryTablesParsed(query, pq)
		for _, table := range tables {
			s.queryCache.InvalidateTable(table)
			if s.metrics != nil {
				s.metrics.CacheInvalidations.Inc()
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
			slog.Debug("cache invalidated", "table", table)
		}
		// Broadcast invalidation to other proxy instances
		if s.invalidator != nil && len(tables) > 0 {
			s.invalidator.Publish(context.Background(), tables)
		}
	}
}

// queryCurrentLSN queries the current WAL LSN from the writer connection.
func (s *Server) queryCurrentLSN(writerConn net.Conn) (router.LSN, error) {
	payload := append([]byte("SELECT pg_current_wal_lsn()"), 0)
	if err := protocol.WriteMessage(writerConn, protocol.MsgQuery, payload); err != nil {
		return 0, fmt.Errorf("send LSN query: %w", err)
	}

	var lsnStr string
	for {
		msg, err := protocol.ReadMessage(writerConn)
		if err != nil {
			return 0, fmt.Errorf("read LSN response: %w", err)
		}
		if msg.Type == protocol.MsgDataRow && len(msg.Payload) >= 6 {
			// DataRow: Int16(numCols) + Int32(len) + Byte[n](value)
			colLen := int(binary.BigEndian.Uint32(msg.Payload[2:6]))
			if colLen > 0 && 6+colLen <= len(msg.Payload) {
				lsnStr = string(msg.Payload[6 : 6+colLen])
			}
		}
		if msg.Type == protocol.MsgErrorResponse {
			return 0, fmt.Errorf("LSN query returned error")
		}
		if msg.Type == protocol.MsgReadyForQuery {
			break
		}
	}

	if lsnStr == "" {
		return 0, fmt.Errorf("no LSN value returned")
	}
	return router.ParseLSN(lsnStr)
}
