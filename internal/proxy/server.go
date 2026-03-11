package proxy

import (
	"context"
	"crypto/tls"
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
	writerPool  *pool.Pool
	readerPools map[string]*pool.Pool
	balancer    *router.RoundRobin
	queryCache  *cache.Cache
	invalidator *cache.Invalidator
	metrics     *metrics.Metrics
	listener    net.Listener
	tlsConfig   *tls.Config
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

		// Initialize cross-instance cache invalidation via Redis Pub/Sub
		if cfg.Cache.Invalidation.Mode == "pubsub" && cfg.Cache.Invalidation.RedisAddr != "" {
			inv, err := cache.NewInvalidator(
				cfg.Cache.Invalidation.RedisAddr,
				cfg.Cache.Invalidation.Channel,
				s.queryCache,
			)
			if err != nil {
				slog.Error("cache invalidator init failed, falling back to local-only",
					"error", err, "redis_addr", cfg.Cache.Invalidation.RedisAddr)
			} else {
				s.invalidator = inv
				slog.Info("cache invalidation pubsub enabled",
					"redis", cfg.Cache.Invalidation.RedisAddr,
					"channel", cfg.Cache.Invalidation.Channel)
			}
		}
	}

	// Initialize writer connection pool
	writerPool, err := pool.New(pool.Config{
		DialFunc: func() (net.Conn, error) {
			return pgConnect(writerAddr, cfg.Backend.User, cfg.Backend.Password, cfg.Backend.Database)
		},
		MinConnections:    0, // lazy creation; backend may not be ready at startup
		MaxConnections:    cfg.Pool.MaxConnections,
		IdleTimeout:       cfg.Pool.IdleTimeout,
		MaxLifetime:       cfg.Pool.MaxLifetime,
		ConnectionTimeout: cfg.Pool.ConnectionTimeout,
	})
	if err != nil {
		slog.Error("create writer pool", "addr", writerAddr, "error", err)
	} else {
		s.writerPool = writerPool
		slog.Info("writer pool created", "addr", writerAddr, "max_conn", cfg.Pool.MaxConnections)
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

	// Load TLS certificate if enabled
	if cfg.TLS.Enabled {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			slog.Error("load TLS certificate", "error", err)
		} else {
			s.tlsConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
			}
			slog.Info("TLS enabled", "cert", cfg.TLS.CertFile)
		}
	}

	slog.Info("server initialized",
		"writer", writerAddr,
		"readers", len(readerAddrs),
		"cache", cfg.Cache.Enabled,
		"tls", cfg.TLS.Enabled,
		"pooling", "transaction")

	return s
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.listenAddr, err)
	}
	s.listener = ln
	slog.Info("proxy listening", "addr", s.listenAddr)

	// Start writer pool health check
	if s.writerPool != nil {
		s.writerPool.StartHealthCheck(ctx, s.cfg.Pool.IdleTimeout/2)
		slog.Debug("writer health check started", "addr", s.writerAddr)
	}

	// Start reader health checks
	for addr, p := range s.readerPools {
		p.StartHealthCheck(ctx, s.cfg.Pool.IdleTimeout/2)
		slog.Debug("reader health check started", "addr", addr)
	}

	// Start balancer health check
	s.balancer.StartHealthCheck(ctx, s.cfg.Pool.ConnectionTimeout)

	// Start cache invalidation subscriber
	if s.invalidator != nil {
		go s.invalidator.Subscribe(ctx)
	}

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
				s.closePools()
				if s.invalidator != nil {
					s.invalidator.Close()
				}
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

func (s *Server) handleConn(ctx context.Context, rawConn net.Conn) {
	defer rawConn.Close()
	clientConn := rawConn // may be upgraded to TLS below
	slog.Info("new connection", "remote", clientConn.RemoteAddr())

	// 1. Read startup message from client
	startup, err := protocol.ReadStartupMessage(clientConn)
	if err != nil {
		slog.Error("read startup message", "error", err)
		return
	}

	// 2. Handle SSL request
	if len(startup.Payload) >= 4 {
		code := binary.BigEndian.Uint32(startup.Payload[0:4])
		if code == protocol.SSLRequestCode {
			if s.tlsConfig != nil {
				// Accept TLS — respond 'S' and upgrade connection
				if _, err := clientConn.Write([]byte{'S'}); err != nil {
					slog.Error("write ssl accept", "error", err)
					return
				}
				tlsConn := tls.Server(clientConn, s.tlsConfig)
				if err := tlsConn.Handshake(); err != nil {
					slog.Error("TLS handshake", "error", err)
					return
				}
				clientConn = tlsConn
				slog.Debug("TLS connection established", "remote", clientConn.RemoteAddr())
			} else {
				// No TLS configured — reject
				if _, err := clientConn.Write([]byte{'N'}); err != nil {
					slog.Error("write ssl reject", "error", err)
					return
				}
			}
			startup, err = protocol.ReadStartupMessage(clientConn)
			if err != nil {
				slog.Error("read startup after ssl", "error", err)
				return
			}
		}
	}

	_, _, params := protocol.ParseStartupParams(startup.Payload)
	slog.Info("client startup", "user", params["user"], "database", params["database"])

	// 3. Authenticate client via temporary backend connection.
	//    Pool connections are pre-authenticated via pgConnect(), so we only need
	//    a temporary connection to relay the client's auth handshake.
	authConn, err := net.Dial("tcp", s.writerAddr)
	if err != nil {
		slog.Error("connect to writer for auth", "addr", s.writerAddr, "error", err)
		s.sendError(clientConn, "cannot connect to backend database")
		return
	}

	// Forward startup message to auth connection
	startupRaw := make([]byte, 4+len(startup.Payload))
	binary.BigEndian.PutUint32(startupRaw[0:4], uint32(4+len(startup.Payload)))
	copy(startupRaw[4:], startup.Payload)
	if err := protocol.WriteRaw(authConn, startupRaw); err != nil {
		authConn.Close()
		slog.Error("forward startup to writer", "error", err)
		return
	}

	// Relay authentication between client and auth connection
	if err := s.relayAuth(clientConn, authConn); err != nil {
		authConn.Close()
		slog.Error("auth relay", "error", err)
		return
	}

	// Auth complete — close temporary connection.
	// All further queries use pooled connections.
	authConn.Close()

	slog.Info("handshake complete", "remote", clientConn.RemoteAddr())

	// 4. Create per-client session router
	session := router.NewSession(s.cfg.Routing.ReadAfterWriteDelay)

	// 5. Relay queries with transaction-level pooling
	s.relayQueries(ctx, clientConn, session)
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

// relayQueries handles the main query loop with transaction-level connection pooling.
// Writer connections are acquired from writerPool per query/transaction and released back.
func (s *Server) relayQueries(ctx context.Context, clientConn net.Conn, session *router.Session) {
	// boundWriter is non-nil when a transaction is in progress.
	// The connection stays bound from BEGIN until COMMIT/ROLLBACK.
	var boundWriter *pool.Conn

	defer func() {
		if boundWriter != nil {
			s.resetAndReleaseWriter(boundWriter)
		}
	}()

	// Extended Query protocol state
	var extBuf []*protocol.Message
	var extRoute router.Route
	var extTxStart, extTxEnd bool

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

			wasInTx := session.InTransaction()
			route := session.Route(query)
			nowInTx := session.InTransaction()

			target := routeName(route)
			slog.Debug("query routed", "sql", query, "route", target)

			start := time.Now()

			if route == router.RouteWriter {
				wConn, acquired, err := s.acquireWriterConn(ctx, boundWriter)
				if err != nil {
					slog.Error("acquire writer", "error", err)
					s.sendError(clientConn, "cannot acquire backend connection")
					return
				}

				s.handleWriteQuery(clientConn, wConn, msg, query)

				// Transaction lifecycle management
				switch {
				case !wasInTx && nowInTx:
					// BEGIN — bind writer for transaction duration
					boundWriter = wConn
				case wasInTx && !nowInTx:
					// COMMIT/ROLLBACK — unbind and release
					boundWriter = nil
					s.resetAndReleaseWriter(wConn)
				case acquired:
					// Single statement outside transaction — release immediately
					s.resetAndReleaseWriter(wConn)
				}
				// If !acquired && still in transaction → keep using boundWriter
			} else {
				if err := s.handleReadQuery(ctx, clientConn, msg, query); err != nil {
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

			// Track transaction lifecycle in Extended Query
			upper := strings.ToUpper(strings.TrimSpace(query))
			if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") {
				extTxStart = true
			}
			if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "END") {
				extTxEnd = true
			}

			if session.InTransaction() || boundWriter != nil || route == router.RouteWriter {
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

			if extRoute == router.RouteReader && !session.InTransaction() && boundWriter == nil {
				// Reader path
				readerAddr := s.balancer.Next()
				if err := s.handleExtendedRead(ctx, clientConn, extBuf, msg, readerAddr); err != nil {
					slog.Error("extended read query", "error", err)
					return
				}
			} else {
				// Writer path — acquire from pool or use bound connection
				wConn, acquired, err := s.acquireWriterConn(ctx, boundWriter)
				if err != nil {
					slog.Error("acquire writer for extended query", "error", err)
					s.sendError(clientConn, "cannot acquire backend connection")
					return
				}

				// Forward all buffered messages + Sync to writer
				writeErr := s.forwardExtBatch(wConn, extBuf, msg)
				if writeErr != nil {
					slog.Error("forward ext batch to writer", "error", writeErr)
					if acquired {
						s.writerPool.Discard(wConn)
					} else if boundWriter != nil {
						s.writerPool.Discard(boundWriter)
						boundWriter = nil
					}
					return
				}

				if err := s.relayUntilReady(clientConn, wConn); err != nil {
					slog.Error("relay writer response (sync)", "error", err)
					if acquired {
						s.writerPool.Discard(wConn)
					} else if boundWriter != nil {
						s.writerPool.Discard(boundWriter)
						boundWriter = nil
					}
					return
				}

				// Update transaction state for Extended Query
				if extTxStart {
					session.SetInTransaction(true)
				}
				if extTxEnd {
					session.SetInTransaction(false)
				}

				// Transaction lifecycle
				switch {
				case extTxStart && !extTxEnd:
					// BEGIN — bind writer
					boundWriter = wConn
				case extTxEnd:
					// COMMIT/ROLLBACK — unbind and release
					boundWriter = nil
					s.resetAndReleaseWriter(wConn)
				case acquired:
					// Single batch outside transaction — release
					s.resetAndReleaseWriter(wConn)
				}
			}

			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(time.Since(start).Seconds())
			}

			// Reset batch state
			extBuf = extBuf[:0]
			extRoute = router.RouteReader
			extTxStart, extTxEnd = false, false

		default:
			// Describe(D), Execute(E), etc. — buffer them
			extBuf = append(extBuf, msg)
		}
	}
}

// acquireWriterConn returns the bound transaction connection or acquires a new one from the pool.
// The bool return indicates whether the connection was newly acquired (true) or was already bound (false).
func (s *Server) acquireWriterConn(ctx context.Context, bound *pool.Conn) (*pool.Conn, bool, error) {
	if bound != nil {
		return bound, false, nil
	}
	acquireStart := time.Now()
	conn, err := s.writerPool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire writer: %w", err)
	}
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("writer", s.writerAddr).Inc()
		s.metrics.PoolAcquireDur.WithLabelValues("writer", s.writerAddr).Observe(time.Since(acquireStart).Seconds())
	}
	return conn, true, nil
}

// resetAndReleaseWriter sends a reset query (DISCARD ALL) and returns the connection to the pool.
// If the reset fails, the connection is discarded instead.
func (s *Server) resetAndReleaseWriter(conn *pool.Conn) {
	if err := s.resetConn(conn); err != nil {
		slog.Warn("reset writer conn failed, discarding", "error", err)
		s.writerPool.Discard(conn)
		return
	}
	s.writerPool.Release(conn)
}

// resetConn sends the configured reset query (e.g. DISCARD ALL) to clean up session state
// before returning a connection to the pool.
func (s *Server) resetConn(conn net.Conn) error {
	resetQuery := s.cfg.Pool.ResetQuery
	if resetQuery == "" {
		return nil
	}
	payload := append([]byte(resetQuery), 0)
	if err := protocol.WriteMessage(conn, protocol.MsgQuery, payload); err != nil {
		return fmt.Errorf("send reset query: %w", err)
	}
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return fmt.Errorf("read reset response: %w", err)
		}
		if msg.Type == protocol.MsgErrorResponse {
			return fmt.Errorf("reset query error")
		}
		if msg.Type == protocol.MsgReadyForQuery {
			return nil
		}
	}
}

// fallbackToWriter acquires a writer connection from the pool and forwards the query.
func (s *Server) fallbackToWriter(ctx context.Context, clientConn net.Conn, msg *protocol.Message) error {
	wConn, err := s.writerPool.Acquire(ctx)
	if err != nil {
		s.sendError(clientConn, "no available backend connections")
		return fmt.Errorf("acquire writer for fallback: %w", err)
	}
	if s.metrics != nil {
		s.metrics.PoolAcquires.WithLabelValues("writer", s.writerAddr).Inc()
	}
	err = s.forwardAndRelay(clientConn, wConn, msg)
	s.resetAndReleaseWriter(wConn)
	return err
}

// handleWriteQuery forwards a write query to the writer and invalidates cache.
func (s *Server) handleWriteQuery(clientConn net.Conn, writerConn net.Conn, msg *protocol.Message, query string) {
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
		// Broadcast invalidation to other proxy instances
		if s.invalidator != nil && len(tables) > 0 {
			s.invalidator.Publish(context.Background(), tables)
		}
	}
}

// handleReadQuery checks cache, acquires a reader from pool, or falls back to writer.
func (s *Server) handleReadQuery(ctx context.Context, clientConn net.Conn, msg *protocol.Message, query string) error {
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
		return s.fallbackToWriter(ctx, clientConn, msg)
	}

	rPool, ok := s.readerPools[readerAddr]
	if !ok {
		slog.Warn("no pool for reader, fallback to writer", "addr", readerAddr)
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.fallbackToWriter(ctx, clientConn, msg)
	}

	acquireStart := time.Now()
	rConn, err := rPool.Acquire(ctx)
	if err != nil {
		slog.Warn("acquire reader failed, fallback to writer", "addr", readerAddr, "error", err)
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.fallbackToWriter(ctx, clientConn, msg)
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
		return s.fallbackToWriter(ctx, clientConn, msg)
	}

	// Relay response and collect bytes for caching
	if s.queryCache != nil {
		collected, err := s.relayAndCollect(clientConn, rConn)
		rPool.Release(rConn)
		if err != nil {
			return fmt.Errorf("relay reader response: %w", err)
		}
		if collected != nil { // nil means oversize, skip cache
			key := cache.CacheKey(query)
			s.queryCache.Set(key, collected, nil)
			if s.metrics != nil {
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
			slog.Debug("cache set", "sql", query, "size", len(collected))
		}
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
func (s *Server) handleExtendedRead(ctx context.Context, clientConn net.Conn, buf []*protocol.Message, syncMsg *protocol.Message, readerAddr string) error {
	// Fallback helper: send entire batch to writer via pool
	fallbackToWriter := func() error {
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		wConn, err := s.writerPool.Acquire(ctx)
		if err != nil {
			s.sendError(clientConn, "no available backend connections")
			return fmt.Errorf("acquire writer for ext fallback: %w", err)
		}
		if err := s.forwardExtBatch(wConn, buf, syncMsg); err != nil {
			s.writerPool.Discard(wConn)
			return fmt.Errorf("forward ext to writer: %w", err)
		}
		if err := s.relayUntilReady(clientConn, wConn); err != nil {
			s.writerPool.Discard(wConn)
			return err
		}
		s.resetAndReleaseWriter(wConn)
		return nil
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
	if err := s.forwardExtBatch(rConn, buf, syncMsg); err != nil {
		slog.Error("forward ext to reader", "addr", readerAddr, "error", err)
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
		// Cache the response keyed by the batch (first Parse query), skip if oversize
		if collected != nil && len(buf) > 0 && buf[0].Type == protocol.MsgParse {
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

// forwardExtBatch sends a batch of Extended Query messages followed by a Sync message.
func (s *Server) forwardExtBatch(backendConn net.Conn, buf []*protocol.Message, syncMsg *protocol.Message) error {
	for _, m := range buf {
		if err := protocol.WriteMessage(backendConn, m.Type, m.Payload); err != nil {
			return fmt.Errorf("forward ext message: %w", err)
		}
	}
	if err := protocol.WriteMessage(backendConn, syncMsg.Type, syncMsg.Payload); err != nil {
		return fmt.Errorf("forward sync: %w", err)
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

// relayAndCollect relays backend responses to client and collects bytes for caching.
// If the collected size exceeds maxResultSize, collection is abandoned (returns nil)
// but relay to client continues until ReadyForQuery.
func (s *Server) relayAndCollect(clientConn, backendConn net.Conn) ([]byte, error) {
	maxSize := parseSize(s.cfg.Cache.MaxResultSize)
	var buf []byte
	oversize := false

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

		// Forward to client (always, regardless of cache)
		if _, err := clientConn.Write(msgBytes); err != nil {
			return nil, fmt.Errorf("forward to client: %w", err)
		}

		// Collect for cache only if within size limit
		if !oversize {
			buf = append(buf, msgBytes...)
			if maxSize > 0 && len(buf) > maxSize {
				slog.Debug("relay collect: result exceeds max_result_size, discarding buffer",
					"size", len(buf), "max", maxSize)
				buf = nil // release memory immediately
				oversize = true
			}
		}

		if msg.Type == protocol.MsgReadyForQuery {
			if oversize {
				return nil, nil
			}
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

// Cache returns the server's query cache (may be nil if disabled).
func (s *Server) Cache() *cache.Cache {
	return s.queryCache
}

// ReaderPools returns the server's reader connection pools.
func (s *Server) ReaderPools() map[string]*pool.Pool {
	return s.readerPools
}

// WriterPool returns the server's writer connection pool.
func (s *Server) WriterPool() *pool.Pool {
	return s.writerPool
}

// Invalidator returns the server's cache invalidator (may be nil).
func (s *Server) Invalidator() *cache.Invalidator {
	return s.invalidator
}

func (s *Server) closePools() {
	if s.writerPool != nil {
		s.writerPool.Close()
		slog.Debug("writer pool closed", "addr", s.writerAddr)
	}
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
