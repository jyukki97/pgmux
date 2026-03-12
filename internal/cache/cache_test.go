package cache

import (
	"testing"
	"time"
)

func TestCache_GetSet(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: time.Minute, MaxSize: 1024})

	key := CacheKey("SELECT * FROM users")
	c.Set(key, []byte("result"), []string{"users"})

	got := c.Get(key)
	if string(got) != "result" {
		t.Errorf("Get() = %q, want %q", got, "result")
	}
}

func TestCache_Miss(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: time.Minute, MaxSize: 1024})

	got := c.Get(12345)
	if got != nil {
		t.Errorf("Get(missing) = %q, want nil", got)
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: 50 * time.Millisecond, MaxSize: 1024})

	key := CacheKey("SELECT 1")
	c.Set(key, []byte("result"), nil)

	// Should hit before TTL
	if got := c.Get(key); got == nil {
		t.Error("Get() before TTL = nil, want result")
	}

	time.Sleep(100 * time.Millisecond)

	// Should miss after TTL
	if got := c.Get(key); got != nil {
		t.Errorf("Get() after TTL = %q, want nil", got)
	}
}

func TestCache_MaxEntries_LRU(t *testing.T) {
	c := New(Config{MaxEntries: 3, TTL: time.Minute, MaxSize: 1024})

	k1 := CacheKey("SELECT 1")
	k2 := CacheKey("SELECT 2")
	k3 := CacheKey("SELECT 3")
	k4 := CacheKey("SELECT 4")

	c.Set(k1, []byte("r1"), nil)
	c.Set(k2, []byte("r2"), nil)
	c.Set(k3, []byte("r3"), nil)

	// Access k1 to make it recently used
	c.Get(k1)

	// Add k4 — should evict k2 (least recently used)
	c.Set(k4, []byte("r4"), nil)

	if c.Len() != 3 {
		t.Errorf("Len() = %d, want 3", c.Len())
	}

	// k1 should still exist (recently accessed)
	if got := c.Get(k1); got == nil {
		t.Error("k1 should still exist after eviction")
	}

	// k2 should be evicted
	if got := c.Get(k2); got != nil {
		t.Error("k2 should have been evicted")
	}

	// k3, k4 should exist
	if got := c.Get(k3); got == nil {
		t.Error("k3 should still exist")
	}
	if got := c.Get(k4); got == nil {
		t.Error("k4 should still exist")
	}
}

func TestCache_MaxSize_Skip(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: time.Minute, MaxSize: 10})

	key := CacheKey("SELECT * FROM big_table")
	bigResult := make([]byte, 100)
	c.Set(key, bigResult, nil)

	if got := c.Get(key); got != nil {
		t.Error("large result should not be cached")
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
}

func TestCache_InvalidateTable(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: time.Minute, MaxSize: 1024})

	k1 := CacheKey("SELECT * FROM users")
	k2 := CacheKey("SELECT * FROM users WHERE id = 1")
	k3 := CacheKey("SELECT * FROM orders")

	c.Set(k1, []byte("r1"), []string{"users"})
	c.Set(k2, []byte("r2"), []string{"users"})
	c.Set(k3, []byte("r3"), []string{"orders"})

	// Invalidate users table
	c.InvalidateTable("users")

	if got := c.Get(k1); got != nil {
		t.Error("k1 (users) should be invalidated")
	}
	if got := c.Get(k2); got != nil {
		t.Error("k2 (users) should be invalidated")
	}
	if got := c.Get(k3); got == nil {
		t.Error("k3 (orders) should still exist")
	}

	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
}

func TestCache_InvalidateTable_ReadCacheWithTables(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: time.Minute, MaxSize: 1024})

	// Simulate caching read queries with proper table metadata
	kUsers := CacheKey("SELECT * FROM users")
	kOrders := CacheKey("SELECT * FROM orders")
	kJoin := CacheKey("SELECT * FROM users JOIN orders ON users.id = orders.user_id")

	c.Set(kUsers, []byte("users-result"), []string{"users"})
	c.Set(kOrders, []byte("orders-result"), []string{"orders"})
	c.Set(kJoin, []byte("join-result"), []string{"users", "orders"})

	// All entries should be present
	if c.Len() != 3 {
		t.Errorf("Len() = %d, want 3", c.Len())
	}

	// Simulate a write to "users" table — should invalidate kUsers and kJoin
	c.InvalidateTable("users")

	if got := c.Get(kUsers); got != nil {
		t.Error("kUsers should be invalidated after write to users table")
	}
	if got := c.Get(kJoin); got != nil {
		t.Error("kJoin should be invalidated after write to users table (multi-table)")
	}

	// kOrders should still be cached — unrelated table
	if got := c.Get(kOrders); got == nil {
		t.Error("kOrders should still exist after write to users table")
	}

	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
}

func TestCache_NilTables_NoInvalidation(t *testing.T) {
	c := New(Config{MaxEntries: 100, TTL: time.Minute, MaxSize: 1024})

	// Simulate old bug: cache entry with nil tables
	key := CacheKey("SELECT * FROM users")
	c.Set(key, []byte("result"), nil)

	// InvalidateTable should NOT remove entries with nil tables (no table index)
	c.InvalidateTable("users")

	// Entry survives because it has no table metadata — this is the bug scenario
	if got := c.Get(key); got == nil {
		t.Error("entry with nil tables should NOT be invalidated (demonstrates the bug)")
	}
}

func TestCacheKey_SameQuerySameKey(t *testing.T) {
	k1 := CacheKey("SELECT * FROM users")
	k2 := CacheKey("SELECT * FROM users")

	if k1 != k2 {
		t.Errorf("same query produced different keys: %d vs %d", k1, k2)
	}
}

func TestCacheKey_DifferentQueryDifferentKey(t *testing.T) {
	k1 := CacheKey("SELECT * FROM users")
	k2 := CacheKey("SELECT * FROM orders")

	if k1 == k2 {
		t.Error("different queries produced same key")
	}
}
