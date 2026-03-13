package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
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

// releaseWriterFast returns the connection to the pool without sending DISCARD ALL.
// Safe for read-only queries that did not modify session state (SET, PREPARE, etc.).
func (s *Server) releaseWriterFast(conn *pool.Conn, dbg *DatabaseGroup) {
	dbg.writerPool.Release(conn)
}

// resetAndReleaseToPool sends DISCARD ALL and returns the connection to the specified pool.
// Use this instead of resetAndReleaseWriter when the connection may outlive a config reload
// (e.g., boundWriter in a transaction), to ensure Release goes to the pool that issued Acquire.
func (s *Server) resetAndReleaseToPool(conn *pool.Conn, p *pool.Pool) {
	if err := s.resetConn(conn); err != nil {
		slog.Warn("reset writer conn failed, discarding", "error", err)
		p.Discard(conn)
		return
	}
	p.Release(conn)
}

// releaseToPool returns the connection to the specified pool without DISCARD ALL.
// Use this instead of releaseWriterFast when the connection may outlive a config reload.
func releaseToPool(conn *pool.Conn, p *pool.Pool) {
	p.Release(conn)
}

// discardToPool discards the connection to the specified pool.
// Use this instead of dbg.writerPool.Discard when the connection may outlive a config reload.
func discardToPool(conn *pool.Conn, p *pool.Pool) {
	p.Discard(conn)
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

// fallbackToWriter acquires a writer connection from the pool and forwards a read query.
// Since this is a read-only fallback, we skip DISCARD ALL on release.
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
	if err != nil {
		dbg.writerPool.Discard(wConn)
	} else {
		s.releaseWriterFast(wConn, dbg)
	}
	return err
}

// handleWriteQuery forwards a write query to the writer and invalidates cache.
// qtype is the pre-classified query type to avoid redundant classification.
func (s *Server) handleWriteQuery(clientConn net.Conn, writerConn net.Conn, msg *protocol.Message, query string, session *router.Session, pq *router.ParsedQuery, qtype router.QueryType, cfg *config.Config, dbg *DatabaseGroup) {
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

	// Track WAL LSN for causal consistency (only for actual writes, not BEGIN/COMMIT)
	if cfg.Routing.CausalConsistency && qtype == router.QueryWrite {
		if lsn, err := s.queryCurrentLSN(writerConn); err != nil {
			slog.Warn("query WAL LSN after write", "error", err)
		} else {
			session.SetLastWriteLSN(lsn)
			slog.Debug("write LSN tracked", "lsn", lsn)
		}
	}

	// Invalidate cache for affected tables (only for actual writes)
	if s.queryCache != nil && qtype == router.QueryWrite {
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

// isSessionModifying returns true if a query modifies persistent session state
// (SET, PREPARE, LISTEN, CREATE TEMP, etc.) that requires DISCARD ALL to clean up.
// SET LOCAL and SET TRANSACTION are transaction-scoped and don't require reset.
func isSessionModifying(query string) bool {
	// Skip leading whitespace
	i := 0
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r') {
		i++
	}
	if i >= len(query) {
		return false
	}

	rest := query[i:]
	n := len(rest)
	ch := rest[0] | 0x20 // lowercase first char

	switch ch {
	case 's': // SET (but not SET LOCAL / SET TRANSACTION)
		if n >= 4 && eqFold3(rest, "SET") && (rest[3] == ' ' || rest[3] == '\t') {
			// Skip to next word
			j := 4
			for j < n && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j+5 < n && (eqFold5(rest[j:], "LOCAL") && (rest[j+5] == ' ' || rest[j+5] == '\t')) {
				return false // SET LOCAL — transaction-scoped
			}
			if j+11 < n && eqFoldN(rest[j:j+11], "TRANSACTION") {
				return false // SET TRANSACTION — transaction-scoped
			}
			return true
		}
	case 'p': // PREPARE
		if n >= 7 && eqFoldN(rest[:7], "PREPARE") && (n == 7 || rest[7] == ' ' || rest[7] == '\t') {
			return true
		}
	case 'd': // DECLARE, DEALLOCATE
		if n >= 7 && eqFoldN(rest[:7], "DECLARE") && (n == 7 || rest[7] == ' ' || rest[7] == '\t') {
			return true
		}
		if n >= 10 && eqFoldN(rest[:10], "DEALLOCATE") && (n == 10 || rest[10] == ' ' || rest[10] == '\t') {
			return true
		}
	case 'l': // LISTEN, LOAD
		if n >= 6 && eqFold6(rest, "LISTEN") && (n == 6 || rest[6] == ' ' || rest[6] == '\t') {
			return true
		}
		if n >= 4 && eqFoldN(rest[:4], "LOAD") && (n == 4 || rest[4] == ' ' || rest[4] == '\t') {
			return true
		}
	case 'u': // UNLISTEN
		if n >= 8 && eqFoldN(rest[:8], "UNLISTEN") && (n == 8 || rest[8] == ' ' || rest[8] == '\t') {
			return true
		}
	case 'c': // CREATE TEMP / CREATE TEMPORARY
		if n >= 6 && eqFold6(rest, "CREATE") {
			j := 6
			for j < n && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j+4 <= n && eqFoldN(rest[j:j+4], "TEMP") {
				return true
			}
		}
	}
	return false
}

// eqFold3, eqFold5, eqFold6, eqFoldN are zero-allocation case-insensitive comparisons.
// Defined in internal/router/router.go — imported via local wrappers to avoid cross-package dependency.
func eqFold3(s, target string) bool {
	return (s[0]|0x20) == (target[0]|0x20) && (s[1]|0x20) == (target[1]|0x20) && (s[2]|0x20) == (target[2]|0x20)
}
func eqFold5(s, target string) bool {
	return (s[0]|0x20) == (target[0]|0x20) && (s[1]|0x20) == (target[1]|0x20) && (s[2]|0x20) == (target[2]|0x20) && (s[3]|0x20) == (target[3]|0x20) && (s[4]|0x20) == (target[4]|0x20)
}
func eqFold6(s, target string) bool {
	return (s[0]|0x20) == (target[0]|0x20) && (s[1]|0x20) == (target[1]|0x20) && (s[2]|0x20) == (target[2]|0x20) && (s[3]|0x20) == (target[3]|0x20) && (s[4]|0x20) == (target[4]|0x20) && (s[5]|0x20) == (target[5]|0x20)
}
func eqFoldN(s, target string) bool {
	if len(s) < len(target) {
		return false
	}
	for i := 0; i < len(target); i++ {
		if (s[i] | 0x20) != (target[i] | 0x20) {
			return false
		}
	}
	return true
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
