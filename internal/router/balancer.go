package router

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	Addr      string
	healthy   atomic.Bool
	replayLSN atomic.Uint64 // latest replay LSN for this reader
}

type RoundRobin struct {
	mu       sync.RWMutex
	backends []*Backend
	index    atomic.Uint64
}

func NewRoundRobin(addrs []string) *RoundRobin {
	backends := make([]*Backend, len(addrs))
	for i, addr := range addrs {
		b := &Backend{Addr: addr}
		b.healthy.Store(true)
		backends[i] = b
	}
	return &RoundRobin{backends: backends}
}

// Next returns the address of the next healthy backend.
// Returns empty string if no healthy backend is available.
func (r *RoundRobin) Next() string {
	r.mu.RLock()
	backends := r.backends
	r.mu.RUnlock()

	n := len(backends)
	if n == 0 {
		return ""
	}

	for i := 0; i < n; i++ {
		idx := int(r.index.Add(1)-1) % n
		if backends[idx].healthy.Load() {
			return backends[idx].Addr
		}
	}
	return ""
}

// MarkUnhealthy marks a backend as unhealthy.
func (r *RoundRobin) MarkUnhealthy(addr string) {
	r.mu.RLock()
	backends := r.backends
	r.mu.RUnlock()

	for _, b := range backends {
		if b.Addr == addr {
			b.healthy.Store(false)
			slog.Warn("backend marked unhealthy", "addr", addr)
			return
		}
	}
}

// StartHealthCheck periodically checks unhealthy backends and restores them.
func (r *RoundRobin) StartHealthCheck(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.checkBackends()
			}
		}
	}()
}

func (r *RoundRobin) checkBackends() {
	r.mu.RLock()
	backends := r.backends
	r.mu.RUnlock()

	for _, b := range backends {
		if !b.healthy.Load() {
			conn, err := net.DialTimeout("tcp", b.Addr, 2*time.Second)
			if err == nil {
				conn.Close()
				b.healthy.Store(true)
				slog.Info("backend recovered", "addr", b.Addr)
			}
		}
	}
}

// UpdateBackends replaces the backend list atomically, preserving
// healthy and replayLSN state for backends that already exist.
func (r *RoundRobin) UpdateBackends(addrs []string) {
	// Snapshot existing backends under read lock.
	r.mu.RLock()
	old := r.backends
	r.mu.RUnlock()

	// Build addr → *Backend map from the current list.
	existing := make(map[string]*Backend, len(old))
	for _, b := range old {
		existing[b.Addr] = b
	}

	// Build new slice, reusing existing backends to preserve runtime state.
	backends := make([]*Backend, len(addrs))
	for i, addr := range addrs {
		if b, ok := existing[addr]; ok {
			backends[i] = b
		} else {
			b := &Backend{Addr: addr}
			b.healthy.Store(true)
			backends[i] = b
		}
	}

	r.mu.Lock()
	r.backends = backends
	r.mu.Unlock()
	slog.Info("balancer backends updated", "count", len(addrs))
}

// NextWithLSN returns the next healthy backend whose replayLSN >= minLSN.
// Returns empty string if no backend meets the criteria.
func (r *RoundRobin) NextWithLSN(minLSN LSN) string {
	if minLSN.IsZero() {
		return r.Next()
	}

	r.mu.RLock()
	backends := r.backends
	r.mu.RUnlock()

	n := len(backends)
	if n == 0 {
		return ""
	}

	for i := 0; i < n; i++ {
		idx := int(r.index.Add(1)-1) % n
		b := backends[idx]
		if b.healthy.Load() && LSN(b.replayLSN.Load()) >= minLSN {
			return b.Addr
		}
	}
	return ""
}

// SetReplayLSN updates the replay LSN for a backend.
func (r *RoundRobin) SetReplayLSN(addr string, lsn LSN) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.backends {
		if b.Addr == addr {
			b.replayLSN.Store(uint64(lsn))
			return
		}
	}
}

// ReplayLSN returns the replay LSN for a backend.
func (r *RoundRobin) ReplayLSN(addr string) LSN {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.backends {
		if b.Addr == addr {
			return LSN(b.replayLSN.Load())
		}
	}
	return InvalidLSN
}

// Backends returns the list of backend addresses.
func (r *RoundRobin) Backends() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	addrs := make([]string, len(r.backends))
	for i, b := range r.backends {
		addrs[i] = b.Addr
	}
	return addrs
}

// HealthyCount returns the number of healthy backends.
func (r *RoundRobin) HealthyCount() int {
	r.mu.RLock()
	backends := r.backends
	r.mu.RUnlock()

	count := 0
	for _, b := range backends {
		if b.healthy.Load() {
			count++
		}
	}
	return count
}
