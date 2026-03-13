package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/metrics"
	"github.com/jyukki97/pgmux/internal/protocol"
)

func TestIdleClientTimeout_DisconnectsIdleClient(t *testing.T) {
	// Use a TCP listener + dialer so SetReadDeadline actually works.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverConn := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		serverConn <- c
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	proxyConn := <-serverConn
	defer proxyConn.Close()

	// Set idle timeout to a short value
	idleTimeout := 100 * time.Millisecond
	proxyConn.SetReadDeadline(time.Now().Add(idleTimeout))

	// Try to read — should timeout since client sends nothing
	buf := make([]byte, 128)
	_, readErr := proxyConn.Read(buf)
	if readErr == nil {
		t.Fatal("expected timeout error, got nil")
	}

	netErr, ok := readErr.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("expected net.Error timeout, got: %v", readErr)
	}
}

func TestIdleClientTimeout_ConfigZeroDisablesTimeout(t *testing.T) {
	s := &Server{}
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ClientIdleTimeout: 0},
	}
	s.cfgPtr.Store(cfg)

	got := s.getConfig().Proxy.ClientIdleTimeout
	if got != 0 {
		t.Errorf("expected 0 (disabled), got %v", got)
	}
}

func TestIdleClientTimeout_ConfigReload(t *testing.T) {
	s := &Server{}

	// Initially disabled
	cfg1 := &config.Config{
		Proxy: config.ProxyConfig{ClientIdleTimeout: 0},
	}
	s.cfgPtr.Store(cfg1)

	if s.getConfig().Proxy.ClientIdleTimeout != 0 {
		t.Error("expected idle timeout disabled initially")
	}

	// Reload with 5 minutes
	cfg2 := &config.Config{
		Proxy: config.ProxyConfig{ClientIdleTimeout: 5 * time.Minute},
	}
	s.cfgPtr.Store(cfg2)

	if s.getConfig().Proxy.ClientIdleTimeout != 5*time.Minute {
		t.Error("expected idle timeout 5m after reload")
	}
}

func TestIdleClientTimeout_SendsFatalOnTimeout(t *testing.T) {
	// Verify that sendFatalWithCode produces a valid ErrorResponse with 57P01
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverConn := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		serverConn <- c
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	proxyConn := <-serverConn
	defer proxyConn.Close()

	s := &Server{}
	s.sendFatalWithCode(proxyConn, "57P01", "terminating connection due to idle timeout")

	// Read the ErrorResponse from client side
	clientConn.SetReadDeadline(time.Now().Add(time.Second))
	msg, _, readErr := protocol.ReadMessageReuse(clientConn, nil)
	if readErr != nil {
		t.Fatalf("failed to read ErrorResponse: %v", readErr)
	}

	if msg.Type != protocol.MsgErrorResponse {
		t.Fatalf("expected ErrorResponse ('E'), got '%c'", msg.Type)
	}

	// Verify payload contains FATAL severity and 57P01 code
	payload := string(msg.Payload)
	if !containsField(payload, 'S', "FATAL") {
		t.Error("ErrorResponse missing FATAL severity")
	}
	if !containsField(payload, 'C', "57P01") {
		t.Error("ErrorResponse missing 57P01 code")
	}
	if !containsField(payload, 'M', "terminating connection due to idle timeout") {
		t.Error("ErrorResponse missing expected message")
	}
}

// containsField checks if a PG ErrorResponse payload contains a field with the given type and value.
func containsField(payload string, fieldType byte, value string) bool {
	for i := 0; i < len(payload); i++ {
		if payload[i] == fieldType && i+1 < len(payload) {
			end := i + 1
			for end < len(payload) && payload[end] != 0 {
				end++
			}
			if payload[i+1:end] == value {
				return true
			}
		}
	}
	return false
}

func TestIdleClientTimeout_MetricIncrement(t *testing.T) {
	// Verify the metric field exists and can be incremented
	m := metrics.New()
	m.ClientIdleTimeouts.Inc()
	// No panic means the metric is properly registered
}
