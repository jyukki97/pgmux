package resilience

import (
	"testing"
	"time"
)

func TestCircuitBreaker_ClosedAllows(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{
		ErrorThreshold: 0.5,
		WindowSize:     10,
		OpenDuration:   time.Second,
	})

	if err := cb.Allow(); err != nil {
		t.Errorf("closed breaker should allow: %v", err)
	}
}

func TestCircuitBreaker_TripsOnErrors(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{
		ErrorThreshold: 0.5,
		WindowSize:     10,
		OpenDuration:   time.Second,
	})

	// 6 failures, 4 successes = 60% error rate → should trip
	for i := 0; i < 6; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	for i := 0; i < 4; i++ {
		cb.Allow()
		cb.RecordSuccess()
	}

	if cb.State() != StateOpen {
		t.Errorf("state = %v, want Open", cb.State())
	}
	if err := cb.Allow(); err == nil {
		t.Error("open breaker should reject")
	}
}

func TestCircuitBreaker_TransitionsToHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{
		ErrorThreshold: 0.5,
		WindowSize:     4,
		OpenDuration:   50 * time.Millisecond,
		HalfOpenMax:    2,
	})

	// Trip the breaker
	for i := 0; i < 4; i++ {
		cb.Allow()
		cb.RecordFailure()
	}

	if cb.State() != StateOpen {
		t.Fatalf("state = %v, want Open", cb.State())
	}

	// Wait for open duration to expire
	time.Sleep(60 * time.Millisecond)

	if cb.State() != StateHalfOpen {
		t.Fatalf("state = %v, want HalfOpen", cb.State())
	}

	if err := cb.Allow(); err != nil {
		t.Errorf("half-open should allow: %v", err)
	}
}

func TestCircuitBreaker_HalfOpenRecovery(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{
		ErrorThreshold: 0.5,
		WindowSize:     4,
		OpenDuration:   50 * time.Millisecond,
		HalfOpenMax:    2,
	})

	// Trip the breaker
	for i := 0; i < 4; i++ {
		cb.Allow()
		cb.RecordFailure()
	}

	time.Sleep(60 * time.Millisecond)

	// Succeed enough times in half-open to close
	for i := 0; i < 2; i++ {
		cb.Allow()
		cb.RecordSuccess()
	}

	if cb.State() != StateClosed {
		t.Errorf("state = %v, want Closed after recovery", cb.State())
	}
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{
		ErrorThreshold: 0.5,
		WindowSize:     4,
		OpenDuration:   50 * time.Millisecond,
		HalfOpenMax:    2,
	})

	// Trip the breaker
	for i := 0; i < 4; i++ {
		cb.Allow()
		cb.RecordFailure()
	}

	time.Sleep(60 * time.Millisecond)

	// One success, then failure in half-open → back to open
	cb.Allow()
	cb.RecordSuccess()
	cb.Allow()
	cb.RecordFailure()

	if cb.State() != StateOpen {
		t.Errorf("state = %v, want Open after half-open failure", cb.State())
	}
}

func TestCircuitBreaker_BelowThresholdStaysClosed(t *testing.T) {
	cb := NewCircuitBreaker(BreakerConfig{
		ErrorThreshold: 0.5,
		WindowSize:     10,
		OpenDuration:   time.Second,
	})

	// 4 failures, 6 successes = 40% error rate → below 50% threshold
	for i := 0; i < 4; i++ {
		cb.Allow()
		cb.RecordFailure()
	}
	for i := 0; i < 6; i++ {
		cb.Allow()
		cb.RecordSuccess()
	}

	if cb.State() != StateClosed {
		t.Errorf("state = %v, want Closed (below threshold)", cb.State())
	}
}
