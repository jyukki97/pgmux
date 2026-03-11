package pool

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

type Conn struct {
	net.Conn
	CreatedAt  time.Time
	LastUsedAt time.Time
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

type Config struct {
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
}

func New(cfg Config) (*Pool, error) {
	p := &Pool{
		cfg:    cfg,
		idle:   make([]*Conn, 0, cfg.MaxConnections),
		waitCh: make(chan struct{}, cfg.MaxConnections),
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

	p.closed = true
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

func (p *Pool) newConn() (*Conn, error) {
	netConn, err := net.DialTimeout("tcp", p.cfg.Addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", p.cfg.Addr, err)
	}
	now := time.Now()
	return &Conn{
		Conn:       netConn,
		CreatedAt:  now,
		LastUsedAt: now,
	}, nil
}

// Ping checks if a connection is still alive.
func Ping(conn *Conn) error {
	conn.Conn.SetDeadline(time.Now().Add(time.Second))
	defer conn.Conn.SetDeadline(time.Time{})

	// Write a single null byte and see if connection errors
	one := make([]byte, 0)
	_, err := conn.Conn.Read(one)
	if nErr, ok := err.(net.Error); ok && nErr.Timeout() {
		return nil // timeout is expected — connection is alive
	}
	return err
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
			case <-ticker.C:
				p.healthCheck()
			}
		}
	}()
}

func (p *Pool) healthCheck() {
	p.mu.Lock()
	defer p.mu.Unlock()

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
