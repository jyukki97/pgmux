package proxy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jyukki97/db-proxy/internal/protocol"
	"golang.org/x/crypto/pbkdf2"
)

// pgConnect establishes an authenticated PostgreSQL connection.
// Supports MD5 and SCRAM-SHA-256 authentication.
func pgConnect(addr, user, password, database string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// Build and send startup message
	startup := protocol.BuildStartupMessage(map[string]string{
		"user":     user,
		"database": database,
	})
	if err := protocol.WriteRaw(conn, startup); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send startup: %w", err)
	}

	// Handle authentication flow
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read auth: %w", err)
		}

		switch msg.Type {
		case protocol.MsgAuthentication:
			if len(msg.Payload) < 4 {
				conn.Close()
				return nil, fmt.Errorf("invalid auth message")
			}
			authType := binary.BigEndian.Uint32(msg.Payload[0:4])
			switch authType {
			case 0: // AuthenticationOk
			case 3: // CleartextPassword
				pwMsg := append([]byte(password), 0)
				if err := protocol.WriteMessage(conn, 'p', pwMsg); err != nil {
					conn.Close()
					return nil, fmt.Errorf("send cleartext password: %w", err)
				}
			case 5: // MD5Password
				if len(msg.Payload) < 8 {
					conn.Close()
					return nil, fmt.Errorf("md5 auth: missing salt")
				}
				salt := msg.Payload[4:8]
				hash := pgMD5Password(user, password, salt)
				pwMsg := append([]byte(hash), 0)
				if err := protocol.WriteMessage(conn, 'p', pwMsg); err != nil {
					conn.Close()
					return nil, fmt.Errorf("send md5 password: %w", err)
				}
			case 10: // AuthenticationSASL
				if err := handleSCRAM(conn, msg.Payload[4:], user, password); err != nil {
					conn.Close()
					return nil, fmt.Errorf("scram auth: %w", err)
				}
			default:
				conn.Close()
				return nil, fmt.Errorf("unsupported auth type: %d", authType)
			}

		case protocol.MsgErrorResponse:
			conn.Close()
			return nil, fmt.Errorf("backend auth error")

		case protocol.MsgReadyForQuery:
			return conn, nil

		default:
			// Skip ParameterStatus, BackendKeyData, etc.
		}
	}
}

// handleSCRAM performs SCRAM-SHA-256 authentication.
func handleSCRAM(conn net.Conn, mechanisms []byte, user, password string) error {
	// Verify SCRAM-SHA-256 is offered
	mechList := string(mechanisms)
	if !strings.Contains(mechList, "SCRAM-SHA-256") {
		return fmt.Errorf("SCRAM-SHA-256 not offered: %s", mechList)
	}

	// Step 1: Send SASLInitialResponse
	clientNonce := generateNonce()
	clientFirstBare := fmt.Sprintf("n=%s,r=%s", user, clientNonce)
	clientFirstMsg := "n,," + clientFirstBare

	// SASLInitialResponse: mechanism name + \0 + int32 length + data
	var saslResp []byte
	saslResp = append(saslResp, []byte("SCRAM-SHA-256")...)
	saslResp = append(saslResp, 0)
	respData := []byte(clientFirstMsg)
	saslResp = binary.BigEndian.AppendUint32(saslResp, uint32(len(respData)))
	saslResp = append(saslResp, respData...)

	if err := protocol.WriteMessage(conn, 'p', saslResp); err != nil {
		return fmt.Errorf("send SASL initial: %w", err)
	}

	// Step 2: Read AuthenticationSASLContinue (type 11)
	msg, err := protocol.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read SASL continue: %w", err)
	}
	if msg.Type == protocol.MsgErrorResponse {
		return fmt.Errorf("SASL auth error from server")
	}
	if msg.Type != protocol.MsgAuthentication || len(msg.Payload) < 4 {
		return fmt.Errorf("unexpected message during SASL: type=%c", msg.Type)
	}
	if binary.BigEndian.Uint32(msg.Payload[0:4]) != 11 {
		return fmt.Errorf("expected SASLContinue(11), got %d", binary.BigEndian.Uint32(msg.Payload[0:4]))
	}

	serverFirstMsg := string(msg.Payload[4:])

	// Parse server-first-message: r=<nonce>,s=<salt>,i=<iterations>
	serverNonce, salt, iterations, err := parseServerFirst(serverFirstMsg)
	if err != nil {
		return fmt.Errorf("parse server-first: %w", err)
	}

	// Verify server nonce starts with client nonce
	if !strings.HasPrefix(serverNonce, clientNonce) {
		return fmt.Errorf("server nonce doesn't match client nonce")
	}

	// Step 3: Compute client proof and send SASLResponse
	saltedPassword := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)

	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)

	clientFinalWithoutProof := fmt.Sprintf("c=biws,r=%s", serverNonce)
	authMessage := clientFirstBare + "," + serverFirstMsg + "," + clientFinalWithoutProof

	clientSignature := hmacSHA256(storedKey, []byte(authMessage))
	clientProof := xorBytes(clientKey, clientSignature)

	clientFinalMsg := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)

	if err := protocol.WriteMessage(conn, 'p', []byte(clientFinalMsg)); err != nil {
		return fmt.Errorf("send SASL response: %w", err)
	}

	// Step 4: Read AuthenticationSASLFinal (type 12)
	msg, err = protocol.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read SASL final: %w", err)
	}
	if msg.Type == protocol.MsgErrorResponse {
		return fmt.Errorf("SASL final: auth failed")
	}
	if msg.Type != protocol.MsgAuthentication || len(msg.Payload) < 4 {
		return fmt.Errorf("unexpected message for SASL final: type=%c", msg.Type)
	}
	authType := binary.BigEndian.Uint32(msg.Payload[0:4])
	if authType != 12 {
		return fmt.Errorf("expected SASLFinal(12), got %d", authType)
	}

	// Verify server signature
	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	expectedServerSig := hmacSHA256(serverKey, []byte(authMessage))

	serverFinalMsg := string(msg.Payload[4:])
	if !strings.HasPrefix(serverFinalMsg, "v=") {
		return fmt.Errorf("invalid server-final: %s", serverFinalMsg)
	}
	serverSig, err := base64.StdEncoding.DecodeString(serverFinalMsg[2:])
	if err != nil {
		return fmt.Errorf("decode server signature: %w", err)
	}
	if !hmac.Equal(serverSig, expectedServerSig) {
		return fmt.Errorf("server signature mismatch")
	}

	// AuthenticationOk (type 0) will follow, handled by the caller's loop
	return nil
}

func parseServerFirst(msg string) (nonce string, salt []byte, iterations int, err error) {
	parts := strings.Split(msg, ",")
	for _, p := range parts {
		if strings.HasPrefix(p, "r=") {
			nonce = p[2:]
		} else if strings.HasPrefix(p, "s=") {
			salt, err = base64.StdEncoding.DecodeString(p[2:])
			if err != nil {
				return "", nil, 0, fmt.Errorf("decode salt: %w", err)
			}
		} else if strings.HasPrefix(p, "i=") {
			fmt.Sscanf(p[2:], "%d", &iterations)
		}
	}
	if nonce == "" || salt == nil || iterations == 0 {
		return "", nil, 0, fmt.Errorf("incomplete server-first-message: %s", msg)
	}
	return nonce, salt, iterations, nil
}

func generateNonce() string {
	buf := make([]byte, 18)
	rand.Read(buf)
	return base64.StdEncoding.EncodeToString(buf)
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Sum(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

func xorBytes(a, b []byte) []byte {
	result := make([]byte, len(a))
	for i := range a {
		result[i] = a[i] ^ b[i]
	}
	return result
}

// pgMD5Password computes the PG MD5 password hash.
// hash = "md5" + md5(md5(password + user) + salt)
func pgMD5Password(user, password string, salt []byte) string {
	h1 := md5.New()
	h1.Write([]byte(password))
	h1.Write([]byte(user))
	inner := fmt.Sprintf("%x", h1.Sum(nil))

	h2 := md5.New()
	h2.Write([]byte(inner))
	h2.Write(salt)
	return "md5" + fmt.Sprintf("%x", h2.Sum(nil))
}
