// Package metrics defines Prometheus metrics for monitoring proxy operations.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus metrics for the proxy.
type Metrics struct {
	// Query routing
	QueriesRouted  *prometheus.CounterVec
	QueryDuration  *prometheus.HistogramVec
	ReaderFallback prometheus.Counter

	// Cache
	CacheHits          prometheus.Counter
	CacheMisses        prometheus.Counter
	CacheEntries       prometheus.Gauge
	CacheInvalidations prometheus.Counter

	// Rate limiting
	RateLimited *prometheus.CounterVec

	// Pool
	PoolOpenConns  *prometheus.GaugeVec
	PoolIdleConns  *prometheus.GaugeVec
	PoolAcquires   *prometheus.CounterVec
	PoolAcquireDur *prometheus.HistogramVec

	// LSN replication lag
	ReaderLSNLag *prometheus.GaugeVec

	// Firewall
	FirewallBlocked *prometheus.CounterVec

	// Audit
	SlowQueries   *prometheus.CounterVec
	WebhookSent   prometheus.Counter
	WebhookErrors prometheus.Counter

	// Query Digest
	DigestPatterns prometheus.Gauge

	// Query timeout
	QueryTimeouts *prometheus.CounterVec

	// Client idle timeout
	ClientIdleTimeouts prometheus.Counter

	// Connection limits
	ConnLimitRejected *prometheus.CounterVec
	ActiveConnsByUser *prometheus.GaugeVec
	ActiveConnsByDB   *prometheus.GaugeVec

	// Maintenance mode
	MaintenanceMode         prometheus.Gauge
	MaintenanceRejectedConn prometheus.Counter

	// Read-only mode
	ReadOnlyMode     prometheus.Gauge
	ReadOnlyRejected prometheus.Counter

	// Session compatibility guard
	SessionDepDetected *prometheus.CounterVec
	SessionDepBlocked  *prometheus.CounterVec
	SessionPinned      *prometheus.CounterVec

	// Query rewriting
	QueryRewritten *prometheus.CounterVec

	// Connection warming
	PoolWarmConns  *prometheus.CounterVec
	PoolWarmErrors *prometheus.CounterVec
}

// New creates and registers all Prometheus metrics.
func New() *Metrics {
	m := &Metrics{
		QueriesRouted: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_queries_routed_total",
				Help: "Total number of queries routed by target.",
			},
			[]string{"target"},
		),
		QueryDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "pgmux_query_duration_seconds",
				Help:    "Query processing duration in seconds.",
				Buckets: []float64{.0001, .0005, .001, .005, .01, .05, .1, .5, 1},
			},
			[]string{"target"},
		),
		ReaderFallback: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_reader_fallback_total",
				Help: "Total number of read queries that fell back to writer.",
			},
		),

		CacheHits: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_cache_hits_total",
				Help: "Total number of cache hits.",
			},
		),
		CacheMisses: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_cache_misses_total",
				Help: "Total number of cache misses.",
			},
		),
		CacheEntries: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pgmux_cache_entries",
				Help: "Current number of entries in the cache.",
			},
		),
		CacheInvalidations: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_cache_invalidations_total",
				Help: "Total number of cache invalidations.",
			},
		),

		RateLimited: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_rate_limited_total",
				Help: "Total number of rate-limited requests.",
			},
			[]string{"scope"},
		),

		PoolOpenConns: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "pgmux_pool_connections_open",
				Help: "Number of open connections by role and address.",
			},
			[]string{"role", "addr"},
		),
		PoolIdleConns: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "pgmux_pool_connections_idle",
				Help: "Number of idle connections by role and address.",
			},
			[]string{"role", "addr"},
		),
		PoolAcquires: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_pool_acquires_total",
				Help: "Total number of connection acquires by role.",
			},
			[]string{"role", "addr"},
		),
		PoolAcquireDur: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "pgmux_pool_acquire_duration_seconds",
				Help:    "Connection acquire duration in seconds.",
				Buckets: []float64{.0001, .0005, .001, .005, .01, .05, .1, .5, 1, 5},
			},
			[]string{"role", "addr"},
		),

		ReaderLSNLag: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "pgmux_reader_lsn_lag_bytes",
				Help: "Reader replication lag in bytes (writer LSN - reader replay LSN).",
			},
			[]string{"addr"},
		),

		FirewallBlocked: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_firewall_blocked_total",
				Help: "Total number of queries blocked by firewall rules.",
			},
			[]string{"rule"},
		),

		SlowQueries: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_slow_queries_total",
				Help: "Total number of slow queries detected.",
			},
			[]string{"target"},
		),
		WebhookSent: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_audit_webhook_sent_total",
				Help: "Total number of audit webhook notifications sent.",
			},
		),
		WebhookErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_audit_webhook_errors_total",
				Help: "Total number of audit webhook send errors.",
			},
		),

		DigestPatterns: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pgmux_digest_patterns",
				Help: "Current number of unique query patterns in the digest.",
			},
		),

		QueryTimeouts: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_query_timeout_total",
				Help: "Total number of queries canceled due to query timeout.",
			},
			[]string{"target"},
		),

		ClientIdleTimeouts: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_client_idle_timeout_total",
				Help: "Total number of client connections closed due to idle timeout.",
			},
		),

		ConnLimitRejected: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_connection_limit_rejected_total",
				Help: "Total number of connections rejected due to per-user or per-database limits.",
			},
			[]string{"user", "database"},
		),
		ActiveConnsByUser: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "pgmux_active_connections_by_user",
				Help: "Current number of active client connections per user.",
			},
			[]string{"user"},
		),
		ActiveConnsByDB: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "pgmux_active_connections_by_database",
				Help: "Current number of active client connections per database.",
			},
			[]string{"database"},
		),

		MaintenanceMode: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pgmux_maintenance_mode",
				Help: "Whether maintenance mode is active (1) or not (0).",
			},
		),
		MaintenanceRejectedConn: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_maintenance_rejected_total",
				Help: "Total number of connections/queries rejected due to maintenance mode.",
			},
		),

		ReadOnlyMode: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pgmux_readonly_mode",
				Help: "Whether the proxy is in read-only mode (1 = active, 0 = inactive).",
			},
		),
		ReadOnlyRejected: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "pgmux_readonly_rejected_total",
				Help: "Total number of write queries rejected due to read-only mode.",
			},
		),

		SessionDepDetected: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_session_dependency_detected_total",
				Help: "Total number of session-dependent features detected by feature type.",
			},
			[]string{"feature"},
		),
		SessionDepBlocked: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_session_dependency_blocked_total",
				Help: "Total number of queries blocked due to session-dependent features.",
			},
			[]string{"feature"},
		),
		SessionPinned: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_session_pinned_total",
				Help: "Total number of sessions pinned to writer due to session-dependent features.",
			},
			[]string{"feature"},
		),

		QueryRewritten: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_query_rewritten_total",
				Help: "Total number of queries rewritten by rewrite rules.",
			},
			[]string{"rule"},
		),

		PoolWarmConns: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_pool_warm_connections_total",
				Help: "Total number of connections pre-created by pool warming.",
			},
			[]string{"role", "addr"},
		),
		PoolWarmErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pgmux_pool_warm_errors_total",
				Help: "Total number of errors during pool warming.",
			},
			[]string{"role", "addr"},
		),
	}

	prometheus.MustRegister(
		m.QueriesRouted,
		m.QueryDuration,
		m.ReaderFallback,
		m.RateLimited,
		m.CacheHits,
		m.CacheMisses,
		m.CacheEntries,
		m.CacheInvalidations,
		m.PoolOpenConns,
		m.PoolIdleConns,
		m.PoolAcquires,
		m.PoolAcquireDur,
		m.ReaderLSNLag,
		m.FirewallBlocked,
		m.SlowQueries,
		m.WebhookSent,
		m.WebhookErrors,
		m.DigestPatterns,
		m.QueryTimeouts,
		m.ClientIdleTimeouts,
		m.ConnLimitRejected,
		m.ActiveConnsByUser,
		m.ActiveConnsByDB,
		m.MaintenanceMode,
		m.MaintenanceRejectedConn,
		m.ReadOnlyMode,
		m.ReadOnlyRejected,
		m.SessionDepDetected,
		m.SessionDepBlocked,
		m.SessionPinned,
		m.QueryRewritten,
		m.PoolWarmConns,
		m.PoolWarmErrors,
	)

	return m
}
