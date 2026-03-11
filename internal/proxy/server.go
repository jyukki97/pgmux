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

// relayQueries reads client messages one at a time, forwards to backend,
// and relays backend responses back to client. Message-level relay enables
// future routing/caching hooks.
func (s *Server) relayQueries(ctx context.Context, clientConn, backendConn net.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Read one message from client
		msg, err := protocol.ReadMessage(clientConn)
		if err != nil {
			slog.Debug("client disconnected", "error", err)
			return
		}

		// Terminate message — close session
		if msg.Type == protocol.MsgTerminate {
			slog.Info("client terminated", "remote", clientConn.RemoteAddr())
			return
		}

		// Extract query text for logging (Query messages: SQL + null terminator)
		if msg.Type == protocol.MsgQuery {
			query := protocol.ExtractQueryText(msg.Payload)
			slog.Debug("query", "sql", query)
		}

		// Forward message to backend
		if err := protocol.WriteMessage(backendConn, msg.Type, msg.Payload); err != nil {
			slog.Error("forward to backend", "error", err)
			return
		}

		// Relay backend responses until ReadyForQuery
		if err := s.relayUntilReady(clientConn, backendConn); err != nil {
			slog.Error("relay backend response", "error", err)
			return
		}
	}
}

// relayUntilReady forwards backend messages to client until ReadyForQuery ('Z').
func (s *Server) relayUntilReady(clientConn, backendConn net.Conn) error {
	for {
		msg, err := protocol.ReadMessage(backendConn)
		if err != nil {
			return fmt.Errorf("read backend response: %w", err)
		}

		if err := protocol.WriteMessage(clientConn, msg.Type, msg.Payload); err != nil {
			return fmt.Errorf("forward to client: %w", err)
		}

		if msg.Type == protocol.MsgReadyForQuery {
			return nil
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
