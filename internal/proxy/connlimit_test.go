package proxy

import (
	"fmt"
	"sync"
	"testing"

	"github.com/jyukki97/pgmux/internal/config"
)

func connLimitTestConfig(defaultUser, defaultDB int, users []config.AuthUser, dbs map[string]config.DatabaseConfig) *config.Config {
	cfg := &config.Config{
		ConnectionLimits: config.ConnectionLimitsConfig{
			Enabled:                      true,
			DefaultMaxConnectionsPerUser: defaultUser,
			DefaultMaxConnectionsPerDB:   defaultDB,
		},
		Auth: config.AuthConfig{Users: users},
		// Use Backend for ResolvedDatabases fallback
		Writer:  config.DBConfig{Host: "localhost", Port: 5432},
		Backend: config.BackendConfig{User: "postgres", Password: "pass", Database: "testdb"},
	}
	if dbs != nil {
		cfg.Databases = dbs
	}
	return cfg
}

func TestConnTracker_BasicAcquireRelease(t *testing.T) {
	cfg := connLimitTestConfig(2, 0, nil, nil)
	ct := NewConnTracker(cfg)

	ok, _ := ct.TryAcquire("alice", "testdb")
	if !ok {
		t.Fatal("first acquire should succeed")
	}

	ok, _ = ct.TryAcquire("alice", "testdb")
	if !ok {
		t.Fatal("second acquire should succeed (limit=2)")
	}

	ok, reason := ct.TryAcquire("alice", "testdb")
	if ok {
		t.Fatal("third acquire should be rejected (limit=2)")
	}
	if reason == "" {
		t.Fatal("reason should be non-empty")
	}

	ct.Release("alice", "testdb")

	ok, _ = ct.TryAcquire("alice", "testdb")
	if !ok {
		t.Fatal("acquire after release should succeed")
	}
}

func TestConnTracker_PerUserOverride(t *testing.T) {
	users := []config.AuthUser{
		{Username: "admin", Password: "pass", MaxConnections: 5},
		{Username: "limited", Password: "pass", MaxConnections: 1},
	}
	cfg := connLimitTestConfig(3, 0, users, nil)
	ct := NewConnTracker(cfg)

	// admin has override of 5
	for i := 0; i < 5; i++ {
		ok, _ := ct.TryAcquire("admin", "testdb")
		if !ok {
			t.Fatalf("admin acquire %d should succeed", i+1)
		}
	}
	ok, _ := ct.TryAcquire("admin", "testdb")
	if ok {
		t.Fatal("admin 6th acquire should be rejected (limit=5)")
	}

	// limited has override of 1
	ok, _ = ct.TryAcquire("limited", "testdb")
	if !ok {
		t.Fatal("limited first acquire should succeed")
	}
	ok, _ = ct.TryAcquire("limited", "testdb")
	if ok {
		t.Fatal("limited second acquire should be rejected (limit=1)")
	}

	// unknown user uses default (3)
	for i := 0; i < 3; i++ {
		ok, _ = ct.TryAcquire("unknown", "testdb")
		if !ok {
			t.Fatalf("unknown acquire %d should succeed", i+1)
		}
	}
	ok, _ = ct.TryAcquire("unknown", "testdb")
	if ok {
		t.Fatal("unknown 4th acquire should be rejected (default=3)")
	}
}

func TestConnTracker_PerDBLimit(t *testing.T) {
	dbs := map[string]config.DatabaseConfig{
		"prod": {
			Writer:         config.DBConfig{Host: "localhost", Port: 5432},
			Backend:        config.BackendConfig{User: "postgres", Password: "pass", Database: "prod"},
			MaxConnections: 2,
		},
	}
	cfg := connLimitTestConfig(0, 10, nil, dbs)
	ct := NewConnTracker(cfg)

	// prod has override of 2
	ok, _ := ct.TryAcquire("alice", "prod")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	ok, _ = ct.TryAcquire("bob", "prod")
	if !ok {
		t.Fatal("second acquire should succeed")
	}
	ok, _ = ct.TryAcquire("carol", "prod")
	if ok {
		t.Fatal("third acquire should be rejected (db limit=2)")
	}
}

func TestConnTracker_UnlimitedWhenZero(t *testing.T) {
	cfg := connLimitTestConfig(0, 0, nil, nil)
	ct := NewConnTracker(cfg)

	for i := 0; i < 1000; i++ {
		ok, _ := ct.TryAcquire("user", "db")
		if !ok {
			t.Fatalf("acquire %d should succeed (unlimited)", i+1)
		}
	}
}

func TestConnTracker_UpdateLimits(t *testing.T) {
	cfg := connLimitTestConfig(2, 0, nil, nil)
	ct := NewConnTracker(cfg)

	ok, _ := ct.TryAcquire("alice", "testdb")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	ok, _ = ct.TryAcquire("alice", "testdb")
	if !ok {
		t.Fatal("second acquire should succeed")
	}

	// Lower limit to 1 — existing connections stay, but new ones rejected
	newCfg := connLimitTestConfig(1, 0, nil, nil)
	ct.UpdateLimits(newCfg)

	ok, _ = ct.TryAcquire("alice", "testdb")
	if ok {
		t.Fatal("acquire after lowering limit should be rejected")
	}

	// Raise limit to 5
	newCfg2 := connLimitTestConfig(5, 0, nil, nil)
	ct.UpdateLimits(newCfg2)

	ok, _ = ct.TryAcquire("alice", "testdb")
	if !ok {
		t.Fatal("acquire after raising limit should succeed")
	}
}

func TestConnTracker_Stats(t *testing.T) {
	users := []config.AuthUser{
		{Username: "admin", Password: "pass", MaxConnections: 10},
	}
	cfg := connLimitTestConfig(5, 100, users, nil)
	ct := NewConnTracker(cfg)

	ct.TryAcquire("admin", "testdb")
	ct.TryAcquire("admin", "testdb")
	ct.TryAcquire("guest", "testdb")

	stats := ct.Stats()

	if stats.Defaults.PerUser != 5 {
		t.Errorf("defaults.per_user = %d, want 5", stats.Defaults.PerUser)
	}
	if stats.Defaults.PerDB != 100 {
		t.Errorf("defaults.per_db = %d, want 100", stats.Defaults.PerDB)
	}
	if stats.ByUser["admin"].Active != 2 {
		t.Errorf("admin active = %d, want 2", stats.ByUser["admin"].Active)
	}
	if stats.ByUser["admin"].Limit != 10 {
		t.Errorf("admin limit = %d, want 10", stats.ByUser["admin"].Limit)
	}
	if stats.ByUser["guest"].Active != 1 {
		t.Errorf("guest active = %d, want 1", stats.ByUser["guest"].Active)
	}
	if stats.ByUser["guest"].Limit != 5 {
		t.Errorf("guest limit = %d, want 5 (default)", stats.ByUser["guest"].Limit)
	}
	if stats.ByDB["testdb"].Active != 3 {
		t.Errorf("testdb active = %d, want 3", stats.ByDB["testdb"].Active)
	}
}

func TestConnTracker_Concurrent(t *testing.T) {
	cfg := connLimitTestConfig(100, 0, nil, nil)
	ct := NewConnTracker(cfg)

	var wg sync.WaitGroup
	acquired := make(chan struct{}, 200)

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user%d", i%10)
			ok, _ := ct.TryAcquire(user, "testdb")
			if ok {
				acquired <- struct{}{}
				ct.Release(user, "testdb")
			}
		}(i)
	}

	wg.Wait()
	close(acquired)

	count := 0
	for range acquired {
		count++
	}

	// All 200 should succeed (10 users × 100 limit, each releases immediately)
	if count != 200 {
		t.Errorf("acquired = %d, want 200", count)
	}
}
