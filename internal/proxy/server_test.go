package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
)

func testConfig(listen string) *config.Config {
	return &config.Config{
		Proxy: config.ProxyConfig{Listen: listen},
		Pool: config.PoolConfig{
			MinConnections:    0,
			MaxConnections:    10,
			IdleTimeout:       10 * time.Minute,
			MaxLifetime:       time.Hour,
			ConnectionTimeout: 5 * time.Second,
		},
		Routing: config.RoutingConfig{
			ReadAfterWriteDelay: 500 * time.Millisecond,
		},
		Cache: config.CacheConfig{
			Enabled:         false, // disable cache for unit tests
			CacheTTL:        10 * time.Second,
			MaxCacheEntries: 1000,
			MaxResultSize:   "1MB",
		},
		Backend: config.BackendConfig{
			User:     "postgres",
			Password: "postgres",
			Database: "testdb",
		},
		Databases: map[string]config.DatabaseConfig{
			"testdb": {
				Writer: config.DBConfig{Host: "127.0.0.1", Port: 5432},
				Readers: []config.DBConfig{
					{Host: "127.0.0.1", Port: 5433},
				},
				Backend: config.BackendConfig{
					User:     "postgres",
					Password: "postgres",
					Database: "testdb",
				},
			},
		},
	}
}

func TestServer_AcceptsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, _ := NewServer(testConfig("127.0.0.1:0"))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = ln
	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.wg.Add(1)
			go func() {
				defer srv.wg.Done()
				defer conn.Close()
				<-ctx.Done()
			}()
		}
	}()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	conn.Close()

	cancel()
}

func TestServer_GracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	srv, _ := NewServer(testConfig("127.0.0.1:0"))

	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timed out")
	}
}

func TestServer_MaintenanceMode(t *testing.T) {
	srv, _ := NewServer(testConfig("127.0.0.1:0"))

	// Initially disabled
	if srv.InMaintenance() {
		t.Error("expected maintenance mode to be disabled initially")
	}
	enabled, at := srv.MaintenanceState()
	if enabled {
		t.Error("expected MaintenanceState.enabled = false")
	}
	if !at.IsZero() {
		t.Error("expected MaintenanceState.at to be zero")
	}

	// Enable maintenance
	srv.SetMaintenance(true)
	if !srv.InMaintenance() {
		t.Error("expected maintenance mode to be enabled")
	}
	enabled, at = srv.MaintenanceState()
	if !enabled {
		t.Error("expected MaintenanceState.enabled = true")
	}
	if at.IsZero() {
		t.Error("expected MaintenanceState.at to be non-zero")
	}
	if time.Since(at) > time.Second {
		t.Error("entered_at should be recent")
	}

	// Disable maintenance
	srv.SetMaintenance(false)
	if srv.InMaintenance() {
		t.Error("expected maintenance mode to be disabled")
	}
	enabled, at = srv.MaintenanceState()
	if enabled {
		t.Error("expected MaintenanceState.enabled = false after disable")
	}
	if !at.IsZero() {
		t.Error("expected MaintenanceState.at to be zero after disable")
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"512KB", 512 * 1024},
		{"1MB", 1024 * 1024},
		{"1024", 1024},
		{"", 0},
	}

	for _, tt := range tests {
		got := parseSize(tt.input)
		if got != tt.want {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
