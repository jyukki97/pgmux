package proxy

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/router"
)

// sendReadyForQuery sends a ReadyForQuery ('Z') message to the client.
func (s *Server) sendReadyForQuery(conn net.Conn, inTransaction bool) {
	var status byte = 'I' // idle
	if inTransaction {
		status = 'T' // in transaction
	}
	protocol.WriteMessage(conn, protocol.MsgReadyForQuery, []byte{status})
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
	protocol.WriteMessage(conn, protocol.MsgErrorResponse, payload)
}

// cacheKey uses semantic or plain cache key based on config.
func (s *Server) cacheKey(query string) uint64 {
	if s.getConfig().Routing.ASTParser {
		return cache.SemanticCacheKey(query)
	}
	return cache.CacheKey(query)
}

// classifyQuery uses AST or string parser based on config.
func (s *Server) classifyQuery(query string) router.QueryType {
	if s.getConfig().Routing.ASTParser {
		return router.ClassifyAST(query)
	}
	return router.Classify(query)
}

// extractQueryTables uses AST or string parser based on config.
func (s *Server) extractQueryTables(query string) []string {
	if s.getConfig().Routing.ASTParser {
		return router.ExtractTablesAST(query)
	}
	return router.ExtractTables(query)
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

