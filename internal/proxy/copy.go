package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/jyukki97/pgmux/internal/protocol"
)

// wireBufPool recycles wire buffers used by relayUntilReady to avoid
// per-call allocation (pprof: 11% of allocs were wire buffer growth).
var wireBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 1024)
		return &b
	},
}

// forwardAndRelay forwards a message to backend and relays the response to client.
func (s *Server) forwardAndRelay(clientConn, backendConn net.Conn, msg *protocol.Message) error {
	if err := protocol.ForwardRaw(backendConn, msg); err != nil {
		return fmt.Errorf("forward message: %w", err)
	}
	return s.relayUntilReady(clientConn, backendConn)
}

// relayUntilReady forwards backend messages to client until ReadyForQuery ('Z').
// Each message is forwarded immediately as it arrives — transparent streaming,
// no buffering. Uses a reusable wire buffer to avoid per-message allocation.
// If the backend sends CopyInResponse ('G'), CopyOutResponse ('H'), or
// CopyBothResponse ('W'), it switches to bidirectional passthrough.
func (s *Server) relayUntilReady(clientConn, backendConn net.Conn) error {
	var hdr [5]byte
	bp := wireBufPool.Get().(*[]byte)
	wire := (*bp)[:0]
	defer func() {
		*bp = wire
		wireBufPool.Put(bp)
	}()
	for {
		if _, err := io.ReadFull(backendConn, hdr[:]); err != nil {
			return fmt.Errorf("read backend response header: %w", err)
		}
		msgType := hdr[0]
		length := int(binary.BigEndian.Uint32(hdr[1:5]))
		if length < 4 {
			return fmt.Errorf("invalid message length: %d", length)
		}
		payloadLen := length - 4
		wireLen := 5 + payloadLen

		// Reuse wire buffer to avoid per-message allocation
		if cap(wire) < wireLen {
			wire = make([]byte, wireLen)
		} else {
			wire = wire[:wireLen]
		}
		copy(wire[:5], hdr[:])
		if payloadLen > 0 {
			if _, err := io.ReadFull(backendConn, wire[5:]); err != nil {
				return fmt.Errorf("read backend response payload: %w", err)
			}
		}

		// Forward immediately — transparent streaming
		if _, err := clientConn.Write(wire); err != nil {
			return fmt.Errorf("forward to client: %w", err)
		}

		switch msgType {
		case protocol.MsgReadyForQuery:
			return nil
		case protocol.MsgCopyInResponse:
			if err := s.relayCopyIn(clientConn, backendConn); err != nil {
				return fmt.Errorf("copy in relay: %w", err)
			}
		case protocol.MsgCopyOutResponse:
			if err := s.relayCopyOut(clientConn, backendConn); err != nil {
				return fmt.Errorf("copy out relay: %w", err)
			}
		case protocol.MsgCopyBothResponse:
			if err := s.relayCopyBoth(clientConn, backendConn); err != nil {
				return fmt.Errorf("copy both relay: %w", err)
			}
		}
	}
}

// relayCopyIn relays CopyData/CopyDone/CopyFail from client to backend (COPY FROM STDIN).
func (s *Server) relayCopyIn(clientConn, backendConn net.Conn) error {
	for {
		msg, err := protocol.ReadMessage(clientConn)
		if err != nil {
			return fmt.Errorf("read client copy data: %w", err)
		}

		if err := protocol.ForwardRaw(backendConn, msg); err != nil {
			return fmt.Errorf("forward copy data to backend: %w", err)
		}

		switch msg.Type {
		case protocol.MsgCopyDone, protocol.MsgCopyFail:
			// Client finished sending data — return to normal relay loop
			return nil
		case protocol.MsgCopyData:
			// Continue relaying data
		default:
			slog.Warn("unexpected message during COPY IN", "type", string(msg.Type))
			return nil
		}
	}
}

// relayCopyOut relays CopyData/CopyDone from backend to client (COPY TO STDOUT).
func (s *Server) relayCopyOut(clientConn, backendConn net.Conn) error {
	for {
		msg, err := protocol.ReadMessage(backendConn)
		if err != nil {
			return fmt.Errorf("read backend copy data: %w", err)
		}

		if err := protocol.ForwardRaw(clientConn, msg); err != nil {
			return fmt.Errorf("forward copy data to client: %w", err)
		}

		switch msg.Type {
		case protocol.MsgCopyDone:
			// Backend finished sending data — return to normal relay loop
			return nil
		case protocol.MsgCopyData:
			// Continue relaying data
		default:
			// ErrorResponse etc. — return to relay loop which will handle it
			return nil
		}
	}
}

// relayCopyBoth handles CopyBothResponse (streaming replication) by relaying
// bidirectionally until one side sends CopyDone or CopyFail.
func (s *Server) relayCopyBoth(clientConn, backendConn net.Conn) error {
	errCh := make(chan error, 2)

	// backend → client
	go func() {
		for {
			msg, err := protocol.ReadMessage(backendConn)
			if err != nil {
				errCh <- fmt.Errorf("read backend copy both: %w", err)
				return
			}
			if err := protocol.ForwardRaw(clientConn, msg); err != nil {
				errCh <- fmt.Errorf("forward copy both to client: %w", err)
				return
			}
			if msg.Type == protocol.MsgCopyDone {
				errCh <- nil
				return
			}
		}
	}()

	// client → backend
	go func() {
		for {
			msg, err := protocol.ReadMessage(clientConn)
			if err != nil {
				errCh <- fmt.Errorf("read client copy both: %w", err)
				return
			}
			if err := protocol.ForwardRaw(backendConn, msg); err != nil {
				errCh <- fmt.Errorf("forward copy both to backend: %w", err)
				return
			}
			if msg.Type == protocol.MsgCopyDone || msg.Type == protocol.MsgCopyFail {
				errCh <- nil
				return
			}
		}
	}()

	// Wait for one direction to complete
	if err := <-errCh; err != nil {
		return err
	}
	return nil
}

// relayAndCollect relays backend responses to client and collects bytes for caching.
// If the collected size exceeds maxResultSize, collection is abandoned (returns nil)
// but relay to client continues until ReadyForQuery.
func (s *Server) relayAndCollect(clientConn, backendConn net.Conn) ([]byte, error) {
	maxSize := parseSize(s.getConfig().Cache.MaxResultSize)
	var buf []byte
	oversize := false

	for {
		msg, err := protocol.ReadMessage(backendConn)
		if err != nil {
			return nil, fmt.Errorf("read backend response: %w", err)
		}

		// Use original wire bytes (zero-copy) instead of re-serializing
		wireBytes := msg.Raw

		// Forward to client (always, regardless of cache)
		if _, err := clientConn.Write(wireBytes); err != nil {
			return nil, fmt.Errorf("forward to client: %w", err)
		}

		// Collect for cache only if within size limit
		if !oversize {
			buf = append(buf, wireBytes...)
			if maxSize > 0 && len(buf) > maxSize {
				slog.Debug("relay collect: result exceeds max_result_size, discarding buffer",
					"size", len(buf), "max", maxSize)
				buf = nil // release memory immediately
				oversize = true
			}
		}

		switch msg.Type {
		case protocol.MsgReadyForQuery:
			if oversize {
				return nil, nil
			}
			return buf, nil
		case protocol.MsgCopyInResponse:
			// Backend expects COPY data from client — relay client→backend until CopyDone/CopyFail
			if err := s.relayCopyIn(clientConn, backendConn); err != nil {
				return nil, fmt.Errorf("copy in relay: %w", err)
			}
			// COPY results are not cacheable
			buf = nil
			oversize = true
		case protocol.MsgCopyOutResponse:
			// Backend will send COPY data to client — relay backend→client until CopyDone
			if err := s.relayCopyOut(clientConn, backendConn); err != nil {
				return nil, fmt.Errorf("copy out relay: %w", err)
			}
			buf = nil
			oversize = true
		case protocol.MsgCopyBothResponse:
			// Streaming replication — relay bidirectionally
			if err := s.relayCopyBoth(clientConn, backendConn); err != nil {
				return nil, fmt.Errorf("copy both relay: %w", err)
			}
			buf = nil
			oversize = true
		}
	}
}
