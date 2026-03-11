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
	Addr    string
	healthy atomic.Bool
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
	for _, b := range r.backends {
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
	for _, b := range r.backends {
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

// UpdateBackends replaces the backend list atomically.
func (r *RoundRobin) UpdateBackends(addrs []string) {
	backends := make([]*Backend, len(addrs))
	for i, addr := range addrs {
		b := &Backend{Addr: addr}
		b.healthy.Store(true)
		backends[i] = b
	}
	r.mu.Lock()
	r.backends = backends
	r.mu.Unlock()
	slog.Info("balancer backends updated", "count", len(addrs))
}

// HealthyCount returns the number of healthy backends.
func (r *RoundRobin) HealthyCount() int {
	count := 0
	for _, b := range r.backends {
		if b.healthy.Load() {
			count++
		}
	}
	return count
}
