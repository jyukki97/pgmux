package dataapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/proxy"
	"github.com/jyukki97/pgmux/internal/resilience"
)

func testServer() *Server {
	cfg := &config.Config{
		Firewall: config.FirewallConfig{Enabled: false},
		Pool:     config.PoolConfig{MaxConnections: 1, IdleTimeout: time.Minute, ResetQuery: "DISCARD ALL"},
		DataAPI: config.DataAPIConfig{
			Enabled: true,
			APIKeys: []string{"test-key-1", "test-key-2"},
		},
		Writer:  config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Backend: config.BackendConfig{Database: "testdb"},
	}
	proxySrv := proxy.NewServer(cfg)
	return New(
		func() *config.Config { return cfg },
		proxySrv.DBGroups,
		proxySrv.DefaultDBName(),
		nilCache,
		nil,
		nilRateLimiter,
		nil,
	)
}

// Helper nil-returning getter functions for tests.
var (
	nilCache       = func() *cache.Cache { return nil }
	nilRateLimiter = func() *resilience.RateLimiter { return nil }
)

func TestAuthRequired(t *testing.T) {
	srv := testServer()

	body := `{"sql": "SELECT 1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthValid(t *testing.T) {
	srv := testServer()

	body := `{"sql": "SELECT 1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-key-1")
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	// Auth should pass — will get 500 from pool connection failure, not 401
	if w.Code == http.StatusUnauthorized {
		t.Error("expected auth to pass, got 401")
	}
	// Expect 500 because no real backend is running
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (no backend), got %d", w.Code)
	}
}

func TestAuthInvalidKey(t *testing.T) {
	srv := testServer()

	body := `{"sql": "SELECT 1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := testServer()

	req := httptest.NewRequest(http.MethodGet, "/v1/query", nil)
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestEmptySQL(t *testing.T) {
	// No API keys = no auth required
	cfg := &config.Config{
		Pool:    config.PoolConfig{MaxConnections: 1, IdleTimeout: time.Minute, ResetQuery: "DISCARD ALL"},
		DataAPI: config.DataAPIConfig{Enabled: true},
		Writer:  config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Backend: config.BackendConfig{Database: "testdb"},
	}
	proxySrv := proxy.NewServer(cfg)
	srv := New(func() *config.Config { return cfg }, proxySrv.DBGroups, proxySrv.DefaultDBName(), nilCache, nil, nilRateLimiter, nil)

	body := `{"sql": ""}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestInvalidBody(t *testing.T) {
	cfg := &config.Config{
		Pool:    config.PoolConfig{MaxConnections: 1, IdleTimeout: time.Minute, ResetQuery: "DISCARD ALL"},
		DataAPI: config.DataAPIConfig{Enabled: true},
		Writer:  config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Backend: config.BackendConfig{Database: "testdb"},
	}
	proxySrv := proxy.NewServer(cfg)
	srv := New(func() *config.Config { return cfg }, proxySrv.DBGroups, proxySrv.DefaultDBName(), nilCache, nil, nilRateLimiter, nil)

	body := `not json`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestFirewallBlock(t *testing.T) {
	cfg := &config.Config{
		Pool: config.PoolConfig{MaxConnections: 1, IdleTimeout: time.Minute, ResetQuery: "DISCARD ALL"},
		Firewall: config.FirewallConfig{
			Enabled:                true,
			BlockDeleteWithoutWhere: true,
		},
		Routing: config.RoutingConfig{ASTParser: true},
		DataAPI: config.DataAPIConfig{Enabled: true},
		Writer:  config.DBConfig{Host: "127.0.0.1", Port: 5432},
		Backend: config.BackendConfig{Database: "testdb"},
	}
	proxySrv := proxy.NewServer(cfg)
	srv := New(func() *config.Config { return cfg }, proxySrv.DBGroups, proxySrv.DefaultDBName(), nilCache, nil, nilRateLimiter, nil)

	body := `{"sql": "DELETE FROM users"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleQuery(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestParseRowDescription(t *testing.T) {
	// Build a RowDescription for: id (int4, oid=23), name (text, oid=25)
	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, 2) // 2 columns

	// Column 1: "id"
	buf = append(buf, []byte("id")...)
	buf = append(buf, 0) // null terminator
	buf = binary.BigEndian.AppendUint32(buf, 0)  // table OID
	buf = binary.BigEndian.AppendUint16(buf, 0)  // column attr
	buf = binary.BigEndian.AppendUint32(buf, 23) // type OID (int4)
	buf = binary.BigEndian.AppendUint16(buf, 4)  // type len
	buf = binary.BigEndian.AppendUint32(buf, 0)  // type mod
	buf = binary.BigEndian.AppendUint16(buf, 0)  // format

	// Column 2: "name"
	buf = append(buf, []byte("name")...)
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint32(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 0)
	buf = binary.BigEndian.AppendUint32(buf, 25) // type OID (text)
	buf = binary.BigEndian.AppendUint16(buf, 0)
	buf = binary.BigEndian.AppendUint32(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 0)

	cols := parseRowDescription(buf)
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}
	if cols[0].Name != "id" || cols[0].TypeName != "int4" {
		t.Errorf("col[0] = %v, want id/int4", cols[0])
	}
	if cols[1].Name != "name" || cols[1].TypeName != "text" {
		t.Errorf("col[1] = %v, want name/text", cols[1])
	}
}

func TestParseDataRow(t *testing.T) {
	columns := []columnInfo{
		{Name: "id", OID: 23, TypeName: "int4"},
		{Name: "name", OID: 25, TypeName: "text"},
		{Name: "active", OID: 16, TypeName: "bool"},
	}

	// Build a DataRow: 3 columns, "42", "Alice", "t"
	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, 3) // 3 columns

	// "42"
	buf = binary.BigEndian.AppendUint32(buf, 2)
	buf = append(buf, []byte("42")...)

	// "Alice"
	buf = binary.BigEndian.AppendUint32(buf, 5)
	buf = append(buf, []byte("Alice")...)

	// "t"
	buf = binary.BigEndian.AppendUint32(buf, 1)
	buf = append(buf, []byte("t")...)

	row := parseDataRow(buf, columns)
	if len(row) != 3 {
		t.Fatalf("expected 3 values, got %d", len(row))
	}
	if row[0] != int64(42) {
		t.Errorf("row[0] = %v (type %T), want int64(42)", row[0], row[0])
	}
	if row[1] != "Alice" {
		t.Errorf("row[1] = %v, want Alice", row[1])
	}
	if row[2] != true {
		t.Errorf("row[2] = %v, want true", row[2])
	}
}

func TestParseDataRow_NULL(t *testing.T) {
	columns := []columnInfo{
		{Name: "id", OID: 23, TypeName: "int4"},
		{Name: "name", OID: 25, TypeName: "text"},
	}

	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, 2)

	// id = 1
	buf = binary.BigEndian.AppendUint32(buf, 1)
	buf = append(buf, '1')

	// name = NULL
	buf = binary.BigEndian.AppendUint32(buf, uint32(0xFFFFFFFF)) // -1

	row := parseDataRow(buf, columns)
	if row[0] != int64(1) {
		t.Errorf("row[0] = %v, want 1", row[0])
	}
	if row[1] != nil {
		t.Errorf("row[1] = %v, want nil", row[1])
	}
}

func TestConvertValue(t *testing.T) {
	tests := []struct {
		val    string
		oid    uint32
		expect any
	}{
		{"t", 16, true},
		{"f", 16, false},
		{"42", 23, int64(42)},
		{"3.14", 701, float64(3.14)},
		{"hello", 25, "hello"},
	}

	for _, tt := range tests {
		got := convertValue(tt.val, tt.oid)
		if got != tt.expect {
			t.Errorf("convertValue(%q, %d) = %v, want %v", tt.val, tt.oid, got, tt.expect)
		}
	}
}

func TestOIDToTypeName(t *testing.T) {
	tests := []struct {
		oid  uint32
		name string
	}{
		{16, "bool"},
		{23, "int4"},
		{25, "text"},
		{701, "float8"},
		{9999, "oid_9999"},
	}

	for _, tt := range tests {
		got := oidToTypeName(tt.oid)
		if got != tt.name {
			t.Errorf("oidToTypeName(%d) = %q, want %q", tt.oid, got, tt.name)
		}
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		header string
		expect string
	}{
		{"Bearer abc123", "abc123"},
		{"Bearer ", ""},
		{"Basic abc123", ""},
		{"", ""},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		got := extractBearerToken(req)
		if got != tt.expect {
			t.Errorf("extractBearerToken(%q) = %q, want %q", tt.header, got, tt.expect)
		}
	}
}

func TestParseCommandComplete(t *testing.T) {
	payload := append([]byte("SELECT 2"), 0)
	got := parseCommandComplete(payload)
	if got != "SELECT 2" {
		t.Errorf("parseCommandComplete = %q, want SELECT 2", got)
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "test error")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}

	var resp ErrorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "test error" {
		t.Errorf("error = %q, want test error", resp.Error)
	}
}
