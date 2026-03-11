package resilience

import (
	"sync"
	"time"
)

// RateLimiter implements a Token Bucket rate limiter.
type RateLimiter struct {
	mu       sync.Mutex
	rate     float64   // tokens per second
	burst    int       // max tokens (bucket capacity)
	tokens   float64   // current tokens
	lastTime time.Time // last refill time
}

// NewRateLimiter creates a new Token Bucket rate limiter.
// rate: tokens added per second. burst: maximum tokens (bucket capacity).
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst), // start full
		lastTime: time.Now(),
	}
}

// Allow checks if a request is allowed. Returns true if a token is available.
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.lastTime = now

	// Refill tokens
	rl.tokens += elapsed * rl.rate
	if rl.tokens > float64(rl.burst) {
		rl.tokens = float64(rl.burst)
	}

	if rl.tokens >= 1.0 {
		rl.tokens--
		return true
	}
	return false
}
