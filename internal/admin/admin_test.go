package admin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/proxy"
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

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return c },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)
	return srv, c
}

func testServerWithGroups(cfg *config.Config) *Server {
	proxySrv := proxy.NewServer(cfg)
	return New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		proxySrv.DBGroups,
		proxySrv.DefaultDBName(),
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)
}

func TestHandleHealth(t *testing.T) {
	cfg := &config.Config{
		Writer:  config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Readers: []config.DBConfig{{Host: "127.0.0.1", Port: 5433}},
		Backend: config.BackendConfig{Database: "testdb"},
		Pool:    config.PoolConfig{MaxConnections: 10, IdleTimeout: time.Minute},
	}
	srv := testServerWithGroups(cfg)
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	databases := resp["databases"].(map[string]any)
	db := databases["testdb"].(map[string]any)
	if db["writer"] == nil {
		t.Error("expected writer field in response")
	}
	if db["readers"] == nil {
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

func TestHandleReload_Error_ContentType(t *testing.T) {
	srv, _ := testServer()
	srv.SetReloadFunc(func() error {
		return fmt.Errorf("config parse error")
	})

	ts := httptest.NewServer(http.HandlerFunc(srv.handleReload))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "error" {
		t.Errorf("status = %q, want error", body["status"])
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

func TestHandleHealth_ParallelTiming(t *testing.T) {
	// All backends point to non-routable addresses (RFC 5737 TEST-NET)
	// to trigger the 2 s dial timeout. With 3 backends checked
	// sequentially this would take ~6 s; parallel should finish in ~2 s.
	cfg := &config.Config{
		Writer: config.DBConfig{Host: "192.0.2.1", Port: 9999},
		Readers: []config.DBConfig{
			{Host: "192.0.2.2", Port: 9999},
			{Host: "192.0.2.3", Port: 9999},
		},
		Backend: config.BackendConfig{Database: "testdb"},
		Pool:    config.PoolConfig{MaxConnections: 10, IdleTimeout: time.Minute},
	}
	srv := testServerWithGroups(cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	w := httptest.NewRecorder()

	start := time.Now()
	srv.handleHealth(w, req)
	elapsed := time.Since(start)

	// Sequential would take ~6 s. Parallel should finish in ~2 s.
	// Use 4 s as threshold to allow margin without flakiness.
	if elapsed > 4*time.Second {
		t.Errorf("health check took %v; expected < 4 s (parallel execution)", elapsed)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	databases := resp["databases"].(map[string]any)
	db := databases["testdb"].(map[string]any)
	writer := db["writer"].(map[string]any)
	if writer["healthy"] != false {
		t.Error("writer should be unhealthy")
	}
	readers := db["readers"].([]any)
	if len(readers) != 2 {
		t.Fatalf("expected 2 readers, got %d", len(readers))
	}
}

func TestHandleHealth_LiveBackends(t *testing.T) {
	// Start real TCP listeners so checkTCP succeeds.
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln1.Close()

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln2.Close()

	go func() {
		for {
			c, err := ln1.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	port1 := ln1.Addr().(*net.TCPAddr).Port
	port2 := ln2.Addr().(*net.TCPAddr).Port
	cfg := &config.Config{
		Writer: config.DBConfig{Host: "127.0.0.1", Port: port1},
		Readers: []config.DBConfig{
			{Host: "127.0.0.1", Port: port1},
			{Host: "127.0.0.1", Port: port2},
		},
		Backend: config.BackendConfig{Database: "testdb"},
		Pool:    config.PoolConfig{MaxConnections: 10, IdleTimeout: time.Minute},
	}
	srv := testServerWithGroups(cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	databases := resp["databases"].(map[string]any)
	db := databases["testdb"].(map[string]any)
	writer := db["writer"].(map[string]any)
	if writer["healthy"] != true {
		t.Error("writer should be healthy")
	}
	readers := db["readers"].([]any)
	if len(readers) != 2 {
		t.Fatalf("expected 2 readers, got %d", len(readers))
	}
	for i, r := range readers {
		rd := r.(map[string]any)
		if rd["healthy"] != true {
			t.Errorf("reader[%d] should be healthy", i)
		}
	}
}

func TestHandleHealth_NoReaders(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	lnAddr := ln.Addr().(*net.TCPAddr)
	cfg := &config.Config{
		Writer:  config.DBConfig{Host: "127.0.0.1", Port: lnAddr.Port},
		Readers: nil,
		Backend: config.BackendConfig{Database: "testdb"},
		Pool:    config.PoolConfig{MaxConnections: 10, IdleTimeout: time.Minute},
	}
	srv := testServerWithGroups(cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	databases := resp["databases"].(map[string]any)
	db := databases["testdb"].(map[string]any)
	writer := db["writer"].(map[string]any)
	if writer["healthy"] != true {
		t.Error("writer should be healthy")
	}
	readers := db["readers"].([]any)
	if len(readers) != 0 {
		t.Errorf("expected 0 readers, got %d", len(readers))
	}
}
