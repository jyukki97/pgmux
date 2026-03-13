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

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
	"github.com/jyukki97/pgmux/internal/telemetry"
)

// handleExtendedRead sends buffered Extended Query messages to a reader, falling back to writer.
func (s *Server) handleExtendedRead(ctx context.Context, clientConn net.Conn, buf []*protocol.Message, syncMsg *protocol.Message, readerAddr string, ct *cancelTarget, dbg *DatabaseGroup, queryTimeout ...time.Duration) error {
	// Cache lookup — mirror the simple-query read path (query_read.go)
	// Skip cache entirely for parameterized queries to avoid returning
	// wrong results when bind parameters differ (issue #207).
	// Also skip for binary result format or partial fetch (maxRows≠0)
	// since the cache key does not capture these settings (#217).
	cacheable := !hasParameterPlaceholders(buf) && !hasBinaryFormatOrPartialFetch(buf)
	if cacheable && s.queryCache != nil && len(buf) > 0 && buf[0].Type == protocol.MsgParse {
		_, query := protocol.ParseParseMessage(buf[0].Payload)
		key := cache.WithNamespace(s.cacheKey(query, dbg.name), cache.NSExtended)
		if cached := s.queryCache.Get(key); cached != nil {
			slog.Debug("extended cache hit", "sql", query)
			if s.metrics != nil {
				s.metrics.CacheHits.Inc()
			}
			_, err := clientConn.Write(cached)
			return err
		}
		if s.metrics != nil {
			s.metrics.CacheMisses.Inc()
		}
	}

	var extTimeout time.Duration
	if len(queryTimeout) > 0 {
		extTimeout = queryTimeout[0]
	}
	// Fallback helper: send entire batch to writer via pool.
	// Capture pool reference before Acquire to prevent cross-pool release on hot-reload.
	fallbackToWriter := func() error {
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		wPool := dbg.writerPool // capture before acquire
		wConn, err := wPool.Acquire(ctx)
		if err != nil {
			s.sendError(clientConn, "no available backend connections")
			return fmt.Errorf("acquire writer for ext fallback: %w", err)
		}
		ct.setFromConn(dbg.writerAddr, wConn)
		if err := s.forwardExtBatch(wConn, buf, syncMsg); err != nil {
			ct.clear()
			wPool.Discard(wConn)
			return fmt.Errorf("forward ext to writer: %w", err)
		}
		if err := s.relayUntilReady(clientConn, wConn); err != nil {
			ct.clear()
			wPool.Discard(wConn)
			return err
		}
		ct.clear()
		s.resetAndReleaseToPool(wConn, wPool)
		return nil
	}

	if readerAddr == "" {
		slog.Warn("no healthy reader for extended query, fallback to writer")
		return fallbackToWriter()
	}

	// Circuit breaker check for reader
	if cb, ok := dbg.ReaderCB(readerAddr); ok {
		if err := cb.Allow(); err != nil {
			slog.Warn("reader circuit breaker open for extended query, fallback to writer", "addr", readerAddr)
			if s.metrics != nil {
				s.metrics.ReaderFallback.Inc()
			}
			return fallbackToWriter()
		}
	}

	rPool, ok := dbg.ReaderPool(readerAddr)
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
		if cb, ok := dbg.ReaderCB(readerAddr); ok {
			cb.RecordFailure()
		}
		dbg.balancer.MarkUnhealthy(readerAddr)
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

	ct.setFromConn(readerAddr, rConn)
	stopTimer := s.startQueryTimer(extTimeout, ct, "reader")

	// Forward all buffered messages + Sync to reader
	if err := s.forwardExtBatch(rConn, buf, syncMsg); err != nil {
		if stopTimer != nil {
			stopTimer()
		}
		ct.clear()
		execSpan.SetStatus(codes.Error, err.Error())
		execSpan.End()
		slog.Error("forward ext to reader", "addr", readerAddr, "error", err)
		rPool.Discard(rConn)
		if cb, ok := dbg.ReaderCB(readerAddr); ok {
			cb.RecordFailure()
		}
		dbg.balancer.MarkUnhealthy(readerAddr)
		return fallbackToWriter()
	}

	// Relay response from reader (with optional caching)
	if s.queryCache != nil {
		collected, err := s.relayAndCollect(clientConn, rConn)
		if stopTimer != nil {
			stopTimer()
		}
		ct.clear()
		execSpan.End()
		if err != nil {
			rPool.Discard(rConn)
			if cb, ok := dbg.ReaderCB(readerAddr); ok {
				cb.RecordFailure()
			}
			dbg.balancer.MarkUnhealthy(readerAddr)
			return fmt.Errorf("relay reader extended response: %w", err)
		}
		rPool.Release(rConn)
		// Cache the response keyed by the batch (first Parse query), skip if oversize.
		// Skip parameterized queries — bind params are not part of the key (#207).
		if cacheable && collected != nil && len(buf) > 0 && buf[0].Type == protocol.MsgParse {
			_, query := protocol.ParseParseMessage(buf[0].Payload)
			key := cache.WithNamespace(s.cacheKey(query, dbg.name), cache.NSExtended)
			tables := s.extractReadQueryTables(query)
			s.queryCache.Set(key, collected, tables)
			if s.metrics != nil {
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
		}
	} else {
		if err := s.relayUntilReady(clientConn, rConn); err != nil {
			if stopTimer != nil {
				stopTimer()
			}
			ct.clear()
			execSpan.SetStatus(codes.Error, err.Error())
			execSpan.End()
			rPool.Discard(rConn)
			if cb, ok := dbg.ReaderCB(readerAddr); ok {
				cb.RecordFailure()
			}
			dbg.balancer.MarkUnhealthy(readerAddr)
			return fmt.Errorf("relay reader extended response: %w", err)
		}
		if stopTimer != nil {
			stopTimer()
		}
		ct.clear()
		rPool.Release(rConn)
		execSpan.End()
	}

	if cb, ok := dbg.ReaderCB(readerAddr); ok {
		cb.RecordSuccess()
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
func (s *Server) executeSynthesizedQuery(ctx context.Context, clientConn net.Conn, query string, route router.Route, session *router.Session, boundWriter **pool.Conn, boundWriterPool **pool.Pool, extTxStart, extTxEnd bool, ct *cancelTarget, dbg *DatabaseGroup) error {
	// Build Simple Query message
	queryPayload := append([]byte(query), 0)

	if route == router.RouteReader && !session.InTransaction() && *boundWriter == nil {
		// Reader path
		readerAddr := dbg.balancer.Next()
		return s.handleSynthesizedRead(ctx, clientConn, queryPayload, readerAddr, ct, dbg)
	}

	// Writer path — capture pool reference before acquire
	acquiredPool := dbg.writerPool
	wConn, acquired, err := s.acquireWriterConn(ctx, *boundWriter, dbg)
	if err != nil {
		s.sendError(clientConn, "cannot acquire backend connection")
		return fmt.Errorf("acquire writer for synthesized query: %w", err)
	}

	ct.setFromConn(dbg.writerAddr, wConn)
	if err := protocol.WriteMessage(wConn, protocol.MsgQuery, queryPayload); err != nil {
		ct.clear()
		if acquired {
			discardToPool(wConn, acquiredPool)
		} else {
			// boundWriter write failed — connection is broken, discard it
			discardToPool(wConn, *boundWriterPool)
			*boundWriter = nil
			*boundWriterPool = nil
		}
		return fmt.Errorf("send synthesized query: %w", err)
	}

	if err := s.relayUntilReady(clientConn, wConn); err != nil {
		ct.clear()
		if acquired {
			discardToPool(wConn, acquiredPool)
		} else if *boundWriter != nil {
			discardToPool(*boundWriter, *boundWriterPool)
			*boundWriter = nil
			*boundWriterPool = nil
		}
		return fmt.Errorf("relay synthesized response: %w", err)
	}
	ct.clear()

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
		*boundWriterPool = acquiredPool
	case extTxEnd:
		bwp := *boundWriterPool
		*boundWriter = nil
		*boundWriterPool = nil
		s.resetAndReleaseToPool(wConn, bwp)
	case acquired:
		s.resetAndReleaseToPool(wConn, acquiredPool)
	}
	return nil
}

// handleSynthesizedRead sends a synthesized Simple Query to a reader.
func (s *Server) handleSynthesizedRead(ctx context.Context, clientConn net.Conn, queryPayload []byte, readerAddr string, ct *cancelTarget, dbg *DatabaseGroup) error {
	// Capture pool reference before Acquire to prevent cross-pool release on hot-reload.
	fallbackToWriter := func() error {
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		wPool := dbg.writerPool // capture before acquire
		wConn, err := wPool.Acquire(ctx)
		if err != nil {
			s.sendError(clientConn, "no available backend connections")
			return fmt.Errorf("acquire writer for synthesized fallback: %w", err)
		}
		ct.setFromConn(dbg.writerAddr, wConn)
		if err := protocol.WriteMessage(wConn, protocol.MsgQuery, queryPayload); err != nil {
			ct.clear()
			wPool.Discard(wConn)
			return fmt.Errorf("send synthesized to writer: %w", err)
		}
		if err := s.relayUntilReady(clientConn, wConn); err != nil {
			ct.clear()
			wPool.Discard(wConn)
			return err
		}
		ct.clear()
		s.resetAndReleaseToPool(wConn, wPool)
		return nil
	}

	if readerAddr == "" {
		return fallbackToWriter()
	}

	// Circuit breaker check for reader
	if cb, ok := dbg.ReaderCB(readerAddr); ok {
		if err := cb.Allow(); err != nil {
			slog.Warn("reader circuit breaker open for synthesized query, fallback to writer", "addr", readerAddr)
			if s.metrics != nil {
				s.metrics.ReaderFallback.Inc()
			}
			return fallbackToWriter()
		}
	}

	rPool, ok := dbg.ReaderPool(readerAddr)
	if !ok {
		return fallbackToWriter()
	}

	rConn, err := rPool.Acquire(ctx)
	if err != nil {
		if cb, ok := dbg.ReaderCB(readerAddr); ok {
			cb.RecordFailure()
		}
		dbg.balancer.MarkUnhealthy(readerAddr)
		return fallbackToWriter()
	}

	ct.setFromConn(readerAddr, rConn)
	if err := protocol.WriteMessage(rConn, protocol.MsgQuery, queryPayload); err != nil {
		ct.clear()
		rPool.Discard(rConn)
		if cb, ok := dbg.ReaderCB(readerAddr); ok {
			cb.RecordFailure()
		}
		dbg.balancer.MarkUnhealthy(readerAddr)
		return fallbackToWriter()
	}

	if err := s.relayUntilReady(clientConn, rConn); err != nil {
		ct.clear()
		rPool.Discard(rConn)
		if cb, ok := dbg.ReaderCB(readerAddr); ok {
			cb.RecordFailure()
		}
		dbg.balancer.MarkUnhealthy(readerAddr)
		return fmt.Errorf("relay reader synthesized response: %w", err)
	}
	ct.clear()
	rPool.Release(rConn)
	if cb, ok := dbg.ReaderCB(readerAddr); ok {
		cb.RecordSuccess()
	}
	return nil
}

// hasParameterPlaceholders reports whether the Parse message in buf contains
// $N parameter placeholders. Queries with placeholders must not be cached
// because the cache key does not include bind parameter values (#207).
func hasParameterPlaceholders(buf []*protocol.Message) bool {
	for _, m := range buf {
		if m.Type == protocol.MsgParse {
			_, query := protocol.ParseParseMessage(m.Payload)
			for i := 0; i < len(query)-1; i++ {
				if query[i] == '$' && query[i+1] >= '1' && query[i+1] <= '9' {
					return true
				}
			}
		}
	}
	return false
}

// hasBinaryFormatOrPartialFetch reports whether the batch contains Bind messages
// with binary result format codes or Execute messages with maxRows != 0 (partial fetch).
// Such batches must not be cached because the cache key (Parse SQL text) does not
// capture these settings, and different settings produce different wire responses (#217).
func hasBinaryFormatOrPartialFetch(buf []*protocol.Message) bool {
	for _, m := range buf {
		switch m.Type {
		case protocol.MsgBind:
			detail, err := protocol.ParseBindMessageFull(m.Payload)
			if err != nil {
				return true // can't parse → skip cache to be safe
			}
			for _, fc := range detail.ResultFormatCodes {
				if fc != 0 { // 0 = text, 1 = binary
					return true
				}
			}
		case protocol.MsgExecute:
			// Execute: portal_name\0 + int32(maxRows)
			idx := bytes.IndexByte(m.Payload, 0)
			if idx >= 0 && idx+5 <= len(m.Payload) {
				maxRows := binary.BigEndian.Uint32(m.Payload[idx+1 : idx+5])
				if maxRows != 0 {
					return true
				}
			}
		}
	}
	return false
}

// handleMultiplexDescribe handles Describe messages in multiplex mode.
// It forwards a temporary Parse → Describe → Close to the backend and relays results.
func (s *Server) handleMultiplexDescribe(ctx context.Context, clientConn net.Conn, msg *protocol.Message, synth *Synthesizer, boundWriter *pool.Conn, ct *cancelTarget, dbg *DatabaseGroup) error {
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

	// Forward to backend: Parse → Describe → Close → Sync, then relay response.
	// Capture pool reference before Acquire to prevent cross-pool release on hot-reload.
	wPool := dbg.writerPool // capture before acquire
	wConn, err := wPool.Acquire(ctx)
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
		wPool.Discard(wConn)
		return fmt.Errorf("send describe parse: %w", err)
	}
	if err := protocol.WriteMessage(wConn, protocol.MsgDescribe, descBuf); err != nil {
		wPool.Discard(wConn)
		return fmt.Errorf("send describe: %w", err)
	}
	if err := protocol.WriteMessage(wConn, protocol.MsgClose, closeBuf); err != nil {
		wPool.Discard(wConn)
		return fmt.Errorf("send describe close: %w", err)
	}
	if err := protocol.WriteMessage(wConn, protocol.MsgSync, nil); err != nil {
		wPool.Discard(wConn)
		return fmt.Errorf("send describe sync: %w", err)
	}

	// Relay responses until ReadyForQuery, skipping ParseComplete and CloseComplete
	for {
		resp, err := protocol.ReadMessage(wConn)
		if err != nil {
			wPool.Discard(wConn)
			return fmt.Errorf("read describe response: %w", err)
		}

		switch resp.Type {
		case '1': // ParseComplete — skip (client already got ours)
			continue
		case '3': // CloseComplete — skip
			continue
		case protocol.MsgReadyForQuery:
			// Don't forward ReadyForQuery — caller handles that
			s.resetAndReleaseToPool(wConn, wPool)
			return nil
		default:
			// Forward ParameterDescription, RowDescription, ErrorResponse, etc.
			if err := protocol.WriteMessage(clientConn, resp.Type, resp.Payload); err != nil {
				wPool.Discard(wConn)
				return fmt.Errorf("forward describe response: %w", err)
			}
		}
	}
}
