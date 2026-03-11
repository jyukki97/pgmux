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

	"github.com/jyukki97/db-proxy/internal/audit"
	"github.com/jyukki97/db-proxy/internal/cache"
	"github.com/jyukki97/db-proxy/internal/config"
	"github.com/jyukki97/db-proxy/internal/pool"
)

// Server is the Admin API HTTP server.
type Server struct {
	cfg         *config.Config
	cache       *cache.Cache
	invalidator *cache.Invalidator
	writerPool  *pool.Pool
	readerPools map[string]*pool.Pool
	auditLogger *audit.Logger
	reloadFunc  func() error
	mu          sync.RWMutex
}

// SetReloadFunc sets the function to call when reload is requested.
func (s *Server) SetReloadFunc(fn func() error) {
	s.reloadFunc = fn
}

// New creates a new Admin server.
func New(cfg *config.Config, c *cache.Cache, inv *cache.Invalidator, writerPool *pool.Pool, readerPools map[string]*pool.Pool, auditLogger *audit.Logger) *Server {
	return &Server{
		cfg:         cfg,
		cache:       c,
		invalidator: inv,
		writerPool:  writerPool,
		readerPools: readerPools,
		auditLogger: auditLogger,
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

	type backendHealth struct {
		Addr    string `json:"addr"`
		Healthy bool   `json:"healthy"`
	}

	writerAddr := fmt.Sprintf("%s:%d", s.cfg.Writer.Host, s.cfg.Writer.Port)
	writerHealthy := checkTCP(writerAddr)

	readers := make([]backendHealth, 0, len(s.cfg.Readers))
	for _, r := range s.cfg.Readers {
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

	poolStats := make(map[string]any)

	// Writer pool stats
	if s.writerPool != nil {
		wOpen, wIdle := s.writerPool.Stats()
		writerAddr := fmt.Sprintf("%s:%d", s.cfg.Writer.Host, s.cfg.Writer.Port)
		poolStats["writer"] = map[string]any{
			"addr": writerAddr,
			"open": wOpen,
			"idle": wIdle,
		}
	}

	// Reader pool stats
	readerStats := make(map[string]any)
	for addr, p := range s.readerPools {
		open, idle := p.Stats()
		readerStats[addr] = map[string]any{
			"open": open,
			"idle": idle,
		}
	}
	poolStats["readers"] = readerStats

	cacheStats := map[string]any{
		"enabled": s.cache != nil,
	}
	if s.cache != nil {
		cacheStats["entries"] = s.cache.Len()
	}

	resp := map[string]any{
		"pool":  poolStats,
		"cache": cacheStats,
	}

	if s.auditLogger != nil {
		slow, sent, errors := s.auditLogger.Stats()
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
		Proxy:   s.cfg.Proxy,
		Writer:  s.cfg.Writer,
		Readers: s.cfg.Readers,
		Pool:    s.cfg.Pool,
		Routing: s.cfg.Routing,
		Cache:   s.cfg.Cache,
		TLS:     s.cfg.TLS,
	}
	safe.Auth.Enabled = s.cfg.Auth.Enabled
	for _, u := range s.cfg.Auth.Users {
		safe.Auth.Users = append(safe.Auth.Users, safeAuthUser{
			Username: u.Username,
			Password: "********",
		})
	}
	safe.Backend.User = s.cfg.Backend.User
	safe.Backend.Password = "********"
	safe.Backend.Database = s.cfg.Backend.Database

	writeJSON(w, safe)
}

// handleCacheFlush flushes the entire cache or a specific table's cache.
func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.cache == nil {
		writeJSON(w, map[string]string{"status": "cache disabled"})
		return
	}

	// Check for table-specific flush: /admin/cache/flush/{table}
	path := strings.TrimPrefix(r.URL.Path, "/admin/cache/flush")
	path = strings.TrimPrefix(path, "/")

	if path != "" {
		// Flush specific table
		s.cache.InvalidateTable(path)
		if s.invalidator != nil {
			s.invalidator.Publish(context.Background(), []string{path})
		}
		slog.Info("admin: cache flushed for table", "table", path)
		writeJSON(w, map[string]string{"status": "flushed", "table": path})
		return
	}

	// Flush all
	s.cache.FlushAll()
	if s.invalidator != nil {
		s.invalidator.PublishFlushAll(context.Background())
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
