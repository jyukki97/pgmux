package resilience

import (
	"fmt"
	"sync"
	"time"
)

// State represents the Circuit Breaker state.
type State int

const (
	StateClosed   State = iota // normal operation
	StateOpen                  // failing — reject all requests
	StateHalfOpen              // testing — allow limited requests
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// BreakerConfig configures the Circuit Breaker.
type BreakerConfig struct {
	ErrorThreshold float64       // error rate (0.0-1.0) to trip to Open
	OpenDuration   time.Duration // how long to stay Open before Half-Open
	HalfOpenMax    int           // max requests allowed in Half-Open state
	WindowSize     int           // rolling window size for counting errors
}

// CircuitBreaker implements the Circuit Breaker pattern.
type CircuitBreaker struct {
	cfg BreakerConfig
	mu  sync.Mutex

	state      State
	successes  int
	failures   int
	total      int
	openedAt   time.Time
	halfOpenOK int // successful requests in half-open state
}

// NewCircuitBreaker creates a new Circuit Breaker.
func NewCircuitBreaker(cfg BreakerConfig) *CircuitBreaker {
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = 10
	}
	if cfg.HalfOpenMax <= 0 {
		cfg.HalfOpenMax = 3
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 10 * time.Second
	}
	if cfg.ErrorThreshold <= 0 {
		cfg.ErrorThreshold = 0.5
	}
	return &CircuitBreaker{
		cfg:   cfg,
		state: StateClosed,
	}
}

// Allow checks if a request is allowed through the circuit breaker.
// Returns an error if the circuit is open.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(cb.openedAt) >= cb.cfg.OpenDuration {
			cb.state = StateHalfOpen
			cb.halfOpenOK = 0
			return nil
		}
		return fmt.Errorf("circuit breaker is open")
	case StateHalfOpen:
		return nil
	}
	return nil
}

// RecordSuccess records a successful request.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		cb.successes++
		cb.total++
		cb.evaluateWindow()
	case StateHalfOpen:
		cb.halfOpenOK++
		if cb.halfOpenOK >= cb.cfg.HalfOpenMax {
			// Enough successes — close the circuit
			cb.state = StateClosed
			cb.resetCounters()
		}
	}
}

// RecordFailure records a failed request.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		cb.failures++
		cb.total++
		cb.evaluateWindow()
	case StateHalfOpen:
		// Any failure in half-open → back to open
		cb.state = StateOpen
		cb.openedAt = time.Now()
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Check for automatic transition from Open to HalfOpen
	if cb.state == StateOpen && time.Since(cb.openedAt) >= cb.cfg.OpenDuration {
		cb.state = StateHalfOpen
		cb.halfOpenOK = 0
	}
	return cb.state
}

// evaluateWindow checks the error rate when the window is full.
// If above threshold → trip to Open. Otherwise → reset window for next cycle.
func (cb *CircuitBreaker) evaluateWindow() {
	if cb.total < cb.cfg.WindowSize {
		return
	}
	errorRate := float64(cb.failures) / float64(cb.total)
	if errorRate >= cb.cfg.ErrorThreshold {
		cb.state = StateOpen
		cb.openedAt = time.Now()
	}
	cb.resetCounters()
}

func (cb *CircuitBreaker) resetCounters() {
	cb.successes = 0
	cb.failures = 0
	cb.total = 0
}
