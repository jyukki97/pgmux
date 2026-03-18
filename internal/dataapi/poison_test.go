package dataapi

import (
	"net"
	"testing"
	"time"
)

// brokenConn simulates a connection that fails on Read (e.g., network error during DISCARD ALL).
type brokenConn struct {
	net.Conn
}

func (b *brokenConn) Write(p []byte) (int, error)        { return len(p), nil }
func (b *brokenConn) Read(p []byte) (int, error)         { return 0, net.ErrClosed }
func (b *brokenConn) SetDeadline(t time.Time) error      { return nil }
func (b *brokenConn) SetReadDeadline(t time.Time) error  { return nil }
func (b *brokenConn) SetWriteDeadline(t time.Time) error { return nil }
func (b *brokenConn) Close() error                       { return nil }
func (b *brokenConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (b *brokenConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }

func TestDrainUntilReady_ReturnsErrorOnBrokenConn(t *testing.T) {
	conn := &brokenConn{}
	err := drainUntilReady(conn)
	if err == nil {
		t.Fatal("expected error from drainUntilReady on broken connection, got nil")
	}
}
