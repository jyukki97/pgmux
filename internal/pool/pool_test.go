package pool

import (
	"context"
	"net"
	"testing"
	"time"
)

// startEchoServer starts a TCP server that accepts connections and holds them open.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1024)
				for {
					_, err := conn.Read(buf)
					if err != nil {
						conn.Close()
						return
					}
				}
			}()
		}
	}()

	return ln.Addr().String()
}

func TestPool_NewCreatesMinConnections(t *testing.T) {
	addr := startEchoServer(t)

	p, err := New(Config{
		Addr:           addr,
		MinConnections: 3,
		MaxConnections: 10,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	numOpen, numIdle := p.Stats()
	if numOpen != 3 {
		t.Errorf("numOpen = %d, want 3", numOpen)
	}
	if numIdle != 3 {
		t.Errorf("numIdle = %d, want 3", numIdle)
	}
}

func TestPool_AcquireRelease(t *testing.T) {
	addr := startEchoServer(t)

	p, err := New(Config{
		Addr:           addr,
		MinConnections: 1,
		MaxConnections: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()

	conn1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}

	numOpen, numIdle := p.Stats()
	if numIdle != 0 {
		t.Errorf("after acquire: numIdle = %d, want 0", numIdle)
	}

	p.Release(conn1)

	numOpen, numIdle = p.Stats()
	if numIdle != 1 {
		t.Errorf("after release: numIdle = %d, want 1", numIdle)
	}

	// Re-acquire should reuse the connection
	conn2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}

	if conn2 != conn1 {
		t.Error("expected connection reuse")
	}
	_ = numOpen

	p.Release(conn2)
}

func TestPool_AcquireTimeout(t *testing.T) {
	addr := startEchoServer(t)

	p, err := New(Config{
		Addr:              addr,
		MinConnections:    0,
		MaxConnections:    1,
		ConnectionTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()

	// Acquire the only connection
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Second acquire should timeout
	_, err = p.Acquire(ctx)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}

	p.Release(conn)
}

func TestPool_IdleTimeout(t *testing.T) {
	addr := startEchoServer(t)

	p, err := New(Config{
		Addr:           addr,
		MinConnections: 0,
		MaxConnections: 5,
		IdleTimeout:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()

	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(conn)

	// Wait for idle timeout
	time.Sleep(100 * time.Millisecond)

	// Should get a new connection (old one expired)
	conn2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(conn2)

	if conn2 == conn {
		t.Error("expected new connection after idle timeout, got same connection")
	}
}

func TestPool_MaxLifetime(t *testing.T) {
	addr := startEchoServer(t)

	p, err := New(Config{
		Addr:           addr,
		MinConnections: 0,
		MaxConnections: 5,
		MaxLifetime:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()

	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(conn)

	time.Sleep(100 * time.Millisecond)

	conn2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(conn2)

	if conn2 == conn {
		t.Error("expected new connection after max lifetime, got same connection")
	}
}

func TestPool_HealthCheck(t *testing.T) {
	addr := startEchoServer(t)

	p, err := New(Config{
		Addr:           addr,
		MinConnections: 2,
		MaxConnections: 5,
		IdleTimeout:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.StartHealthCheck(ctx, 30*time.Millisecond)

	// Wait for idle connections to expire and be replenished
	time.Sleep(150 * time.Millisecond)

	numOpen, _ := p.Stats()
	if numOpen < 2 {
		t.Errorf("after healthcheck: numOpen = %d, want >= 2 (min_connections)", numOpen)
	}
}
