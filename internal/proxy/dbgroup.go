package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/resilience"
	"github.com/jyukki97/pgmux/internal/router"
)

// DatabaseGroup holds all per-database resources: connection pools, balancer, and circuit breakers.
type DatabaseGroup struct {
	mu          sync.RWMutex
	name        string
	writerAddr  string
	writerPool  *pool.Pool
	readerPools map[string]*pool.Pool
	balancer    *router.RoundRobin
	writerCB    *resilience.CircuitBreaker
	readerCBs   map[string]*resilience.CircuitBreaker
	backendCfg  config.BackendConfig
}

// newDatabaseGroup creates a DatabaseGroup with writer/reader pools and circuit breakers.
func newDatabaseGroup(name string, dbCfg config.DatabaseConfig, cbCfg config.CircuitBreakerConfig) *DatabaseGroup {
	writerAddr := fmt.Sprintf("%s:%d", dbCfg.Writer.Host, dbCfg.Writer.Port)

	readerAddrs := make([]string, len(dbCfg.Readers))
	for i, r := range dbCfg.Readers {
		readerAddrs[i] = fmt.Sprintf("%s:%d", r.Host, r.Port)
	}

	dbg := &DatabaseGroup{
		name:        name,
		writerAddr:  writerAddr,
		balancer:    router.NewRoundRobin(readerAddrs),
		readerPools: make(map[string]*pool.Pool),
		backendCfg:  dbCfg.Backend,
	}

	// Writer pool
	wp, err := pool.New(pool.Config{
		DialFunc: func() (net.Conn, error) {
			return pgConnect(writerAddr, dbCfg.Backend.User, dbCfg.Backend.Password, dbCfg.Backend.Database)
		},
		MinConnections:    0, // lazy creation; backend may not be ready at startup
		MaxConnections:    dbCfg.Pool.MaxConnections,
		IdleTimeout:       dbCfg.Pool.IdleTimeout,
		MaxLifetime:       dbCfg.Pool.MaxLifetime,
		ConnectionTimeout: dbCfg.Pool.ConnectionTimeout,
	})
	if err != nil {
		slog.Error("create writer pool", "db", name, "addr", writerAddr, "error", err)
	} else {
		dbg.writerPool = wp
		slog.Info("writer pool created", "db", name, "addr", writerAddr, "max_conn", dbCfg.Pool.MaxConnections)
	}

	// Reader pools
	for _, addr := range readerAddrs {
		addr := addr
		p, err := pool.New(pool.Config{
			DialFunc: func() (net.Conn, error) {
				return pgConnect(addr, dbCfg.Backend.User, dbCfg.Backend.Password, dbCfg.Backend.Database)
			},
			MinConnections:    0,
			MaxConnections:    dbCfg.Pool.MaxConnections,
			IdleTimeout:       dbCfg.Pool.IdleTimeout,
			MaxLifetime:       dbCfg.Pool.MaxLifetime,
			ConnectionTimeout: dbCfg.Pool.ConnectionTimeout,
		})
		if err != nil {
			slog.Error("create reader pool", "db", name, "addr", addr, "error", err)
			continue
		}
		dbg.readerPools[addr] = p
		slog.Info("reader pool created", "db", name, "addr", addr, "max_conn", dbCfg.Pool.MaxConnections)
	}

	// Circuit breakers
	if cbCfg.Enabled {
		brCfg := resilience.BreakerConfig{
			ErrorThreshold: cbCfg.ErrorThreshold,
			OpenDuration:   cbCfg.OpenDuration,
			HalfOpenMax:    cbCfg.HalfOpenMax,
			WindowSize:     cbCfg.WindowSize,
		}
		dbg.writerCB = resilience.NewCircuitBreaker(brCfg)
		dbg.readerCBs = make(map[string]*resilience.CircuitBreaker)
		for _, addr := range readerAddrs {
			dbg.readerCBs[addr] = resilience.NewCircuitBreaker(brCfg)
		}
		slog.Info("circuit breakers enabled", "db", name)
	}

	if len(readerAddrs) == 0 {
		slog.Info("no readers configured, all queries routed to writer", "db", name)
	}

	return dbg
}

// Close shuts down all connection pools in this group.
func (g *DatabaseGroup) Close() {
	if g.writerPool != nil {
		g.writerPool.Close()
	}
	g.mu.RLock()
	pools := g.readerPools
	g.mu.RUnlock()
	for _, p := range pools {
		p.Close()
	}
}

// Reload updates writer/reader pools and circuit breakers for a config change.
// If backend credentials (user, password, database) changed, all pools are
// recreated so that new connections use the updated credentials.
func (g *DatabaseGroup) Reload(dbCfg config.DatabaseConfig, cbCfg config.CircuitBreakerConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()

	oldCfg := g.backendCfg
	credsChanged := oldCfg.User != dbCfg.Backend.User ||
		oldCfg.Password != dbCfg.Backend.Password ||
		oldCfg.Database != dbCfg.Backend.Database

	if credsChanged {
		slog.Info("reload: backend credentials changed, recreating pools", "db", g.name)
	}

	// --- Writer pool ---
	newWriterAddr := fmt.Sprintf("%s:%d", dbCfg.Writer.Host, dbCfg.Writer.Port)
	if credsChanged || newWriterAddr != g.writerAddr {
		if g.writerPool != nil {
			g.writerPool.Close()
			slog.Info("reload: writer pool closed (recreating)", "db", g.name, "old_addr", g.writerAddr)
		}
		wp, err := pool.New(pool.Config{
			DialFunc: func() (net.Conn, error) {
				return pgConnect(newWriterAddr, dbCfg.Backend.User, dbCfg.Backend.Password, dbCfg.Backend.Database)
			},
			MinConnections:    0,
			MaxConnections:    dbCfg.Pool.MaxConnections,
			IdleTimeout:       dbCfg.Pool.IdleTimeout,
			MaxLifetime:       dbCfg.Pool.MaxLifetime,
			ConnectionTimeout: dbCfg.Pool.ConnectionTimeout,
		})
		if err != nil {
			slog.Error("reload: create writer pool", "db", g.name, "addr", newWriterAddr, "error", err)
			g.writerPool = nil
		} else {
			g.writerPool = wp
			slog.Info("reload: writer pool recreated", "db", g.name, "addr", newWriterAddr)
		}
		g.writerAddr = newWriterAddr
	}

	// --- Reader pools ---
	newReaderAddrs := make([]string, len(dbCfg.Readers))
	for i, r := range dbCfg.Readers {
		newReaderAddrs[i] = fmt.Sprintf("%s:%d", r.Host, r.Port)
	}

	newPools := make(map[string]*pool.Pool)
	for _, addr := range newReaderAddrs {
		if !credsChanged {
			if p, ok := g.readerPools[addr]; ok {
				newPools[addr] = p
				continue
			}
		}
		addr := addr
		p, err := pool.New(pool.Config{
			DialFunc: func() (net.Conn, error) {
				return pgConnect(addr, dbCfg.Backend.User, dbCfg.Backend.Password, dbCfg.Backend.Database)
			},
			MinConnections:    0,
			MaxConnections:    dbCfg.Pool.MaxConnections,
			IdleTimeout:       dbCfg.Pool.IdleTimeout,
			MaxLifetime:       dbCfg.Pool.MaxLifetime,
			ConnectionTimeout: dbCfg.Pool.ConnectionTimeout,
		})
		if err != nil {
			slog.Error("reload: create reader pool", "db", g.name, "addr", addr, "error", err)
			continue
		}
		newPools[addr] = p
		slog.Info("reload: reader pool added", "db", g.name, "addr", addr)
	}

	for addr, p := range g.readerPools {
		if _, ok := newPools[addr]; !ok {
			p.Close()
			slog.Info("reload: reader pool removed", "db", g.name, "addr", addr)
		}
	}

	g.readerPools = newPools
	g.balancer.UpdateBackends(newReaderAddrs)
	g.backendCfg = dbCfg.Backend

	if cbCfg.Enabled {
		brCfg := resilience.BreakerConfig{
			ErrorThreshold: cbCfg.ErrorThreshold,
			OpenDuration:   cbCfg.OpenDuration,
			HalfOpenMax:    cbCfg.HalfOpenMax,
			WindowSize:     cbCfg.WindowSize,
		}

		// Writer circuit breaker — create if nil (was previously disabled)
		if g.writerCB == nil {
			g.writerCB = resilience.NewCircuitBreaker(brCfg)
			slog.Info("reload: writer circuit breaker enabled", "db", g.name)
		}

		// Reader circuit breakers — preserve existing, create for new addrs
		newCBs := make(map[string]*resilience.CircuitBreaker)
		for _, addr := range newReaderAddrs {
			if cb, ok := g.readerCBs[addr]; ok {
				newCBs[addr] = cb
			} else {
				newCBs[addr] = resilience.NewCircuitBreaker(brCfg)
			}
		}
		g.readerCBs = newCBs
	} else {
		// CB disabled — clear writer and reader circuit breakers
		if g.writerCB != nil {
			g.writerCB = nil
			slog.Info("reload: writer circuit breaker disabled", "db", g.name)
		}
		if g.readerCBs != nil {
			g.readerCBs = nil
			slog.Info("reload: reader circuit breakers disabled", "db", g.name)
		}
	}
}

// --- Exported getters (for admin/dataapi packages) ---

// Name returns the database group name.
func (g *DatabaseGroup) Name() string { return g.name }

// WriterAddr returns the writer backend address.
func (g *DatabaseGroup) WriterAddr() string { return g.writerAddr }

// WriterPool returns the writer connection pool.
func (g *DatabaseGroup) WriterPool() *pool.Pool { return g.writerPool }

// Balancer returns the reader load balancer.
func (g *DatabaseGroup) Balancer() *router.RoundRobin { return g.balancer }

// BackendCfg returns the backend configuration.
func (g *DatabaseGroup) BackendCfg() config.BackendConfig { return g.backendCfg }

// ReaderPools returns all reader pools (thread-safe).
func (g *DatabaseGroup) ReaderPools() map[string]*pool.Pool {
	g.mu.RLock()
	pools := g.readerPools
	g.mu.RUnlock()
	return pools
}

// ReaderPool returns the pool for a specific reader address (thread-safe).
func (g *DatabaseGroup) ReaderPool(addr string) (*pool.Pool, bool) {
	g.mu.RLock()
	p, ok := g.readerPools[addr]
	g.mu.RUnlock()
	return p, ok
}

// ReaderCB returns the circuit breaker for a specific reader (thread-safe).
func (g *DatabaseGroup) ReaderCB(addr string) (*resilience.CircuitBreaker, bool) {
	g.mu.RLock()
	cb, ok := g.readerCBs[addr]
	g.mu.RUnlock()
	return cb, ok
}
