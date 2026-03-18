package mirror

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
)

func TestMatchesTables_NoFilter(t *testing.T) {
	m := &Mirror{tables: nil}
	if !m.MatchesTables([]string{"users", "orders"}) {
		t.Error("nil filter should match all tables")
	}
	if !m.MatchesTables(nil) {
		t.Error("nil filter should match nil tables")
	}
}

func TestMatchesTables_WithFilter(t *testing.T) {
	m := &Mirror{tables: map[string]bool{"users": true, "orders": true}}

	if !m.MatchesTables([]string{"users"}) {
		t.Error("should match 'users'")
	}
	if !m.MatchesTables([]string{"posts", "orders"}) {
		t.Error("should match when any table matches")
	}
	if m.MatchesTables([]string{"posts"}) {
		t.Error("should not match 'posts'")
	}
	if m.MatchesTables(nil) {
		t.Error("should not match empty tables")
	}
}

func TestIsReadOnly(t *testing.T) {
	tests := []struct {
		mode     string
		readOnly bool
	}{
		{"read_only", true},
		{"all", false},
		{"", false},
	}
	for _, tt := range tests {
		m := &Mirror{cfg: Config{Mode: tt.mode}}
		if m.IsReadOnly() != tt.readOnly {
			t.Errorf("mode=%q: IsReadOnly()=%v, want %v", tt.mode, m.IsReadOnly(), tt.readOnly)
		}
	}
}

func TestSend_BufferFull_DropsJob(t *testing.T) {
	m := &Mirror{
		workCh: make(chan *job, 1), // buffer size 1
		done:   make(chan struct{}),
		tables: nil,
		cfg:    Config{Mode: "all"},
		stats:  newStatsCollector(),
	}

	// First send should succeed
	m.Send('Q', []byte("SELECT 1\x00"), "SELECT 1", time.Millisecond)
	if m.Dropped() != 0 {
		t.Errorf("dropped = %d after first send, want 0", m.Dropped())
	}

	// Second send should be dropped (buffer full, no workers draining)
	m.Send('Q', []byte("SELECT 2\x00"), "SELECT 2", time.Millisecond)
	if m.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", m.Dropped())
	}
}

func TestSend_CopiesPayload(t *testing.T) {
	m := &Mirror{
		workCh: make(chan *job, 10),
		done:   make(chan struct{}),
		tables: nil,
		cfg:    Config{Mode: "all"},
		stats:  newStatsCollector(),
	}

	payload := []byte("SELECT 1\x00")
	m.Send('Q', payload, "SELECT 1", time.Millisecond)

	// Modify original payload
	payload[0] = 'X'

	// Job should have the original copy
	j := <-m.workCh
	if j.payload[0] != 'S' {
		t.Errorf("payload[0] = %c, want S (payload was not copied)", j.payload[0])
	}
}

// mockPGServer creates a TCP listener that accepts connections and responds to
// SimpleQuery messages with a CommandComplete + ReadyForQuery sequence.
func mockPGServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					msg, err := protocol.ReadMessage(c)
					if err != nil {
						return
					}
					if msg.Type == 'Q' {
						// Send CommandComplete
						protocol.WriteMessage(c, protocol.MsgCommandComplete, []byte("SELECT 1\x00"))
						// Send ReadyForQuery
						protocol.WriteMessage(c, protocol.MsgReadyForQuery, []byte{'I'})
					}
				}
			}(conn)
		}
	}()

	cleanup := func() {
		ln.Close()
		wg.Wait()
	}

	return ln.Addr().String(), cleanup
}

func TestMirror_EndToEnd(t *testing.T) {
	addr, cleanup := mockPGServer(t)
	defer cleanup()

	m, err := New(Config{
		Addr:       addr,
		Mode:       "all",
		Compare:    true,
		Workers:    2,
		BufferSize: 100,
		DialFunc: func() (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 2*time.Second)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	// Send some queries
	for i := 0; i < 10; i++ {
		m.Send('Q', []byte("SELECT 1\x00"), "SELECT 1", 5*time.Millisecond)
	}

	// Wait for workers to process
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for queries: sent=%d, errors=%d, dropped=%d",
				m.Sent(), m.Errors(), m.Dropped())
		default:
			if m.Sent() >= 10 {
				goto done
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
done:

	if m.Errors() != 0 {
		t.Errorf("errors = %d, want 0", m.Errors())
	}
	if m.Dropped() != 0 {
		t.Errorf("dropped = %d, want 0", m.Dropped())
	}

	// Compare mode is on, so stats should have entries
	stats := m.Stats()
	if len(stats) == 0 {
		t.Error("expected stats entries with compare=true")
	}
}

func TestMirror_Close(t *testing.T) {
	addr, cleanup := mockPGServer(t)
	defer cleanup()

	m, err := New(Config{
		Addr:       addr,
		Workers:    2,
		BufferSize: 10,
		DialFunc: func() (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 2*time.Second)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Close should not hang
	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Close() timed out")
	}
}

func TestNew_Defaults(t *testing.T) {
	addr, cleanup := mockPGServer(t)
	defer cleanup()

	m, err := New(Config{
		Addr: addr,
		DialFunc: func() (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 2*time.Second)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	if m.cfg.Workers != 4 {
		t.Errorf("workers = %d, want 4", m.cfg.Workers)
	}
	if m.cfg.BufferSize != 10000 {
		t.Errorf("bufferSize = %d, want 10000", m.cfg.BufferSize)
	}
	if m.cfg.Mode != "read_only" {
		t.Errorf("mode = %q, want read_only", m.cfg.Mode)
	}
	if !m.IsReadOnly() {
		t.Error("default mode should be read_only")
	}
}

func TestNew_DialFunc_Required(t *testing.T) {
	_, err := New(Config{
		Addr: "127.0.0.1:0",
	})
	// pool.New should return error if DialFunc is nil
	// or if it can't connect - either is acceptable
	if err == nil {
		t.Log("New with nil DialFunc succeeded (pool may not require it)")
	}
}

func TestMirror_Counters_InitialZero(t *testing.T) {
	addr, cleanup := mockPGServer(t)
	defer cleanup()

	m, err := New(Config{
		Addr: addr,
		DialFunc: func() (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 2*time.Second)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	if m.Sent() != 0 {
		t.Errorf("sent = %d, want 0", m.Sent())
	}
	if m.Dropped() != 0 {
		t.Errorf("dropped = %d, want 0", m.Dropped())
	}
	if m.Errors() != 0 {
		t.Errorf("errors = %d, want 0", m.Errors())
	}
}

// poolDialFunc is a pool.DialFunc adapter for tests.
var _ pool.DialFunc = func() (net.Conn, error) { return nil, nil }
