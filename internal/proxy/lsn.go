package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
)

// startLSNPolling periodically queries each reader's replay LSN and updates the balancer.
func (s *Server) startLSNPolling(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollReaderLSNs(ctx)
			}
		}
	}()
	slog.Info("LSN polling started", "interval", interval)
}

// pollReaderLSNs queries each reader's replay LSN and updates the balancer.
func (s *Server) pollReaderLSNs(ctx context.Context) {
	readers := s.getReaderPools()
	for _, addr := range s.balancer.Backends() {
		rPool, ok := readers[addr]
		if !ok {
			continue
		}

		conn, err := rPool.Acquire(ctx)
		if err != nil {
			slog.Debug("LSN poll: acquire reader failed", "addr", addr, "error", err)
			continue
		}

		lsn, err := s.queryReplayLSN(conn)
		if err != nil {
			rPool.Discard(conn)
			slog.Debug("LSN poll: query replay LSN failed, discarding connection", "addr", addr, "error", err)
			continue
		}
		rPool.Release(conn)

		s.balancer.SetReplayLSN(addr, lsn)

		if s.metrics != nil {
			s.metrics.ReaderLSNLag.WithLabelValues(addr).Set(float64(lsn))
		}

		slog.Debug("LSN poll updated", "addr", addr, "replay_lsn", lsn)
	}
}

// queryReplayLSN queries the replay LSN from a reader connection.
func (s *Server) queryReplayLSN(readerConn net.Conn) (router.LSN, error) {
	payload := append([]byte("SELECT pg_last_wal_replay_lsn()"), 0)
	if err := protocol.WriteMessage(readerConn, protocol.MsgQuery, payload); err != nil {
		return 0, fmt.Errorf("send replay LSN query: %w", err)
	}

	var lsnStr string
	for {
		msg, err := protocol.ReadMessage(readerConn)
		if err != nil {
			return 0, fmt.Errorf("read replay LSN response: %w", err)
		}
		if msg.Type == protocol.MsgDataRow && len(msg.Payload) >= 6 {
			colLen := int(binary.BigEndian.Uint32(msg.Payload[2:6]))
			if colLen > 0 && 6+colLen <= len(msg.Payload) {
				lsnStr = string(msg.Payload[6 : 6+colLen])
			}
		}
		if msg.Type == protocol.MsgErrorResponse {
			return 0, fmt.Errorf("replay LSN query returned error")
		}
		if msg.Type == protocol.MsgReadyForQuery {
			break
		}
	}

	if lsnStr == "" {
		return 0, fmt.Errorf("no replay LSN value returned")
	}
	return router.ParseLSN(lsnStr)
}
