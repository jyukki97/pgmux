package resilience

import (
	"sync"
	"testing"
	"time"
)

func TestRegistry_DefaultRateLimit(t *testing.T) {
	r := NewRateLimiterRegistry(RegistryConfig{
		DefaultRate:  10,
		DefaultBurst: 3,
		CleanupTTL:   time.Minute,
	})
	defer r.Close()

	// Should allow burst of 3 for a new key
	for i := 0; i < 3; i++ {
		if !r.Allow("user1") {
			t.Errorf("request %d for user1 should be allowed (within burst)", i)
		}
	}

	// 4th should be rejected
	if r.Allow("user1") {
		t.Error("4th request for user1 should be rejected (burst exhausted)")
	}

	// Different key has its own bucket
	if !r.Allow("user2") {
		t.Error("first request for user2 should be allowed")
	}
}

func TestRegistry_Override(t *testing.T) {
	r := NewRateLimiterRegistry(RegistryConfig{
		DefaultRate:  10,
		DefaultBurst: 2,
		Overrides: map[string]Override{
			"vip": {Rate: 10, Burst: 5},
		},
		CleanupTTL: time.Minute,
	})
	defer r.Close()

	// Default user: burst=2
	for i := 0; i < 2; i++ {
		if !r.Allow("regular") {
			t.Errorf("request %d for regular should be allowed", i)
		}
	}
	if r.Allow("regular") {
		t.Error("3rd request for regular should be rejected")
	}

	// VIP user: burst=5
	for i := 0; i < 5; i++ {
		if !r.Allow("vip") {
			t.Errorf("request %d for vip should be allowed", i)
		}
	}
	if r.Allow("vip") {
		t.Error("6th request for vip should be rejected")
	}
}

func TestRegistry_Eviction(t *testing.T) {
	r := NewRateLimiterRegistry(RegistryConfig{
		DefaultRate:  100,
		DefaultBurst: 10,
		CleanupTTL:   50 * time.Millisecond, // very short for test
	})
	defer r.Close()

	r.Allow("ephemeral")
	if r.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", r.Len())
	}

	// Wait for eviction to kick in
	time.Sleep(100 * time.Millisecond)
	r.evict() // force immediate eviction

	if r.Len() != 0 {
		t.Errorf("expected 0 entries after eviction, got %d", r.Len())
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRateLimiterRegistry(RegistryConfig{
		DefaultRate:  10000,
		DefaultBurst: 100,
		CleanupTTL:   time.Minute,
	})
	defer r.Close()

	var wg sync.WaitGroup
	keys := []string{"a", "b", "c", "d", "e"}
	for _, key := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				r.Allow(k)
			}
		}(key)
	}
	wg.Wait()

	if r.Len() != len(keys) {
		t.Errorf("expected %d entries, got %d", len(keys), r.Len())
	}
}

func TestRegistry_Len(t *testing.T) {
	r := NewRateLimiterRegistry(RegistryConfig{
		DefaultRate:  10,
		DefaultBurst: 5,
		CleanupTTL:   time.Minute,
	})
	defer r.Close()

	if r.Len() != 0 {
		t.Errorf("expected 0 entries, got %d", r.Len())
	}

	r.Allow("a")
	r.Allow("b")
	r.Allow("a") // same key, should not create duplicate

	if r.Len() != 2 {
		t.Errorf("expected 2 entries, got %d", r.Len())
	}
}
