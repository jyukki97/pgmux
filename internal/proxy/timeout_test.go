package proxy

import (
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
)

func TestResolveQueryTimeout(t *testing.T) {
	s := &Server{}

	cfg := &config.Config{Pool: config.PoolConfig{QueryTimeout: 30 * time.Second}}
	s.cfgPtr.Store(cfg)

	// Global config timeout
	got := s.resolveQueryTimeout("SELECT 1", cfg)
	if got != 30*time.Second {
		t.Errorf("expected 30s, got %v", got)
	}

	// Per-query hint overrides global
	got = s.resolveQueryTimeout("/* timeout:5s */ SELECT 1", cfg)
	if got != 5*time.Second {
		t.Errorf("expected 5s, got %v", got)
	}

	// No timeout configured
	cfgNoTimeout := &config.Config{Pool: config.PoolConfig{QueryTimeout: 0}}
	got = s.resolveQueryTimeout("SELECT 1", cfgNoTimeout)
	if got != 0 {
		t.Errorf("expected 0, got %v", got)
	}

	// Hint still works when global is 0
	got = s.resolveQueryTimeout("/* timeout:2s */ SELECT 1", cfgNoTimeout)
	if got != 2*time.Second {
		t.Errorf("expected 2s, got %v", got)
	}
}

func TestStartQueryTimer_Disabled(t *testing.T) {
	s := &Server{}
	ct := &cancelTarget{proxyPID: 1, proxySecret: 100}

	// Zero timeout returns nil (disabled)
	stop := s.startQueryTimer(0, ct, "reader")
	if stop != nil {
		t.Error("expected nil stop func for zero timeout")
	}
}

func TestStartQueryTimer_FiresCancel(t *testing.T) {
	// Start a mock "backend" that accepts cancel requests
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var received atomic.Bool
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var buf [16]byte
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err = conn.Read(buf[:])
		if err != nil {
			return
		}
		code := binary.BigEndian.Uint32(buf[4:8])
		if code == protocol.CancelRequestCode {
			received.Store(true)
		}
	}()

	s := &Server{}
	ct := &cancelTarget{proxyPID: 1, proxySecret: 100}
	// Simulate an active backend query
	ct.setFromConn(ln.Addr().String(), &pool.Conn{
		BackendPID:    42,
		BackendSecret: 99,
	})

	// Start a very short timer
	stop := s.startQueryTimer(50*time.Millisecond, ct, "writer")
	if stop == nil {
		t.Fatal("expected non-nil stop func")
	}

	// Wait for timer to fire
	time.Sleep(200 * time.Millisecond)

	if !received.Load() {
		t.Error("backend did not receive cancel request from timeout")
	}
}

func TestStartQueryTimer_StoppedBeforeFiring(t *testing.T) {
	// Start a mock backend
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var received atomic.Bool
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var buf [16]byte
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, err = conn.Read(buf[:])
		if err != nil {
			return // expected — timeout or no data
		}
		received.Store(true)
	}()

	s := &Server{}
	ct := &cancelTarget{proxyPID: 1, proxySecret: 100}
	ct.setFromConn(ln.Addr().String(), &pool.Conn{
		BackendPID:    42,
		BackendSecret: 99,
	})

	// Start a timer and stop it before it fires
	stop := s.startQueryTimer(200*time.Millisecond, ct, "reader")
	if stop == nil {
		t.Fatal("expected non-nil stop func")
	}

	// Stop immediately — query completed in time
	stop()

	// Wait to make sure timer doesn't fire
	time.Sleep(400 * time.Millisecond)

	if received.Load() {
		t.Error("cancel request should not be sent when timer is stopped")
	}
}
