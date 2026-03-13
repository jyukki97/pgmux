package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
	"github.com/jyukki97/pgmux/internal/telemetry"
)

// handleReadQueryTraced handles read queries with OpenTelemetry child spans for
// cache lookup, pool acquire, backend exec, and cache store.
func (s *Server) handleReadQueryTraced(traceCtx, poolCtx context.Context, clientConn net.Conn, msg *protocol.Message, query string, session *router.Session, ct *cancelTarget, pq *router.ParsedQuery, dbg *DatabaseGroup) error {
	// Cache lookup span
	_, cacheLookupSpan := telemetry.Tracer().Start(traceCtx, "pgmux.cache.lookup")
	if s.queryCache != nil {
		key := s.cacheKeyParsed(query, pq, dbg.name)
		if cached := s.queryCache.Get(key); cached != nil {
			cacheLookupSpan.SetAttributes(attribute.Bool("pgmux.cached", true))
			cacheLookupSpan.End()
			slog.Debug("cache hit", "sql", query)
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
	cacheLookupSpan.SetAttributes(attribute.Bool("pgmux.cached", false))
	cacheLookupSpan.End()

	// Determine reader address
	var readerAddr string
	if s.getConfig().Routing.CausalConsistency {
		minLSN := session.LastWriteLSN()
		readerAddr = dbg.balancer.NextWithLSN(minLSN)
	} else {
		readerAddr = dbg.balancer.Next()
	}
	if readerAddr == "" {
		slog.Warn("no healthy reader, fallback to writer")
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.fallbackToWriter(poolCtx, clientConn, msg, ct, dbg)
	}

	// Circuit breaker check for reader
	if cb, ok := dbg.ReaderCB(readerAddr); ok {
		if err := cb.Allow(); err != nil {
			slog.Warn("reader circuit breaker open, fallback to writer", "addr", readerAddr)
			if s.metrics != nil {
				s.metrics.ReaderFallback.Inc()
			}
			return s.fallbackToWriter(poolCtx, clientConn, msg, ct, dbg)
		}
	}

	rPool, ok := dbg.ReaderPool(readerAddr)
	if !ok {
		slog.Warn("no pool for reader, fallback to writer", "addr", readerAddr)
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.fallbackToWriter(poolCtx, clientConn, msg, ct, dbg)
	}

	// Pool acquire span
	_, acquireSpan := telemetry.Tracer().Start(traceCtx, "pgmux.pool.acquire",
		trace.WithAttributes(attribute.String("pgmux.route", "reader")),
	)
	acquireStart := time.Now()
	rConn, err := rPool.Acquire(poolCtx)
	if err != nil {
		acquireSpan.SetStatus(codes.Error, err.Error())
		acquireSpan.End()
		slog.Warn("acquire reader failed, fallback to writer", "addr", readerAddr, "error", err)
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		if cb, ok := dbg.ReaderCB(readerAddr); ok {
			cb.RecordFailure()
		}
		return s.fallbackToWriter(poolCtx, clientConn, msg, ct, dbg)
	}
	acquireSpan.End()
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("reader", readerAddr).Inc()
		s.metrics.PoolAcquireDur.WithLabelValues("reader", readerAddr).Observe(time.Since(acquireStart).Seconds())
	}

	// Backend exec span
	_, execSpan := telemetry.Tracer().Start(traceCtx, "pgmux.backend.exec",
		trace.WithAttributes(attribute.String("pgmux.route", "reader")),
	)

	ct.setFromConn(readerAddr, rConn)

	// Forward query to reader
	if err := protocol.WriteMessage(rConn, msg.Type, msg.Payload); err != nil {
		ct.clear()
		execSpan.SetStatus(codes.Error, err.Error())
		execSpan.End()
		slog.Error("forward to reader", "addr", readerAddr, "error", err)
		rPool.Discard(rConn)
		return s.fallbackToWriter(poolCtx, clientConn, msg, ct, dbg)
	}

	// Relay response and collect bytes for caching
	if s.queryCache != nil {
		collected, err := s.relayAndCollect(clientConn, rConn)
		ct.clear()
		execSpan.End()
		if err != nil {
			rPool.Discard(rConn)
			if cb, ok := dbg.ReaderCB(readerAddr); ok {
				cb.RecordFailure()
			}
			return fmt.Errorf("relay reader response: %w", err)
		}
		rPool.Release(rConn)
		if collected != nil {
			// Cache store span
			_, storeSpan := telemetry.Tracer().Start(traceCtx, "pgmux.cache.store")
			key := s.cacheKeyParsed(query, pq, dbg.name)
			tables := s.extractReadQueryTablesParsed(query, pq)
			s.queryCache.Set(key, collected, tables)
			if s.metrics != nil {
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
			slog.Debug("cache set", "sql", query, "size", len(collected))
			storeSpan.End()
		}
	} else {
		if err := s.relayUntilReady(clientConn, rConn); err != nil {
			ct.clear()
			execSpan.SetStatus(codes.Error, err.Error())
			execSpan.End()
			rPool.Discard(rConn)
			if cb, ok := dbg.ReaderCB(readerAddr); ok {
				cb.RecordFailure()
			}
			return fmt.Errorf("relay reader response: %w", err)
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
