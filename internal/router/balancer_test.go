package router

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRoundRobin_Distribution(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2", "c:3"})

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		addr := rb.Next()
		counts[addr]++
	}

	for _, addr := range []string{"a:1", "b:2", "c:3"} {
		if counts[addr] != 3 {
			t.Errorf("count[%s] = %d, want 3", addr, counts[addr])
		}
	}
}

func TestRoundRobin_SkipUnhealthy(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2", "c:3"})

	rb.MarkUnhealthy("b:2")

	for i := 0; i < 6; i++ {
		addr := rb.Next()
		if addr == "b:2" {
			t.Error("unhealthy backend b:2 should be skipped")
		}
	}
}

func TestRoundRobin_AllUnhealthy(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2"})

	rb.MarkUnhealthy("a:1")
	rb.MarkUnhealthy("b:2")

	if addr := rb.Next(); addr != "" {
		t.Errorf("Next() = %q, want empty when all unhealthy", addr)
	}
}

func TestRoundRobin_HealthyCount(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2", "c:3"})

	if got := rb.HealthyCount(); got != 3 {
		t.Errorf("HealthyCount() = %d, want 3", got)
	}

	rb.MarkUnhealthy("b:2")

	if got := rb.HealthyCount(); got != 2 {
		t.Errorf("HealthyCount() = %d, want 2", got)
	}
}

func TestRoundRobin_UpdateBackends(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2"})

	if got := rb.HealthyCount(); got != 2 {
		t.Fatalf("initial HealthyCount() = %d, want 2", got)
	}

	// Update to new set of backends
	rb.UpdateBackends([]string{"c:3", "d:4", "e:5"})

	if got := rb.HealthyCount(); got != 3 {
		t.Errorf("after update HealthyCount() = %d, want 3", got)
	}

	// Verify old backends are gone and new ones are reachable
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		addr := rb.Next()
		counts[addr]++
	}

	for _, addr := range []string{"c:3", "d:4", "e:5"} {
		if counts[addr] != 2 {
			t.Errorf("count[%s] = %d, want 2", addr, counts[addr])
		}
	}
	for _, addr := range []string{"a:1", "b:2"} {
		if counts[addr] != 0 {
			t.Errorf("old backend %s should not appear, got count %d", addr, counts[addr])
		}
	}
}

func TestRoundRobin_NextWithLSN(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2", "c:3"})

	// Set replay LSNs: a=100, b=200, c=150
	rb.SetReplayLSN("a:1", LSN(100))
	rb.SetReplayLSN("b:2", LSN(200))
	rb.SetReplayLSN("c:3", LSN(150))

	// minLSN=0 → same as Next(), returns any healthy
	addr := rb.NextWithLSN(InvalidLSN)
	if addr == "" {
		t.Error("NextWithLSN(0) should return a backend")
	}

	// minLSN=180 → only b:2 qualifies (200 >= 180)
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		addr := rb.NextWithLSN(LSN(180))
		counts[addr]++
	}
	if counts["b:2"] != 6 {
		t.Errorf("expected only b:2 for minLSN=180, got %v", counts)
	}

	// minLSN=250 → none qualify
	if addr := rb.NextWithLSN(LSN(250)); addr != "" {
		t.Errorf("NextWithLSN(250) = %q, want empty", addr)
	}
}

func TestRoundRobin_NextWithLSN_SkipsUnhealthy(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2"})
	rb.SetReplayLSN("a:1", LSN(200))
	rb.SetReplayLSN("b:2", LSN(200))

	rb.MarkUnhealthy("a:1")

	// Only b:2 is healthy and has sufficient LSN
	for i := 0; i < 4; i++ {
		addr := rb.NextWithLSN(LSN(100))
		if addr != "b:2" {
			t.Errorf("expected b:2, got %q", addr)
		}
	}
}

func TestRoundRobin_SetReplayLSN(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2"})

	rb.SetReplayLSN("a:1", LSN(12345))
	if got := rb.ReplayLSN("a:1"); got != LSN(12345) {
		t.Errorf("ReplayLSN(a:1) = %d, want 12345", got)
	}

	// Unknown addr returns InvalidLSN
	if got := rb.ReplayLSN("unknown:1"); got != InvalidLSN {
		t.Errorf("ReplayLSN(unknown) = %d, want 0", got)
	}
}

func TestRoundRobin_Backends(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2", "c:3"})
	addrs := rb.Backends()
	if len(addrs) != 3 {
		t.Errorf("Backends() len = %d, want 3", len(addrs))
	}
}

func TestRoundRobin_ConcurrentUpdateAndRead(t *testing.T) {
	rb := NewRoundRobin([]string{"a:1", "b:2", "c:3"})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Concurrent UpdateBackends
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			rb.UpdateBackends([]string{"x:1", "y:2"})
			rb.UpdateBackends([]string{"a:1", "b:2", "c:3"})
		}
	}()

	// Concurrent reads: Next, MarkUnhealthy, HealthyCount, checkBackends
	go func() {
		for ctx.Err() == nil {
			rb.Next()
		}
	}()
	go func() {
		for ctx.Err() == nil {
			rb.MarkUnhealthy("a:1")
		}
	}()
	go func() {
		for ctx.Err() == nil {
			rb.HealthyCount()
		}
	}()
	go func() {
		for ctx.Err() == nil {
			rb.checkBackends()
		}
	}()

	<-ctx.Done()
}

func TestRoundRobin_Recovery(t *testing.T) {
	// Start a real TCP server to simulate recovery
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	addr := ln.Addr().String()
	rb := NewRoundRobin([]string{addr})
	rb.MarkUnhealthy(addr)

	if rb.HealthyCount() != 0 {
		t.Error("expected 0 healthy after marking unhealthy")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rb.StartHealthCheck(ctx, 50*time.Millisecond)

	time.Sleep(150 * time.Millisecond)

	if rb.HealthyCount() != 1 {
		t.Error("expected backend to recover after health check")
	}
}
