package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
	"github.com/jyukki97/pgmux/internal/telemetry"
)

// relayQueries handles the main query loop with transaction-level connection pooling.
// Writer connections are acquired from writerPool per query/transaction and released back.
func (s *Server) relayQueries(ctx context.Context, clientConn net.Conn, session *router.Session, ct *cancelTarget, dbg *DatabaseGroup) {
	// boundWriter is non-nil when a transaction is in progress.
	// The connection stays bound from BEGIN until COMMIT/ROLLBACK.
	var boundWriter *pool.Conn
	// boundWriterPool tracks the pool from which boundWriter was acquired.
	// On config reload, dbg.writerPool may be replaced with a new pool.
	// We must Release/Discard to the original pool to avoid cross-pool contamination.
	var boundWriterPool *pool.Pool
	// connDirty tracks if the current borrow cycle has seen session-modifying commands
	// (SET, PREPARE, LISTEN, CREATE TEMP, etc.) that require DISCARD ALL on release.
	var connDirty bool

	defer func() {
		if boundWriter != nil {
			if connDirty {
				s.resetAndReleaseToPool(boundWriter, boundWriterPool)
			} else {
				releaseToPool(boundWriter, boundWriterPool)
			}
		}
	}()

	// Reusable read buffer for client messages (ReadMessageReuse).
	// Pre-allocated to avoid initial growth allocation (pprof: 20% of allocs).
	readBuf := make([]byte, 0, 512)

	// Extended Query protocol state
	var extBuf []*protocol.Message
	var extRoute router.Route
	var extTxStart, extTxEnd bool

	// Multiplexing mode: synthesizer for Prepared Statement → Simple Query conversion
	multiplexMode := s.getConfig().Pool.PreparedStatementMode == "multiplex"
	var synth *Synthesizer
	var muxBindDetail *protocol.BindMessageDetail // current Bind for synthesis
	if multiplexMode {
		synth = NewSynthesizer()
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set idle timeout deadline on client read.
		// Only apply when not in a transaction (boundWriter == nil).
		if idleTimeout := s.getConfig().Proxy.ClientIdleTimeout; idleTimeout > 0 && boundWriter == nil {
			_ = clientConn.SetReadDeadline(time.Now().Add(idleTimeout))
		} else {
			_ = clientConn.SetReadDeadline(time.Time{}) // clear deadline
		}

		var msg *protocol.Message
		var err error
		msg, readBuf, err = protocol.ReadMessageReuse(clientConn, readBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Info("client idle timeout", "remote", clientConn.RemoteAddr(),
					"timeout", s.getConfig().Proxy.ClientIdleTimeout)
				if s.metrics != nil {
					s.metrics.ClientIdleTimeouts.Inc()
				}
				s.sendFatalWithCode(clientConn, "57P01", "terminating connection due to idle timeout")
				return
			}
			slog.Debug("client disconnected", "error", err)
			return
		}

		if msg.Type == protocol.MsgTerminate {
			slog.Info("client terminated", "remote", clientConn.RemoteAddr())
			return
		}

		// Rate limit check
		if rl := s.getRateLimiter(); rl != nil && !rl.Allow() {
			slog.Warn("rate limited", "remote", clientConn.RemoteAddr())
			if s.metrics != nil {
				s.metrics.RateLimited.Inc()
			}
			s.sendError(clientConn, "too many requests")
			// Send ReadyForQuery so the client can continue
			_ = protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'})
			continue
		}

		// --- Simple Query Protocol ---
		if msg.Type == protocol.MsgQuery {
			query := protocol.ExtractQueryText(msg.Payload)

			// Cache config once per query to avoid repeated RLock
			queryCfg := s.getConfig()
			tracingEnabled := queryCfg.Telemetry.Enabled

			// Start root span for query (only allocate attributes when tracing)
			var queryCtx context.Context
			var querySpan trace.Span
			if tracingEnabled {
				queryCtx, querySpan = telemetry.Tracer().Start(ctx, "pgmux.query",
					trace.WithAttributes(
						attribute.String("db.system", "postgresql"),
						attribute.String("db.statement", truncateSQL(query)),
					),
				)
			} else {
				queryCtx = ctx
			}

			// Pre-parse AST once when AST mode is enabled
			var parsedQuery *router.ParsedQuery
			if queryCfg.Routing.ASTParser {
				if pq, err := router.NewParsedQuery(query); err == nil {
					parsedQuery = pq
				}
			}

			// Firewall check
			if queryCfg.Firewall.Enabled {
				var fwResult router.FirewallResult
				fwCfg := router.FirewallConfig{
					Enabled:                 queryCfg.Firewall.Enabled,
					BlockDeleteWithoutWhere: queryCfg.Firewall.BlockDeleteWithoutWhere,
					BlockUpdateWithoutWhere: queryCfg.Firewall.BlockUpdateWithoutWhere,
					BlockDropTable:          queryCfg.Firewall.BlockDropTable,
					BlockTruncate:           queryCfg.Firewall.BlockTruncate,
				}
				if parsedQuery != nil {
					fwResult = router.CheckFirewallWithTree(parsedQuery, fwCfg)
				} else {
					fwResult = router.CheckFirewall(query, fwCfg)
				}
				if fwResult.Blocked {
					slog.Warn("firewall blocked query", "rule", fwResult.Rule, "sql", query)
					if s.metrics != nil {
						s.metrics.FirewallBlocked.WithLabelValues(string(fwResult.Rule)).Inc()
					}
					if tracingEnabled {
						querySpan.SetAttributes(attribute.String("pgmux.firewall.rule", string(fwResult.Rule)))
						querySpan.SetStatus(codes.Error, "firewall blocked")
						querySpan.End()
					}
					s.sendError(clientConn, fwResult.Message)
					_ = protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'})
					continue
				}
			}

			// Route query + get before/after tx state in single lock acquisition
			route, wasInTx, nowInTx := session.RouteWithTxState(query)
			target := routeName(route)

			// Derive query type from route to avoid redundant classification.
			// Only call classifyQueryParsed for writer-routed queries where we need
			// the distinction between actual writes vs transaction control (BEGIN/COMMIT).
			var qtype router.QueryType
			if route == router.RouteReader {
				qtype = router.QueryRead
			} else {
				qtype = s.classifyQueryParsed(query, parsedQuery)
			}

			if tracingEnabled {
				var dbOp string
				if qtype == router.QueryWrite {
					dbOp = "write"
				} else {
					dbOp = "read"
				}
				querySpan.SetAttributes(
					attribute.String("db.operation", dbOp),
					attribute.String("pgmux.route", target),
				)
			}

			slog.Debug("query routed", "sql", query, "route", target)

			start := time.Now()

			// Resolve query timeout (per-query hint overrides global config)
			queryTimeout := s.resolveQueryTimeout(query, queryCfg)

			if route == router.RouteWriter {
				// Capture the current writerPool reference before acquire.
				// If acquired==true, this is the pool the conn came from.
				// If acquired==false (reusing boundWriter), acquiredPool is unused.
				acquiredPool := dbg.writerPool
				wConn, acquired, err := s.acquireWriterConn(ctx, boundWriter, dbg)
				if err != nil {
					if tracingEnabled {
						querySpan.SetStatus(codes.Error, "acquire writer failed")
						querySpan.End()
					}
					slog.Error("acquire writer", "error", err)
					s.sendError(clientConn, "cannot acquire backend connection")
					return
				}

				ct.setFromConn(dbg.writerAddr, wConn)
				stopTimer := s.startQueryTimer(queryTimeout, ct, target)
				s.handleWriteQuery(clientConn, wConn, msg, query, session, parsedQuery, qtype, queryCfg, dbg)
				if stopTimer != nil {
					stopTimer()
				}
				ct.clear()

				// Track session-modifying queries for dirty flag
				if isSessionModifying(query) {
					connDirty = true
				}

				// Transaction lifecycle management
				switch {
				case !wasInTx && nowInTx:
					// BEGIN — bind writer for transaction duration
					boundWriter = wConn
					boundWriterPool = acquiredPool
				case wasInTx && !nowInTx:
					// COMMIT/ROLLBACK — unbind and release to the pool that issued Acquire
					bwp := boundWriterPool
					boundWriter = nil
					boundWriterPool = nil
					if connDirty {
						s.resetAndReleaseToPool(wConn, bwp)
					} else {
						releaseToPool(wConn, bwp)
					}
					connDirty = false
				case acquired:
					// Single statement outside transaction — release immediately
					// Skip DISCARD ALL unless session state was modified
					if connDirty || isSessionModifying(query) {
						s.resetAndReleaseToPool(wConn, acquiredPool)
						connDirty = false
					} else {
						releaseToPool(wConn, acquiredPool)
					}
				}
				// If !acquired && still in transaction → keep using boundWriter
			} else {
				if err := s.handleReadQueryTraced(queryCtx, ctx, clientConn, msg, query, session, ct, parsedQuery, queryCfg, dbg, queryTimeout); err != nil {
					if tracingEnabled {
						querySpan.SetStatus(codes.Error, err.Error())
						querySpan.End()
					}
					slog.Error("handle read query", "error", err)
					return
				}
			}

			elapsed := time.Since(start)
			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(elapsed.Seconds())
			}
			s.emitAuditEvent(clientConn, query, target, elapsed, false)
			s.recordDigest(query, elapsed)
			s.mirrorQuery(msg, query, qtype, elapsed, parsedQuery)
			if tracingEnabled {
				querySpan.End()
			}
			continue
		}

		// --- Extended Query Protocol ---
		switch msg.Type {
		case protocol.MsgParse:
			if multiplexMode {
				stmtName, query, paramOIDs, err := protocol.ParseParseMessageFull(msg.Payload)
				if err != nil {
					slog.Warn("parse message full failed, falling back", "error", err)
					stmtName, query = protocol.ParseParseMessage(msg.Payload)
				}
				route := session.RegisterStatement(stmtName, query)
				slog.Debug("parse registered (multiplex)", "stmt", stmtName, "sql", query, "route", routeName(route))
				synth.RegisterStatement(stmtName, query, paramOIDs)

				upper := strings.ToUpper(strings.TrimSpace(query))
				if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") {
					extTxStart = true
				}
				if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "END") {
					extTxEnd = true
				}

				if session.InTransaction() || boundWriter != nil || route == router.RouteWriter {
					extRoute = router.RouteWriter
				} else {
					extRoute = route
				}
				// In multiplex mode, we don't buffer Parse — we'll synthesize later
				// But we need to send ParseComplete to the client
				if err := protocol.WriteMessage(clientConn, '1', nil); err != nil {
					slog.Error("send ParseComplete", "error", err)
					return
				}
			} else {
				stmtName, query := protocol.ParseParseMessage(msg.Payload)
				route := session.RegisterStatement(stmtName, query)
				slog.Debug("parse registered", "stmt", stmtName, "sql", query, "route", routeName(route))

				upper := strings.ToUpper(strings.TrimSpace(query))
				if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") {
					extTxStart = true
				}
				if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "END") {
					extTxEnd = true
				}

				if session.InTransaction() || boundWriter != nil || route == router.RouteWriter {
					extRoute = router.RouteWriter
				} else {
					extRoute = route
				}
				extBuf = append(extBuf, protocol.CopyMessage(msg))
			}

		case protocol.MsgBind:
			if multiplexMode {
				detail, err := protocol.ParseBindMessageFull(msg.Payload)
				if err != nil {
					slog.Error("parse bind message full failed", "error", err)
					s.sendError(clientConn, fmt.Sprintf("invalid bind message: %v", err))
					return
				}
				route := session.StatementRoute(detail.StatementName)
				if route == router.RouteWriter {
					extRoute = router.RouteWriter
				}
				muxBindDetail = detail
				// Send BindComplete to client
				if err := protocol.WriteMessage(clientConn, '2', nil); err != nil {
					slog.Error("send BindComplete", "error", err)
					return
				}
			} else {
				_, stmtName := protocol.ParseBindMessage(msg.Payload)
				route := session.StatementRoute(stmtName)
				if route == router.RouteWriter {
					extRoute = router.RouteWriter
				}
				extBuf = append(extBuf, protocol.CopyMessage(msg))
			}

		case protocol.MsgClose:
			closeType, name := protocol.ParseCloseMessage(msg.Payload)
			if closeType == 'S' {
				session.CloseStatement(name)
				if multiplexMode {
					synth.CloseStatement(name)
				}
			}
			if multiplexMode {
				// Send CloseComplete to client
				if err := protocol.WriteMessage(clientConn, '3', nil); err != nil {
					slog.Error("send CloseComplete", "error", err)
					return
				}
			} else {
				extBuf = append(extBuf, protocol.CopyMessage(msg))
			}

		case protocol.MsgDescribe:
			if multiplexMode {
				// In multiplex mode, handle Describe by forwarding to backend
				if err := s.handleMultiplexDescribe(ctx, clientConn, msg, synth, boundWriter, ct, dbg); err != nil {
					slog.Error("multiplex describe", "error", err)
					return
				}
			} else {
				extBuf = append(extBuf, protocol.CopyMessage(msg))
			}

		case protocol.MsgExecute:
			if multiplexMode {
				// In multiplex mode, Execute is handled in Sync
				// (the synthesized query already replaces Parse+Bind+Execute)
			} else {
				extBuf = append(extBuf, protocol.CopyMessage(msg))
			}

		case protocol.MsgSync:
			start := time.Now()
			target := routeName(extRoute)
			extQueryTimeout := s.getConfig().Pool.QueryTimeout

			// Start root span for extended query batch
			extCtx, extSpan := telemetry.Tracer().Start(ctx, "pgmux.extended_query",
				trace.WithAttributes(
					attribute.String("db.system", "postgresql"),
					attribute.String("pgmux.route", target),
				),
			)

			if multiplexMode && muxBindDetail != nil {
				// Multiplex mode: synthesize Simple Query from Parse+Bind
				synthesized, synthErr := synth.Synthesize(
					muxBindDetail.StatementName,
					muxBindDetail.Parameters,
					muxBindDetail.FormatCodes,
				)
				if synthErr != nil {
					extSpan.SetStatus(codes.Error, synthErr.Error())
					extSpan.End()
					slog.Error("synthesize query failed", "error", synthErr)
					s.sendError(clientConn, fmt.Sprintf("query synthesis failed: %v", synthErr))
					// Send ReadyForQuery to keep client in sync
					s.sendReadyForQuery(clientConn, session.InTransaction())
					extBuf = extBuf[:0]
					extRoute = router.RouteReader
					extTxStart, extTxEnd = false, false
					muxBindDetail = nil
					continue
				}

				slog.Debug("synthesized query", "sql", synthesized, "route", target)
				extSpan.SetAttributes(attribute.String("db.statement", truncateStr(synthesized, 100)))

				if err := s.executeSynthesizedQuery(extCtx, clientConn, synthesized, extRoute, session, &boundWriter, &boundWriterPool, extTxStart, extTxEnd, ct, dbg); err != nil {
					extSpan.SetStatus(codes.Error, err.Error())
					extSpan.End()
					slog.Error("execute synthesized query", "error", err)
					return
				}
			} else if multiplexMode && muxBindDetail == nil {
				// Multiplex mode but no Bind (e.g., Parse-only or empty batch)
				// Just send ReadyForQuery
				s.sendReadyForQuery(clientConn, session.InTransaction())
			} else if extRoute == router.RouteReader && !session.InTransaction() && boundWriter == nil {
				// Reader path (proxy mode)
				readerAddr := dbg.balancer.Next()
				if err := s.handleExtendedRead(extCtx, clientConn, extBuf, msg, readerAddr, ct, dbg, extQueryTimeout); err != nil {
					extSpan.SetStatus(codes.Error, err.Error())
					extSpan.End()
					slog.Error("extended read query", "error", err)
					return
				}
			} else {
				// Writer path (proxy mode) — acquire from pool or use bound connection
				acquiredPool := dbg.writerPool
				_, acquireSpan := telemetry.Tracer().Start(extCtx, "pgmux.pool.acquire",
					trace.WithAttributes(attribute.String("pgmux.route", "writer")),
				)
				wConn, acquired, err := s.acquireWriterConn(ctx, boundWriter, dbg)
				if err != nil {
					acquireSpan.SetStatus(codes.Error, err.Error())
					acquireSpan.End()
					extSpan.SetStatus(codes.Error, "acquire writer failed")
					extSpan.End()
					slog.Error("acquire writer for extended query", "error", err)
					s.sendError(clientConn, "cannot acquire backend connection")
					return
				}
				acquireSpan.End()

				// Backend exec span
				_, execSpan := telemetry.Tracer().Start(extCtx, "pgmux.backend.exec",
					trace.WithAttributes(attribute.String("pgmux.route", "writer")),
				)

				// Forward all buffered messages + Sync to writer
				ct.setFromConn(dbg.writerAddr, wConn)
				stopTimer := s.startQueryTimer(extQueryTimeout, ct, "writer")
				writeErr := s.forwardExtBatch(wConn, extBuf, msg)
				if writeErr != nil {
					if stopTimer != nil {
						stopTimer()
					}
					ct.clear()
					execSpan.SetStatus(codes.Error, writeErr.Error())
					execSpan.End()
					extSpan.SetStatus(codes.Error, "forward ext batch failed")
					extSpan.End()
					slog.Error("forward ext batch to writer", "error", writeErr)
					if acquired {
						discardToPool(wConn, acquiredPool)
					} else if boundWriter != nil {
						discardToPool(boundWriter, boundWriterPool)
						boundWriter = nil
						boundWriterPool = nil
					}
					return
				}

				if err := s.relayUntilReady(clientConn, wConn); err != nil {
					if stopTimer != nil {
						stopTimer()
					}
					ct.clear()
					execSpan.SetStatus(codes.Error, err.Error())
					execSpan.End()
					extSpan.SetStatus(codes.Error, "relay writer response failed")
					extSpan.End()
					slog.Error("relay writer response (sync)", "error", err)
					if acquired {
						discardToPool(wConn, acquiredPool)
					} else if boundWriter != nil {
						discardToPool(boundWriter, boundWriterPool)
						boundWriter = nil
						boundWriterPool = nil
					}
					return
				}
				if stopTimer != nil {
					stopTimer()
				}
				ct.clear()
				execSpan.End()

				// Update transaction state for Extended Query
				if extTxStart {
					session.SetInTransaction(true)
				}
				if extTxEnd {
					session.SetInTransaction(false)
				}

				// Transaction lifecycle
				switch {
				case extTxStart && !extTxEnd:
					// BEGIN — bind writer
					boundWriter = wConn
					boundWriterPool = acquiredPool
				case extTxEnd:
					// COMMIT/ROLLBACK — unbind and release to origin pool
					bwp := boundWriterPool
					boundWriter = nil
					boundWriterPool = nil
					if connDirty {
						s.resetAndReleaseToPool(wConn, bwp)
					} else {
						releaseToPool(wConn, bwp)
					}
					connDirty = false
				case acquired:
					// Single batch outside transaction — release
					if connDirty {
						s.resetAndReleaseToPool(wConn, acquiredPool)
						connDirty = false
					} else {
						releaseToPool(wConn, acquiredPool)
					}
				}
			}

			elapsed := time.Since(start)
			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(elapsed.Seconds())
			}
			s.emitAuditEvent(clientConn, "(extended query)", target, elapsed, false)
			s.recordDigest("(extended query)", elapsed)
			extSpan.End()

			// Reset batch state
			extBuf = extBuf[:0]
			extRoute = router.RouteReader
			extTxStart, extTxEnd = false, false
			muxBindDetail = nil

		default:
			// Other messages — buffer them (proxy mode only)
			if !multiplexMode {
				extBuf = append(extBuf, protocol.CopyMessage(msg))
			}
		}
	}
}
