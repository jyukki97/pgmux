package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	content := `
proxy:
  listen: "0.0.0.0:5432"
pool:
  min_connections: 5
  max_connections: 50
  idle_timeout: 10m
  max_lifetime: 1h
  connection_timeout: 5s
routing:
  read_after_write_delay: 500ms
cache:
  enabled: true
  cache_ttl: 10s
  max_cache_entries: 10000
  max_result_size: "1MB"
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
    readers:
      - host: "replica-1.db.internal"
        port: 5432
      - host: "replica-2.db.internal"
        port: 5432
    backend:
      user: "postgres"
      password: "postgres"
      database: "testdb"
`

	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Proxy
	if cfg.Proxy.Listen != "0.0.0.0:5432" {
		t.Errorf("Proxy.Listen = %q, want %q", cfg.Proxy.Listen, "0.0.0.0:5432")
	}

	// Databases
	db, ok := cfg.Databases["testdb"]
	if !ok {
		t.Fatal("databases[testdb] not found")
	}
	if db.Writer.Host != "primary.db.internal" {
		t.Errorf("Writer.Host = %q, want %q", db.Writer.Host, "primary.db.internal")
	}
	if db.Writer.Port != 5432 {
		t.Errorf("Writer.Port = %d, want %d", db.Writer.Port, 5432)
	}
	if len(db.Readers) != 2 {
		t.Fatalf("len(Readers) = %d, want 2", len(db.Readers))
	}
	if db.Readers[0].Host != "replica-1.db.internal" {
		t.Errorf("Readers[0].Host = %q, want %q", db.Readers[0].Host, "replica-1.db.internal")
	}

	// Pool
	if cfg.Pool.MinConnections != 5 {
		t.Errorf("Pool.MinConnections = %d, want 5", cfg.Pool.MinConnections)
	}
	if cfg.Pool.MaxConnections != 50 {
		t.Errorf("Pool.MaxConnections = %d, want 50", cfg.Pool.MaxConnections)
	}
	if cfg.Pool.IdleTimeout != 10*time.Minute {
		t.Errorf("Pool.IdleTimeout = %v, want 10m", cfg.Pool.IdleTimeout)
	}
	if cfg.Pool.MaxLifetime != time.Hour {
		t.Errorf("Pool.MaxLifetime = %v, want 1h", cfg.Pool.MaxLifetime)
	}
	if cfg.Pool.ConnectionTimeout != 5*time.Second {
		t.Errorf("Pool.ConnectionTimeout = %v, want 5s", cfg.Pool.ConnectionTimeout)
	}

	// Routing
	if cfg.Routing.ReadAfterWriteDelay != 500*time.Millisecond {
		t.Errorf("Routing.ReadAfterWriteDelay = %v, want 500ms", cfg.Routing.ReadAfterWriteDelay)
	}

	// Cache
	if !cfg.Cache.Enabled {
		t.Error("Cache.Enabled = false, want true")
	}
	if cfg.Cache.CacheTTL != 10*time.Second {
		t.Errorf("Cache.CacheTTL = %v, want 10s", cfg.Cache.CacheTTL)
	}
	if cfg.Cache.MaxCacheEntries != 10000 {
		t.Errorf("Cache.MaxCacheEntries = %d, want 10000", cfg.Cache.MaxCacheEntries)
	}
	if cfg.Cache.MaxResultSize != "1MB" {
		t.Errorf("Cache.MaxResultSize = %q, want %q", cfg.Cache.MaxResultSize, "1MB")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("nonexistent.yaml")
	if err == nil {
		t.Error("Load() expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("invalid: yaml: [broken"); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	_, err = Load(tmpFile.Name())
	if err == nil {
		t.Error("Load() expected error for invalid YAML, got nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
    readers:
      - host: "replica-1.db.internal"
        port: 5432
`
	cfg := loadFromString(t, content)

	if cfg.Proxy.Listen != "0.0.0.0:5432" {
		t.Errorf("default Proxy.Listen = %q, want %q", cfg.Proxy.Listen, "0.0.0.0:5432")
	}
	if cfg.Pool.MinConnections != 2 {
		t.Errorf("default Pool.MinConnections = %d, want 2", cfg.Pool.MinConnections)
	}
	if cfg.Pool.MaxConnections != 10 {
		t.Errorf("default Pool.MaxConnections = %d, want 10", cfg.Pool.MaxConnections)
	}
	if cfg.Pool.IdleTimeout != 10*time.Minute {
		t.Errorf("default Pool.IdleTimeout = %v, want 10m", cfg.Pool.IdleTimeout)
	}
	if cfg.Cache.MaxResultSize != "1MB" {
		t.Errorf("default Cache.MaxResultSize = %q, want %q", cfg.Cache.MaxResultSize, "1MB")
	}
}

func TestLoad_Validation_NoDatabases(t *testing.T) {
	content := `
pool:
  min_connections: 5
  max_connections: 50
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for missing databases")
	}
}

func TestLoad_Validation_MissingWriter(t *testing.T) {
	content := `
databases:
  testdb:
    readers:
      - host: "replica-1.db.internal"
        port: 5432
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for missing writer host")
	}
}

func TestLoad_Validation_NoReaders(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
`
	cfg, err := loadFromStringRaw(t, content)
	if err != nil {
		t.Fatalf("unexpected error for no readers: %v", err)
	}
	if len(cfg.Databases["testdb"].Readers) != 0 {
		t.Errorf("expected empty readers, got %d", len(cfg.Databases["testdb"].Readers))
	}
}

func TestLoad_Validation_MinGreaterThanMax(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
pool:
  min_connections: 100
  max_connections: 10
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for min > max connections")
	}
}

func TestLoad_Validation_InvalidPort(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 99999
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for invalid writer port")
	}
}

func TestLoad_Auth_Disabled(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
`
	cfg := loadFromString(t, content)
	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled should be false by default")
	}
}

func TestLoad_Auth_Enabled(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
auth:
  enabled: true
  users:
    - username: "app_user"
      password: "secret"
    - username: "readonly"
      password: "readonly_pass"
`
	cfg := loadFromString(t, content)
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled should be true")
	}
	if len(cfg.Auth.Users) != 2 {
		t.Fatalf("len(Auth.Users) = %d, want 2", len(cfg.Auth.Users))
	}
	if cfg.Auth.Users[0].Username != "app_user" {
		t.Errorf("Auth.Users[0].Username = %q, want app_user", cfg.Auth.Users[0].Username)
	}
}

func TestLoad_Auth_EnabledNoUsers(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
auth:
  enabled: true
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for auth.enabled with no users")
	}
}

func TestLoad_TLS_Disabled(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
`
	cfg := loadFromString(t, content)
	if cfg.TLS.Enabled {
		t.Error("TLS.Enabled should be false by default")
	}
}

func TestLoad_TLS_MissingCertFile(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
tls:
  enabled: true
  key_file: "/tmp/nonexistent.key"
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for missing cert_file")
	}
}

func TestLoad_TLS_MissingKeyFile(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
tls:
  enabled: true
  cert_file: "/tmp/nonexistent.crt"
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for missing key_file")
	}
}

func TestLoad_TLS_FileNotFound(t *testing.T) {
	content := `
databases:
  testdb:
    writer:
      host: "primary.db.internal"
      port: 5432
tls:
  enabled: true
  cert_file: "/tmp/nonexistent.crt"
  key_file: "/tmp/nonexistent.key"
`
	_, err := loadFromStringRaw(t, content)
	if err == nil {
		t.Error("expected error for nonexistent TLS files")
	}
}

func TestLoad_DatabasesInheritDefaults(t *testing.T) {
	content := `
backend:
  user: "shared_user"
  password: "shared_pass"
pool:
  max_connections: 100
  idle_timeout: 15m
databases:
  mydb:
    writer:
      host: "primary.db.internal"
      port: 5432
`
	cfg := loadFromString(t, content)

	db := cfg.Databases["mydb"]
	if db.Backend.User != "shared_user" {
		t.Errorf("db.Backend.User = %q, want %q", db.Backend.User, "shared_user")
	}
	if db.Backend.Password != "shared_pass" {
		t.Errorf("db.Backend.Password = %q, want %q", db.Backend.Password, "shared_pass")
	}
	if db.Backend.Database != "mydb" {
		t.Errorf("db.Backend.Database = %q, want %q (should default to key name)", db.Backend.Database, "mydb")
	}
	if db.Pool.MaxConnections != 100 {
		t.Errorf("db.Pool.MaxConnections = %d, want 100 (inherited)", db.Pool.MaxConnections)
	}
	if db.Pool.IdleTimeout != 15*time.Minute {
		t.Errorf("db.Pool.IdleTimeout = %v, want 15m (inherited)", db.Pool.IdleTimeout)
	}
}

func TestDefaultDatabaseName(t *testing.T) {
	cfg := &Config{
		Backend: BackendConfig{Database: "mydb"},
		Databases: map[string]DatabaseConfig{
			"mydb":  {Writer: DBConfig{Host: "h", Port: 5432}},
			"other": {Writer: DBConfig{Host: "h", Port: 5432}},
		},
	}
	if got := cfg.DefaultDatabaseName(); got != "mydb" {
		t.Errorf("DefaultDatabaseName() = %q, want %q", got, "mydb")
	}

	// When backend.database doesn't match, return lexicographic first
	cfg.Backend.Database = "nonexistent"
	if got := cfg.DefaultDatabaseName(); got != "mydb" {
		t.Errorf("DefaultDatabaseName() = %q, want %q (lex first)", got, "mydb")
	}
}

func loadFromString(t *testing.T, content string) *Config {
	t.Helper()
	cfg, err := loadFromStringRaw(t, content)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return cfg
}

func loadFromStringRaw(t *testing.T, content string) (*Config, error) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	return Load(tmpFile.Name())
}
