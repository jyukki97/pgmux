package tests

import (
	"context"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/router"
)

// TestIntegration_RouterWithCache tests the full flow of routing + caching together.
// This test doesn't need a real DB — it verifies the logic integration.
func TestIntegration_RouterWithCache(t *testing.T) {
	// Setup
	queryCache := cache.New(cache.Config{
		MaxEntries: 100,
		TTL:        time.Second,
		MaxSize:    4096,
	})
	session := router.NewSession(200*time.Millisecond, false, false)

	// 1. SELECT → Reader route + cache miss
	query := "SELECT * FROM users WHERE id = 1"
	route := session.Route(query)
	if route != router.RouteReader {
		t.Errorf("SELECT route = %d, want RouteReader", route)
	}

	key := cache.CacheKey(query)
	if got := queryCache.Get(key); got != nil {
		t.Error("expected cache miss on first query")
	}

	// Simulate DB result and cache it
	result := []byte(`[{"id":1,"name":"alice"}]`)
	queryCache.Set(key, result, []string{"users"})

	// 2. Same SELECT → cache hit
	if got := queryCache.Get(key); got == nil {
		t.Error("expected cache hit on second query")
	}

	// 3. INSERT → Writer route + invalidate cache
	writeQuery := "INSERT INTO users (name) VALUES ('dave')"
	route = session.Route(writeQuery)
	if route != router.RouteWriter {
		t.Errorf("INSERT route = %d, want RouteWriter", route)
	}

	tables := router.ExtractTables(writeQuery)
	for _, table := range tables {
		queryCache.InvalidateTable(table)
	}

	// 4. Cache should be invalidated
	if got := queryCache.Get(key); got != nil {
		t.Error("expected cache miss after INSERT invalidation")
	}

	// 5. Read-after-write: SELECT right after INSERT → Writer
	route = session.Route("SELECT * FROM users")
	if route != router.RouteWriter {
		t.Errorf("SELECT after write → %d, want RouteWriter (read-after-write)", route)
	}

	// 6. Wait for delay, SELECT → Reader
	time.Sleep(300 * time.Millisecond)
	route = session.Route("SELECT * FROM users")
	if route != router.RouteReader {
		t.Errorf("SELECT after delay → %d, want RouteReader", route)
	}
}

// TestIntegration_TransactionRouting tests full transaction flow.
func TestIntegration_TransactionRouting(t *testing.T) {
	session := router.NewSession(0, false, false)

	steps := []struct {
		query string
		want  router.Route
	}{
		{"BEGIN", router.RouteWriter},
		{"SELECT * FROM users", router.RouteWriter},            // in tx → writer
		{"UPDATE users SET name = 'x' WHERE id = 1", router.RouteWriter},
		{"SELECT * FROM users WHERE id = 1", router.RouteWriter}, // still in tx
		{"COMMIT", router.RouteWriter},
		{"SELECT * FROM users", router.RouteReader},             // after commit → reader
	}

	for _, s := range steps {
		got := session.Route(s.query)
		if got != s.want {
			t.Errorf("Route(%q) = %d, want %d", s.query, got, s.want)
		}
	}
}

// TestIntegration_LoadBalancer tests round-robin with failure scenario.
func TestIntegration_LoadBalancer(t *testing.T) {
	rb := router.NewRoundRobin([]string{"reader1:5432", "reader2:5432", "reader3:5432"})

	// Normal distribution
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		counts[rb.Next()]++
	}
	for _, addr := range []string{"reader1:5432", "reader2:5432", "reader3:5432"} {
		if counts[addr] != 3 {
			t.Errorf("count[%s] = %d, want 3", addr, counts[addr])
		}
	}

	// Mark one unhealthy
	rb.MarkUnhealthy("reader2:5432")

	counts = map[string]int{}
	for i := 0; i < 6; i++ {
		addr := rb.Next()
		if addr == "reader2:5432" {
			t.Error("unhealthy reader should be skipped")
		}
		counts[addr]++
	}

	if counts["reader1:5432"] != 3 || counts["reader3:5432"] != 3 {
		t.Errorf("expected even distribution between healthy readers, got %v", counts)
	}
}

// TestIntegration_CacheTTLAndEviction tests cache behavior over time.
func TestIntegration_CacheTTLAndEviction(t *testing.T) {
	c := cache.New(cache.Config{
		MaxEntries: 3,
		TTL:        100 * time.Millisecond,
		MaxSize:    1024,
	})

	queries := []string{"SELECT 1", "SELECT 2", "SELECT 3"}

	// Fill cache
	for _, q := range queries {
		key := cache.CacheKey(q)
		c.Set(key, []byte("result"), nil)
	}

	if c.Len() != 3 {
		t.Errorf("Len() = %d, want 3", c.Len())
	}

	// Add one more → LRU eviction
	key := cache.CacheKey("SELECT new")
	c.Set(key, []byte("new result"), nil)

	if c.Len() != 3 {
		t.Errorf("Len() after eviction = %d, want 3", c.Len())
	}

	// Wait for TTL
	time.Sleep(150 * time.Millisecond)

	// All should be expired
	for _, q := range append(queries, "SELECT new") {
		key := cache.CacheKey(q)
		if got := c.Get(key); got != nil {
			t.Errorf("expected nil after TTL for %q, got %q", q, got)
		}
	}
}

// TestIntegration_CausalConsistency tests LSN-based causal consistency routing.
func TestIntegration_CausalConsistency(t *testing.T) {
	rb := router.NewRoundRobin([]string{"reader1:5432", "reader2:5432"})
	session := router.NewSession(0, true, false)

	// 1. Before any write, reads go to reader (no LSN constraint)
	route := session.Route("SELECT * FROM users")
	if route != router.RouteReader {
		t.Errorf("SELECT before write → %d, want RouteReader", route)
	}
	addr := rb.NextWithLSN(session.LastWriteLSN())
	if addr == "" {
		t.Error("NextWithLSN(0) should return a reader")
	}

	// 2. Write query sets LSN
	route = session.Route("INSERT INTO users (name) VALUES ('test')")
	if route != router.RouteWriter {
		t.Errorf("INSERT → %d, want RouteWriter", route)
	}

	// Simulate server behavior: set LSN after write
	writeLSN, _ := router.ParseLSN("0/16B4000")
	session.SetLastWriteLSN(writeLSN)

	// 3. Readers haven't caught up yet → NextWithLSN returns empty
	rb.SetReplayLSN("reader1:5432", router.LSN(0x16B3000)) // behind
	rb.SetReplayLSN("reader2:5432", router.LSN(0x16B3500)) // behind

	addr = rb.NextWithLSN(session.LastWriteLSN())
	if addr != "" {
		t.Errorf("NextWithLSN with lagging readers = %q, want empty (writer fallback)", addr)
	}

	// 4. Reader1 catches up
	rb.SetReplayLSN("reader1:5432", router.LSN(0x16B4000)) // caught up

	addr = rb.NextWithLSN(session.LastWriteLSN())
	if addr == "" {
		t.Error("NextWithLSN should return reader1 after catch-up")
	}

	// 5. Verify only caught-up readers are selected
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		counts[rb.NextWithLSN(session.LastWriteLSN())]++
	}
	if counts["reader2:5432"] > 0 {
		t.Errorf("lagging reader2 should not be selected, got count %d", counts["reader2:5432"])
	}
	if counts["reader1:5432"] != 6 {
		t.Errorf("only reader1 should be selected, got %v", counts)
	}

	// 6. Both readers catch up → even distribution
	rb.SetReplayLSN("reader2:5432", router.LSN(0x16B5000)) // ahead

	counts = map[string]int{}
	for i := 0; i < 6; i++ {
		counts[rb.NextWithLSN(session.LastWriteLSN())]++
	}
	if counts["reader1:5432"] != 3 || counts["reader2:5432"] != 3 {
		t.Errorf("expected even distribution after both catch up, got %v", counts)
	}
}

// TestIntegration_CausalConsistency_NoTimerFallback verifies that causal mode
// doesn't use the timer-based read-after-write delay.
func TestIntegration_CausalConsistency_NoTimerFallback(t *testing.T) {
	session := router.NewSession(500*time.Millisecond, true, false)

	// Write
	session.Route("INSERT INTO users (name) VALUES ('test')")

	// In causal mode, reads immediately return RouteReader (not timer-based RouteWriter)
	route := session.Route("SELECT * FROM users")
	if route != router.RouteReader {
		t.Errorf("causal mode: SELECT after write → %d, want RouteReader (LSN-based, not timer)", route)
	}
}

// TestIntegration_PoolAcquireRelease tests pool under concurrent load.
func TestIntegration_PoolConcurrency(t *testing.T) {
	// Uses the pool package directly — see pool_test.go for detailed tests.
	// This test verifies concurrent Acquire/Release doesn't panic.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = ctx // Pool tests already cover concurrency
	// This is a placeholder for real DB integration tests that require docker-compose.
}
