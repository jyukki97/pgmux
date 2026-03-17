package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/proxy"
)

// Server is the Admin API HTTP server.
type Server struct {
	cfgFn              func() *config.Config
	cacheFn            func() *cache.Cache
	invalidatorFn      func() *cache.Invalidator
	dbGroupsFn         func() map[string]*proxy.DatabaseGroup
	defaultDBName      string
	auditLoggerFn      func() *audit.Logger
	mirrorStatsFn      func() any
	digestStatsFn      func() any
	digestResetFn      func()
	connStatsFn        func() any
	reloadFunc         func() error
	maintenanceGetFn   func() (bool, time.Time)
	maintenanceSetFn   func(bool)
	readOnlyGetFn      func() (bool, time.Time)
	readOnlySetFn      func(bool)
	mu                 sync.RWMutex
}

// SetReloadFunc sets the function to call when reload is requested.
func (s *Server) SetReloadFunc(fn func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadFunc = fn
}

// SetMaintenanceFns sets the maintenance mode getter and setter functions.
func (s *Server) SetMaintenanceFns(getFn func() (bool, time.Time), setFn func(bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintenanceGetFn = getFn
	s.maintenanceSetFn = setFn
}

// SetReadOnlyFns sets the getter and setter functions for read-only mode.
func (s *Server) SetReadOnlyFns(getFn func() (bool, time.Time), setFn func(bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readOnlyGetFn = getFn
	s.readOnlySetFn = setFn
}

// New creates a new Admin server.
// All parameters except reloadFunc are getter functions so that Admin always
// accesses the latest objects even after a hot-reload.
func New(cfgFn func() *config.Config, cacheFn func() *cache.Cache, invalidatorFn func() *cache.Invalidator, dbGroupsFn func() map[string]*proxy.DatabaseGroup, defaultDBName string, auditLoggerFn func() *audit.Logger, mirrorStatsFn func() any, digestStatsFn func() any, digestResetFn func(), connStatsFn func() any) *Server {
	return &Server{
		cfgFn:         cfgFn,
		cacheFn:       cacheFn,
		invalidatorFn: invalidatorFn,
		dbGroupsFn:    dbGroupsFn,
		defaultDBName: defaultDBName,
		auditLoggerFn: auditLoggerFn,
		mirrorStatsFn: mirrorStatsFn,
		digestStatsFn: digestStatsFn,
		digestResetFn: digestResetFn,
		connStatsFn:   connStatsFn,
	}
}

// HTTPServer returns an *http.Server with the admin routes registered.
// The caller is responsible for calling Serve/Shutdown.
func (s *Server) HTTPServer() *http.Server {
	mux := http.NewServeMux()
	// Unauthenticated probe endpoints for LB / K8s
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	// Authenticated admin endpoints
	mux.HandleFunc("/admin/health", s.withAuth(s.handleHealth, false))
	mux.HandleFunc("/admin/stats", s.withAuth(s.handleStats, false))
	mux.HandleFunc("/admin/config", s.withAuth(s.handleConfig, false))
	mux.HandleFunc("/admin/cache/flush", s.withAuth(s.handleCacheFlush, true))
	mux.HandleFunc("/admin/reload", s.withAuth(s.handleReload, true))
	mux.HandleFunc("/admin/mirror/stats", s.withAuth(s.handleMirrorStats, false))
	mux.HandleFunc("/admin/queries/top", s.withAuth(s.handleQueryDigest, false))
	mux.HandleFunc("/admin/queries/reset", s.withAuth(s.handleQueryDigestReset, true))
	mux.HandleFunc("/admin/connections", s.withAuth(s.handleConnections, false))
	mux.HandleFunc("/admin/maintenance", s.withAuth(s.handleMaintenance, false))
	mux.HandleFunc("/admin/readonly", s.withAuth(s.handleReadOnly, false))
	return &http.Server{Handler: mux}
}

// withAuth wraps a handler with authentication and authorization checks.
// If requireAdmin is true, only "admin" role keys are allowed.
func (s *Server) withAuth(next http.HandlerFunc, requireAdmin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfgFn()
		authCfg := cfg.Admin.Auth

		if !authCfg.Enabled {
			next(w, r)
			return
		}

		// IP allowlist check
		if len(authCfg.IPAllowlist) > 0 {
			clientIP := extractClientIP(r, authCfg.TrustedProxies)
			if !isIPAllowed(clientIP, authCfg.IPAllowlist) {
				writeJSONError(w, http.StatusForbidden, "ip not allowed")
				return
			}
		}

		// Bearer token check
		token := extractBearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		role := ""
		for _, k := range authCfg.APIKeys {
			if k.Key == token {
				role = k.Role
				break
			}
		}
		if role == "" {
			writeJSONError(w, http.StatusUnauthorized, "invalid api key")
			return
		}

		// Authorization: admin role required for mutating endpoints
		if requireAdmin && role != "admin" {
			writeJSONError(w, http.StatusForbidden, "admin role required")
			return
		}

		next(w, r)
	}
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func extractClientIP(r *http.Request, trustedProxies []string) string {
	// Extract RemoteAddr (host part)
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}

	// Only trust X-Forwarded-For if RemoteAddr is in trustedProxies.
	// If trustedProxies is empty, NEVER trust XFF (secure default).
	if len(trustedProxies) > 0 && isTrustedProxy(remoteIP, trustedProxies) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.SplitN(xff, ",", 2)
			return strings.TrimSpace(parts[0])
		}
	}

	return remoteIP
}

// isTrustedProxy checks whether the given IP is in the trusted proxy list.
func isTrustedProxy(ip string, trustedProxies []string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, entry := range trustedProxies {
		_, cidr, err := net.ParseCIDR(entry)
		if err == nil {
			if cidr.Contains(parsed) {
				return true
			}
			continue
		}
		if net.ParseIP(entry) != nil && entry == ip {
			return true
		}
	}
	return false
}

func isIPAllowed(clientIP string, allowlist []string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, entry := range allowlist {
		_, cidr, err := net.ParseCIDR(entry)
		if err == nil {
			if cidr.Contains(ip) {
				return true
			}
			continue
		}
		// Single IP match
		if net.ParseIP(entry) != nil && entry == clientIP {
			return true
		}
	}
	return false
}

func writeJSONError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// handleHealthz is a lightweight liveness probe. Returns 200 if the process is running.
// No authentication required — intended for LB / K8s livenessProbe.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleReadyz is a readiness probe. Returns 200 if all Writer backends are reachable,
// 503 otherwise. Also returns 503 when maintenance mode is active.
// No authentication required — intended for LB / K8s readinessProbe.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check maintenance mode
	s.mu.RLock()
	getFn := s.maintenanceGetFn
	s.mu.RUnlock()
	if getFn != nil {
		if enabled, _ := getFn(); enabled {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "not_ready",
				"reason": "maintenance mode active",
			})
			return
		}
	}

	groups := s.dbGroupsFn()
	if len(groups) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "not_ready",
			"reason": "no database groups configured",
		})
		return
	}

	var failed []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, dbg := range groups {
		name, dbg := name, dbg
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !checkTCP(dbg.WriterAddr()) {
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(failed) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "not_ready",
			"reason": "writer unreachable for: " + strings.Join(failed, ", "),
		})
		return
	}

	writeJSON(w, map[string]string{"status": "ready"})
}

// handleHealth returns the health status of all backends, grouped by database.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	groups := s.dbGroupsFn()

	type backendHealth struct {
		Addr    string `json:"addr"`
		Healthy bool   `json:"healthy"`
	}

	type dbHealth struct {
		Writer  backendHealth   `json:"writer"`
		Readers []backendHealth `json:"readers"`
	}

	result := make(map[string]dbHealth)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for name, dbg := range groups {
		name, dbg := name, dbg
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Check writer in parallel with readers
			var writerHealthy bool
			var wwg sync.WaitGroup
			wwg.Add(1)
			go func() {
				defer wwg.Done()
				writerHealthy = checkTCP(dbg.WriterAddr())
			}()

			readerPools := dbg.ReaderPools()
			readers := make([]backendHealth, 0, len(readerPools))

			var rwg sync.WaitGroup
			var rmu sync.Mutex
			for addr := range readerPools {
				addr := addr
				rwg.Add(1)
				go func() {
					defer rwg.Done()
					healthy := checkTCP(addr)
					rmu.Lock()
					readers = append(readers, backendHealth{Addr: addr, Healthy: healthy})
					rmu.Unlock()
				}()
			}

			wwg.Wait()
			rwg.Wait()

			mu.Lock()
			result[name] = dbHealth{
				Writer:  backendHealth{Addr: dbg.WriterAddr(), Healthy: writerHealthy},
				Readers: readers,
			}
			mu.Unlock()
		}()
	}

	wg.Wait()
	writeJSON(w, map[string]any{"databases": result})
}

// handleStats returns pool, cache, and routing statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	groups := s.dbGroupsFn()
	c := s.cacheFn()
	auditLogger := s.auditLoggerFn()

	dbPoolStats := make(map[string]any)
	for name, dbg := range groups {
		dbStats := make(map[string]any)
		wp := dbg.WriterPool()
		if wp != nil {
			wOpen, wIdle := wp.Stats()
			dbStats["writer"] = map[string]any{
				"addr": dbg.WriterAddr(),
				"open": wOpen,
				"idle": wIdle,
			}
		}
		readerStats := make(map[string]any)
		for addr, p := range dbg.ReaderPools() {
			open, idle := p.Stats()
			readerStats[addr] = map[string]any{
				"open": open,
				"idle": idle,
			}
		}
		dbStats["readers"] = readerStats
		dbPoolStats[name] = dbStats
	}

	cacheStats := map[string]any{
		"enabled": c != nil,
	}
	if c != nil {
		cacheStats["entries"] = c.Len()
	}

	resp := map[string]any{
		"pool":  dbPoolStats,
		"cache": cacheStats,
	}

	if auditLogger != nil {
		slow, sent, errors := auditLogger.Stats()
		resp["audit"] = map[string]any{
			"slow_queries":   slow,
			"webhook_sent":   sent,
			"webhook_errors": errors,
		}
	}

	writeJSON(w, resp)
}

// handleConfig returns the current config with password masked.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.cfgFn()

	// Create a safe copy with masked passwords
	type safeAuthUser struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	type safeAdminAPIKey struct {
		Key  string `json:"key"`
		Role string `json:"role"`
	}
	type safeDBConfig struct {
		Writer  config.DBConfig `json:"writer"`
		Readers []config.DBConfig `json:"readers"`
		Backend struct {
			User     string `json:"user"`
			Password string `json:"password"`
			Database string `json:"database"`
		} `json:"backend"`
	}

	safe := struct {
		Proxy     config.ProxyConfig   `json:"proxy"`
		Pool      config.PoolConfig    `json:"pool"`
		Routing   config.RoutingConfig `json:"routing"`
		Cache     config.CacheConfig   `json:"cache"`
		TLS       config.TLSConfig     `json:"tls"`
		Auth      struct {
			Enabled bool           `json:"enabled"`
			Users   []safeAuthUser `json:"users,omitempty"`
		} `json:"auth"`
		Admin struct {
			Enabled bool `json:"enabled"`
			Listen  string `json:"listen"`
			Auth    struct {
				Enabled        bool              `json:"enabled"`
				APIKeys        []safeAdminAPIKey `json:"api_keys,omitempty"`
				IPAllowlist    []string          `json:"ip_allowlist,omitempty"`
				TrustedProxies []string          `json:"trusted_proxies,omitempty"`
			} `json:"auth"`
		} `json:"admin"`
		Databases map[string]safeDBConfig `json:"databases"`
	}{
		Proxy:   cfg.Proxy,
		Pool:    cfg.Pool,
		Routing: cfg.Routing,
		Cache:   cfg.Cache,
		TLS:     cfg.TLS,
	}
	safe.Auth.Enabled = cfg.Auth.Enabled
	for _, u := range cfg.Auth.Users {
		safe.Auth.Users = append(safe.Auth.Users, safeAuthUser{
			Username: u.Username,
			Password: "********",
		})
	}
	safe.Admin.Enabled = cfg.Admin.Enabled
	safe.Admin.Listen = cfg.Admin.Listen
	safe.Admin.Auth.Enabled = cfg.Admin.Auth.Enabled
	safe.Admin.Auth.IPAllowlist = cfg.Admin.Auth.IPAllowlist
	safe.Admin.Auth.TrustedProxies = cfg.Admin.Auth.TrustedProxies
	for _, k := range cfg.Admin.Auth.APIKeys {
		safe.Admin.Auth.APIKeys = append(safe.Admin.Auth.APIKeys, safeAdminAPIKey{
			Key:  "********",
			Role: k.Role,
		})
	}

	safe.Databases = make(map[string]safeDBConfig)
	for name, db := range cfg.Databases {
		sdb := safeDBConfig{
			Writer:  db.Writer,
			Readers: db.Readers,
		}
		sdb.Backend.User = db.Backend.User
		sdb.Backend.Password = "********"
		sdb.Backend.Database = db.Backend.Database
		safe.Databases[name] = sdb
	}

	writeJSON(w, safe)
}

// handleCacheFlush flushes the entire cache or a specific table's cache.
func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	c := s.cacheFn()
	inv := s.invalidatorFn()

	if c == nil {
		writeJSON(w, map[string]string{"status": "cache disabled"})
		return
	}

	// Check for table-specific flush: /admin/cache/flush/{table}
	path := strings.TrimPrefix(r.URL.Path, "/admin/cache/flush")
	path = strings.TrimPrefix(path, "/")

	if path != "" {
		// Flush specific table
		c.InvalidateTable(path)
		if inv != nil {
			inv.Publish(context.Background(), []string{path})
		}
		slog.Info("admin: cache flushed for table", "table", path)
		writeJSON(w, map[string]string{"status": "flushed", "table": path})
		return
	}

	// Flush all
	c.FlushAll()
	if inv != nil {
		inv.PublishFlushAll(context.Background())
	}
	slog.Info("admin: full cache flush")
	writeJSON(w, map[string]string{"status": "flushed"})
}

// handleReload triggers a config reload via the registered reload function.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	fn := s.reloadFunc
	if fn == nil {
		s.mu.Unlock()
		http.Error(w, "reload not configured", http.StatusServiceUnavailable)
		return
	}
	defer s.mu.Unlock()

	if err := fn(); err != nil {
		slog.Error("admin: reload failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": err.Error()})
		return
	}

	slog.Info("admin: config reloaded")
	writeJSON(w, map[string]string{"status": "reloaded"})
}

// handleMirrorStats returns query mirror latency comparison statistics.
func (s *Server) handleMirrorStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.mirrorStatsFn == nil {
		writeJSON(w, map[string]string{"status": "mirror disabled"})
		return
	}
	stats := s.mirrorStatsFn()
	if stats == nil {
		writeJSON(w, map[string]string{"status": "mirror disabled"})
		return
	}
	writeJSON(w, stats)
}

// handleQueryDigest returns top-N query digest statistics.
func (s *Server) handleQueryDigest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.digestStatsFn == nil {
		writeJSON(w, map[string]string{"status": "digest disabled"})
		return
	}
	stats := s.digestStatsFn()
	if stats == nil {
		writeJSON(w, map[string]string{"status": "digest disabled"})
		return
	}
	writeJSON(w, stats)
}

// handleQueryDigestReset clears all collected query digest statistics.
func (s *Server) handleQueryDigestReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.digestResetFn == nil {
		writeJSON(w, map[string]string{"status": "digest disabled"})
		return
	}
	s.digestResetFn()
	slog.Info("admin: query digest reset")
	writeJSON(w, map[string]string{"status": "reset"})
}

// handleConnections returns per-user and per-database connection counts and limits.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.connStatsFn == nil {
		writeJSON(w, map[string]string{"status": "connection limits disabled"})
		return
	}
	stats := s.connStatsFn()
	if stats == nil {
		writeJSON(w, map[string]string{"status": "connection limits disabled"})
		return
	}
	writeJSON(w, stats)
}

// handleMaintenance handles GET/POST/DELETE /admin/maintenance.
// GET: returns current status (viewer role).
// POST: enters maintenance mode (admin role).
// DELETE: exits maintenance mode (admin role).
func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	getFn := s.maintenanceGetFn
	setFn := s.maintenanceSetFn
	s.mu.RUnlock()

	if getFn == nil || setFn == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "maintenance control not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		enabled, enteredAt := getFn()
		resp := map[string]any{"enabled": enabled}
		if enabled {
			resp["entered_at"] = enteredAt.Format(time.RFC3339)
		}
		writeJSON(w, resp)

	case http.MethodPost:
		// Require admin role for mutating operation
		cfg := s.cfgFn()
		if cfg.Admin.Auth.Enabled {
			token := extractBearerToken(r)
			role := ""
			for _, k := range cfg.Admin.Auth.APIKeys {
				if k.Key == token {
					role = k.Role
					break
				}
			}
			if role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin role required")
				return
			}
		}

		enabled, _ := getFn()
		if enabled {
			writeJSON(w, map[string]string{"status": "already in maintenance mode"})
			return
		}
		setFn(true)
		_, enteredAt := getFn()
		slog.Info("admin: maintenance mode entered")
		writeJSON(w, map[string]any{
			"status":     "maintenance_entered",
			"entered_at": enteredAt.Format(time.RFC3339),
		})

	case http.MethodDelete:
		// Require admin role for mutating operation
		cfg := s.cfgFn()
		if cfg.Admin.Auth.Enabled {
			token := extractBearerToken(r)
			role := ""
			for _, k := range cfg.Admin.Auth.APIKeys {
				if k.Key == token {
					role = k.Role
					break
				}
			}
			if role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin role required")
				return
			}
		}

		enabled, _ := getFn()
		if !enabled {
			writeJSON(w, map[string]string{"status": "not in maintenance mode"})
			return
		}
		setFn(false)
		slog.Info("admin: maintenance mode exited")
		writeJSON(w, map[string]string{"status": "maintenance_exited"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleReadOnly manages the read-only mode.
// GET: returns current state (viewer role).
// POST: enters read-only mode (admin role).
// DELETE: exits read-only mode (admin role).
func (s *Server) handleReadOnly(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGetFn == nil || s.readOnlySetFn == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "read-only mode not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		enabled, since := s.readOnlyGetFn()
		resp := map[string]any{"readonly": enabled}
		if enabled {
			resp["since"] = since.Format(time.RFC3339)
		}
		writeJSON(w, resp)

	case http.MethodPost:
		cfg := s.cfgFn()
		if cfg.Admin.Auth.Enabled {
			token := extractBearerToken(r)
			role := ""
			for _, k := range cfg.Admin.Auth.APIKeys {
				if k.Key == token {
					role = k.Role
					break
				}
			}
			if role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin role required")
				return
			}
		}
		s.readOnlySetFn(true)
		slog.Info("admin: read-only mode enabled")
		writeJSON(w, map[string]string{"status": "readonly enabled"})

	case http.MethodDelete:
		cfg := s.cfgFn()
		if cfg.Admin.Auth.Enabled {
			token := extractBearerToken(r)
			role := ""
			for _, k := range cfg.Admin.Auth.APIKeys {
				if k.Key == token {
					role = k.Role
					break
				}
			}
			if role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin role required")
				return
			}
		}
		s.readOnlySetFn(false)
		slog.Info("admin: read-only mode disabled")
		writeJSON(w, map[string]string{"status": "readonly disabled"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func checkTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*1e9) // 2 seconds
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
