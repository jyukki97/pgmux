package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jyukki97/db-proxy/internal/cache"
	"github.com/jyukki97/db-proxy/internal/config"
	"github.com/jyukki97/db-proxy/internal/pool"
)

func testServer() *Server {
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

	return New(cfg, c, nil, nil, map[string]*pool.Pool{}, nil)
}

func TestHandleHealth(t *testing.T) {
	srv := testServer()
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
	srv := testServer()
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
	srv := testServer()
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
	srv := testServer()

	// Add some cache entries
	srv.cache.Set(cache.CacheKey("SELECT 1"), []byte("result1"), []string{"users"})
	srv.cache.Set(cache.CacheKey("SELECT 2"), []byte("result2"), []string{"orders"})

	if srv.cache.Len() != 2 {
		t.Fatalf("cache len = %d, want 2", srv.cache.Len())
	}

	// Flush all
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/flush", nil)
	w := httptest.NewRecorder()
	srv.handleCacheFlush(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if srv.cache.Len() != 0 {
		t.Errorf("cache len after flush = %d, want 0", srv.cache.Len())
	}
}

func TestHandleCacheFlush_ByTable(t *testing.T) {
	srv := testServer()

	srv.cache.Set(cache.CacheKey("SELECT * FROM users"), []byte("r1"), []string{"users"})
	srv.cache.Set(cache.CacheKey("SELECT * FROM orders"), []byte("r2"), []string{"orders"})

	// Flush only users table
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/flush/users", nil)
	w := httptest.NewRecorder()
	srv.handleCacheFlush(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// users entry should be gone, orders should remain
	if srv.cache.Len() != 1 {
		t.Errorf("cache len after table flush = %d, want 1", srv.cache.Len())
	}
}

func TestHandleHealth_MethodNotAllowed(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleReload_Success(t *testing.T) {
	srv := testServer()
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
	srv := testServer()
	srv.SetReloadFunc(func() error {
		return fmt.Errorf("config parse error")
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
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
	srv := testServer()
	// No reload func set

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleReload_MethodNotAllowed(t *testing.T) {
	srv := testServer()

	req := httptest.NewRequest(http.MethodGet, "/admin/reload", nil)
	w := httptest.NewRecorder()
	srv.handleReload(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
