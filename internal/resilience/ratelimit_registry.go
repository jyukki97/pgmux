package resilience

import (
	"sync"
	"sync/atomic"
	"time"
)

// Override stores per-key rate/burst overrides.
type Override struct {
	Rate  float64
	Burst int
}

// entry wraps a RateLimiter with a last-access timestamp for TTL eviction.
type entry struct {
	limiter    *RateLimiter
	lastAccess atomic.Int64 // unix nano
}

func (e *entry) touch() {
	e.lastAccess.Store(time.Now().UnixNano())
}

// RateLimiterRegistry manages per-key (user/IP) rate limiters with TTL eviction.
type RateLimiterRegistry struct {
	mu           sync.RWMutex
	limiters     map[string]*entry
	defaultRate  float64
	defaultBurst int
	overrides    map[string]Override
	cleanupTTL   time.Duration
	stopCleanup  chan struct{}
}

// RegistryConfig holds configuration for creating a RateLimiterRegistry.
type RegistryConfig struct {
	DefaultRate  float64
	DefaultBurst int
	Overrides    map[string]Override
	CleanupTTL   time.Duration // 0 = default 10m
}

// NewRateLimiterRegistry creates a new per-key rate limiter registry.
func NewRateLimiterRegistry(cfg RegistryConfig) *RateLimiterRegistry {
	ttl := cfg.CleanupTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	r := &RateLimiterRegistry{
		limiters:     make(map[string]*entry),
		defaultRate:  cfg.DefaultRate,
		defaultBurst: cfg.DefaultBurst,
		overrides:    cfg.Overrides,
		cleanupTTL:   ttl,
		stopCleanup:  make(chan struct{}),
	}
	if r.overrides == nil {
		r.overrides = make(map[string]Override)
	}
	go r.cleanupLoop()
	return r
}

// Allow checks whether a request for the given key is allowed.
// It lazily creates a rate limiter for new keys.
func (r *RateLimiterRegistry) Allow(key string) bool {
	// Fast path: read-lock lookup
	r.mu.RLock()
	e, ok := r.limiters[key]
	r.mu.RUnlock()

	if ok {
		e.touch()
		return e.limiter.Allow()
	}

	// Slow path: create new entry
	r.mu.Lock()
	// Double-check after acquiring write lock
	if e, ok = r.limiters[key]; ok {
		r.mu.Unlock()
		e.touch()
		return e.limiter.Allow()
	}

	rate, burst := r.defaultRate, r.defaultBurst
	if o, found := r.overrides[key]; found {
		rate, burst = o.Rate, o.Burst
	}

	e = &entry{limiter: NewRateLimiter(rate, burst)}
	e.touch()
	r.limiters[key] = e
	r.mu.Unlock()

	return e.limiter.Allow()
}

// cleanupLoop periodically evicts entries that have not been accessed within cleanupTTL.
func (r *RateLimiterRegistry) cleanupLoop() {
	ticker := time.NewTicker(r.cleanupTTL / 2)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCleanup:
			return
		case <-ticker.C:
			r.evict()
		}
	}
}

func (r *RateLimiterRegistry) evict() {
	cutoff := time.Now().Add(-r.cleanupTTL).UnixNano()
	r.mu.Lock()
	for key, e := range r.limiters {
		if e.lastAccess.Load() < cutoff {
			delete(r.limiters, key)
		}
	}
	r.mu.Unlock()
}

// Close stops the background cleanup goroutine.
func (r *RateLimiterRegistry) Close() {
	close(r.stopCleanup)
}

// Len returns the number of active limiters (for testing/admin).
func (r *RateLimiterRegistry) Len() int {
	r.mu.RLock()
	n := len(r.limiters)
	r.mu.RUnlock()
	return n
}
