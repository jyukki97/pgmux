package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/jyukki97/db-proxy/internal/config"
	"github.com/jyukki97/db-proxy/internal/protocol"
)

type Server struct {
	listenAddr string
	writerAddr string
	listener   net.Listener
	wg         sync.WaitGroup
}

func NewServer(cfg *config.Config) *Server {
	return &Server{
		listenAddr: cfg.Proxy.Listen,
		writerAddr: fmt.Sprintf("%s:%d", cfg.Writer.Host, cfg.Writer.Port),
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

func (s *Server) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()
	slog.Info("new connection", "remote", clientConn.RemoteAddr())

	// 1. Read startup message from client
	startup, err := protocol.ReadStartupMessage(clientConn)
	if err != nil {
		slog.Error("read startup message", "error", err)
		return
	}

	// 2. Handle SSL request — reject and wait for real startup
	if len(startup.Payload) >= 4 {
		code := binary.BigEndian.Uint32(startup.Payload[0:4])
		if code == protocol.SSLRequestCode {
			// Send 'N' (SSL not supported)
			if _, err := clientConn.Write([]byte{'N'}); err != nil {
				slog.Error("write ssl reject", "error", err)
				return
			}
			// Read the real startup message
			startup, err = protocol.ReadStartupMessage(clientConn)
			if err != nil {
				slog.Error("read startup after ssl reject", "error", err)
				return
			}
		}
	}

	_, _, params := protocol.ParseStartupParams(startup.Payload)
	slog.Info("client startup", "user", params["user"], "database", params["database"])

	// 3. Connect to backend DB
	backendConn, err := net.Dial("tcp", s.writerAddr)
	if err != nil {
		slog.Error("connect to backend", "addr", s.writerAddr, "error", err)
		s.sendError(clientConn, "cannot connect to backend database")
		return
	}
	defer backendConn.Close()

	// 4. Forward startup message to backend (rebuild with length prefix)
	startupRaw := make([]byte, 4+len(startup.Payload))
	binary.BigEndian.PutUint32(startupRaw[0:4], uint32(4+len(startup.Payload)))
	copy(startupRaw[4:], startup.Payload)
	if err := protocol.WriteRaw(backendConn, startupRaw); err != nil {
		slog.Error("forward startup to backend", "error", err)
		return
	}

	// 5. Relay authentication messages from backend to client until ReadyForQuery
	if err := s.relayAuth(clientConn, backendConn); err != nil {
		slog.Error("auth relay", "error", err)
		return
	}

	slog.Info("handshake complete", "remote", clientConn.RemoteAddr())

	// 6. Relay queries (T2-3)
	s.relayQueries(ctx, clientConn, backendConn)
}

// relayAuth forwards all messages from backend to client until ReadyForQuery ('Z').
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
	}
}

// relayQueries relays messages between client and backend bidirectionally.
// TODO: T2-3에서 본격 구현 (현재는 단순 릴레이)
func (s *Server) relayQueries(ctx context.Context, clientConn, backendConn net.Conn) {
	done := make(chan struct{}, 2)

	// Client → Backend
	go func() {
		defer func() { done <- struct{}{} }()
		relay(clientConn, backendConn)
	}()

	// Backend → Client
	go func() {
		defer func() { done <- struct{}{} }()
		relay(backendConn, clientConn)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func relay(src, dst net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (s *Server) sendError(conn net.Conn, msg string) {
	// PG ErrorResponse: 'E' + length + severity + message + terminator
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, []byte("ERROR")...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, []byte(msg)...)
	payload = append(payload, 0)
	payload = append(payload, 0) // terminator
	protocol.WriteMessage(conn, protocol.MsgErrorResponse, payload)
}
