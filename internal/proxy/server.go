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
	"github.com/jyukki97/pgmux/internal/digest"
	"github.com/jyukki97/pgmux/internal/metrics"
	"github.com/jyukki97/pgmux/internal/mirror"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
	"github.com/jyukki97/pgmux/internal/resilience"
	"github.com/jyukki97/pgmux/internal/router"
)

type Server struct {
	mu           sync.RWMutex // protects dbGroups, defaultDB
	cfgPtr       atomic.Pointer[config.Config]
	rateLimitPtr atomic.Pointer[resilience.RateLimiter]
	listenAddr   string
	dbGroups     map[string]*DatabaseGroup
	defaultDB    string
	queryCache   *cache.Cache
	invalidator  *cache.Invalidator
	metrics      *metrics.Metrics
	listener     net.Listener
	tlsConfig    *tls.Config
	auditLogger  *audit.Logger
	mirror       *mirror.Mirror
	queryDigest  *digest.Digest
	wg           sync.WaitGroup
	cancelMap    sync.Map         // cancelKeyPair → *cancelTarget
	nextProxyPID atomic.Uint32
	connTracker  *ConnTracker    // per-user/per-DB connection limits (nil if disabled)
}

func NewServer(cfg *config.Config) *Server {
	s := &Server{
		listenAddr: cfg.Proxy.Listen,
		dbGroups:   make(map[string]*DatabaseGroup),
		defaultDB:  cfg.DefaultDatabaseName(),
	}
	s.cfgPtr.Store(cfg)

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

	// Create database groups
	for name, dbCfg := range cfg.ResolvedDatabases() {
		dbg := newDatabaseGroup(name, dbCfg, cfg.CircuitBreaker)
		s.dbGroups[name] = dbg
	}

	// Initialize Rate Limiter
	if cfg.RateLimit.Enabled {
		rl := resilience.NewRateLimiter(cfg.RateLimit.Rate, cfg.RateLimit.Burst)
		s.rateLimitPtr.Store(rl)
		slog.Info("rate limiter enabled", "rate", cfg.RateLimit.Rate, "burst", cfg.RateLimit.Burst)
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

	// Initialize Query Mirror
	if cfg.Mirror.Enabled {
		mirrorAddr := fmt.Sprintf("%s:%d", cfg.Mirror.Host, cfg.Mirror.Port)
		mirrorUser := cfg.Mirror.User
		if mirrorUser == "" {
			mirrorUser = cfg.Backend.User
		}
		mirrorPass := cfg.Mirror.Password
		if mirrorPass == "" {
			mirrorPass = cfg.Backend.Password
		}
		mirrorDB := cfg.Mirror.Database
		if mirrorDB == "" {
			mirrorDB = cfg.Backend.Database
		}

		m, err := mirror.New(mirror.Config{
			Addr:       mirrorAddr,
			Mode:       cfg.Mirror.Mode,
			Tables:     cfg.Mirror.Tables,
			Compare:    cfg.Mirror.Compare,
			Workers:    cfg.Mirror.Workers,
			BufferSize: cfg.Mirror.BufferSize,
			DialFunc: func() (net.Conn, error) {
				return pgConnect(mirrorAddr, mirrorUser, mirrorPass, mirrorDB)
			},
		})
		if err != nil {
			slog.Error("create mirror", "addr", mirrorAddr, "error", err)
		} else {
			s.mirror = m
			slog.Info("query mirror enabled",
				"addr", mirrorAddr, "mode", cfg.Mirror.Mode,
				"compare", cfg.Mirror.Compare, "workers", cfg.Mirror.Workers)
		}
	}

	// Initialize Query Digest
	if cfg.Digest.Enabled {
		s.queryDigest = digest.New(digest.Config{
			MaxPatterns:       cfg.Digest.MaxPatterns,
			SamplesPerPattern: cfg.Digest.SamplesPerPattern,
		})
		slog.Info("query digest enabled",
			"max_patterns", cfg.Digest.MaxPatterns,
			"samples_per_pattern", cfg.Digest.SamplesPerPattern)
	}

	// Initialize connection limits tracker
	if cfg.ConnectionLimits.Enabled {
		s.connTracker = NewConnTracker(cfg)
		slog.Info("connection limits enabled",
			"default_per_user", cfg.ConnectionLimits.DefaultMaxConnectionsPerUser,
			"default_per_db", cfg.ConnectionLimits.DefaultMaxConnectionsPerDB)
	}

	slog.Info("server initialized",
		"databases", len(s.dbGroups),
		"default_db", s.defaultDB,
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

	// Start health checks for all database groups
	for name, dbg := range s.dbGroups {
		if dbg.writerPool != nil {
			dbg.writerPool.StartHealthCheck(ctx, s.cfgPtr.Load().Pool.IdleTimeout/2)
			slog.Debug("writer health check started", "db", name, "addr", dbg.writerAddr)
		}
		for addr, p := range dbg.ReaderPools() {
			p.StartHealthCheck(ctx, s.cfgPtr.Load().Pool.IdleTimeout/2)
			slog.Debug("reader health check started", "db", name, "addr", addr)
		}
		dbg.balancer.StartHealthCheck(ctx, s.cfgPtr.Load().Pool.ConnectionTimeout)
	}

	// Start cache invalidation subscriber
	if s.invalidator != nil {
		go s.invalidator.Subscribe(ctx)
	}

	// Start LSN polling for causal consistency
	if s.cfgPtr.Load().Routing.CausalConsistency {
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
				if s.mirror != nil {
					s.mirror.Close()
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

	// 3. Resolve database group
	dbName := params["database"]
	if dbName == "" {
		dbName = s.defaultDB
	}
	dbg := s.resolveDBGroup(dbName)
	if dbg == nil {
		s.sendError(clientConn, fmt.Sprintf("unknown database %q", dbName))
		return
	}

	// 4. Check per-user and per-database connection limits
	username := params["user"]
	if s.connTracker != nil {
		ok, reason := s.connTracker.TryAcquire(username, dbName)
		if !ok {
			if s.metrics != nil {
				s.metrics.ConnLimitRejected.WithLabelValues(username, dbName).Inc()
			}
			slog.Warn("connection limit exceeded",
				"user", username, "database", dbName, "reason", reason)
			s.sendFatalWithCode(clientConn, "53300", reason)
			return
		}
		if s.metrics != nil {
			s.metrics.ActiveConnsByUser.WithLabelValues(username).Inc()
			s.metrics.ActiveConnsByDB.WithLabelValues(dbName).Inc()
		}
		defer func() {
			s.connTracker.Release(username, dbName)
			if s.metrics != nil {
				s.metrics.ActiveConnsByUser.WithLabelValues(username).Dec()
				s.metrics.ActiveConnsByDB.WithLabelValues(dbName).Dec()
			}
		}()
	}

	// 5. Generate proxy cancel key for this session
	ct := s.newCancelTarget()
	defer s.removeCancelTarget(ct)

	// 6. Authenticate client
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
		authConn, err := net.Dial("tcp", dbg.writerAddr)
		if err != nil {
			slog.Error("connect to writer for auth", "addr", dbg.writerAddr, "error", err)
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

	// 7. Create per-client session router
	session := router.NewSession(cfg.Routing.ReadAfterWriteDelay, cfg.Routing.CausalConsistency, cfg.Routing.ASTParser)

	// 8. Relay queries with transaction-level pooling
	s.relayQueries(ctx, clientConn, session, ct, dbg)
}

// Reload applies a new configuration without restarting the proxy.
// Reloadable: reader list (add/remove), rate limit settings, database groups.
// NOT reloadable: proxy.listen, writer address (per-group), pool sizes (existing pools), cache TTL.
func (s *Server) Reload(newCfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newDBs := newCfg.ResolvedDatabases()

	// Update existing groups and add new ones
	for name, dbCfg := range newDBs {
		if existing, ok := s.dbGroups[name]; ok {
			existing.Reload(dbCfg, newCfg.CircuitBreaker)
			slog.Info("reload: database group updated", "db", name)
		} else {
			dbg := newDatabaseGroup(name, dbCfg, newCfg.CircuitBreaker)
			s.dbGroups[name] = dbg
			slog.Info("reload: database group added", "db", name)
		}
	}

	// Close removed groups
	for name, dbg := range s.dbGroups {
		if _, ok := newDBs[name]; !ok {
			dbg.Close()
			delete(s.dbGroups, name)
			slog.Info("reload: database group removed", "db", name)
		}
	}

	// Update rate limiter (atomic — no lock needed for readers)
	if newCfg.RateLimit.Enabled {
		rl := resilience.NewRateLimiter(newCfg.RateLimit.Rate, newCfg.RateLimit.Burst)
		s.rateLimitPtr.Store(rl)
	} else {
		s.rateLimitPtr.Store(nil)
	}

	// Update connection limits
	if newCfg.ConnectionLimits.Enabled {
		if s.connTracker != nil {
			s.connTracker.UpdateLimits(newCfg)
		} else {
			s.connTracker = NewConnTracker(newCfg)
		}
	} else {
		s.connTracker = nil
	}

	// Update default DB
	s.defaultDB = newCfg.DefaultDatabaseName()

	// Update config reference (atomic — no lock needed for readers)
	s.cfgPtr.Store(newCfg)

	slog.Info("config reloaded",
		"databases", len(newDBs),
		"rate_limit", newCfg.RateLimit.Enabled,
		"connection_limits", newCfg.ConnectionLimits.Enabled)

	return nil
}

func (s *Server) closePools() {
	s.mu.RLock()
	groups := s.dbGroups
	s.mu.RUnlock()
	for name, dbg := range groups {
		dbg.Close()
		slog.Debug("database group closed", "db", name)
	}
}

// resolveDBGroup returns the DatabaseGroup for the given name (thread-safe).
func (s *Server) resolveDBGroup(name string) *DatabaseGroup {
	s.mu.RLock()
	dbg := s.dbGroups[name]
	s.mu.RUnlock()
	return dbg
}

// getConfig returns the current config snapshot (lock-free via atomic.Pointer).
func (s *Server) getConfig() *config.Config {
	return s.cfgPtr.Load()
}

// getRateLimiter returns the current rate limiter (lock-free via atomic.Pointer).
func (s *Server) getRateLimiter() *resilience.RateLimiter {
	return s.rateLimitPtr.Load()
}

// getDBGroups returns the current database groups map snapshot (thread-safe).
func (s *Server) getDBGroups() map[string]*DatabaseGroup {
	s.mu.RLock()
	groups := s.dbGroups
	s.mu.RUnlock()
	return groups
}

// --- Public getters ---

// Cfg returns the current config (thread-safe).
func (s *Server) Cfg() *config.Config {
	return s.getConfig()
}

// Cache returns the server's query cache (may be nil if disabled).
func (s *Server) Cache() *cache.Cache {
	return s.queryCache
}

// Invalidator returns the server's cache invalidator (may be nil).
func (s *Server) Invalidator() *cache.Invalidator {
	return s.invalidator
}

// ProxyMetrics returns the server's Prometheus metrics (may be nil).
func (s *Server) ProxyMetrics() *metrics.Metrics {
	return s.metrics
}

// RateLimiter returns the server's rate limiter (thread-safe, may be nil).
func (s *Server) RateLimiter() *resilience.RateLimiter {
	return s.getRateLimiter()
}

// DBGroup returns a specific database group by name (thread-safe).
func (s *Server) DBGroup(name string) *DatabaseGroup {
	return s.resolveDBGroup(name)
}

// DBGroups returns all database groups (thread-safe).
func (s *Server) DBGroups() map[string]*DatabaseGroup {
	return s.getDBGroups()
}

// DefaultDBName returns the default database name.
func (s *Server) DefaultDBName() string {
	s.mu.RLock()
	name := s.defaultDB
	s.mu.RUnlock()
	return name
}

// ConnTracker returns the connection limit tracker (may be nil if disabled).
func (s *Server) ConnTracker() *ConnTracker {
	return s.connTracker
}

// --- Backward-compatible getters (delegate to default DB group) ---

// WriterPool returns the default DB group's writer connection pool.
func (s *Server) WriterPool() *pool.Pool {
	dbg := s.resolveDBGroup(s.DefaultDBName())
	if dbg == nil {
		return nil
	}
	return dbg.writerPool
}

// ReaderPools returns the default DB group's reader connection pools (thread-safe).
func (s *Server) ReaderPools() map[string]*pool.Pool {
	dbg := s.resolveDBGroup(s.DefaultDBName())
	if dbg == nil {
		return nil
	}
	return dbg.ReaderPools()
}

// Balancer returns the default DB group's reader load balancer.
func (s *Server) Balancer() *router.RoundRobin {
	dbg := s.resolveDBGroup(s.DefaultDBName())
	if dbg == nil {
		return nil
	}
	return dbg.balancer
}
