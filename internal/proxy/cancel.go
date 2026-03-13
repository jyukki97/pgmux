package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
)

// cancelTarget tracks the current backend connection for a client session,
// enabling cancel request forwarding.
type cancelTarget struct {
	proxyPID    uint32
	proxySecret uint32

	mu            sync.Mutex
	backendAddr   string
	backendPID    uint32
	backendSecret uint32
}

func (ct *cancelTarget) setFromConn(addr string, c *pool.Conn) {
	ct.mu.Lock()
	ct.backendAddr = addr
	ct.backendPID = c.BackendPID
	ct.backendSecret = c.BackendSecret
	ct.mu.Unlock()
}

func (ct *cancelTarget) clear() {
	ct.mu.Lock()
	ct.backendAddr = ""
	ct.backendPID = 0
	ct.backendSecret = 0
	ct.mu.Unlock()
}

func (ct *cancelTarget) get() (addr string, pid, secret uint32) {
	ct.mu.Lock()
	addr, pid, secret = ct.backendAddr, ct.backendPID, ct.backendSecret
	ct.mu.Unlock()
	return
}

// cancelKeyPair is the lookup key for the cancel map.
type cancelKeyPair struct {
	pid    uint32
	secret uint32
}

// newCancelTarget creates a new cancel target with a unique proxy PID and random secret.
func (s *Server) newCancelTarget() *cancelTarget {
	pid := s.nextProxyPID.Add(1)
	var secretBuf [4]byte
	_, _ = rand.Read(secretBuf[:])
	secret := binary.BigEndian.Uint32(secretBuf[:])

	ct := &cancelTarget{
		proxyPID:    pid,
		proxySecret: secret,
	}
	s.cancelMap.Store(cancelKeyPair{pid: pid, secret: secret}, ct)
	return ct
}

// removeCancelTarget removes the cancel target from the map.
func (s *Server) removeCancelTarget(ct *cancelTarget) {
	s.cancelMap.Delete(cancelKeyPair{pid: ct.proxyPID, secret: ct.proxySecret})
}

// handleCancelRequest processes a PostgreSQL CancelRequest.
func (s *Server) handleCancelRequest(payload []byte) {
	if len(payload) < 12 {
		slog.Warn("cancel request: payload too short")
		return
	}
	pid := binary.BigEndian.Uint32(payload[4:8])
	secret := binary.BigEndian.Uint32(payload[8:12])

	key := cancelKeyPair{pid: pid, secret: secret}
	val, ok := s.cancelMap.Load(key)
	if !ok {
		slog.Debug("cancel request: no matching session", "pid", pid)
		return
	}

	ct := val.(*cancelTarget)
	addr, bPID, bSecret := ct.get()
	if addr == "" || bPID == 0 {
		slog.Debug("cancel request: no active backend query", "pid", pid)
		return
	}

	slog.Info("forwarding cancel request",
		"proxy_pid", pid, "backend_addr", addr, "backend_pid", bPID)
	if err := forwardCancel(addr, bPID, bSecret); err != nil {
		slog.Warn("cancel request forward failed", "error", err)
	}
}

// forwardCancel sends a CancelRequest to the specified backend.
func forwardCancel(addr string, pid, secret uint32) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s for cancel: %w", addr, err)
	}
	defer conn.Close()

	var buf [16]byte
	binary.BigEndian.PutUint32(buf[0:4], 16) // length
	binary.BigEndian.PutUint32(buf[4:8], protocol.CancelRequestCode)
	binary.BigEndian.PutUint32(buf[8:12], pid)
	binary.BigEndian.PutUint32(buf[12:16], secret)

	_, err = conn.Write(buf[:])
	return err
}
