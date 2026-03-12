package mirror

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pg_query "github.com/pganalyze/pg_query_go/v5"

	"github.com/jyukki97/pgmux/internal/pool"
	"github.com/jyukki97/pgmux/internal/protocol"
)

// Config configures the query mirror.
type Config struct {
	Addr       string
	Mode       string   // "read_only" (default) | "all"
	Tables     []string // empty = all tables
	Compare    bool
	Workers    int
	BufferSize int
	DialFunc   pool.DialFunc
}

type job struct {
	msgType    byte
	payload    []byte
	query      string
	primaryDur time.Duration
}

// Mirror asynchronously forwards queries to a shadow database
// and collects per-pattern latency comparison statistics.
type Mirror struct {
	cfg     Config
	pool    *pool.Pool
	workCh  chan *job
	stats   *statsCollector
	done    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64
	sent    atomic.Int64
	errors  atomic.Int64
	tables  map[string]bool // nil means all tables
}

// New creates a new Mirror with a connection pool and worker goroutines.
func New(cfg Config) (*Mirror, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 10000
	}
	if cfg.Mode == "" {
		cfg.Mode = "read_only"
	}

	p, err := pool.New(pool.Config{
		DialFunc:          cfg.DialFunc,
		Addr:              cfg.Addr,
		MinConnections:    0,
		MaxConnections:    cfg.Workers * 2,
		IdleTimeout:       10 * time.Minute,
		MaxLifetime:       time.Hour,
		ConnectionTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	var tableMap map[string]bool
	if len(cfg.Tables) > 0 {
		tableMap = make(map[string]bool, len(cfg.Tables))
		for _, t := range cfg.Tables {
			tableMap[t] = true
		}
	}

	m := &Mirror{
		cfg:    cfg,
		pool:   p,
		workCh: make(chan *job, cfg.BufferSize),
		stats:  newStatsCollector(),
		done:   make(chan struct{}),
		tables: tableMap,
	}

	for i := 0; i < cfg.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}

	return m, nil
}

// MatchesTables returns true if any of the given tables pass the filter.
// Returns true if no filter is set (all tables match).
func (m *Mirror) MatchesTables(tables []string) bool {
	if m.tables == nil {
		return true
	}
	for _, t := range tables {
		if m.tables[t] {
			return true
		}
	}
	return false
}

// IsReadOnly returns true if the mirror only accepts read queries.
func (m *Mirror) IsReadOnly() bool {
	return m.cfg.Mode == "read_only"
}

// Send enqueues a query for asynchronous mirror execution.
// Drops the job silently if the buffer is full (no production impact).
func (m *Mirror) Send(msgType byte, payload []byte, query string, primaryDur time.Duration) {
	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)

	j := &job{
		msgType:    msgType,
		payload:    payloadCopy,
		query:      query,
		primaryDur: primaryDur,
	}

	select {
	case m.workCh <- j:
	default:
		m.dropped.Add(1)
	}
}

func (m *Mirror) worker() {
	defer m.wg.Done()
	ctx := context.Background()

	for {
		select {
		case <-m.done:
			return
		case j := <-m.workCh:
			m.execute(ctx, j)
		}
	}
}

func (m *Mirror) execute(ctx context.Context, j *job) {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		slog.Debug("mirror: acquire failed", "error", err)
		m.errors.Add(1)
		return
	}

	start := time.Now()
	if err := protocol.WriteMessage(conn, j.msgType, j.payload); err != nil {
		m.pool.Discard(conn)
		m.errors.Add(1)
		return
	}

	// Read and discard response until ReadyForQuery
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			m.pool.Discard(conn)
			m.errors.Add(1)
			return
		}
		if msg.Type == protocol.MsgReadyForQuery {
			break
		}
	}
	mirrorDur := time.Since(start)
	m.pool.Release(conn)
	m.sent.Add(1)

	if m.cfg.Compare {
		normalized, err := pg_query.Normalize(j.query)
		if err != nil {
			normalized = j.query
		}
		m.stats.record(normalized, j.primaryDur, mirrorDur)
	}
}

// Stats returns per-pattern latency comparison, sorted by count descending.
func (m *Mirror) Stats() []QueryStats {
	return m.stats.snapshot()
}

// Dropped returns the number of jobs dropped due to a full buffer.
func (m *Mirror) Dropped() int64 {
	return m.dropped.Load()
}

// Sent returns the number of successfully mirrored queries.
func (m *Mirror) Sent() int64 {
	return m.sent.Load()
}

// Errors returns the number of mirror execution errors.
func (m *Mirror) Errors() int64 {
	return m.errors.Load()
}

// Close stops all workers and closes the mirror connection pool.
func (m *Mirror) Close() {
	close(m.done)
	m.wg.Wait()
	m.pool.Close()
}
