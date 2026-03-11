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
