package resilience

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsBurst(t *testing.T) {
	rl := NewRateLimiter(10, 5) // 10/s, burst 5

	// Should allow burst of 5
	for i := 0; i < 5; i++ {
		if !rl.Allow() {
			t.Errorf("request %d should be allowed (within burst)", i)
		}
	}

	// 6th should be rejected (burst exhausted)
	if rl.Allow() {
		t.Error("6th request should be rejected (burst exhausted)")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	rl := NewRateLimiter(100, 5) // 100/s, burst 5

	// Exhaust all tokens
	for i := 0; i < 5; i++ {
		rl.Allow()
	}

	// Wait for some tokens to refill (100/s = 1 token per 10ms)
	time.Sleep(30 * time.Millisecond) // should refill ~3 tokens

	allowed := 0
	for i := 0; i < 5; i++ {
		if rl.Allow() {
			allowed++
		}
	}

	// Should have refilled at least 1 token (timing can be imprecise)
	if allowed < 1 {
		t.Errorf("expected at least 1 allowed after refill, got %d", allowed)
	}
}

func TestRateLimiter_SteadyRate(t *testing.T) {
	rl := NewRateLimiter(1000, 1) // 1000/s, burst 1

	// Exhaust the 1 burst token
	if !rl.Allow() {
		t.Error("first request should be allowed")
	}
	if rl.Allow() {
		t.Error("second immediate request should be rejected")
	}

	// Wait 2ms for ~2 tokens to refill
	time.Sleep(2 * time.Millisecond)

	if !rl.Allow() {
		t.Error("request after refill should be allowed")
	}
}
