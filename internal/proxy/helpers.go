package proxy

import (
	"hash/fnv"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/digest"
	"github.com/jyukki97/pgmux/internal/mirror"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
)

// sendReadyForQuery sends a ReadyForQuery ('Z') message to the client.
func (s *Server) sendReadyForQuery(conn net.Conn, inTransaction bool) {
	var status byte = 'I' // idle
	if inTransaction {
		status = 'T' // in transaction
	}
	_ = protocol.WriteMessage(conn, protocol.MsgReadyForQuery, []byte{status})
}

// truncateStr truncates a string to maxLen characters.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (s *Server) sendError(conn net.Conn, msg string) {
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, []byte("ERROR")...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, []byte(msg)...)
	payload = append(payload, 0)
	payload = append(payload, 0) // terminator
	_ = protocol.WriteMessage(conn, protocol.MsgErrorResponse, payload)
}

// sendFatalWithCode sends a FATAL ErrorResponse with SQLSTATE code.
// Used for connection-level rejection (e.g., too_many_connections = 53300).
func (s *Server) sendFatalWithCode(conn net.Conn, code, msg string) {
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, []byte("FATAL")...)
	payload = append(payload, 0)
	payload = append(payload, 'C')
	payload = append(payload, []byte(code)...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, []byte(msg)...)
	payload = append(payload, 0)
	payload = append(payload, 0) // terminator
	_ = protocol.WriteMessage(conn, protocol.MsgErrorResponse, payload)
}

// sendErrorWithCode sends an ErrorResponse with a SQLSTATE code.
func (s *Server) sendErrorWithCode(conn net.Conn, code, msg string) {
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, []byte("ERROR")...)
	payload = append(payload, 0)
	payload = append(payload, 'C')
	payload = append(payload, []byte(code)...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, []byte(msg)...)
	payload = append(payload, 0)
	payload = append(payload, 0) // terminator
	_ = protocol.WriteMessage(conn, protocol.MsgErrorResponse, payload)
}

// resolveQueryTimeout returns the effective query timeout for a query.
// Per-query hint overrides the global config. Returns 0 if no timeout applies.
func (s *Server) resolveQueryTimeout(query string, cfg *config.Config) time.Duration {
	if hint := router.ExtractTimeoutHint(query); hint > 0 {
		return hint
	}
	return cfg.Pool.QueryTimeout
}

// startQueryTimer starts a timer that sends a CancelRequest to the backend
// when the timeout expires. Returns a stop function that must be called to
// prevent the timer from firing (e.g., when the query completes normally).
// Returns nil if timeout is 0 (disabled).
func (s *Server) startQueryTimer(timeout time.Duration, ct *cancelTarget, target string) func() {
	if timeout <= 0 {
		return nil
	}
	timer := time.AfterFunc(timeout, func() {
		addr, pid, secret := ct.get()
		if addr == "" || pid == 0 {
			return
		}
		slog.Warn("query timeout exceeded, sending cancel request",
			"timeout", timeout, "backend_pid", pid, "backend_addr", addr)
		if s.metrics != nil {
			s.metrics.QueryTimeouts.WithLabelValues(target).Inc()
		}
		if err := forwardCancel(addr, pid, secret); err != nil {
			slog.Warn("query timeout cancel failed", "error", err)
		}
	})
	return func() { timer.Stop() }
}

// cacheKey uses semantic or plain cache key based on config, mixed with dbName for multi-DB isolation.
func (s *Server) cacheKey(query string, dbName string) uint64 {
	var key uint64
	if s.getConfig().Routing.ASTParser {
		key = cache.SemanticCacheKey(query)
	} else {
		key = cache.CacheKey(query)
	}
	return mixDBName(key, dbName)
}

// cacheKeyParsed uses a pre-parsed AST tree for semantic cache key generation, mixed with dbName.
func (s *Server) cacheKeyParsed(query string, pq *router.ParsedQuery, dbName string) uint64 {
	var key uint64
	if s.getConfig().Routing.ASTParser && pq != nil {
		key = cache.SemanticCacheKeyWithTree(pq.Tree, query)
	} else if s.getConfig().Routing.ASTParser {
		key = cache.SemanticCacheKey(query)
	} else {
		key = cache.CacheKey(query)
	}
	return mixDBName(key, dbName)
}

// mixDBName XORs the cache key with a hash of the database name to isolate per-DB caches.
func mixDBName(key uint64, dbName string) uint64 {
	if dbName == "" {
		return key
	}
	h := fnv.New64a()
	h.Write([]byte(dbName))
	return key ^ h.Sum64()
}

// classifyQuery uses AST or string parser based on config.
func (s *Server) classifyQuery(query string) router.QueryType {
	if s.getConfig().Routing.ASTParser {
		return router.ClassifyAST(query)
	}
	return router.Classify(query)
}

// classifyQueryParsed uses a pre-parsed AST tree for query classification.
func (s *Server) classifyQueryParsed(query string, pq *router.ParsedQuery) router.QueryType {
	if s.getConfig().Routing.ASTParser && pq != nil {
		return router.ClassifyASTWithTree(query, pq)
	}
	return s.classifyQuery(query)
}

// extractQueryTables uses AST or string parser based on config.
func (s *Server) extractQueryTables(query string) []string {
	if s.getConfig().Routing.ASTParser {
		return router.ExtractTablesAST(query)
	}
	return router.ExtractTables(query)
}

// extractReadQueryTables uses AST or string parser based on config to extract tables from read queries.
func (s *Server) extractReadQueryTables(query string) []string {
	if s.getConfig().Routing.ASTParser {
		return router.ExtractReadTablesAST(query)
	}
	return router.ExtractReadTables(query)
}

// extractReadQueryTablesParsed uses a pre-parsed AST tree for read table extraction.
func (s *Server) extractReadQueryTablesParsed(query string, pq *router.ParsedQuery) []string {
	if s.getConfig().Routing.ASTParser && pq != nil {
		return router.ExtractReadTablesASTWithTree(pq)
	}
	return s.extractReadQueryTables(query)
}

// extractQueryTablesParsed uses a pre-parsed AST tree for table extraction.
func (s *Server) extractQueryTablesParsed(query string, pq *router.ParsedQuery) []string {
	if s.getConfig().Routing.ASTParser && pq != nil {
		return router.ExtractTablesASTWithTree(pq)
	}
	return s.extractQueryTables(query)
}

// truncateSQL returns the first 100 characters of a SQL statement for span attributes.
func truncateSQL(sql string) string {
	if len(sql) > 100 {
		return sql[:100]
	}
	return sql
}

func routeName(r router.Route) string {
	if r == router.RouteWriter {
		return "writer"
	}
	return "reader"
}

// parseSize converts a size string like "512KB" or "1MB" to bytes.
func parseSize(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	if strings.HasSuffix(s, "MB") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "MB"))
		return n * 1024 * 1024
	}
	if strings.HasSuffix(s, "KB") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "KB"))
		return n * 1024
	}
	n, _ := strconv.Atoi(s)
	return n
}

// emitAuditEvent sends a query audit event to the audit logger if enabled.
func (s *Server) emitAuditEvent(clientConn net.Conn, query, target string, elapsed time.Duration, cached bool) {
	if s.auditLogger == nil {
		return
	}

	durationMS := float64(elapsed.Microseconds()) / 1000.0

	// Record slow query metric
	auditCfg := s.getConfig()
	if s.metrics != nil && durationMS >= float64(auditCfg.Audit.SlowQueryThreshold.Milliseconds()) {
		s.metrics.SlowQueries.WithLabelValues(target).Inc()
	}

	sourceIP := ""
	if addr := clientConn.RemoteAddr(); addr != nil {
		sourceIP = addr.String()
	}

	s.auditLogger.Log(audit.Event{
		Timestamp:  time.Now(),
		User:       auditCfg.Backend.User,
		SourceIP:   sourceIP,
		Query:      query,
		DurationMS: durationMS,
		Target:     target,
		Cached:     cached,
	})
}

// AuditLogger returns the audit logger for external access (e.g., admin API).
func (s *Server) AuditLogger() *audit.Logger {
	return s.auditLogger
}

// QueryMirror returns the server's query mirror (may be nil if disabled).
func (s *Server) QueryMirror() *mirror.Mirror {
	return s.mirror
}

// QueryDigest returns the server's query digest (may be nil if disabled).
func (s *Server) QueryDigest() *digest.Digest {
	return s.queryDigest
}

// recordDigest records a query execution in the digest collector.
func (s *Server) recordDigest(query string, elapsed time.Duration) {
	if s.queryDigest == nil {
		return
	}
	s.queryDigest.Record(query, elapsed)
	if s.metrics != nil {
		s.metrics.DigestPatterns.Set(float64(s.queryDigest.PatternCount()))
	}
}

// mirrorQuery sends a query to the mirror target if configured and matching filters.
func (s *Server) mirrorQuery(msg *protocol.Message, query string, qtype router.QueryType, elapsed time.Duration, pq *router.ParsedQuery) {
	if s.mirror == nil {
		return
	}
	if s.mirror.IsReadOnly() && qtype == router.QueryWrite {
		return
	}
	if s.mirror.MatchesTables(s.extractQueryTablesParsed(query, pq)) {
		s.mirror.Send(msg.Type, msg.Payload, query, elapsed)
	}
}

