package dataapi

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
)

// slowConn simulates a PostgreSQL connection that delays before responding.
// It uses an internal pipe so that protocol.ReadMessage can properly parse
// the framed PG messages byte by byte.
type slowConn struct {
	delay time.Duration

	mu        sync.Mutex
	deadline  time.Time
	deadlineC chan struct{}

	closed     atomic.Bool
	closeCount atomic.Int32

	// pipe for feeding responses to the reader
	pr *io.PipeReader
	pw *io.PipeWriter
}

func newSlowConn(delay time.Duration) *slowConn {
	pr, pw := io.Pipe()
	return &slowConn{
		delay:     delay,
		deadlineC: make(chan struct{}, 1),
		pr:        pr,
		pw:        pw,
	}
}

// startResponse starts a goroutine that writes a ReadyForQuery message
// to the pipe after the configured delay, or aborts if a deadline is set.
func (c *slowConn) startResponse() {
	go func() {
		timer := time.NewTimer(c.delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			// Write ReadyForQuery: type 'Z' + length(5) + status 'I'
			msg := make([]byte, 6)
			msg[0] = protocol.MsgReadyForQuery
			binary.BigEndian.PutUint32(msg[1:5], 5)
			msg[5] = 'I'
			c.pw.Write(msg)
		case <-c.deadlineC:
			// Deadline was set — close the pipe to unblock reads with an error
			c.pw.CloseWithError(&net.OpError{Op: "read", Err: &timeoutError{}})
		}
	}()
}

func (c *slowConn) Read(p []byte) (int, error) {
	return c.pr.Read(p)
}

func (c *slowConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	// After a write (query sent), start the delayed response
	c.startResponse()
	return len(p), nil
}

func (c *slowConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	// Signal that a deadline was set
	select {
	case c.deadlineC <- struct{}{}:
	default:
	}
	return nil
}

func (c *slowConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *slowConn) SetWriteDeadline(t time.Time) error { return nil }

func (c *slowConn) Close() error {
	c.closed.Store(true)
	c.closeCount.Add(1)
	c.pr.Close()
	c.pw.Close()
	return nil
}

func (c *slowConn) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (c *slowConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

// TestExecuteOnPool_ContextCancel verifies that when the HTTP context is
// cancelled during a slow query, the connection is unblocked and discarded
// (not returned to the pool), preventing zombie goroutines.
func TestExecuteOnPool_ContextCancel(t *testing.T) {
	sc := newSlowConn(10 * time.Second) // query would take 10s without cancellation
	now := time.Now()
	pconn := &pool.Conn{
		Conn:       sc,
		CreatedAt:  now,
		LastUsedAt: now,
	}

	p, err := pool.New(pool.Config{
		DialFunc: func() (net.Conn, error) {
			return newSlowConn(10 * time.Second), nil
		},
		Addr:              "127.0.0.1:5432",
		MinConnections:    0,
		MaxConnections:    1,
		IdleTimeout:       time.Minute,
		MaxLifetime:       time.Minute,
		ConnectionTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	defer p.Close()

	// Seed the pool with our slow connection
	p.Release(pconn)

	cfg := &config.Config{
		Pool: config.PoolConfig{ResetQuery: "DISCARD ALL"},
	}
	srv := New(func() *config.Config { return cfg }, func() *pool.Pool { return p }, nil, nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := srv.executeOnPool(ctx, "SELECT pg_sleep(100)", p)
		done <- err
	}()

	// Give the goroutine time to start the query and block on Read
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after context cancellation, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executeOnPool did not return within 3s after context cancellation — zombie goroutine leak")
	}

	// The connection should have been discarded (closed), not released back
	if !sc.closed.Load() {
		t.Error("expected connection to be closed (discarded), but it was not")
	}
}

// TestExecuteOnPool_NormalCompletion verifies that normal query execution still
// works correctly and the watchdog goroutine does not leak.
func TestExecuteOnPool_NormalCompletion(t *testing.T) {
	// Use a very fast delay so the query completes normally
	sc := newSlowConn(10 * time.Millisecond)
	now := time.Now()
	pconn := &pool.Conn{
		Conn:       sc,
		CreatedAt:  now,
		LastUsedAt: now,
	}

	p, err := pool.New(pool.Config{
		DialFunc: func() (net.Conn, error) {
			return newSlowConn(10 * time.Millisecond), nil
		},
		Addr:              "127.0.0.1:5432",
		MinConnections:    0,
		MaxConnections:    1,
		IdleTimeout:       time.Minute,
		MaxLifetime:       time.Minute,
		ConnectionTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	defer p.Close()

	p.Release(pconn)

	cfg := &config.Config{
		Pool: config.PoolConfig{ResetQuery: "DISCARD ALL"},
	}
	srv := New(func() *config.Config { return cfg }, func() *pool.Pool { return p }, nil, nil, nil, nil, nil)

	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := srv.executeOnPool(ctx, "SELECT 1", p)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error on normal completion, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executeOnPool did not return within 3s on normal path")
	}
}

// TestExecuteOnPool_DeadlineExceeded verifies that context.DeadlineExceeded
// is properly propagated.
func TestExecuteOnPool_DeadlineExceeded(t *testing.T) {
	sc := newSlowConn(10 * time.Second)
	now := time.Now()
	pconn := &pool.Conn{
		Conn:       sc,
		CreatedAt:  now,
		LastUsedAt: now,
	}

	p, err := pool.New(pool.Config{
		DialFunc: func() (net.Conn, error) {
			return newSlowConn(10 * time.Second), nil
		},
		Addr:              "127.0.0.1:5432",
		MinConnections:    0,
		MaxConnections:    1,
		IdleTimeout:       time.Minute,
		MaxLifetime:       time.Minute,
		ConnectionTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	defer p.Close()

	p.Release(pconn)

	cfg := &config.Config{
		Pool: config.PoolConfig{ResetQuery: "DISCARD ALL"},
	}
	srv := New(func() *config.Config { return cfg }, func() *pool.Pool { return p }, nil, nil, nil, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := srv.executeOnPool(ctx, "SELECT pg_sleep(100)", p)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after deadline exceeded, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected context.DeadlineExceeded, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executeOnPool did not return within 3s after deadline — zombie goroutine leak")
	}

	if !sc.closed.Load() {
		t.Error("expected connection to be closed (discarded), but it was not")
	}
}
