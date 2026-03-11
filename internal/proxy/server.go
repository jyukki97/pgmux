package proxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jyukki97/db-proxy/internal/audit"
	"github.com/jyukki97/db-proxy/internal/cache"
	"github.com/jyukki97/db-proxy/internal/config"
	"github.com/jyukki97/db-proxy/internal/metrics"
	"github.com/jyukki97/db-proxy/internal/pool"
	"github.com/jyukki97/db-proxy/internal/protocol"
	"github.com/jyukki97/db-proxy/internal/resilience"
	"github.com/jyukki97/db-proxy/internal/router"
)

type Server struct {
	cfg          *config.Config
	listenAddr   string
	writerAddr   string
	writerPool   *pool.Pool
	readerPools  map[string]*pool.Pool
	balancer     *router.RoundRobin
	queryCache   *cache.Cache
	invalidator  *cache.Invalidator
	metrics      *metrics.Metrics
	listener     net.Listener
	tlsConfig    *tls.Config
	writerCB     *resilience.CircuitBreaker
	readerCBs    map[string]*resilience.CircuitBreaker
	rateLimiter  *resilience.RateLimiter
	auditLogger  *audit.Logger
	wg           sync.WaitGroup
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

	// Initialize Rate Limiter
	if cfg.RateLimit.Enabled {
		s.rateLimiter = resilience.NewRateLimiter(cfg.RateLimit.Rate, cfg.RateLimit.Burst)
		slog.Info("rate limiter enabled", "rate", cfg.RateLimit.Rate, "burst", cfg.RateLimit.Burst)
	}

	// Initialize Circuit Breakers
	if cfg.CircuitBreaker.Enabled {
		cbCfg := resilience.BreakerConfig{
			ErrorThreshold: cfg.CircuitBreaker.ErrorThreshold,
			OpenDuration:   cfg.CircuitBreaker.OpenDuration,
			HalfOpenMax:    cfg.CircuitBreaker.HalfOpenMax,
			WindowSize:     cfg.CircuitBreaker.WindowSize,
		}
		s.writerCB = resilience.NewCircuitBreaker(cbCfg)
		s.readerCBs = make(map[string]*resilience.CircuitBreaker)
		for _, addr := range readerAddrs {
			s.readerCBs[addr] = resilience.NewCircuitBreaker(cbCfg)
		}
		slog.Info("circuit breakers enabled",
			"threshold", cbCfg.ErrorThreshold,
			"open_duration", cbCfg.OpenDuration)
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

	// Initialize Audit Logger
	if cfg.Audit.Enabled {
		s.auditLogger = audit.New(audit.Config{
			Enabled:            true,
			SlowQueryThreshold: cfg.Audit.SlowQueryThreshold,
			LogAllQueries:      cfg.Audit.LogAllQueries,
			Webhook: audit.WebhookConfig{
				Enabled: cfg.Audit.Webhook.Enabled,
				URL:     cfg.Audit.Webhook.URL,
				Timeout: cfg.Audit.Webhook.Timeout,
			},
		})
		slog.Info("audit logging enabled",
			"slow_threshold", cfg.Audit.SlowQueryThreshold,
			"log_all", cfg.Audit.LogAllQueries,
			"webhook", cfg.Audit.Webhook.Enabled)
	}

	slog.Info("server initialized",
		"writer", writerAddr,
		"readers", len(readerAddrs),
		"cache", cfg.Cache.Enabled,
		"tls", cfg.TLS.Enabled,
		"audit", cfg.Audit.Enabled,
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

	// Start LSN polling for causal consistency
	if s.cfg.Routing.CausalConsistency {
		s.startLSNPolling(ctx, time.Second)
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
				if s.auditLogger != nil {
					s.auditLogger.Close()
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

	// 3. Authenticate client
	if s.cfg.Auth.Enabled {
		// Front-end auth: proxy authenticates the client directly using MD5.
		if err := s.frontendAuth(clientConn, params["user"]); err != nil {
			slog.Warn("frontend auth failed", "user", params["user"], "remote", rawConn.RemoteAddr(), "error", err)
			return
		}
		slog.Info("frontend auth success", "user", params["user"], "remote", rawConn.RemoteAddr())
	} else {
		// Backend auth relay: temporary connection to relay the client's auth handshake.
		authConn, err := net.Dial("tcp", s.writerAddr)
		if err != nil {
			slog.Error("connect to writer for auth", "addr", s.writerAddr, "error", err)
			s.sendError(clientConn, "cannot connect to backend database")
			return
		}

		startupRaw := make([]byte, 4+len(startup.Payload))
		binary.BigEndian.PutUint32(startupRaw[0:4], uint32(4+len(startup.Payload)))
		copy(startupRaw[4:], startup.Payload)
		if err := protocol.WriteRaw(authConn, startupRaw); err != nil {
			authConn.Close()
			slog.Error("forward startup to writer", "error", err)
			return
		}

		if err := s.relayAuth(clientConn, authConn); err != nil {
			authConn.Close()
			slog.Error("auth relay", "error", err)
			return
		}
		authConn.Close()
	}

	slog.Info("handshake complete", "remote", rawConn.RemoteAddr())

	// 4. Create per-client session router
	session := router.NewSession(s.cfg.Routing.ReadAfterWriteDelay, s.cfg.Routing.CausalConsistency)

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

// frontendAuth authenticates the client directly at the proxy using MD5 auth.
// If the user is not in the configured auth.users list, returns an error.
func (s *Server) frontendAuth(clientConn net.Conn, username string) error {
	// Look up user in config
	var password string
	found := false
	for _, u := range s.cfg.Auth.Users {
		if u.Username == username {
			password = u.Password
			found = true
			break
		}
	}

	if !found {
		s.sendError(clientConn, fmt.Sprintf("user \"%s\" is not allowed to connect", username))
		return fmt.Errorf("user %q not in auth.users", username)
	}

	// Send MD5 auth challenge (AuthenticationMD5Password, type=5)
	salt := make([]byte, 4)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	authPayload := make([]byte, 8)
	binary.BigEndian.PutUint32(authPayload[0:4], 5) // MD5Password
	copy(authPayload[4:8], salt)
	if err := protocol.WriteMessage(clientConn, protocol.MsgAuthentication, authPayload); err != nil {
		return fmt.Errorf("send MD5 challenge: %w", err)
	}

	// Read client's password response ('p')
	msg, err := protocol.ReadMessage(clientConn)
	if err != nil {
		return fmt.Errorf("read password response: %w", err)
	}
	if msg.Type != 'p' {
		return fmt.Errorf("expected password message, got %c", msg.Type)
	}

	// Client sends: "md5" + md5(md5(password + user) + salt) + \0
	clientHash := strings.TrimRight(string(msg.Payload), "\x00")
	expectedHash := pgMD5Password(username, password, salt)

	if clientHash != expectedHash {
		s.sendError(clientConn, "password authentication failed for user \""+username+"\"")
		return fmt.Errorf("MD5 password mismatch for user %q", username)
	}

	// Send AuthenticationOk (type=0)
	okPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(okPayload[0:4], 0)
	if err := protocol.WriteMessage(clientConn, protocol.MsgAuthentication, okPayload); err != nil {
		return fmt.Errorf("send auth ok: %w", err)
	}

	// Send ReadyForQuery ('Z', status='I' for idle)
	if err := protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'}); err != nil {
		return fmt.Errorf("send ready for query: %w", err)
	}

	return nil
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

		// Rate limit check
		if s.rateLimiter != nil && !s.rateLimiter.Allow() {
			slog.Warn("rate limited", "remote", clientConn.RemoteAddr())
			if s.metrics != nil {
				s.metrics.RateLimited.Inc()
			}
			s.sendError(clientConn, "too many requests")
			// Send ReadyForQuery so the client can continue
			protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'})
			continue
		}

		// --- Simple Query Protocol ---
		if msg.Type == protocol.MsgQuery {
			query := protocol.ExtractQueryText(msg.Payload)

			// Firewall check
			if s.cfg.Firewall.Enabled {
				fwResult := router.CheckFirewall(query, router.FirewallConfig{
					Enabled:                 s.cfg.Firewall.Enabled,
					BlockDeleteWithoutWhere: s.cfg.Firewall.BlockDeleteWithoutWhere,
					BlockUpdateWithoutWhere: s.cfg.Firewall.BlockUpdateWithoutWhere,
					BlockDropTable:          s.cfg.Firewall.BlockDropTable,
					BlockTruncate:           s.cfg.Firewall.BlockTruncate,
				})
				if fwResult.Blocked {
					slog.Warn("firewall blocked query", "rule", fwResult.Rule, "sql", query)
					if s.metrics != nil {
						s.metrics.FirewallBlocked.WithLabelValues(string(fwResult.Rule)).Inc()
					}
					s.sendError(clientConn, fwResult.Message)
					protocol.WriteMessage(clientConn, protocol.MsgReadyForQuery, []byte{'I'})
					continue
				}
			}

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

				s.handleWriteQuery(clientConn, wConn, msg, query, session)

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
				if err := s.handleReadQuery(ctx, clientConn, msg, query, session); err != nil {
					slog.Error("handle read query", "error", err)
					return
				}
			}

			elapsed := time.Since(start)
			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(elapsed.Seconds())
			}
			s.emitAuditEvent(clientConn, query, target, elapsed, false)
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

			elapsed := time.Since(start)
			if s.metrics != nil {
				s.metrics.QueriesRouted.WithLabelValues(target).Inc()
				s.metrics.QueryDuration.WithLabelValues(target).Observe(elapsed.Seconds())
			}
			s.emitAuditEvent(clientConn, "(extended query)", target, elapsed, false)

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
	// Circuit breaker check
	if s.writerCB != nil {
		if err := s.writerCB.Allow(); err != nil {
			return nil, false, fmt.Errorf("writer circuit breaker open: %w", err)
		}
	}
	acquireStart := time.Now()
	conn, err := s.writerPool.Acquire(ctx)
	if err != nil {
		if s.writerCB != nil {
			s.writerCB.RecordFailure()
		}
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
func (s *Server) handleWriteQuery(clientConn net.Conn, writerConn net.Conn, msg *protocol.Message, query string, session *router.Session) {
	if err := s.forwardAndRelay(clientConn, writerConn, msg); err != nil {
		slog.Error("forward write to writer", "error", err)
		if s.writerCB != nil {
			s.writerCB.RecordFailure()
		}
		return
	}
	if s.writerCB != nil {
		s.writerCB.RecordSuccess()
	}

	// Track WAL LSN for causal consistency
	if s.cfg.Routing.CausalConsistency && s.classifyQuery(query) == router.QueryWrite {
		if lsn, err := s.queryCurrentLSN(writerConn); err != nil {
			slog.Warn("query WAL LSN after write", "error", err)
		} else {
			session.SetLastWriteLSN(lsn)
			slog.Debug("write LSN tracked", "lsn", lsn)
		}
	}

	// Invalidate cache for affected tables
	if s.queryCache != nil && s.classifyQuery(query) == router.QueryWrite {
		tables := s.extractQueryTables(query)
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

// queryCurrentLSN queries the current WAL LSN from the writer connection.
func (s *Server) queryCurrentLSN(writerConn net.Conn) (router.LSN, error) {
	payload := append([]byte("SELECT pg_current_wal_lsn()"), 0)
	if err := protocol.WriteMessage(writerConn, protocol.MsgQuery, payload); err != nil {
		return 0, fmt.Errorf("send LSN query: %w", err)
	}

	var lsnStr string
	for {
		msg, err := protocol.ReadMessage(writerConn)
		if err != nil {
			return 0, fmt.Errorf("read LSN response: %w", err)
		}
		if msg.Type == protocol.MsgDataRow && len(msg.Payload) >= 6 {
			// DataRow: Int16(numCols) + Int32(len) + Byte[n](value)
			colLen := int(binary.BigEndian.Uint32(msg.Payload[2:6]))
			if colLen > 0 && 6+colLen <= len(msg.Payload) {
				lsnStr = string(msg.Payload[6 : 6+colLen])
			}
		}
		if msg.Type == protocol.MsgErrorResponse {
			return 0, fmt.Errorf("LSN query returned error")
		}
		if msg.Type == protocol.MsgReadyForQuery {
			break
		}
	}

	if lsnStr == "" {
		return 0, fmt.Errorf("no LSN value returned")
	}
	return router.ParseLSN(lsnStr)
}

// handleReadQuery checks cache, acquires a reader from pool, or falls back to writer.
func (s *Server) handleReadQuery(ctx context.Context, clientConn net.Conn, msg *protocol.Message, query string, session *router.Session) error {
	// Check cache
	if s.queryCache != nil {
		key := s.cacheKey(query)
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
	var readerAddr string
	if s.cfg.Routing.CausalConsistency {
		minLSN := session.LastWriteLSN()
		readerAddr = s.balancer.NextWithLSN(minLSN)
	} else {
		readerAddr = s.balancer.Next()
	}
	if readerAddr == "" {
		slog.Warn("no healthy reader, fallback to writer")
		if s.metrics != nil {
			s.metrics.ReaderFallback.Inc()
		}
		return s.fallbackToWriter(ctx, clientConn, msg)
	}

	// Circuit breaker check for reader
	if cb, ok := s.readerCBs[readerAddr]; ok {
		if err := cb.Allow(); err != nil {
			slog.Warn("reader circuit breaker open, fallback to writer", "addr", readerAddr)
			if s.metrics != nil {
				s.metrics.ReaderFallback.Inc()
			}
			return s.fallbackToWriter(ctx, clientConn, msg)
		}
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
		if cb, ok := s.readerCBs[readerAddr]; ok {
			cb.RecordFailure()
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
			key := s.cacheKey(query)
			s.queryCache.Set(key, collected, nil)
			if s.metrics != nil {
				s.metrics.CacheEntries.Set(float64(s.queryCache.Len()))
			}
			slog.Debug("cache set", "sql", query, "size", len(collected))
		}
	} else {
		if err := s.relayUntilReady(clientConn, rConn); err != nil {
			rPool.Discard(rConn)
			if cb, ok := s.readerCBs[readerAddr]; ok {
				cb.RecordFailure()
			}
			return fmt.Errorf("relay reader response: %w", err)
		}
		rPool.Release(rConn)
	}

	if cb, ok := s.readerCBs[readerAddr]; ok {
		cb.RecordSuccess()
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
			key := s.cacheKey(query)
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

// Balancer returns the server's reader load balancer.
func (s *Server) Balancer() *router.RoundRobin {
	return s.balancer
}

// ProxyMetrics returns the server's Prometheus metrics (may be nil).
func (s *Server) ProxyMetrics() *metrics.Metrics {
	return s.metrics
}

// RateLimiter returns the server's rate limiter (may be nil).
func (s *Server) RateLimiter() *resilience.RateLimiter {
	return s.rateLimiter
}

// startLSNPolling periodically queries each reader's replay LSN and updates the balancer.
func (s *Server) startLSNPolling(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollReaderLSNs(ctx)
			}
		}
	}()
	slog.Info("LSN polling started", "interval", interval)
}

// pollReaderLSNs queries each reader's replay LSN and updates the balancer.
func (s *Server) pollReaderLSNs(ctx context.Context) {
	for _, addr := range s.balancer.Backends() {
		rPool, ok := s.readerPools[addr]
		if !ok {
			continue
		}

		conn, err := rPool.Acquire(ctx)
		if err != nil {
			slog.Debug("LSN poll: acquire reader failed", "addr", addr, "error", err)
			continue
		}

		lsn, err := s.queryReplayLSN(conn)
		rPool.Release(conn)
		if err != nil {
			slog.Debug("LSN poll: query replay LSN failed", "addr", addr, "error", err)
			continue
		}

		s.balancer.SetReplayLSN(addr, lsn)

		if s.metrics != nil {
			s.metrics.ReaderLSNLag.WithLabelValues(addr).Set(float64(lsn))
		}

		slog.Debug("LSN poll updated", "addr", addr, "replay_lsn", lsn)
	}
}

// queryReplayLSN queries the replay LSN from a reader connection.
func (s *Server) queryReplayLSN(readerConn net.Conn) (router.LSN, error) {
	payload := append([]byte("SELECT pg_last_wal_replay_lsn()"), 0)
	if err := protocol.WriteMessage(readerConn, protocol.MsgQuery, payload); err != nil {
		return 0, fmt.Errorf("send replay LSN query: %w", err)
	}

	var lsnStr string
	for {
		msg, err := protocol.ReadMessage(readerConn)
		if err != nil {
			return 0, fmt.Errorf("read replay LSN response: %w", err)
		}
		if msg.Type == protocol.MsgDataRow && len(msg.Payload) >= 6 {
			colLen := int(binary.BigEndian.Uint32(msg.Payload[2:6]))
			if colLen > 0 && 6+colLen <= len(msg.Payload) {
				lsnStr = string(msg.Payload[6 : 6+colLen])
			}
		}
		if msg.Type == protocol.MsgErrorResponse {
			return 0, fmt.Errorf("replay LSN query returned error")
		}
		if msg.Type == protocol.MsgReadyForQuery {
			break
		}
	}

	if lsnStr == "" {
		return 0, fmt.Errorf("no replay LSN value returned")
	}
	return router.ParseLSN(lsnStr)
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

// Reload applies a new configuration without restarting the proxy.
// Reloadable: readers, pool sizes, cache TTL, rate limit settings.
// NOT reloadable: proxy.listen, writer address.
func (s *Server) Reload(newCfg *config.Config) error {
	oldCfg := s.cfg

	// Update readers if changed
	newReaderAddrs := make([]string, len(newCfg.Readers))
	for i, r := range newCfg.Readers {
		newReaderAddrs[i] = fmt.Sprintf("%s:%d", r.Host, r.Port)
	}

	oldReaderAddrs := make(map[string]bool)
	for _, r := range oldCfg.Readers {
		oldReaderAddrs[fmt.Sprintf("%s:%d", r.Host, r.Port)] = true
	}

	// Create new reader pools and close removed ones
	newReaderPools := make(map[string]*pool.Pool)
	for _, addr := range newReaderAddrs {
		if existingPool, ok := s.readerPools[addr]; ok {
			// Keep existing pool
			newReaderPools[addr] = existingPool
		} else {
			// Create new pool for added reader
			addr := addr
			p, err := pool.New(pool.Config{
				DialFunc: func() (net.Conn, error) {
					return pgConnect(addr, newCfg.Backend.User, newCfg.Backend.Password, newCfg.Backend.Database)
				},
				MinConnections:    0,
				MaxConnections:    newCfg.Pool.MaxConnections,
				IdleTimeout:       newCfg.Pool.IdleTimeout,
				MaxLifetime:       newCfg.Pool.MaxLifetime,
				ConnectionTimeout: newCfg.Pool.ConnectionTimeout,
			})
			if err != nil {
				slog.Error("reload: create reader pool", "addr", addr, "error", err)
				continue
			}
			newReaderPools[addr] = p
			slog.Info("reload: reader pool added", "addr", addr)
		}
	}

	// Close removed reader pools
	for addr, p := range s.readerPools {
		found := false
		for _, newAddr := range newReaderAddrs {
			if addr == newAddr {
				found = true
				break
			}
		}
		if !found {
			p.Close()
			slog.Info("reload: reader pool removed", "addr", addr)
		}
	}

	s.readerPools = newReaderPools
	s.balancer.UpdateBackends(newReaderAddrs)

	// Update rate limiter
	if newCfg.RateLimit.Enabled {
		s.rateLimiter = resilience.NewRateLimiter(newCfg.RateLimit.Rate, newCfg.RateLimit.Burst)
	} else {
		s.rateLimiter = nil
	}

	// Update config reference
	s.cfg = newCfg

	slog.Info("config reloaded",
		"readers", len(newReaderAddrs),
		"rate_limit", newCfg.RateLimit.Enabled)

	return nil
}

// CfgPath returns the config file path (stored externally by main).
func (s *Server) Cfg() *config.Config {
	return s.cfg
}

// cacheKey uses semantic or plain cache key based on config.
func (s *Server) cacheKey(query string) uint64 {
	if s.cfg.Routing.ASTParser {
		return cache.SemanticCacheKey(query)
	}
	return cache.CacheKey(query)
}

// classifyQuery uses AST or string parser based on config.
func (s *Server) classifyQuery(query string) router.QueryType {
	if s.cfg.Routing.ASTParser {
		return router.ClassifyAST(query)
	}
	return router.Classify(query)
}

// extractQueryTables uses AST or string parser based on config.
func (s *Server) extractQueryTables(query string) []string {
	if s.cfg.Routing.ASTParser {
		return router.ExtractTablesAST(query)
	}
	return router.ExtractTables(query)
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

// emitAuditEvent sends a query audit event to the audit logger if enabled.
func (s *Server) emitAuditEvent(clientConn net.Conn, query, target string, elapsed time.Duration, cached bool) {
	if s.auditLogger == nil {
		return
	}

	durationMS := float64(elapsed.Microseconds()) / 1000.0

	// Record slow query metric
	if s.metrics != nil && durationMS >= float64(s.cfg.Audit.SlowQueryThreshold.Milliseconds()) {
		s.metrics.SlowQueries.WithLabelValues(target).Inc()
	}

	sourceIP := ""
	if addr := clientConn.RemoteAddr(); addr != nil {
		sourceIP = addr.String()
	}

	s.auditLogger.Log(audit.Event{
		Timestamp:  time.Now(),
		User:       s.cfg.Backend.User,
		SourceIP:   sourceIP,
		Query:      query,
		DurationMS: durationMS,
		Target:     target,
		Cached:     cached,
	})
}

// AuditLogger returns the audit logger for external access (e.g., admin API).
func (s *Server) AuditLogger() *audit.Logger {
	return s.auditLogger
}
