package proxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jyukki97/pgmux/internal/audit"
	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/metrics"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/resilience"
	"github.com/jyukki97/pgmux/internal/router"
)

type Server struct {
	mu           sync.RWMutex // protects cfg, readerPools, readerCBs, rateLimiter
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
	cancelMap    sync.Map         // cancelKeyPair → *cancelTarget
	nextProxyPID atomic.Uint32
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

	if len(readerAddrs) == 0 {
		slog.Info("no readers configured, all queries routed to writer")
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
				timeout := s.getConfig().Proxy.ShutdownTimeout
				done := make(chan struct{})
				go func() {
					s.wg.Wait()
					close(done)
				}()
				select {
				case <-done:
					slog.Info("all client connections drained")
				case <-time.After(timeout):
					slog.Warn("graceful shutdown timed out, forcing exit",
						"timeout", timeout)
				}
				s.closePools()
				if s.invalidator != nil {
					s.invalidator.Close()
				}
				if s.auditLogger != nil {
					s.auditLogger.Close()
				}
				slog.Info("proxy shut down")
				return nil
			default:
				slog.Error("accept connection", "error", err)
				continue
			}
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in client handler, connection isolated",
						"remote", conn.RemoteAddr(),
						"panic", r,
					)
				}
			}()
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

	// Handle CancelRequest (new TCP connection for query cancellation)
	if len(startup.Payload) >= 4 {
		code := binary.BigEndian.Uint32(startup.Payload[0:4])
		if code == protocol.CancelRequestCode {
			s.handleCancelRequest(startup.Payload)
			return
		}
	}

	_, _, params := protocol.ParseStartupParams(startup.Payload)
	slog.Info("client startup", "user", params["user"], "database", params["database"])

	// 3. Generate proxy cancel key for this session
	ct := s.newCancelTarget()
	defer s.removeCancelTarget(ct)

	// 4. Authenticate client
	cfg := s.getConfig()
	if cfg.Auth.Enabled {
		// Front-end auth: proxy authenticates the client directly using MD5.
		if err := s.frontendAuth(clientConn, params["user"], ct.proxyPID, ct.proxySecret); err != nil {
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

		if err := s.relayAuth(clientConn, authConn, ct.proxyPID, ct.proxySecret); err != nil {
			authConn.Close()
			slog.Error("auth relay", "error", err)
			return
		}
		authConn.Close()
	}

	slog.Info("handshake complete", "remote", rawConn.RemoteAddr())

	// 5. Create per-client session router
	session := router.NewSession(cfg.Routing.ReadAfterWriteDelay, cfg.Routing.CausalConsistency, cfg.Routing.ASTParser)

	// 6. Relay queries with transaction-level pooling
	s.relayQueries(ctx, clientConn, session, ct)
}

// Reload applies a new configuration without restarting the proxy.
// Reloadable: reader list (add/remove), rate limit settings.
// NOT reloadable: proxy.listen, writer address, pool sizes (existing pools), cache TTL.
func (s *Server) Reload(newCfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

func (s *Server) closePools() {
	if s.writerPool != nil {
		s.writerPool.Close()
		slog.Debug("writer pool closed", "addr", s.writerAddr)
	}
	for addr, p := range s.getReaderPools() {
		p.Close()
		slog.Debug("reader pool closed", "addr", addr)
	}
}

// getConfig returns the current config snapshot (thread-safe).
func (s *Server) getConfig() *config.Config {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return cfg
}

// getReaderPool returns the pool for the given reader address (thread-safe).
func (s *Server) getReaderPool(addr string) (*pool.Pool, bool) {
	s.mu.RLock()
	p, ok := s.readerPools[addr]
	s.mu.RUnlock()
	return p, ok
}

// getReaderPools returns the current reader pools map snapshot (thread-safe).
func (s *Server) getReaderPools() map[string]*pool.Pool {
	s.mu.RLock()
	pools := s.readerPools
	s.mu.RUnlock()
	return pools
}

// getRateLimiter returns the current rate limiter (thread-safe).
func (s *Server) getRateLimiter() *resilience.RateLimiter {
	s.mu.RLock()
	rl := s.rateLimiter
	s.mu.RUnlock()
	return rl
}

// getReaderCBs returns the circuit breaker for the given reader address (thread-safe).
func (s *Server) getReaderCB(addr string) (*resilience.CircuitBreaker, bool) {
	s.mu.RLock()
	cb, ok := s.readerCBs[addr]
	s.mu.RUnlock()
	return cb, ok
}

// Cfg returns the current config (thread-safe).
func (s *Server) Cfg() *config.Config {
	return s.getConfig()
}

// Cache returns the server's query cache (may be nil if disabled).
func (s *Server) Cache() *cache.Cache {
	return s.queryCache
}

// ReaderPools returns the server's reader connection pools (thread-safe).
func (s *Server) ReaderPools() map[string]*pool.Pool {
	return s.getReaderPools()
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

// RateLimiter returns the server's rate limiter (thread-safe, may be nil).
func (s *Server) RateLimiter() *resilience.RateLimiter {
	return s.getRateLimiter()
}
