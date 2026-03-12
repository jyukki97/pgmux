package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/pool"
)

func testServer() (*Server, *cache.Cache) {
	cfg := &config.Config{
		Proxy:  config.ProxyConfig{Listen: "0.0.0.0:5432"},
		Writer: config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Readers: []config.DBConfig{
			{Host: "127.0.0.1", Port: 5433},
		},
		Pool: config.PoolConfig{
			MaxConnections:    10,
			IdleTimeout:       10 * time.Minute,
			MaxLifetime:       time.Hour,
			ConnectionTimeout: 5 * time.Second,
		},
		Cache: config.CacheConfig{
			Enabled:         true,
			CacheTTL:        10 * time.Second,
			MaxCacheEntries: 1000,
			MaxResultSize:   "1MB",
		},
		Backend: config.BackendConfig{
			User:     "postgres",
			Password: "secret123",
			Database: "testdb",
		},
	}

	c := cache.New(cache.Config{
		MaxEntries: 1000,
		TTL:        10 * time.Second,
	})
	readerPools := map[string]*pool.Pool{}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return c },
		func() *cache.Invalidator { return nil },
		func() *pool.Pool { return nil },
		func() map[string]*pool.Pool { return readerPools },
		func() *audit.Logger { return nil },
	)
	return srv, c
}

func TestHandleHealth(t *testing.T) {
	srv, _ := testServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["writer"] == nil {
		t.Error("expected writer field in response")
	}
	if resp["readers"] == nil {
		t.Error("expected readers field in response")
	}
}

func TestHandleStats(t *testing.T) {
	srv, _ := testServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	w := httptest.NewRecorder()

	srv.handleStats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	cacheStats := resp["cache"].(map[string]any)
	if cacheStats["enabled"] != true {
		t.Error("expected cache.enabled = true")
	}
}

func TestHandleConfig_MasksPassword(t *testing.T) {
	srv, _ := testServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	w := httptest.NewRecorder()

	srv.handleConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	backend := resp["backend"].(map[string]any)
	if backend["password"] != "********" {
		t.Errorf("password = %q, want masked", backend["password"])
	}
	if backend["user"] != "postgres" {
		t.Errorf("user = %q, want postgres", backend["user"])
	}
}

func TestHandleCacheFlush(t *testing.T) {
	srv, c := testServer()

	// Add some cache entries
	c.Set(cache.CacheKey("SELECT 1"), []byte("result1"), []string{"users"})
	c.Set(cache.CacheKey("SELECT 2"), []byte("result2"), []string{"orders"})

	if c.Len() != 2 {
		t.Fatalf("cache len = %d, want 2", c.Len())
	}

	// Flush all
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/flush", nil)
	w := httptest.NewRecorder()
	srv.handleCacheFlush(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if c.Len() != 0 {
		t.Errorf("cache len after flush = %d, want 0", c.Len())
	}
}

func TestHandleCacheFlush_ByTable(t *testing.T) {
	srv, c := testServer()

	c.Set(cache.CacheKey("SELECT * FROM users"), []byte("r1"), []string{"users"})
	c.Set(cache.CacheKey("SELECT * FROM orders"), []byte("r2"), []string{"orders"})

	// Flush only users table
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/flush/users", nil)
	w := httptest.NewRecorder()
	srv.handleCacheFlush(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// users entry should be gone, orders should remain
	if c.Len() != 1 {
		t.Errorf("cache len after table flush = %d, want 1", c.Len())
	}
}

func TestHandleHealth_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleReload_Success(t *testing.T) {
	srv, _ := testServer()
	srv.SetReloadFunc(func() error {
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "reloaded" {
		t.Errorf("status = %q, want reloaded", resp["status"])
	}
}

func TestHandleReload_Error(t *testing.T) {
	srv, _ := testServer()
	srv.SetReloadFunc(func() error {
		return fmt.Errorf("config parse error")
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "error" {
		t.Errorf("status = %q, want error", resp["status"])
	}
	if resp["error"] == nil {
		t.Error("expected error field in response")
	}
}

func TestHandleReload_NotConfigured(t *testing.T) {
	srv, _ := testServer()
	// No reload func set

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleReload_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer()

	req := httptest.NewRequest(http.MethodGet, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
