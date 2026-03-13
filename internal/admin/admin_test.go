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

func testConfig() *config.Config {
	return &config.Config{
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
}

func testServer() (*Server, *cache.Cache) {
	cfg := testConfig()

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

func TestHandleConfig_MasksAdminAPIKeys(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "super-secret-key", Role: "admin"},
		},
		IPAllowlist: []string{"10.0.0.0/8"},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	w := httptest.NewRecorder()
	srv.handleConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	adminSection := resp["admin"].(map[string]any)
	authSection := adminSection["auth"].(map[string]any)
	keys := authSection["api_keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected 1 api key, got %d", len(keys))
	}
	key := keys[0].(map[string]any)
	if key["key"] != "********" {
		t.Errorf("api key not masked: %q", key["key"])
	}
	if key["role"] != "admin" {
		t.Errorf("role = %q, want admin", key["role"])
	}

	// Verify raw key is not leaked
	body := w.Body.String()
	if strings.Contains(body, "super-secret-key") {
		t.Error("raw API key leaked in config response")
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

// --- Admin Auth Tests ---

func TestAuth_Disabled_NoToken(t *testing.T) {
	srv, _ := testServer()
	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (auth disabled)", resp.StatusCode)
	}
}

func TestAuth_Enabled_NoToken(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "admin-key", Role: "admin"},
		},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Error("expected WWW-Authenticate: Bearer header")
	}
}

func TestAuth_Enabled_InvalidToken(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "admin-key", Role: "admin"},
		},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_AdminRole_AllEndpoints(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "admin-key", Role: "admin"},
		},
	}

	c := cache.New(cache.Config{MaxEntries: 100, TTL: 10 * time.Second})
	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return c },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)
	srv.SetReloadFunc(func() error { return nil })

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	tests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/admin/stats", 200},
		{http.MethodGet, "/admin/health", 200},
		{http.MethodGet, "/admin/config", 200},
		{http.MethodPost, "/admin/cache/flush", 200},
		{http.MethodPost, "/admin/reload", 200},
	}

	for _, tt := range tests {
		req, _ := http.NewRequest(tt.method, ts.URL+tt.path, nil)
		req.Header.Set("Authorization", "Bearer admin-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tt.method, tt.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s: status = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
		}
	}
}

func TestAuth_ViewerRole_ReadOnly(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "viewer-key", Role: "viewer"},
		},
	}

	c := cache.New(cache.Config{MaxEntries: 100, TTL: 10 * time.Second})
	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return c },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)
	srv.SetReloadFunc(func() error { return nil })

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	// GET endpoints should work
	getEndpoints := []string{"/admin/stats", "/admin/health", "/admin/config", "/admin/connections"}
	for _, path := range getEndpoints {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer viewer-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
	}

	// POST endpoints should be forbidden
	postEndpoints := []string{"/admin/cache/flush", "/admin/reload", "/admin/queries/reset"}
	for _, path := range postEndpoints {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer viewer-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s: status = %d, want 403", path, resp.StatusCode)
		}
	}
}

func TestAuth_IPAllowlist_Allowed(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "key", Role: "admin"},
		},
		IPAllowlist: []string{"127.0.0.0/8"},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (localhost allowed)", resp.StatusCode)
	}
}

func TestAuth_IPAllowlist_Denied(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "key", Role: "admin"},
		},
		IPAllowlist: []string{"10.0.0.0/8"},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (IP not in allowlist)", resp.StatusCode)
	}
}

func TestAuth_IPAllowlist_XForwardedFor(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "key", Role: "admin"},
		},
		IPAllowlist: []string{"192.168.1.100"},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	// Use httptest.NewRecorder to control RemoteAddr
	handler := srv.HTTPServer().Handler

	// X-Forwarded-For with allowed IP
	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("X-Forwarded-For", "192.168.1.100, 10.0.0.1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("XFF allowed: status = %d, want 200", w.Code)
	}

	// X-Forwarded-For with denied IP
	req2 := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req2.Header.Set("Authorization", "Bearer key")
	req2.Header.Set("X-Forwarded-For", "10.0.0.1")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusForbidden {
		t.Errorf("XFF denied: status = %d, want 403", w2.Code)
	}
}

func TestAuth_HotReload(t *testing.T) {
	cfg := testConfig()
	// Start with auth disabled
	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	// Auth disabled → should work without token
	resp, _ := http.Get(ts.URL + "/admin/stats")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth disabled: status = %d, want 200", resp.StatusCode)
	}

	// Enable auth via config mutation (simulating hot-reload)
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "new-key", Role: "admin"},
		},
	}

	// Without token → should now be 401
	resp2, _ := http.Get(ts.URL + "/admin/stats")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("auth enabled: status = %d, want 401", resp2.StatusCode)
	}

	// With new token → should work
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer new-key")
	resp3, _ := http.DefaultClient.Do(req)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("auth with token: status = %d, want 200", resp3.StatusCode)
	}
}

func TestAuth_ErrorResponseFormat(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Auth = config.AdminAuthConfig{
		Enabled: true,
		APIKeys: []config.AdminAPIKey{
			{Key: "key", Role: "admin"},
		},
	}

	srv := New(
		func() *config.Config { return cfg },
		func() *cache.Cache { return nil },
		func() *cache.Invalidator { return nil },
		func() map[string]*proxy.DatabaseGroup { return nil },
		"testdb",
		func() *audit.Logger { return nil },
		nil, nil, nil, nil,
	)

	ts := httptest.NewServer(srv.HTTPServer().Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "authentication required" {
		t.Errorf("error = %q, want \"authentication required\"", body["error"])
	}
}

func TestExtractClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{"remote addr with port", "192.168.1.1:12345", "", "192.168.1.1"},
		{"xff single", "10.0.0.1:1234", "192.168.1.100", "192.168.1.100"},
		{"xff chain", "10.0.0.1:1234", "192.168.1.100, 10.0.0.2, 10.0.0.3", "192.168.1.100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			got := extractClientIP(r)
			if got != tt.want {
				t.Errorf("extractClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsIPAllowed(t *testing.T) {
	tests := []struct {
		clientIP  string
		allowlist []string
		want      bool
	}{
		{"192.168.1.1", []string{"192.168.1.0/24"}, true},
		{"192.168.2.1", []string{"192.168.1.0/24"}, false},
		{"10.0.0.5", []string{"10.0.0.5"}, true},
		{"10.0.0.6", []string{"10.0.0.5"}, false},
		{"10.0.0.1", []string{"192.168.0.0/16", "10.0.0.0/8"}, true},
		{"invalid", []string{"10.0.0.0/8"}, false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s_in_%v", tt.clientIP, tt.allowlist)
		t.Run(name, func(t *testing.T) {
			got := isIPAllowed(tt.clientIP, tt.allowlist)
			if got != tt.want {
				t.Errorf("isIPAllowed(%q, %v) = %v, want %v", tt.clientIP, tt.allowlist, got, tt.want)
			}
		})
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
