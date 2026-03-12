package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"github.com/jyukki97/pgmux/internal/protocol"
)

// relayAuth relays the full bidirectional authentication flow between client and backend.
// Backend sends auth challenges → proxy forwards to client → client responds → proxy forwards to backend.
func (s *Server) relayAuth(clientConn, backendConn net.Conn) error {
	for {
		msg, err := protocol.ReadMessage(backendConn)
		if err != nil {
			return fmt.Errorf("read backend auth message: %w", err)
		}

		if err := protocol.WriteMessage(clientConn, msg.Type, msg.Payload); err != nil {
			return fmt.Errorf("forward auth message to client: %w", err)
		}

		if msg.Type == protocol.MsgErrorResponse {
			return fmt.Errorf("backend auth error")
		}

		if msg.Type == protocol.MsgReadyForQuery {
			return nil
		}

		// If backend requests authentication, read client's response and forward to backend
		if msg.Type == protocol.MsgAuthentication && len(msg.Payload) >= 4 {
			authType := binary.BigEndian.Uint32(msg.Payload[0:4])
			if authNeedsResponse(authType) {
				clientMsg, err := protocol.ReadMessage(clientConn)
				if err != nil {
					return fmt.Errorf("read client auth response: %w", err)
				}
				if err := protocol.WriteMessage(backendConn, clientMsg.Type, clientMsg.Payload); err != nil {
					return fmt.Errorf("forward client auth to backend: %w", err)
				}
			}
		}
	}
}

// frontendAuth authenticates the client directly at the proxy using MD5 auth.
// If the user is not in the configured auth.users list, returns an error.
func (s *Server) frontendAuth(clientConn net.Conn, username string) error {
	// Look up user in config
	cfg := s.getConfig()
	var password string
	found := false
	for _, u := range cfg.Auth.Users {
		if u.Username == username {
			password = u.Password
			found = true
			break
		}
	}

	if !found {
		s.sendError(clientConn, fmt.Sprintf("user \"%s\" is not allowed to connect", username))
		return fmt.Errorf("user %q not in auth.users", username)
	}

	// Send MD5 auth challenge (AuthenticationMD5Password, type=5)
	salt := make([]byte, 4)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	authPayload := make([]byte, 8)
	binary.BigEndian.PutUint32(authPayload[0:4], 5) // MD5Password
	copy(authPayload[4:8], salt)
	if err := protocol.WriteMessage(clientConn, protocol.MsgAuthentication, authPayload); err != nil {
		return fmt.Errorf("send MD5 challenge: %w", err)
	}

	// Read client's password response ('p')
	msg, err := protocol.ReadMessage(clientConn)
	if err != nil {
		return fmt.Errorf("read password response: %w", err)
	}
	if msg.Type != 'p' {
		return fmt.Errorf("expected password message, got %c", msg.Type)
	}

	// Client sends: "md5" + md5(md5(password + user) + salt) + \0
	clientHash := strings.TrimRight(string(msg.Payload), "\x00")
	expectedHash := pgMD5Password(username, password, salt)

	if clientHash != expectedHash {
		s.sendError(clientConn, "password authentication failed for user \""+username+"\"")
		return fmt.Errorf("MD5 password mismatch for user %q", username)
	}

	// Send AuthenticationOk (type=0)
	okPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(okPayload[0:4], 0)
	if err := protocol.WriteMessage(clientConn, protocol.MsgAuthentication, okPayload); err != nil {
		return fmt.Errorf("send auth ok: %w", err)
	}

	// Send ReadyForQuery ('Z', status='I' for idle)
	if err := protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'}); err != nil {
		return fmt.Errorf("send ready for query: %w", err)
	}

	return nil
}

// authNeedsResponse returns true if the PG auth type requires a client response.
func authNeedsResponse(authType uint32) bool {
	switch authType {
	case 3: // CleartextPassword
		return true
	case 5: // MD5Password
		return true
	case 10: // SASL (SCRAM-SHA-256 init)
		return true
	case 11: // SASLContinue
		return true
	default: // 0 (Ok), 12 (SASLFinal), etc.
		return false
	}
}

