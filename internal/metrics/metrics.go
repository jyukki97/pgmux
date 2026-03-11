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
	RateLimited prometheus.Counter

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
	SlowQueries    *prometheus.CounterVec
	WebhookSent    prometheus.Counter
	WebhookErrors  prometheus.Counter
}

// New creates and registers all Prometheus metrics.
func New() *Metrics {
	m := &Metrics{
		QueriesRouted: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dbproxy_queries_routed_total",
				Help: "Total number of queries routed by target.",
			},
			[]string{"target"},
		),
		QueryDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "dbproxy_query_duration_seconds",
				Help:    "Query processing duration in seconds.",
				Buckets: []float64{.0001, .0005, .001, .005, .01, .05, .1, .5, 1},
			},
			[]string{"target"},
		),
		ReaderFallback: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_reader_fallback_total",
				Help: "Total number of read queries that fell back to writer.",
			},
		),

		CacheHits: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_cache_hits_total",
				Help: "Total number of cache hits.",
			},
		),
		CacheMisses: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_cache_misses_total",
				Help: "Total number of cache misses.",
			},
		),
		CacheEntries: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "dbproxy_cache_entries",
				Help: "Current number of entries in the cache.",
			},
		),
		CacheInvalidations: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_cache_invalidations_total",
				Help: "Total number of cache invalidations.",
			},
		),

		RateLimited: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_rate_limited_total",
				Help: "Total number of rate-limited requests.",
			},
		),

		PoolOpenConns: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dbproxy_pool_connections_open",
				Help: "Number of open connections by role and address.",
			},
			[]string{"role", "addr"},
		),
		PoolIdleConns: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dbproxy_pool_connections_idle",
				Help: "Number of idle connections by role and address.",
			},
			[]string{"role", "addr"},
		),
		PoolAcquires: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dbproxy_pool_acquires_total",
				Help: "Total number of connection acquires by role.",
			},
			[]string{"role", "addr"},
		),
		PoolAcquireDur: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "dbproxy_pool_acquire_duration_seconds",
				Help:    "Connection acquire duration in seconds.",
				Buckets: []float64{.0001, .0005, .001, .005, .01, .05, .1, .5, 1, 5},
			},
			[]string{"role", "addr"},
		),

		ReaderLSNLag: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dbproxy_reader_lsn_lag_bytes",
				Help: "Reader replication lag in bytes (writer LSN - reader replay LSN).",
			},
			[]string{"addr"},
		),

		FirewallBlocked: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dbproxy_firewall_blocked_total",
				Help: "Total number of queries blocked by firewall rules.",
			},
			[]string{"rule"},
		),

		SlowQueries: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dbproxy_slow_queries_total",
				Help: "Total number of slow queries detected.",
			},
			[]string{"target"},
		),
		WebhookSent: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_audit_webhook_sent_total",
				Help: "Total number of audit webhook notifications sent.",
			},
		),
		WebhookErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dbproxy_audit_webhook_errors_total",
				Help: "Total number of audit webhook send errors.",
			},
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
	)

	return m
}
