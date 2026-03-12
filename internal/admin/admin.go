package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/pool"
)

// Server is the Admin API HTTP server.
type Server struct {
	cfgFn         func() *config.Config
	cacheFn       func() *cache.Cache
	invalidatorFn func() *cache.Invalidator
	writerPoolFn  func() *pool.Pool
	readerPoolsFn func() map[string]*pool.Pool
	auditLoggerFn func() *audit.Logger
	reloadFunc    func() error
	mu            sync.RWMutex
}

// SetReloadFunc sets the function to call when reload is requested.
func (s *Server) SetReloadFunc(fn func() error) {
	s.reloadFunc = fn
}

// New creates a new Admin server.
// All parameters except reloadFunc are getter functions so that Admin always
// accesses the latest objects even after a hot-reload.
func New(cfgFn func() *config.Config, cacheFn func() *cache.Cache, invalidatorFn func() *cache.Invalidator, writerPoolFn func() *pool.Pool, readerPoolsFn func() map[string]*pool.Pool, auditLoggerFn func() *audit.Logger) *Server {
	return &Server{
		cfgFn:         cfgFn,
		cacheFn:       cacheFn,
		invalidatorFn: invalidatorFn,
		writerPoolFn:  writerPoolFn,
		readerPoolsFn: readerPoolsFn,
		auditLoggerFn: auditLoggerFn,
	}
}

// ListenAndServe starts the admin HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/health", s.handleHealth)
	mux.HandleFunc("/admin/stats", s.handleStats)
	mux.HandleFunc("/admin/config", s.handleConfig)
	mux.HandleFunc("/admin/cache/flush", s.handleCacheFlush)
	mux.HandleFunc("/admin/reload", s.handleReload)

	slog.Info("admin server starting", "listen", addr)
	return http.ListenAndServe(addr, mux)
}

// handleHealth returns the health status of all backends.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.cfgFn()

	type backendHealth struct {
		Addr    string `json:"addr"`
		Healthy bool   `json:"healthy"`
	}

	writerAddr := fmt.Sprintf("%s:%d", cfg.Writer.Host, cfg.Writer.Port)
	writerHealthy := checkTCP(writerAddr)

	readers := make([]backendHealth, 0, len(cfg.Readers))
	for _, r := range cfg.Readers {
		addr := fmt.Sprintf("%s:%d", r.Host, r.Port)
		readers = append(readers, backendHealth{
			Addr:    addr,
			Healthy: checkTCP(addr),
		})
	}

	resp := map[string]any{
		"writer":  backendHealth{Addr: writerAddr, Healthy: writerHealthy},
		"readers": readers,
	}

	writeJSON(w, resp)
}

// handleStats returns pool, cache, and routing statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.cfgFn()
	writerPool := s.writerPoolFn()
	readerPools := s.readerPoolsFn()
	c := s.cacheFn()
	auditLogger := s.auditLoggerFn()

	poolStats := make(map[string]any)

	// Writer pool stats
	if writerPool != nil {
		wOpen, wIdle := writerPool.Stats()
		writerAddr := fmt.Sprintf("%s:%d", cfg.Writer.Host, cfg.Writer.Port)
		poolStats["writer"] = map[string]any{
			"addr": writerAddr,
			"open": wOpen,
			"idle": wIdle,
		}
	}

	// Reader pool stats
	readerStats := make(map[string]any)
	for addr, p := range readerPools {
		open, idle := p.Stats()
		readerStats[addr] = map[string]any{
			"open": open,
			"idle": idle,
		}
	}
	poolStats["readers"] = readerStats

	cacheStats := map[string]any{
		"enabled": c != nil,
	}
	if c != nil {
		cacheStats["entries"] = c.Len()
	}

	resp := map[string]any{
		"pool":  poolStats,
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
	safe := struct {
		Proxy   config.ProxyConfig   `json:"proxy"`
		Writer  config.DBConfig      `json:"writer"`
		Readers []config.DBConfig    `json:"readers"`
		Pool    config.PoolConfig    `json:"pool"`
		Routing config.RoutingConfig `json:"routing"`
		Cache   config.CacheConfig   `json:"cache"`
		TLS     config.TLSConfig     `json:"tls"`
		Auth    struct {
			Enabled bool           `json:"enabled"`
			Users   []safeAuthUser `json:"users,omitempty"`
		} `json:"auth"`
		Backend struct {
			User     string `json:"user"`
			Password string `json:"password"`
			Database string `json:"database"`
		} `json:"backend"`
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

	s.mu.RLock()
	fn := s.reloadFunc
	s.mu.RUnlock()

	if fn == nil {
		http.Error(w, "reload not configured", http.StatusServiceUnavailable)
		return
	}

	if err := fn(); err != nil {
		slog.Error("admin: reload failed", "error", err)
		writeJSON(w, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	slog.Info("admin: config reloaded")
	writeJSON(w, map[string]string{"status": "reloaded"})
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
	json.NewEncoder(w).Encode(v)
}
