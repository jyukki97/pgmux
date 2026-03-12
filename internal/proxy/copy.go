package proxy

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"

	"github.com/jyukki97/pgmux/internal/protocol"
)

// forwardAndRelay forwards a message to backend and relays the response to client.
func (s *Server) forwardAndRelay(clientConn, backendConn net.Conn, msg *protocol.Message) error {
	if err := protocol.WriteMessage(backendConn, msg.Type, msg.Payload); err != nil {
		return fmt.Errorf("forward message: %w", err)
	}
	return s.relayUntilReady(clientConn, backendConn)
}

// relayUntilReady forwards backend messages to client until ReadyForQuery ('Z').
// If the backend sends CopyInResponse ('G') or CopyOutResponse ('H'),
// it switches to bidirectional passthrough to avoid deadlock.
func (s *Server) relayUntilReady(clientConn, backendConn net.Conn) error {
	for {
		msg, err := protocol.ReadMessage(backendConn)
		if err != nil {
			return fmt.Errorf("read backend response: %w", err)
		}

		if err := protocol.WriteMessage(clientConn, msg.Type, msg.Payload); err != nil {
			return fmt.Errorf("forward to client: %w", err)
		}

		switch msg.Type {
		case protocol.MsgReadyForQuery:
			return nil
		case protocol.MsgCopyInResponse:
			// Backend expects COPY data from client — relay client→backend until CopyDone/CopyFail
			if err := s.relayCopyIn(clientConn, backendConn); err != nil {
				return fmt.Errorf("copy in relay: %w", err)
			}
			// After CopyIn completes, backend sends CommandComplete + ReadyForQuery
			// Continue the loop to catch them
		case protocol.MsgCopyOutResponse:
			// Backend will send COPY data to client — relay backend→client until CopyDone
			if err := s.relayCopyOut(clientConn, backendConn); err != nil {
				return fmt.Errorf("copy out relay: %w", err)
			}
			// After CopyOut, backend sends CommandComplete + ReadyForQuery
			// Continue the loop to catch them
		case protocol.MsgCopyBothResponse:
			// Streaming replication — relay bidirectionally
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

		if err := protocol.WriteMessage(backendConn, msg.Type, msg.Payload); err != nil {
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

		if err := protocol.WriteMessage(clientConn, msg.Type, msg.Payload); err != nil {
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
			if err := protocol.WriteMessage(clientConn, msg.Type, msg.Payload); err != nil {
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
			if err := protocol.WriteMessage(backendConn, msg.Type, msg.Payload); err != nil {
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

		// Serialize message to wire format
		msgBytes := make([]byte, 1+4+len(msg.Payload))
		msgBytes[0] = msg.Type
		binary.BigEndian.PutUint32(msgBytes[1:5], uint32(4+len(msg.Payload)))
		copy(msgBytes[5:], msg.Payload)

		// Forward to client (always, regardless of cache)
		if _, err := clientConn.Write(msgBytes); err != nil {
			return nil, fmt.Errorf("forward to client: %w", err)
		}

		// Collect for cache only if within size limit
		if !oversize {
			buf = append(buf, msgBytes...)
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
