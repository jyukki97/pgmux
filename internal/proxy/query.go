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
func (s *Server) relayQueries(ctx context.Context, clientConn net.Conn, session *router.Session) {
	// boundWriter is non-nil when a transaction is in progress.
	// The connection stays bound from BEGIN until COMMIT/ROLLBACK.
	var boundWriter *pool.Conn

	defer func() {
		if boundWriter != nil {
			s.resetAndReleaseWriter(boundWriter)
		}
	}()

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

		msg, err := protocol.ReadMessage(clientConn)
		if err != nil {
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
			protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'})
			continue
		}

		// --- Simple Query Protocol ---
		if msg.Type == protocol.MsgQuery {
			query := protocol.ExtractQueryText(msg.Payload)

			// Start root span for query
			queryCtx, querySpan := telemetry.Tracer().Start(ctx, "pgmux.query",
				trace.WithAttributes(
					attribute.String("db.system", "postgresql"),
					attribute.String("db.statement", truncateSQL(query)),
				),
			)

			// Firewall check
			queryCfg := s.getConfig()
			if queryCfg.Firewall.Enabled {
				fwResult := router.CheckFirewall(query, router.FirewallConfig{
					Enabled:                 queryCfg.Firewall.Enabled,
					BlockDeleteWithoutWhere: queryCfg.Firewall.BlockDeleteWithoutWhere,
					BlockUpdateWithoutWhere: queryCfg.Firewall.BlockUpdateWithoutWhere,
					BlockDropTable:          queryCfg.Firewall.BlockDropTable,
					BlockTruncate:           queryCfg.Firewall.BlockTruncate,
				})
				if fwResult.Blocked {
					slog.Warn("firewall blocked query", "rule", fwResult.Rule, "sql", query)
					if s.metrics != nil {
						s.metrics.FirewallBlocked.WithLabelValues(string(fwResult.Rule)).Inc()
					}
					querySpan.SetAttributes(attribute.String("pgmux.firewall.rule", string(fwResult.Rule)))
					querySpan.SetStatus(codes.Error, "firewall blocked")
					querySpan.End()
					s.sendError(clientConn, fwResult.Message)
					protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'})
					continue
				}
			}

			// Parse/classify query
			_, parseSpan := telemetry.Tracer().Start(queryCtx, "pgmux.parse")
			wasInTx := session.InTransaction()
			route := session.Route(query)
			nowInTx := session.InTransaction()
			target := routeName(route)
			qtype := s.classifyQuery(query)
			var dbOp string
			if qtype == router.QueryWrite {
				dbOp = "write"
			} else {
				dbOp = "read"
			}
			parseSpan.SetAttributes(
				attribute.String("db.operation", dbOp),
				attribute.String("pgmux.route", target),
			)
			parseSpan.End()

			querySpan.SetAttributes(
				attribute.String("db.operation", dbOp),
				attribute.String("pgmux.route", target),
			)

			slog.Debug("query routed", "sql", query, "route", target)

			start := time.Now()

			if route == router.RouteWriter {
				// Pool acquire span
				_, acquireSpan := telemetry.Tracer().Start(queryCtx, "pgmux.pool.acquire",
					trace.WithAttributes(attribute.String("pgmux.route", "writer")),
				)
				wConn, acquired, err := s.acquireWriterConn(ctx, boundWriter)
				if err != nil {
					acquireSpan.SetStatus(codes.Error, err.Error())
					acquireSpan.End()
					querySpan.SetStatus(codes.Error, "acquire writer failed")
					querySpan.End()
					slog.Error("acquire writer", "error", err)
					s.sendError(clientConn, "cannot acquire backend connection")
					return
				}
				acquireSpan.End()

				// Backend exec span
				_, execSpan := telemetry.Tracer().Start(queryCtx, "pgmux.backend.exec",
					trace.WithAttributes(attribute.String("pgmux.route", "writer")),
				)
				s.handleWriteQuery(clientConn, wConn, msg, query, session)
				execSpan.End()

				// Transaction lifecycle management
				switch {
				case !wasInTx && nowInTx:
					// BEGIN — bind writer for transaction duration
					boundWriter = wConn
				case wasInTx && !nowInTx:
					// COMMIT/ROLLBACK — unbind and release
					boundWriter = nil
					s.resetAndReleaseWriter(wConn)
				case acquired:
					// Single statement outside transaction — release immediately
					s.resetAndReleaseWriter(wConn)
				}
				// If !acquired && still in transaction → keep using boundWriter
			} else {
				if err := s.handleReadQueryTraced(queryCtx, ctx, clientConn, msg, query, session); err != nil {
					querySpan.SetStatus(codes.Error, err.Error())
					querySpan.End()
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
			querySpan.End()
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
				extBuf = append(extBuf, msg)
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
				extBuf = append(extBuf, msg)
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
				extBuf = append(extBuf, msg)
			}

		case protocol.MsgDescribe:
			if multiplexMode {
				// In multiplex mode, handle Describe by forwarding to backend
				if err := s.handleMultiplexDescribe(ctx, clientConn, msg, synth, boundWriter); err != nil {
					slog.Error("multiplex describe", "error", err)
					return
				}
			} else {
				extBuf = append(extBuf, msg)
			}

		case protocol.MsgExecute:
			if multiplexMode {
				// In multiplex mode, Execute is handled in Sync
				// (the synthesized query already replaces Parse+Bind+Execute)
			} else {
				extBuf = append(extBuf, msg)
			}

		case protocol.MsgSync:
			start := time.Now()
			target := routeName(extRoute)

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

				if err := s.executeSynthesizedQuery(extCtx, clientConn, synthesized, extRoute, session, &boundWriter, extTxStart, extTxEnd); err != nil {
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
				readerAddr := s.balancer.Next()
				if err := s.handleExtendedRead(extCtx, clientConn, extBuf, msg, readerAddr); err != nil {
					extSpan.SetStatus(codes.Error, err.Error())
					extSpan.End()
					slog.Error("extended read query", "error", err)
					return
				}
			} else {
				// Writer path (proxy mode) — acquire from pool or use bound connection
				_, acquireSpan := telemetry.Tracer().Start(extCtx, "pgmux.pool.acquire",
					trace.WithAttributes(attribute.String("pgmux.route", "writer")),
				)
				wConn, acquired, err := s.acquireWriterConn(ctx, boundWriter)
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
				writeErr := s.forwardExtBatch(wConn, extBuf, msg)
				if writeErr != nil {
					execSpan.SetStatus(codes.Error, writeErr.Error())
					execSpan.End()
					extSpan.SetStatus(codes.Error, "forward ext batch failed")
					extSpan.End()
					slog.Error("forward ext batch to writer", "error", writeErr)
					if acquired {
						s.writerPool.Discard(wConn)
					} else if boundWriter != nil {
						s.writerPool.Discard(boundWriter)
						boundWriter = nil
					}
					return
				}

				if err := s.relayUntilReady(clientConn, wConn); err != nil {
					execSpan.SetStatus(codes.Error, err.Error())
					execSpan.End()
					extSpan.SetStatus(codes.Error, "relay writer response failed")
					extSpan.End()
					slog.Error("relay writer response (sync)", "error", err)
					if acquired {
						s.writerPool.Discard(wConn)
					} else if boundWriter != nil {
						s.writerPool.Discard(boundWriter)
						boundWriter = nil
					}
					return
				}
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
				case extTxEnd:
					// COMMIT/ROLLBACK — unbind and release
					boundWriter = nil
					s.resetAndReleaseWriter(wConn)
				case acquired:
					// Single batch outside transaction — release
					s.resetAndReleaseWriter(wConn)
				}
			}

			elapsed := time.Since(start)
			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(elapsed.Seconds())
			}
			s.emitAuditEvent(clientConn, "(extended query)", target, elapsed, false)
			extSpan.End()

			// Reset batch state
			extBuf = extBuf[:0]
			extRoute = router.RouteReader
			extTxStart, extTxEnd = false, false
			muxBindDetail = nil

		default:
			// Other messages — buffer them (proxy mode only)
			if !multiplexMode {
				extBuf = append(extBuf, msg)
			}
		}
	}
}

