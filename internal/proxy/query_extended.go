package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
	"github.com/jyukki97/pgmux/internal/telemetry"
)

// handleExtendedRead sends buffered Extended Query messages to a reader, falling back to writer.
func (s *Server) handleExtendedRead(ctx context.Context, clientConn net.Conn, buf []*protocol.Message, syncMsg *protocol.Message, readerAddr string) error {
	// Fallback helper: send entire batch to writer via pool
	fallbackToWriter := func() error {
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		wConn, err := s.writerPool.Acquire(ctx)
		if err != nil {
			s.sendError(clientConn, "no available backend connections")
			return fmt.Errorf("acquire writer for ext fallback: %w", err)
		}
		if err := s.forwardExtBatch(wConn, buf, syncMsg); err != nil {
			s.writerPool.Discard(wConn)
			return fmt.Errorf("forward ext to writer: %w", err)
		}
		if err := s.relayUntilReady(clientConn, wConn); err != nil {
			s.writerPool.Discard(wConn)
			return err
		}
		s.resetAndReleaseWriter(wConn)
		return nil
	}

	if readerAddr == "" {
		slog.Warn("no healthy reader for extended query, fallback to writer")
		return fallbackToWriter()
	}

	rPool, ok := s.getReaderPool(readerAddr)
	if !ok {
		slog.Warn("no pool for reader, fallback to writer", "addr", readerAddr)
		return fallbackToWriter()
	}

	// Pool acquire span
	_, acquireSpan := telemetry.Tracer().Start(ctx, "pgmux.pool.acquire",
		trace.WithAttributes(attribute.String("pgmux.route", "reader")),
	)
	acquireStart := time.Now()
	rConn, err := rPool.Acquire(ctx)
	if err != nil {
		acquireSpan.SetStatus(codes.Error, err.Error())
		acquireSpan.End()
		slog.Warn("acquire reader failed for extended query, fallback to writer", "addr", readerAddr, "error", err)
		return fallbackToWriter()
	}
	acquireSpan.End()
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("reader", readerAddr).Inc()
		s.metrics.PoolAcquireDur.WithLabelValues("reader", readerAddr).Observe(time.Since(acquireStart).Seconds())
	}

	// Backend exec span
	_, execSpan := telemetry.Tracer().Start(ctx, "pgmux.backend.exec",
		trace.WithAttributes(attribute.String("pgmux.route", "reader")),
	)

	// Forward all buffered messages + Sync to reader
	if err := s.forwardExtBatch(rConn, buf, syncMsg); err != nil {
		execSpan.SetStatus(codes.Error, err.Error())
		execSpan.End()
		slog.Error("forward ext to reader", "addr", readerAddr, "error", err)
		rPool.Discard(rConn)
		return fallbackToWriter()
	}

	// Relay response from reader (with optional caching)
	if s.queryCache != nil {
		collected, err := s.relayAndCollect(clientConn, rConn)
		rPool.Release(rConn)
		execSpan.End()
		if err != nil {
			return fmt.Errorf("relay reader extended response: %w", err)
		}
		// Cache the response keyed by the batch (first Parse query), skip if oversize
		if collected != nil && len(buf) > 0 && buf[0].Type == protocol.MsgParse {
			_, query := protocol.ParseParseMessage(buf[0].Payload)
			key := s.cacheKey(query)
			s.queryCache.Set(key, collected, nil)
			if s.metrics != nil {
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
		}
	} else {
		if err := s.relayUntilReady(clientConn, rConn); err != nil {
			execSpan.SetStatus(codes.Error, err.Error())
			execSpan.End()
			rPool.Discard(rConn)
			return fmt.Errorf("relay reader extended response: %w", err)
		}
		rPool.Release(rConn)
		execSpan.End()
	}

	return nil
}

// forwardExtBatch sends a batch of Extended Query messages followed by a Sync message.
func (s *Server) forwardExtBatch(backendConn net.Conn, buf []*protocol.Message, syncMsg *protocol.Message) error {
	for _, m := range buf {
		if err := protocol.WriteMessage(backendConn, m.Type, m.Payload); err != nil {
			return fmt.Errorf("forward ext message: %w", err)
		}
	}
	if err := protocol.WriteMessage(backendConn, syncMsg.Type, syncMsg.Payload); err != nil {
		return fmt.Errorf("forward sync: %w", err)
	}
	return nil
}

// executeSynthesizedQuery executes a synthesized Simple Query on the appropriate backend.
func (s *Server) executeSynthesizedQuery(ctx context.Context, clientConn net.Conn, query string, route router.Route, session *router.Session, boundWriter **pool.Conn, extTxStart, extTxEnd bool) error {
	// Build Simple Query message
	queryPayload := append([]byte(query), 0)

	if route == router.RouteReader && !session.InTransaction() && *boundWriter == nil {
		// Reader path
		readerAddr := s.balancer.Next()
		return s.handleSynthesizedRead(ctx, clientConn, queryPayload, readerAddr)
	}

	// Writer path
	wConn, acquired, err := s.acquireWriterConn(ctx, *boundWriter)
	if err != nil {
		s.sendError(clientConn, "cannot acquire backend connection")
		return fmt.Errorf("acquire writer for synthesized query: %w", err)
	}

	if err := protocol.WriteMessage(wConn, protocol.MsgQuery, queryPayload); err != nil {
		if acquired {
			s.writerPool.Discard(wConn)
		}
		return fmt.Errorf("send synthesized query: %w", err)
	}

	if err := s.relayUntilReady(clientConn, wConn); err != nil {
		if acquired {
			s.writerPool.Discard(wConn)
		} else if *boundWriter != nil {
			s.writerPool.Discard(*boundWriter)
			*boundWriter = nil
		}
		return fmt.Errorf("relay synthesized response: %w", err)
	}

	// Update transaction state
	if extTxStart {
		session.SetInTransaction(true)
	}
	if extTxEnd {
		session.SetInTransaction(false)
	}

	switch {
	case extTxStart && !extTxEnd:
		*boundWriter = wConn
	case extTxEnd:
		*boundWriter = nil
		s.resetAndReleaseWriter(wConn)
	case acquired:
		s.resetAndReleaseWriter(wConn)
	}
	return nil
}

// handleSynthesizedRead sends a synthesized Simple Query to a reader.
func (s *Server) handleSynthesizedRead(ctx context.Context, clientConn net.Conn, queryPayload []byte, readerAddr string) error {
	fallbackToWriter := func() error {
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		wConn, err := s.writerPool.Acquire(ctx)
		if err != nil {
			s.sendError(clientConn, "no available backend connections")
			return fmt.Errorf("acquire writer for synthesized fallback: %w", err)
		}
		if err := protocol.WriteMessage(wConn, protocol.MsgQuery, queryPayload); err != nil {
			s.writerPool.Discard(wConn)
			return fmt.Errorf("send synthesized to writer: %w", err)
		}
		if err := s.relayUntilReady(clientConn, wConn); err != nil {
			s.writerPool.Discard(wConn)
			return err
		}
		s.resetAndReleaseWriter(wConn)
		return nil
	}

	if readerAddr == "" {
		return fallbackToWriter()
	}

	rPool, ok := s.getReaderPool(readerAddr)
	if !ok {
		return fallbackToWriter()
	}

	rConn, err := rPool.Acquire(ctx)
	if err != nil {
		return fallbackToWriter()
	}

	if err := protocol.WriteMessage(rConn, protocol.MsgQuery, queryPayload); err != nil {
		rPool.Discard(rConn)
		return fallbackToWriter()
	}

	if err := s.relayUntilReady(clientConn, rConn); err != nil {
		rPool.Discard(rConn)
		return fmt.Errorf("relay reader synthesized response: %w", err)
	}
	rPool.Release(rConn)
	return nil
}

// handleMultiplexDescribe handles Describe messages in multiplex mode.
// It forwards a temporary Parse → Describe → Close to the backend and relays results.
func (s *Server) handleMultiplexDescribe(ctx context.Context, clientConn net.Conn, msg *protocol.Message, synth *Synthesizer, boundWriter *pool.Conn) error {
	if len(msg.Payload) < 2 {
		s.sendError(clientConn, "invalid describe message")
		return nil
	}
	descType := msg.Payload[0]
	nameEnd := bytes.IndexByte(msg.Payload[1:], 0)
	if nameEnd < 0 {
		s.sendError(clientConn, "invalid describe message")
		return nil
	}
	name := string(msg.Payload[1 : 1+nameEnd])

	if descType == 'P' {
		// Portal describe — not supported in multiplex mode
		s.sendError(clientConn, "portal describe not supported in multiplex mode")
		return nil
	}

	// Statement describe — look up the registered statement
	stmt := synth.GetStatement(name)
	if stmt == nil {
		s.sendError(clientConn, fmt.Sprintf("unknown statement: %q", name))
		return nil
	}

	// Forward to backend: Parse → Describe → Close → Sync, then relay response
	wConn, err := s.writerPool.Acquire(ctx)
	if err != nil {
		s.sendError(clientConn, "cannot acquire backend connection for describe")
		return nil
	}

	// Use a unique temporary statement name to avoid conflicts
	tmpName := "__pgmux_desc_tmp__"

	// Build Parse message
	var parseBuf []byte
	parseBuf = append(parseBuf, []byte(tmpName)...)
	parseBuf = append(parseBuf, 0)
	parseBuf = append(parseBuf, []byte(stmt.Query)...)
	parseBuf = append(parseBuf, 0)
	parseBuf = binary.BigEndian.AppendUint16(parseBuf, 0) // no param type hints

	// Build Describe message
	descBuf := []byte{'S'}
	descBuf = append(descBuf, []byte(tmpName)...)
	descBuf = append(descBuf, 0)

	// Build Close message
	closeBuf := []byte{'S'}
	closeBuf = append(closeBuf, []byte(tmpName)...)
	closeBuf = append(closeBuf, 0)

	// Send Parse → Describe → Close → Sync
	if err := protocol.WriteMessage(wConn, protocol.MsgParse, parseBuf); err != nil {
		s.writerPool.Discard(wConn)
		return fmt.Errorf("send describe parse: %w", err)
	}
	if err := protocol.WriteMessage(wConn, protocol.MsgDescribe, descBuf); err != nil {
		s.writerPool.Discard(wConn)
		return fmt.Errorf("send describe: %w", err)
	}
	if err := protocol.WriteMessage(wConn, protocol.MsgClose, closeBuf); err != nil {
		s.writerPool.Discard(wConn)
		return fmt.Errorf("send describe close: %w", err)
	}
	if err := protocol.WriteMessage(wConn, protocol.MsgSync, nil); err != nil {
		s.writerPool.Discard(wConn)
		return fmt.Errorf("send describe sync: %w", err)
	}

	// Relay responses until ReadyForQuery, skipping ParseComplete and CloseComplete
	for {
		resp, err := protocol.ReadMessage(wConn)
		if err != nil {
			s.writerPool.Discard(wConn)
			return fmt.Errorf("read describe response: %w", err)
		}

		switch resp.Type {
		case '1': // ParseComplete — skip (client already got ours)
			continue
		case '3': // CloseComplete — skip
			continue
		case protocol.MsgReadyForQuery:
			// Don't forward ReadyForQuery — caller handles that
			s.resetAndReleaseWriter(wConn)
			return nil
		default:
			// Forward ParameterDescription, RowDescription, ErrorResponse, etc.
			if err := protocol.WriteMessage(clientConn, resp.Type, resp.Payload); err != nil {
				s.writerPool.Discard(wConn)
				return fmt.Errorf("forward describe response: %w", err)
			}
		}
	}
}
