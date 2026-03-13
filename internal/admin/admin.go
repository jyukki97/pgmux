package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/proxy"
)

// Server is the Admin API HTTP server.
type Server struct {
	cfgFn         func() *config.Config
	cacheFn       func() *cache.Cache
	invalidatorFn func() *cache.Invalidator
	dbGroupsFn    func() map[string]*proxy.DatabaseGroup
	defaultDBName string
	auditLoggerFn func() *audit.Logger
	mirrorStatsFn func() any
	digestStatsFn func() any
	digestResetFn  func()
	connStatsFn    func() any
	reloadFunc     func() error
	mu             sync.RWMutex
}

// SetReloadFunc sets the function to call when reload is requested.
func (s *Server) SetReloadFunc(fn func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadFunc = fn
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
	mux.HandleFunc("/admin/health", s.handleHealth)
	mux.HandleFunc("/admin/stats", s.handleStats)
	mux.HandleFunc("/admin/config", s.handleConfig)
	mux.HandleFunc("/admin/cache/flush", s.handleCacheFlush)
	mux.HandleFunc("/admin/reload", s.handleReload)
	mux.HandleFunc("/admin/mirror/stats", s.handleMirrorStats)
	mux.HandleFunc("/admin/queries/top", s.handleQueryDigest)
	mux.HandleFunc("/admin/queries/reset", s.handleQueryDigestReset)
	mux.HandleFunc("/admin/connections", s.handleConnections)
	return &http.Server{Handler: mux}
}

// ListenAndServe starts the admin HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	srv := s.HTTPServer()
	srv.Addr = addr
	slog.Info("admin server starting", "listen", addr)
	return srv.ListenAndServe()
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
		Writer    config.DBConfig      `json:"writer,omitempty"`
		Readers   []config.DBConfig    `json:"readers,omitempty"`
		Pool      config.PoolConfig    `json:"pool"`
		Routing   config.RoutingConfig `json:"routing"`
		Cache     config.CacheConfig   `json:"cache"`
		TLS       config.TLSConfig     `json:"tls"`
		Auth      struct {
			Enabled bool           `json:"enabled"`
			Users   []safeAuthUser `json:"users,omitempty"`
		} `json:"auth"`
		Backend struct {
			User     string `json:"user"`
			Password string `json:"password"`
			Database string `json:"database"`
		} `json:"backend"`
		Databases map[string]safeDBConfig `json:"databases,omitempty"`
	}{
		Proxy:   cfg.Proxy,
		Writer:  cfg.Writer,
		Readers: cfg.Readers,
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
	safe.Backend.User = cfg.Backend.User
	safe.Backend.Password = "********"
	safe.Backend.Database = cfg.Backend.Database

	if len(cfg.Databases) > 0 {
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
