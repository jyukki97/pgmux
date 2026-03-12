package pool

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// BackendKeyHolder is implemented by connections that carry PostgreSQL BackendKeyData.
type BackendKeyHolder interface {
	BackendKey() (pid, secret uint32)
}

type Conn struct {
	net.Conn
	CreatedAt     time.Time
	LastUsedAt    time.Time
	BackendPID    uint32
	BackendSecret uint32
}

func (c *Conn) expired(maxLifetime time.Duration) bool {
	if maxLifetime <= 0 {
		return false
	}
	return time.Since(c.CreatedAt) > maxLifetime
}

func (c *Conn) idle(idleTimeout time.Duration) bool {
	if idleTimeout <= 0 {
		return false
	}
	return time.Since(c.LastUsedAt) > idleTimeout
}

// DialFunc creates a new connection. If nil, raw TCP dial is used.
type DialFunc func() (net.Conn, error)

type Config struct {
	DialFunc          DialFunc
	Addr              string
	MinConnections    int
	MaxConnections    int
	IdleTimeout       time.Duration
	MaxLifetime       time.Duration
	ConnectionTimeout time.Duration
}

type Pool struct {
	cfg     Config
	mu      sync.Mutex
	idle    []*Conn
	numOpen int
	waitCh  chan struct{} // signals that a conn was released
	closed  bool
	done    chan struct{} // closed on Pool.Close() to stop background goroutines
}

func New(cfg Config) (*Pool, error) {
	p := &Pool{
		cfg:    cfg,
		idle:   make([]*Conn, 0, cfg.MaxConnections),
		waitCh: make(chan struct{}, cfg.MaxConnections),
		done:   make(chan struct{}),
	}

	// Pre-create min connections
	for i := 0; i < cfg.MinConnections; i++ {
		conn, err := p.newConn()
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("pre-create connection %d: %w", i, err)
		}
		p.idle = append(p.idle, conn)
		p.numOpen++
	}

	return p, nil
}

func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	p.mu.Lock()

	// Try to get a valid idle connection
	for len(p.idle) > 0 {
		conn := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]

		if conn.expired(p.cfg.MaxLifetime) || conn.idle(p.cfg.IdleTimeout) {
			conn.Close()
			p.numOpen--
			continue
		}

		conn.LastUsedAt = time.Now()
		p.mu.Unlock()
		return conn, nil
	}

	// Can we create a new connection?
	if p.numOpen < p.cfg.MaxConnections {
		p.numOpen++
		p.mu.Unlock()

		conn, err := p.newConn()
		if err != nil {
			p.mu.Lock()
			p.numOpen--
			p.mu.Unlock()
			return nil, err
		}
		return conn, nil
	}

	p.mu.Unlock()

	// Wait for a released connection
	timeout := p.cfg.ConnectionTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-p.waitCh:
		return p.Acquire(ctx)
	case <-timer.C:
		return nil, fmt.Errorf("connection pool: acquire timeout after %v", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Pool) Release(conn *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		conn.Close()
		p.numOpen--
		return
	}

	conn.LastUsedAt = time.Now()
	p.idle = append(p.idle, conn)

	// Signal waiters
	select {
	case p.waitCh <- struct{}{}:
	default:
	}
}

func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	close(p.done) // stop health check goroutine
	for _, conn := range p.idle {
		conn.Close()
	}
	p.idle = nil
	p.numOpen = 0
}

// Stats returns current pool statistics.
func (p *Pool) Stats() (numOpen, numIdle int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.numOpen, len(p.idle)
}

// Discard closes a broken connection and decrements the open count.
// Use this instead of Release when the connection is no longer usable.
func (p *Pool) Discard(conn *Conn) {
	conn.Close()
	p.mu.Lock()
	p.numOpen--
	p.mu.Unlock()

	select {
	case p.waitCh <- struct{}{}:
	default:
	}
}

func (p *Pool) newConn() (*Conn, error) {
	var netConn net.Conn
	var err error
	if p.cfg.DialFunc != nil {
		netConn, err = p.cfg.DialFunc()
	} else {
		netConn, err = net.DialTimeout("tcp", p.cfg.Addr, 5*time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", p.cfg.Addr, err)
	}
	now := time.Now()
	c := &Conn{
		Conn:       netConn,
		CreatedAt:  now,
		LastUsedAt: now,
	}
	if bk, ok := netConn.(BackendKeyHolder); ok {
		c.BackendPID, c.BackendSecret = bk.BackendKey()
	}
	return c, nil
}

// StartHealthCheck runs a periodic health check goroutine.
func (p *Pool) StartHealthCheck(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-p.done:
				return
			case <-ticker.C:
				p.healthCheck()
			}
		}
	}()
}

func (p *Pool) healthCheck() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	alive := p.idle[:0]
	for _, c := range p.idle {
		if c.expired(p.cfg.MaxLifetime) || c.idle(p.cfg.IdleTimeout) {
			c.Close()
			p.numOpen--
			slog.Debug("healthcheck: removed expired connection")
		} else {
			alive = append(alive, c)
		}
	}
	p.idle = alive

	// Replenish to min connections
	for p.numOpen < p.cfg.MinConnections {
		conn, err := p.newConn()
		if err != nil {
			slog.Error("healthcheck: replenish connection", "error", err)
			break
		}
		p.idle = append(p.idle, conn)
		p.numOpen++
	}
}
