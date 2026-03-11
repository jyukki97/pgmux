package proxy

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestServer_AcceptsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewServer("127.0.0.1:0")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = ln
	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.wg.Add(1)
			go func() {
				defer srv.wg.Done()
				srv.handleConn(ctx, conn)
			}()
		}
	}()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	conn.Close()

	cancel()
}

func TestServer_GracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	srv := NewServer("127.0.0.1:0")

	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
