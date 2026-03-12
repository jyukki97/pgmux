package dataapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/metrics"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/resilience"
	"github.com/jyukki97/pgmux/internal/router"
	"github.com/jyukki97/pgmux/internal/telemetry"
)

// QueryRequest is the HTTP request body for /v1/query.
type QueryRequest struct {
	SQL string `json:"sql"`
}

// QueryResponse is the HTTP response body for /v1/query.
type QueryResponse struct {
	Columns  []string `json:"columns,omitempty"`
	Types    []string `json:"types,omitempty"`
	Rows     [][]any  `json:"rows,omitempty"`
	RowCount int      `json:"row_count"`
	Command  string   `json:"command,omitempty"`
}

// ErrorResponse is the HTTP error response body.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Server is the Data API HTTP server.
type Server struct {
	cfg         *config.Config
	writerPool  *pool.Pool
	readerPools map[string]*pool.Pool
	balancer    *router.RoundRobin
	queryCache  *cache.Cache
	met         *metrics.Metrics
	rateLimiter *resilience.RateLimiter
	apiKeys     map[string]bool
}

// New creates a new Data API server.
func New(cfg *config.Config, writerPool *pool.Pool, readerPools map[string]*pool.Pool, balancer *router.RoundRobin, queryCache *cache.Cache, met *metrics.Metrics, rateLimiter *resilience.RateLimiter) *Server {
	keys := make(map[string]bool, len(cfg.DataAPI.APIKeys))
	for _, k := range cfg.DataAPI.APIKeys {
		keys[k] = true
	}
	return &Server{
		cfg:         cfg,
		writerPool:  writerPool,
		readerPools: readerPools,
		balancer:    balancer,
		queryCache:  queryCache,
		met:         met,
		rateLimiter: rateLimiter,
		apiKeys:     keys,
	}
}

// ListenAndServe starts the Data API HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/query", s.handleQuery)

	slog.Info("data api server starting", "listen", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Extract trace context from HTTP headers (traceparent, etc.)
	propagator := otel.GetTextMapPropagator()
	ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	// Auth check
	if len(s.apiKeys) > 0 {
		token := extractBearerToken(r)
		if token == "" || !s.apiKeys[token] {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	// Rate limit
	if s.rateLimiter != nil && !s.rateLimiter.Allow() {
		if s.met != nil {
			s.met.RateLimited.Inc()
		}
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	// Parse request
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SQL == "" {
		writeError(w, http.StatusBadRequest, "sql is required")
		return
	}

	// Start root span for Data API query
	ctx, querySpan := telemetry.Tracer().Start(ctx, "pgmux.dataapi.query",
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", truncateSQL(req.SQL)),
		),
	)
	defer querySpan.End()

	// Firewall check
	if s.cfg.Firewall.Enabled {
		fwResult := router.CheckFirewall(req.SQL, router.FirewallConfig{
			Enabled:                s.cfg.Firewall.Enabled,
			BlockDeleteWithoutWhere: s.cfg.Firewall.BlockDeleteWithoutWhere,
			BlockUpdateWithoutWhere: s.cfg.Firewall.BlockUpdateWithoutWhere,
			BlockDropTable:          s.cfg.Firewall.BlockDropTable,
			BlockTruncate:           s.cfg.Firewall.BlockTruncate,
		})
		if fwResult.Blocked {
			if s.met != nil {
				s.met.FirewallBlocked.WithLabelValues(string(fwResult.Rule)).Inc()
			}
			querySpan.SetAttributes(attribute.String("pgmux.firewall.rule", string(fwResult.Rule)))
			querySpan.SetStatus(codes.Error, "firewall blocked")
			writeError(w, http.StatusForbidden, fwResult.Message)
			return
		}
	}

	// Classify query
	_, parseSpan := telemetry.Tracer().Start(ctx, "pgmux.parse")
	qtype := s.classifyQuery(req.SQL)
	target := "reader"
	if qtype == router.QueryWrite {
		target = "writer"
	}
	parseSpan.SetAttributes(
		attribute.String("db.operation", target),
		attribute.String("pgmux.route", target),
	)
	parseSpan.End()

	querySpan.SetAttributes(
		attribute.String("db.operation", target),
		attribute.String("pgmux.route", target),
	)

	start := time.Now()

	var resp *QueryResponse
	var err error

	if qtype == router.QueryWrite {
		resp, err = s.executeWrite(ctx, req.SQL)
	} else {
		resp, err = s.executeRead(ctx, req.SQL)
	}

	elapsed := time.Since(start)
	if s.met != nil {
		s.met.QueriesRouted.WithLabelValues(target).Inc()
		s.met.QueryDuration.WithLabelValues(target).Observe(elapsed.Seconds())
	}

	if err != nil {
		querySpan.SetStatus(codes.Error, err.Error())
		slog.Error("data api query error", "sql", req.SQL, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) executeRead(ctx context.Context, sql string) (*QueryResponse, error) {
	// Cache lookup span
	_, cacheLookupSpan := telemetry.Tracer().Start(ctx, "pgmux.cache.lookup")
	if s.queryCache != nil {
		key := s.cacheKey(sql)
		if cached := s.queryCache.Get(key); cached != nil {
			if s.met != nil {
				s.met.CacheHits.Inc()
			}
			cacheLookupSpan.SetAttributes(attribute.Bool("pgmux.cached", true))
			cacheLookupSpan.End()
			// Decode cached JSON response
			var resp QueryResponse
			if err := json.Unmarshal(cached, &resp); err == nil {
				return &resp, nil
			}
		}
		if s.met != nil {
			s.met.CacheMisses.Inc()
		}
	}
	cacheLookupSpan.SetAttributes(attribute.Bool("pgmux.cached", false))
	cacheLookupSpan.End()

	var readerAddr string
	if s.balancer != nil {
		readerAddr = s.balancer.Next()
	}
	if readerAddr == "" {
		return s.executeOnPool(ctx, sql, s.writerPool)
	}

	rPool, ok := s.readerPools[readerAddr]
	if !ok {
		return s.executeOnPool(ctx, sql, s.writerPool)
	}

	resp, err := s.executeOnPool(ctx, sql, rPool)
	if err != nil {
		// Fallback to writer
		if s.met != nil {
			s.met.ReaderFallback.Inc()
		}
		return s.executeOnPool(ctx, sql, s.writerPool)
	}

	// Cache store span
	if s.queryCache != nil && resp != nil {
		_, storeSpan := telemetry.Tracer().Start(ctx, "pgmux.cache.store")
		key := s.cacheKey(sql)
		if data, err := json.Marshal(resp); err == nil {
			tables := s.extractTables(sql)
			s.queryCache.Set(key, data, tables)
			if s.met != nil {
				s.met.CacheEntries.Set(float64(s.queryCache.Len()))
			}
		}
		storeSpan.End()
	}

	return resp, nil
}

func (s *Server) executeWrite(ctx context.Context, sql string) (*QueryResponse, error) {
	resp, err := s.executeOnPool(ctx, sql, s.writerPool)
	if err != nil {
		return nil, err
	}

	// Invalidate cache
	if s.queryCache != nil {
		tables := s.extractTables(sql)
		for _, table := range tables {
			s.queryCache.InvalidateTable(table)
			if s.met != nil {
				s.met.CacheInvalidations.Inc()
				s.met.CacheEntries.Set(float64(s.queryCache.Len()))
			}
		}
	}

	return resp, nil
}

func (s *Server) executeOnPool(ctx context.Context, sql string, p *pool.Pool) (*QueryResponse, error) {
	if p == nil {
		return nil, fmt.Errorf("no connection pool available")
	}

	// Pool acquire span
	_, acquireSpan := telemetry.Tracer().Start(ctx, "pgmux.pool.acquire")
	conn, err := p.Acquire(ctx)
	if err != nil {
		acquireSpan.SetStatus(codes.Error, err.Error())
		acquireSpan.End()
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	acquireSpan.End()

	// Context cancellation watchdog: when ctx is cancelled, set a past deadline
	// on the connection to unblock any blocking protocol.ReadMessage calls.
	var cancelled atomic.Bool
	stopCh := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			conn.SetDeadline(time.Now()) // unblock blocking reads
			slog.Debug("dataapi: context cancelled, forced connection deadline",
				"sql", truncateSQL(sql))
		case <-stopCh:
			// Normal completion — caller signalled us to exit.
		}
	}()

	// stopWatchdog signals the watchdog goroutine to exit and waits for it
	// to finish. Must be called on every non-cancelled path.
	stopWatchdog := func() {
		close(stopCh)
		<-watchdogDone
	}

	// Backend exec span
	_, execSpan := telemetry.Tracer().Start(ctx, "pgmux.backend.exec")
	resp, execErr := executeQuery(conn, sql)

	if cancelled.Load() {
		<-watchdogDone // ensure watchdog goroutine has exited
		execSpan.SetStatus(codes.Error, "context cancelled")
		execSpan.End()
		p.Discard(conn)
		return nil, fmt.Errorf("execute query: %w", ctx.Err())
	}

	if execErr != nil {
		execSpan.SetStatus(codes.Error, execErr.Error())
		execSpan.End()
		stopWatchdog()
		p.Discard(conn)
		return nil, execErr
	}
	execSpan.End()

	// Reset session state before returning to pool
	resetPayload := append([]byte(s.cfg.Pool.ResetQuery), 0)
	if err := protocol.WriteMessage(conn, protocol.MsgQuery, resetPayload); err != nil {
		stopWatchdog()
		p.Discard(conn)
		return resp, nil // return result even if reset fails
	}
	// Drain reset response
	drainErr := drainUntilReady(conn)

	if cancelled.Load() {
		<-watchdogDone // ensure watchdog goroutine has exited
		p.Discard(conn)
		return nil, fmt.Errorf("drain reset: %w", ctx.Err())
	}

	stopWatchdog()

	if drainErr != nil {
		p.Discard(conn)
		return resp, nil
	}
	p.Release(conn)
	return resp, nil
}

// executeQuery sends SQL via Simple Query Protocol and parses the response.
// The sql parameter is only used if non-empty; otherwise the query was already sent.
func executeQuery(conn net.Conn, sql string) (*QueryResponse, error) {
	if sql != "" {
		payload := append([]byte(sql), 0)
		if err := protocol.WriteMessage(conn, protocol.MsgQuery, payload); err != nil {
			return nil, fmt.Errorf("send query: %w", err)
		}
	}

	resp := &QueryResponse{}
	var columns []columnInfo

	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		switch msg.Type {
		case protocol.MsgRowDescription:
			columns = parseRowDescription(msg.Payload)
			resp.Columns = make([]string, len(columns))
			resp.Types = make([]string, len(columns))
			for i, c := range columns {
				resp.Columns[i] = c.Name
				resp.Types[i] = c.TypeName
			}

		case protocol.MsgDataRow:
			row := parseDataRow(msg.Payload, columns)
			resp.Rows = append(resp.Rows, row)
			resp.RowCount++

		case protocol.MsgCommandComplete:
			resp.Command = parseCommandComplete(msg.Payload)

		case protocol.MsgErrorResponse:
			errMsg := parseErrorResponse(msg.Payload)
			// Continue reading until ReadyForQuery
			for {
				m, e := protocol.ReadMessage(conn)
				if e != nil || m.Type == protocol.MsgReadyForQuery {
					break
				}
			}
			return nil, fmt.Errorf("query error: %s", errMsg)

		case protocol.MsgReadyForQuery:
			return resp, nil

		case protocol.MsgNoticeResponse:
			// Ignore notices
		}
	}
}

func drainUntilReady(conn net.Conn) error {
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return err
		}
		if msg.Type == protocol.MsgReadyForQuery {
			return nil
		}
	}
}

type columnInfo struct {
	Name     string
	OID      uint32
	TypeName string
}

// parseRowDescription parses RowDescription (T) message payload.
// Format: int16 num_cols, then for each: name\0 + table_oid(4) + col_attr(2) + type_oid(4) + type_len(2) + type_mod(4) + format(2)
func parseRowDescription(payload []byte) []columnInfo {
	if len(payload) < 2 {
		return nil
	}
	numCols := int(binary.BigEndian.Uint16(payload[0:2]))
	cols := make([]columnInfo, 0, numCols)
	pos := 2

	for i := 0; i < numCols && pos < len(payload); i++ {
		// Find null-terminated name
		nameEnd := pos
		for nameEnd < len(payload) && payload[nameEnd] != 0 {
			nameEnd++
		}
		name := string(payload[pos:nameEnd])
		pos = nameEnd + 1 // skip null

		// Skip: table_oid(4) + col_attr(2) = 6 bytes
		if pos+6 > len(payload) {
			break
		}
		pos += 6

		// type_oid(4)
		if pos+4 > len(payload) {
			break
		}
		oid := binary.BigEndian.Uint32(payload[pos : pos+4])
		pos += 4

		// Skip: type_len(2) + type_mod(4) + format(2) = 8 bytes
		if pos+8 > len(payload) {
			break
		}
		pos += 8

		cols = append(cols, columnInfo{
			Name:     name,
			OID:      oid,
			TypeName: oidToTypeName(oid),
		})
	}
	return cols
}

// parseDataRow parses DataRow (D) message payload.
// Format: int16 num_cols, then for each: int32 len (-1 = NULL) + bytes
func parseDataRow(payload []byte, columns []columnInfo) []any {
	if len(payload) < 2 {
		return nil
	}
	numCols := int(binary.BigEndian.Uint16(payload[0:2]))
	row := make([]any, numCols)
	pos := 2

	for i := 0; i < numCols && pos < len(payload); i++ {
		if pos+4 > len(payload) {
			break
		}
		colLen := int(int32(binary.BigEndian.Uint32(payload[pos : pos+4])))
		pos += 4

		if colLen == -1 {
			row[i] = nil
			continue
		}

		if pos+colLen > len(payload) {
			break
		}
		val := string(payload[pos : pos+colLen])
		pos += colLen

		// Convert to appropriate Go type based on OID
		if i < len(columns) {
			row[i] = convertValue(val, columns[i].OID)
		} else {
			row[i] = val
		}
	}
	return row
}

func parseCommandComplete(payload []byte) string {
	end := 0
	for end < len(payload) && payload[end] != 0 {
		end++
	}
	return string(payload[:end])
}

func parseErrorResponse(payload []byte) string {
	// Error fields are type_byte + string\0, terminated by \0
	var msg string
	pos := 0
	for pos < len(payload) && payload[pos] != 0 {
		fieldType := payload[pos]
		pos++
		end := pos
		for end < len(payload) && payload[end] != 0 {
			end++
		}
		if fieldType == 'M' {
			msg = string(payload[pos:end])
		}
		pos = end + 1
	}
	return msg
}

func convertValue(val string, oid uint32) any {
	switch oid {
	case 16: // bool
		return val == "t" || val == "true"
	case 20, 21, 23: // int8, int2, int4
		var n int64
		fmt.Sscanf(val, "%d", &n)
		return n
	case 700, 701: // float4, float8
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	default:
		return val
	}
}

func oidToTypeName(oid uint32) string {
	switch oid {
	case 16:
		return "bool"
	case 20:
		return "int8"
	case 21:
		return "int2"
	case 23:
		return "int4"
	case 25:
		return "text"
	case 114:
		return "json"
	case 700:
		return "float4"
	case 701:
		return "float8"
	case 1042:
		return "bpchar"
	case 1043:
		return "varchar"
	case 1082:
		return "date"
	case 1114:
		return "timestamp"
	case 1184:
		return "timestamptz"
	case 2950:
		return "uuid"
	case 3802:
		return "jsonb"
	default:
		return fmt.Sprintf("oid_%d", oid)
	}
}

func (s *Server) classifyQuery(sql string) router.QueryType {
	if s.cfg.Routing.ASTParser {
		return router.ClassifyAST(sql)
	}
	return router.Classify(sql)
}

func (s *Server) cacheKey(sql string) uint64 {
	if s.cfg.Routing.ASTParser {
		return cache.SemanticCacheKey(sql)
	}
	return cache.CacheKey(sql)
}

func (s *Server) extractTables(sql string) []string {
	if s.cfg.Routing.ASTParser {
		return router.ExtractTablesAST(sql)
	}
	return router.ExtractTables(sql)
}

// truncateSQL returns the first 100 characters of a SQL statement for span attributes.
func truncateSQL(sql string) string {
	if len(sql) > 100 {
		return sql[:100]
	}
	return sql
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{Error: msg})
}
