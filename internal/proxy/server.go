package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
)

type Server struct {
	listenAddr string
	listener   net.Listener
	wg         sync.WaitGroup
}

func NewServer(listenAddr string) *Server {
	return &Server{
		listenAddr: listenAddr,
	}
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.listenAddr, err)
	}
	s.listener = ln
	slog.Info("proxy listening", "addr", s.listenAddr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				s.wg.Wait()
				slog.Info("proxy shut down gracefully")
				return nil
			default:
				slog.Error("accept connection", "error", err)
				continue
			}
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	slog.Info("new connection", "remote", conn.RemoteAddr())

	// TODO: PG 핸드셰이크 및 쿼리 릴레이 (T2-2, T2-3에서 구현)
	<-ctx.Done()
}
