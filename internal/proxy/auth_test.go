package proxy

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/protocol"
)

func TestPgMD5Password(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		password string
		salt     []byte
	}{
		{
			name:     "basic",
			user:     "testuser",
			password: "testpass",
			salt:     []byte{0x01, 0x02, 0x03, 0x04},
		},
		{
			name:     "empty password",
			user:     "admin",
			password: "",
			salt:     []byte{0xAA, 0xBB, 0xCC, 0xDD},
		},
		{
			name:     "empty user",
			user:     "",
			password: "secret",
			salt:     []byte{0x00, 0x00, 0x00, 0x00},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pgMD5Password(tc.user, tc.password, tc.salt)

			// Compute expected: "md5" + hex(md5(hex(md5(password + user)) + salt))
			h1 := md5.New()
			h1.Write([]byte(tc.password))
			h1.Write([]byte(tc.user))
			inner := fmt.Sprintf("%x", h1.Sum(nil))

			h2 := md5.New()
			h2.Write([]byte(inner))
			h2.Write(tc.salt)
			want := "md5" + fmt.Sprintf("%x", h2.Sum(nil))

			if got != want {
				t.Errorf("pgMD5Password(%q, %q, %v) = %q, want %q", tc.user, tc.password, tc.salt, got, want)
			}

			// Must start with "md5" prefix
			if !strings.HasPrefix(got, "md5") {
				t.Errorf("result %q does not start with 'md5' prefix", got)
			}

			// md5 hex digest is 32 chars, total length = 3 + 32 = 35
			if len(got) != 35 {
				t.Errorf("result length = %d, want 35", len(got))
			}
		})
	}
}

func TestAuthNeedsResponse(t *testing.T) {
	tests := []struct {
		name     string
		authType uint32
		want     bool
	}{
		{"Ok", 0, false},
		{"CleartextPassword", 3, true},
		{"MD5Password", 5, true},
		{"SASL", 10, true},
		{"SASLContinue", 11, true},
		{"SASLFinal", 12, false},
		{"unknown 99", 99, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := authNeedsResponse(tc.authType)
			if got != tc.want {
				t.Errorf("authNeedsResponse(%d) = %v, want %v", tc.authType, got, tc.want)
			}
		})
	}
}

// newAuthTestServer creates a minimal Server with auth config for testing.
func newAuthTestServer(t *testing.T, users []config.AuthUser) *Server {
	t.Helper()
	cfg := &config.Config{
		Auth: config.AuthConfig{
			Enabled: true,
			Users:   users,
		},
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func TestFrontendAuth_Success(t *testing.T) {
	srv := newAuthTestServer(t, []config.AuthUser{
		{Username: "testuser", Password: "testpass"},
	})

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var proxyPID uint32 = 42
	var proxySecret uint32 = 12345

	errCh := make(chan error, 1)

	// Client goroutine: simulate PG client
	go func() {
		defer close(errCh)

		// 1. Read MD5 challenge (type 'R', authType=5)
		msg, err := protocol.ReadMessage(clientConn)
		if err != nil {
			errCh <- fmt.Errorf("read MD5 challenge: %w", err)
			return
		}
		if msg.Type != protocol.MsgAuthentication {
			errCh <- fmt.Errorf("expected Authentication message, got %c", msg.Type)
			return
		}
		if len(msg.Payload) < 8 {
			errCh <- fmt.Errorf("MD5 challenge payload too short: %d", len(msg.Payload))
			return
		}
		authType := binary.BigEndian.Uint32(msg.Payload[0:4])
		if authType != 5 {
			errCh <- fmt.Errorf("expected authType=5, got %d", authType)
			return
		}

		// 2. Extract salt and compute MD5 hash
		salt := msg.Payload[4:8]
		hash := pgMD5Password("testuser", "testpass", salt)

		// 3. Send password response (type 'p', null-terminated)
		pwMsg := append([]byte(hash), 0)
		if err := protocol.WriteMessage(clientConn, 'p', pwMsg); err != nil {
			errCh <- fmt.Errorf("send password: %w", err)
			return
		}

		// 4. Read AuthenticationOk (type 'R', authType=0)
		msg, err = protocol.ReadMessage(clientConn)
		if err != nil {
			errCh <- fmt.Errorf("read AuthOk: %w", err)
			return
		}
		if msg.Type != protocol.MsgAuthentication {
			errCh <- fmt.Errorf("expected Authentication, got %c", msg.Type)
			return
		}
		okType := binary.BigEndian.Uint32(msg.Payload[0:4])
		if okType != 0 {
			errCh <- fmt.Errorf("expected authType=0, got %d", okType)
			return
		}

		// 5. Read BackendKeyData (type 'K')
		msg, err = protocol.ReadMessage(clientConn)
		if err != nil {
			errCh <- fmt.Errorf("read BackendKeyData: %w", err)
			return
		}
		if msg.Type != protocol.MsgBackendKeyData {
			errCh <- fmt.Errorf("expected BackendKeyData, got %c", msg.Type)
			return
		}
		pid := binary.BigEndian.Uint32(msg.Payload[0:4])
		secret := binary.BigEndian.Uint32(msg.Payload[4:8])
		if pid != proxyPID || secret != proxySecret {
			errCh <- fmt.Errorf("BackendKeyData mismatch: pid=%d secret=%d", pid, secret)
			return
		}

		// 6. Read ReadyForQuery (type 'Z')
		msg, err = protocol.ReadMessage(clientConn)
		if err != nil {
			errCh <- fmt.Errorf("read ReadyForQuery: %w", err)
			return
		}
		if msg.Type != protocol.MsgReadyForQuery {
			errCh <- fmt.Errorf("expected ReadyForQuery, got %c", msg.Type)
			return
		}
	}()

	// Server side: call frontendAuth
	if err := srv.frontendAuth(serverConn, "testuser", proxyPID, proxySecret); err != nil {
		t.Fatalf("frontendAuth returned error: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("client goroutine error: %v", err)
	}
}

func TestFrontendAuth_UnknownUser(t *testing.T) {
	srv := newAuthTestServer(t, []config.AuthUser{
		{Username: "testuser", Password: "testpass"},
	})

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Drain client-side so sendError does not block
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := clientConn.Read(buf); err != nil {
				return
			}
		}
	}()

	err := srv.frontendAuth(serverConn, "unknown", 1, 1)
	if err == nil {
		t.Fatal("expected error for unknown user, got nil")
	}
	if !strings.Contains(err.Error(), "not in auth.users") {
		t.Errorf("error %q does not contain 'not in auth.users'", err.Error())
	}
}

func TestFrontendAuth_WrongPassword(t *testing.T) {
	srv := newAuthTestServer(t, []config.AuthUser{
		{Username: "testuser", Password: "testpass"},
	})

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	errCh := make(chan error, 1)

	// Client goroutine: read challenge, send wrong hash
	go func() {
		defer close(errCh)

		// Read MD5 challenge
		msg, err := protocol.ReadMessage(clientConn)
		if err != nil {
			errCh <- fmt.Errorf("read challenge: %w", err)
			return
		}
		if msg.Type != protocol.MsgAuthentication || len(msg.Payload) < 8 {
			errCh <- fmt.Errorf("unexpected challenge message")
			return
		}

		// Send wrong password hash (null-terminated)
		wrongHash := append([]byte("md5ffffffffffffffffffffffffffffffff"), 0)
		if err := protocol.WriteMessage(clientConn, 'p', wrongHash); err != nil {
			errCh <- fmt.Errorf("send wrong password: %w", err)
			return
		}

		// Drain any error response from server
		buf := make([]byte, 4096)
		for {
			if _, err := clientConn.Read(buf); err != nil {
				return
			}
		}
	}()

	err := srv.frontendAuth(serverConn, "testuser", 1, 1)
	if err == nil {
		t.Fatal("expected error for wrong password, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error %q does not contain 'mismatch'", err.Error())
	}

	// Close serverConn to unblock the client goroutine's drain loop
	serverConn.Close()
	<-errCh
}
