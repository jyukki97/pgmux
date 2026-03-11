package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jyukki97/db-proxy/internal/cache"
	"github.com/jyukki97/db-proxy/internal/config"
	"github.com/jyukki97/db-proxy/internal/metrics"
	"github.com/jyukki97/db-proxy/internal/pool"
	"github.com/jyukki97/db-proxy/internal/protocol"
	"github.com/jyukki97/db-proxy/internal/router"
)

type Server struct {
	cfg         *config.Config
	listenAddr  string
	writerAddr  string
	readerPools map[string]*pool.Pool
	balancer    *router.RoundRobin
	queryCache  *cache.Cache
	metrics     *metrics.Metrics
	listener    net.Listener
	wg          sync.WaitGroup
}

func NewServer(cfg *config.Config) *Server {
	writerAddr := fmt.Sprintf("%s:%d", cfg.Writer.Host, cfg.Writer.Port)

	readerAddrs := make([]string, len(cfg.Readers))
	for i, r := range cfg.Readers {
		readerAddrs[i] = fmt.Sprintf("%s:%d", r.Host, r.Port)
	}

	s := &Server{
		cfg:         cfg,
		listenAddr:  cfg.Proxy.Listen,
		writerAddr:  writerAddr,
		balancer:    router.NewRoundRobin(readerAddrs),
		readerPools: make(map[string]*pool.Pool),
	}

	// Initialize Prometheus metrics
	if cfg.Metrics.Enabled {
		s.metrics = metrics.New()
		slog.Info("prometheus metrics enabled", "listen", cfg.Metrics.Listen)
	}

	// Initialize query cache
	if cfg.Cache.Enabled {
		s.queryCache = cache.New(cache.Config{
			MaxEntries: cfg.Cache.MaxCacheEntries,
			TTL:        cfg.Cache.CacheTTL,
			MaxSize:    parseSize(cfg.Cache.MaxResultSize),
		})
		slog.Info("query cache enabled",
			"max_entries", cfg.Cache.MaxCacheEntries,
			"ttl", cfg.Cache.CacheTTL)
	}

	// Initialize reader connection pools (PG-aware via DialFunc)
	for _, addr := range readerAddrs {
		addr := addr // capture for closure
		p, err := pool.New(pool.Config{
			DialFunc: func() (net.Conn, error) {
				return pgConnect(addr, cfg.Backend.User, cfg.Backend.Password, cfg.Backend.Database)
			},
			MinConnections:    0, // lazy creation; backends may not be ready at startup
			MaxConnections:    cfg.Pool.MaxConnections,
			IdleTimeout:       cfg.Pool.IdleTimeout,
			MaxLifetime:       cfg.Pool.MaxLifetime,
			ConnectionTimeout: cfg.Pool.ConnectionTimeout,
		})
		if err != nil {
			slog.Error("create reader pool", "addr", addr, "error", err)
			continue
		}
		s.readerPools[addr] = p
		slog.Info("reader pool created", "addr", addr, "max_conn", cfg.Pool.MaxConnections)
	}

	slog.Info("server initialized",
		"writer", writerAddr,
		"readers", len(readerAddrs),
		"cache", cfg.Cache.Enabled)

	return s
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.listenAddr, err)
	}
	s.listener = ln
	slog.Info("proxy listening", "addr", s.listenAddr)

	// Start reader health checks
	for addr, p := range s.readerPools {
		p.StartHealthCheck(ctx, s.cfg.Pool.IdleTimeout/2)
		slog.Debug("reader health check started", "addr", addr)
	}

	// Start balancer health check
	s.balancer.StartHealthCheck(ctx, s.cfg.Pool.ConnectionTimeout)

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
				s.closeReaderPools()
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
			if _, err := clientConn.Write([]byte{'N'}); err != nil {
				slog.Error("write ssl reject", "error", err)
				return
			}
			startup, err = protocol.ReadStartupMessage(clientConn)
			if err != nil {
				slog.Error("read startup after ssl reject", "error", err)
				return
			}
		}
	}

	_, _, params := protocol.ParseStartupParams(startup.Payload)
	slog.Info("client startup", "user", params["user"], "database", params["database"])

	// 3. Connect to writer backend (dedicated per client session)
	writerConn, err := net.Dial("tcp", s.writerAddr)
	if err != nil {
		slog.Error("connect to writer", "addr", s.writerAddr, "error", err)
		s.sendError(clientConn, "cannot connect to backend database")
		return
	}
	defer writerConn.Close()

	// 4. Forward startup message to writer
	startupRaw := make([]byte, 4+len(startup.Payload))
	binary.BigEndian.PutUint32(startupRaw[0:4], uint32(4+len(startup.Payload)))
	copy(startupRaw[4:], startup.Payload)
	if err := protocol.WriteRaw(writerConn, startupRaw); err != nil {
		slog.Error("forward startup to writer", "error", err)
		return
	}

	// 5. Relay authentication between client and writer until ReadyForQuery
	if err := s.relayAuth(clientConn, writerConn); err != nil {
		slog.Error("auth relay", "error", err)
		return
	}

	slog.Info("handshake complete", "remote", clientConn.RemoteAddr())

	// 6. Create per-client session router
	session := router.NewSession(s.cfg.Routing.ReadAfterWriteDelay)

	// 7. Relay queries with routing and caching
	s.relayQueries(ctx, clientConn, writerConn, session)
}

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

// relayQueries handles the main query loop with R/W routing and caching.
func (s *Server) relayQueries(ctx context.Context, clientConn, writerConn net.Conn, session *router.Session) {
	// extBuf collects Extended Query messages destined for a reader, flushed on Sync.
	var extBuf []*protocol.Message
	var extRoute router.Route
	var extReaderAddr string

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msg, err := protocol.ReadMessage(clientConn)
		if err != nil {
			slog.Debug("client disconnected", "error", err)
			return
		}

		if msg.Type == protocol.MsgTerminate {
			slog.Info("client terminated", "remote", clientConn.RemoteAddr())
			return
		}

		// --- Simple Query Protocol ---
		if msg.Type == protocol.MsgQuery {
			query := protocol.ExtractQueryText(msg.Payload)
			route := session.Route(query)
			target := routeName(route)
			slog.Debug("query routed", "sql", query, "route", target)

			start := time.Now()

			if route == router.RouteWriter {
				s.handleWriteQuery(clientConn, writerConn, msg, query)
			} else {
				if err := s.handleReadQuery(ctx, clientConn, writerConn, msg, query); err != nil {
					slog.Error("handle read query", "error", err)
					return
				}
			}

			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(time.Since(start).Seconds())
			}
			continue
		}

		// --- Extended Query Protocol ---
		switch msg.Type {
		case protocol.MsgParse:
			stmtName, query := protocol.ParseParseMessage(msg.Payload)
			route := session.RegisterStatement(stmtName, query)
			slog.Debug("parse registered", "stmt", stmtName, "sql", query, "route", routeName(route))

			if session.InTransaction() || route == router.RouteWriter {
				extRoute = router.RouteWriter
			} else {
				extRoute = route
			}
			extBuf = append(extBuf, msg)

		case protocol.MsgBind:
			_, stmtName := protocol.ParseBindMessage(msg.Payload)
			route := session.StatementRoute(stmtName)
			// If any statement in the batch is a writer, the whole batch goes to writer
			if route == router.RouteWriter {
				extRoute = router.RouteWriter
			}
			extBuf = append(extBuf, msg)

		case protocol.MsgClose:
			closeType, name := protocol.ParseCloseMessage(msg.Payload)
			if closeType == 'S' {
				session.CloseStatement(name)
			}
			extBuf = append(extBuf, msg)

		case protocol.MsgSync:
			start := time.Now()
			target := routeName(extRoute)

			if extRoute == router.RouteReader && !session.InTransaction() {
				// Try to send buffered messages to a reader
				extReaderAddr = s.balancer.Next()
				if err := s.handleExtendedRead(ctx, clientConn, writerConn, extBuf, msg, extReaderAddr); err != nil {
					slog.Error("extended read query", "error", err)
					return
				}
			} else {
				// Send to writer
				for _, m := range extBuf {
					if err := protocol.WriteMessage(writerConn, m.Type, m.Payload); err != nil {
						slog.Error("forward ext to writer", "error", err)
						return
					}
				}
				if err := protocol.WriteMessage(writerConn, msg.Type, msg.Payload); err != nil {
					slog.Error("forward sync to writer", "error", err)
					return
				}
				if err := s.relayUntilReady(clientConn, writerConn); err != nil {
					slog.Error("relay writer response (sync)", "error", err)
					return
				}
			}

			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(time.Since(start).Seconds())
			}

			// Reset batch state
			extBuf = extBuf[:0]
			extRoute = router.RouteReader
			extReaderAddr = ""

		default:
			// Describe(D), Execute(E), etc. — buffer them
			extBuf = append(extBuf, msg)
		}
	}
}

// handleWriteQuery forwards a write query to the writer and invalidates cache.
func (s *Server) handleWriteQuery(clientConn, writerConn net.Conn, msg *protocol.Message, query string) {
	if err := s.forwardAndRelay(clientConn, writerConn, msg); err != nil {
		slog.Error("forward write to writer", "error", err)
		return
	}

	// Invalidate cache for affected tables
	if s.queryCache != nil && router.Classify(query) == router.QueryWrite {
		tables := router.ExtractTables(query)
		for _, table := range tables {
			s.queryCache.InvalidateTable(table)
			if s.metrics != nil {
				s.metrics.CacheInvalidations.Inc()
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
			slog.Debug("cache invalidated", "table", table)
		}
	}
}

// handleReadQuery checks cache, acquires a reader from pool, or falls back to writer.
func (s *Server) handleReadQuery(ctx context.Context, clientConn, writerConn net.Conn, msg *protocol.Message, query string) error {
	// Check cache
	if s.queryCache != nil {
		key := cache.CacheKey(query)
		if cached := s.queryCache.Get(key); cached != nil {
			slog.Debug("cache hit", "sql", query)
			if s.metrics != nil {
				s.metrics.CacheHits.Inc()
			}
			_, err := clientConn.Write(cached)
			return err
		}
		if s.metrics != nil {
			s.metrics.CacheMisses.Inc()
		}
	}

	// Try to acquire a reader connection from pool
	readerAddr := s.balancer.Next()
	if readerAddr == "" {
		slog.Warn("no healthy reader, fallback to writer")
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.forwardAndRelay(clientConn, writerConn, msg)
	}

	rPool, ok := s.readerPools[readerAddr]
	if !ok {
		slog.Warn("no pool for reader, fallback to writer", "addr", readerAddr)
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.forwardAndRelay(clientConn, writerConn, msg)
	}

	acquireStart := time.Now()
	rConn, err := rPool.Acquire(ctx)
	if err != nil {
		slog.Warn("acquire reader failed, fallback to writer", "addr", readerAddr, "error", err)
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.forwardAndRelay(clientConn, writerConn, msg)
	}
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("reader", readerAddr).Inc()
		s.metrics.PoolAcquireDur.WithLabelValues("reader", readerAddr).Observe(time.Since(acquireStart).Seconds())
	}

	// Forward query to reader
	if err := protocol.WriteMessage(rConn, msg.Type, msg.Payload); err != nil {
		slog.Error("forward to reader", "addr", readerAddr, "error", err)
		rPool.Discard(rConn)
		// Fallback to writer
		return s.forwardAndRelay(clientConn, writerConn, msg)
	}

	// Relay response and collect bytes for caching
	if s.queryCache != nil {
		collected, err := s.relayAndCollect(clientConn, rConn)
		rPool.Release(rConn)
		if err != nil {
			return fmt.Errorf("relay reader response: %w", err)
		}
		key := cache.CacheKey(query)
		s.queryCache.Set(key, collected, nil)
		if s.metrics != nil {
			s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
		}
		slog.Debug("cache set", "sql", query, "size", len(collected))
	} else {
		if err := s.relayUntilReady(clientConn, rConn); err != nil {
			rPool.Discard(rConn)
			return fmt.Errorf("relay reader response: %w", err)
		}
		rPool.Release(rConn)
	}

	return nil
}

// handleExtendedRead sends buffered Extended Query messages to a reader, falling back to writer.
func (s *Server) handleExtendedRead(ctx context.Context, clientConn, writerConn net.Conn, buf []*protocol.Message, syncMsg *protocol.Message, readerAddr string) error {
	// Fallback helper: send entire batch to writer
	fallbackToWriter := func() error {
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		for _, m := range buf {
			if err := protocol.WriteMessage(writerConn, m.Type, m.Payload); err != nil {
				return fmt.Errorf("forward ext to writer: %w", err)
			}
		}
		if err := protocol.WriteMessage(writerConn, syncMsg.Type, syncMsg.Payload); err != nil {
			return fmt.Errorf("forward sync to writer: %w", err)
		}
		return s.relayUntilReady(clientConn, writerConn)
	}

	if readerAddr == "" {
		slog.Warn("no healthy reader for extended query, fallback to writer")
		return fallbackToWriter()
	}

	rPool, ok := s.readerPools[readerAddr]
	if !ok {
		slog.Warn("no pool for reader, fallback to writer", "addr", readerAddr)
		return fallbackToWriter()
	}

	acquireStart := time.Now()
	rConn, err := rPool.Acquire(ctx)
	if err != nil {
		slog.Warn("acquire reader failed for extended query, fallback to writer", "addr", readerAddr, "error", err)
		return fallbackToWriter()
	}
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("reader", readerAddr).Inc()
		s.metrics.PoolAcquireDur.WithLabelValues("reader", readerAddr).Observe(time.Since(acquireStart).Seconds())
	}

	// Forward all buffered messages + Sync to reader
	for _, m := range buf {
		if err := protocol.WriteMessage(rConn, m.Type, m.Payload); err != nil {
			slog.Error("forward ext to reader", "addr", readerAddr, "error", err)
			rPool.Discard(rConn)
			return fallbackToWriter()
		}
	}
	if err := protocol.WriteMessage(rConn, syncMsg.Type, syncMsg.Payload); err != nil {
		slog.Error("forward sync to reader", "addr", readerAddr, "error", err)
		rPool.Discard(rConn)
		return fallbackToWriter()
	}

	// Relay response from reader (with optional caching)
	if s.queryCache != nil {
		collected, err := s.relayAndCollect(clientConn, rConn)
		rPool.Release(rConn)
		if err != nil {
			return fmt.Errorf("relay reader extended response: %w", err)
		}
		// Cache the response keyed by the batch (first Parse query)
		if len(buf) > 0 && buf[0].Type == protocol.MsgParse {
			_, query := protocol.ParseParseMessage(buf[0].Payload)
			key := cache.CacheKey(query)
			s.queryCache.Set(key, collected, nil)
			if s.metrics != nil {
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
		}
	} else {
		if err := s.relayUntilReady(clientConn, rConn); err != nil {
			rPool.Discard(rConn)
			return fmt.Errorf("relay reader extended response: %w", err)
		}
		rPool.Release(rConn)
	}

	return nil
}

// forwardAndRelay forwards a message to backend and relays the response to client.
func (s *Server) forwardAndRelay(clientConn, backendConn net.Conn, msg *protocol.Message) error {
	if err := protocol.WriteMessage(backendConn, msg.Type, msg.Payload); err != nil {
		return fmt.Errorf("forward message: %w", err)
	}
	return s.relayUntilReady(clientConn, backendConn)
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

// relayAndCollect relays backend responses to client and collects all bytes for caching.
func (s *Server) relayAndCollect(clientConn, backendConn net.Conn) ([]byte, error) {
	var buf []byte
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

		// Forward to client
		if _, err := clientConn.Write(msgBytes); err != nil {
			return nil, fmt.Errorf("forward to client: %w", err)
		}

		buf = append(buf, msgBytes...)

		if msg.Type == protocol.MsgReadyForQuery {
			return buf, nil
		}
	}
}

func (s *Server) sendError(conn net.Conn, msg string) {
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

func (s *Server) closeReaderPools() {
	for addr, p := range s.readerPools {
		p.Close()
		slog.Debug("reader pool closed", "addr", addr)
	}
}

func routeName(r router.Route) string {
	if r == router.RouteWriter {
		return "writer"
	}
	return "reader"
}

// parseSize converts a size string like "512KB" or "1MB" to bytes.
func parseSize(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	if strings.HasSuffix(s, "MB") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "MB"))
		return n * 1024 * 1024
	}
	if strings.HasSuffix(s, "KB") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "KB"))
		return n * 1024
	}
	n, _ := strconv.Atoi(s)
	return n
}
