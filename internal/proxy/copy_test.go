package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/protocol"
)

func newTestServer() *Server {
	cfg := &config.Config{
		Cache: config.CacheConfig{MaxResultSize: "1MB"},
	}
	return NewServer(cfg)
}

// TestRelayCopyIn tests that relayCopyIn correctly relays CopyData and CopyDone
// from client to backend.
func TestRelayCopyIn(t *testing.T) {
	srv := newTestServer()

	proxyClientConn, testClientConn := net.Pipe()
	proxyBackendConn, testBackendConn := net.Pipe()
	defer proxyClientConn.Close()
	defer testClientConn.Close()
	defer proxyBackendConn.Close()
	defer testBackendConn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.relayCopyIn(proxyClientConn, proxyBackendConn)
	}()

	// Client sends CopyData then CopyDone (in goroutine because net.Pipe is synchronous)
	go func() {
		protocol.WriteMessage(testClientConn, protocol.MsgCopyData, []byte("row1\n"))
		protocol.WriteMessage(testClientConn, protocol.MsgCopyData, []byte("row2\n"))
		protocol.WriteMessage(testClientConn, protocol.MsgCopyDone, nil)
	}()

	// Read what the backend receives
	msg1, err := protocol.ReadMessage(testBackendConn)
	if err != nil {
		t.Fatalf("read msg1: %v", err)
	}
	if msg1.Type != protocol.MsgCopyData {
		t.Errorf("msg1.Type = %c, want %c", msg1.Type, protocol.MsgCopyData)
	}

	msg2, err := protocol.ReadMessage(testBackendConn)
	if err != nil {
		t.Fatalf("read msg2: %v", err)
	}
	if msg2.Type != protocol.MsgCopyData {
		t.Errorf("msg2.Type = %c, want %c", msg2.Type, protocol.MsgCopyData)
	}

	msg3, err := protocol.ReadMessage(testBackendConn)
	if err != nil {
		t.Fatalf("read msg3: %v", err)
	}
	if msg3.Type != protocol.MsgCopyDone {
		t.Errorf("msg3.Type = %c, want %c", msg3.Type, protocol.MsgCopyDone)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("relayCopyIn error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayCopyIn timed out")
	}
}

// TestRelayCopyIn_CopyFail tests that CopyFail from client also terminates COPY IN.
func TestRelayCopyIn_CopyFail(t *testing.T) {
	srv := newTestServer()

	proxyClientConn, testClientConn := net.Pipe()
	proxyBackendConn, testBackendConn := net.Pipe()
	defer proxyClientConn.Close()
	defer testClientConn.Close()
	defer proxyBackendConn.Close()
	defer testBackendConn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.relayCopyIn(proxyClientConn, proxyBackendConn)
	}()

	go func() {
		protocol.WriteMessage(testClientConn, protocol.MsgCopyFail, []byte("aborted\x00"))
	}()

	msg, err := protocol.ReadMessage(testBackendConn)
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if msg.Type != protocol.MsgCopyFail {
		t.Errorf("msg.Type = %c, want %c", msg.Type, protocol.MsgCopyFail)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("relayCopyIn error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayCopyIn timed out")
	}
}

// TestRelayCopyOut tests that relayCopyOut correctly relays CopyData and CopyDone
// from backend to client.
func TestRelayCopyOut(t *testing.T) {
	srv := newTestServer()

	proxyClientConn, testClientConn := net.Pipe()
	proxyBackendConn, testBackendConn := net.Pipe()
	defer proxyClientConn.Close()
	defer testClientConn.Close()
	defer proxyBackendConn.Close()
	defer testBackendConn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.relayCopyOut(proxyClientConn, proxyBackendConn)
	}()

	go func() {
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyData, []byte("col1\tcol2\n"))
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyData, []byte("val1\tval2\n"))
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyDone, nil)
	}()

	msg1, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read msg1: %v", err)
	}
	if msg1.Type != protocol.MsgCopyData {
		t.Errorf("msg1.Type = %c, want %c", msg1.Type, protocol.MsgCopyData)
	}

	msg2, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read msg2: %v", err)
	}
	if msg2.Type != protocol.MsgCopyData {
		t.Errorf("msg2.Type = %c, want %c", msg2.Type, protocol.MsgCopyData)
	}

	msg3, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read msg3: %v", err)
	}
	if msg3.Type != protocol.MsgCopyDone {
		t.Errorf("msg3.Type = %c, want %c", msg3.Type, protocol.MsgCopyDone)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("relayCopyOut error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayCopyOut timed out")
	}
}

// TestRelayUntilReady_CopyIn tests the full flow: backend sends CopyInResponse,
// proxy relays COPY data from client, then receives CommandComplete + ReadyForQuery.
func TestRelayUntilReady_CopyIn(t *testing.T) {
	srv := newTestServer()

	proxyClientConn, testClientConn := net.Pipe()
	proxyBackendConn, testBackendConn := net.Pipe()
	defer proxyClientConn.Close()
	defer testClientConn.Close()
	defer proxyBackendConn.Close()
	defer testBackendConn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.relayUntilReady(proxyClientConn, proxyBackendConn)
	}()

	// Simulate the backend: send CopyInResponse, receive COPY data, send completion
	go func() {
		// Send CopyInResponse
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyInResponse, []byte{0, 0, 1, 0, 0})

		// Receive CopyData + CopyDone from proxy
		for {
			msg, err := protocol.ReadMessage(testBackendConn)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgCopyDone || msg.Type == protocol.MsgCopyFail {
				break
			}
		}

		// Send CommandComplete + ReadyForQuery
		protocol.WriteMessage(testBackendConn, protocol.MsgCommandComplete, append([]byte("COPY 1"), 0))
		protocol.WriteMessage(testBackendConn, protocol.MsgReadyForQuery, []byte{'I'})
	}()

	// Simulate the client: receive CopyInResponse, send COPY data, receive completion
	msg, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read CopyInResponse: %v", err)
	}
	if msg.Type != protocol.MsgCopyInResponse {
		t.Fatalf("expected CopyInResponse (G), got %c", msg.Type)
	}

	// Send COPY data
	if err := protocol.WriteMessage(testClientConn, protocol.MsgCopyData, []byte("data\n")); err != nil {
		t.Fatalf("write CopyData: %v", err)
	}
	if err := protocol.WriteMessage(testClientConn, protocol.MsgCopyDone, nil); err != nil {
		t.Fatalf("write CopyDone: %v", err)
	}

	// Receive CommandComplete + ReadyForQuery
	cc, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read CommandComplete: %v", err)
	}
	if cc.Type != protocol.MsgCommandComplete {
		t.Errorf("expected CommandComplete (C), got %c", cc.Type)
	}

	rq, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read ReadyForQuery: %v", err)
	}
	if rq.Type != protocol.MsgReadyForQuery {
		t.Errorf("expected ReadyForQuery (Z), got %c", rq.Type)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("relayUntilReady error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayUntilReady deadlocked — COPY protocol not handled")
	}
}

// TestRelayAndCollect_CopyIn tests that relayAndCollect handles CopyInResponse
// without deadlocking, and returns nil cache buffer (COPY is not cacheable).
func TestRelayAndCollect_CopyIn(t *testing.T) {
	srv := newTestServer()

	proxyClientConn, testClientConn := net.Pipe()
	proxyBackendConn, testBackendConn := net.Pipe()
	defer proxyClientConn.Close()
	defer testClientConn.Close()
	defer proxyBackendConn.Close()
	defer testBackendConn.Close()

	type result struct {
		buf []byte
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		buf, err := srv.relayAndCollect(proxyClientConn, proxyBackendConn)
		resCh <- result{buf, err}
	}()

	// Simulate backend
	go func() {
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyInResponse, []byte{0, 0, 1, 0, 0})

		for {
			msg, err := protocol.ReadMessage(testBackendConn)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgCopyDone || msg.Type == protocol.MsgCopyFail {
				break
			}
		}

		protocol.WriteMessage(testBackendConn, protocol.MsgCommandComplete, append([]byte("COPY 1"), 0))
		protocol.WriteMessage(testBackendConn, protocol.MsgReadyForQuery, []byte{'I'})
	}()

	// Simulate client
	msg, err := protocol.ReadMessage(testClientConn)
	if err != nil {
		t.Fatalf("read CopyInResponse: %v", err)
	}
	if msg.Type != protocol.MsgCopyInResponse {
		t.Fatalf("expected CopyInResponse, got %c", msg.Type)
	}

	protocol.WriteMessage(testClientConn, protocol.MsgCopyData, []byte("row\n"))
	protocol.WriteMessage(testClientConn, protocol.MsgCopyDone, nil)

	cc, _ := protocol.ReadMessage(testClientConn)
	if cc.Type != protocol.MsgCommandComplete {
		t.Errorf("expected CommandComplete, got %c", cc.Type)
	}
	rq, _ := protocol.ReadMessage(testClientConn)
	if rq.Type != protocol.MsgReadyForQuery {
		t.Errorf("expected ReadyForQuery, got %c", rq.Type)
	}

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Errorf("relayAndCollect error: %v", res.err)
		}
		if res.buf != nil {
			t.Errorf("expected nil cache buffer for COPY, got %d bytes", len(res.buf))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayAndCollect deadlocked — COPY protocol not handled")
	}
}

// TestRelayAndCollect_CopyOut tests that relayAndCollect handles CopyOutResponse.
func TestRelayAndCollect_CopyOut(t *testing.T) {
	srv := newTestServer()

	proxyClientConn, testClientConn := net.Pipe()
	proxyBackendConn, testBackendConn := net.Pipe()
	defer proxyClientConn.Close()
	defer testClientConn.Close()
	defer proxyBackendConn.Close()
	defer testBackendConn.Close()

	type result struct {
		buf []byte
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		buf, err := srv.relayAndCollect(proxyClientConn, proxyBackendConn)
		resCh <- result{buf, err}
	}()

	// Backend sends CopyOutResponse + CopyData + CopyDone + CommandComplete + ReadyForQuery
	go func() {
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyOutResponse, []byte{0, 0, 1, 0, 0})
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyData, []byte("exported\n"))
		protocol.WriteMessage(testBackendConn, protocol.MsgCopyDone, nil)
		protocol.WriteMessage(testBackendConn, protocol.MsgCommandComplete, append([]byte("COPY 1"), 0))
		protocol.WriteMessage(testBackendConn, protocol.MsgReadyForQuery, []byte{'I'})
	}()

	// Client reads all forwarded messages
	expected := []byte{protocol.MsgCopyOutResponse, protocol.MsgCopyData, protocol.MsgCopyDone, protocol.MsgCommandComplete, protocol.MsgReadyForQuery}
	for i, exp := range expected {
		msg, err := protocol.ReadMessage(testClientConn)
		if err != nil {
			t.Fatalf("read msg %d: %v", i, err)
		}
		if msg.Type != exp {
			t.Errorf("msg[%d].Type = %c, want %c", i, msg.Type, exp)
		}
	}

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Errorf("relayAndCollect error: %v", res.err)
		}
		if res.buf != nil {
			t.Errorf("expected nil cache buffer for COPY, got %d bytes", len(res.buf))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayAndCollect deadlocked")
	}
}
