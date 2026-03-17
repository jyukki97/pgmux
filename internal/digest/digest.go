// Package digest tracks top-N query patterns by frequency and latency.
package digest

import (
	"sort"
	"sync"
	"time"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

const defaultMaxSamples = 1000

// QueryDigestStats holds aggregated latency statistics for a normalized query pattern.
type QueryDigestStats struct {
	Pattern  string  `json:"query_pattern"`
	Count    int64   `json:"count"`
	TotalMs  float64 `json:"total_ms"`
	AvgMs    float64 `json:"avg_ms"`
	MinMs    float64 `json:"min_ms"`
	MaxMs    float64 `json:"max_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P99Ms    float64 `json:"p99_ms"`
	RowsRead int64   `json:"rows_read"`
}

// Config holds digest configuration.
type Config struct {
	MaxPatterns       int
	SamplesPerPattern int
}

type patternStats struct {
	mu       sync.Mutex
	count    int64
	totalMs  float64
	minMs    float64
	maxMs    float64
	durs     []time.Duration
	idx      int
	maxSamps int
}

func newPatternStats(maxSamples int) *patternStats {
	return &patternStats{
		minMs:    -1,
		maxSamps: maxSamples,
	}
}

func (ps *patternStats) record(dur time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.count++
	ms := durationMs(dur)
	ps.totalMs += ms

	if ps.minMs < 0 || ms < ps.minMs {
		ps.minMs = ms
	}
	if ms > ps.maxMs {
		ps.maxMs = ms
	}

	if len(ps.durs) < ps.maxSamps {
		ps.durs = append(ps.durs, dur)
	} else {
		ps.durs[ps.idx] = dur
		ps.idx = (ps.idx + 1) % ps.maxSamps
	}
}

func (ps *patternStats) snapshot() QueryDigestStats {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.count == 0 {
		return QueryDigestStats{}
	}

	n := len(ps.durs)
	avgMs := ps.totalMs / float64(ps.count)

	var p50, p99 float64
	if n > 0 {
		sorted := make([]time.Duration, n)
		copy(sorted, ps.durs)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		p50 = durationMs(percentile(sorted, 0.50))
		p99 = durationMs(percentile(sorted, 0.99))
	}

	minMs := ps.minMs
	if minMs < 0 {
		minMs = 0
	}

	return QueryDigestStats{
		Count:   ps.count,
		TotalMs: ps.totalMs,
		AvgMs:   avgMs,
		MinMs:   minMs,
		MaxMs:   ps.maxMs,
		P50Ms:   p50,
		P99Ms:   p99,
	}
}

// Digest collects per-pattern query statistics.
type Digest struct {
	mu          sync.RWMutex
	patterns    map[string]*patternStats
	maxPatterns int
	maxSamples  int
}

// New creates a new Digest collector.
func New(cfg Config) *Digest {
	maxPatterns := cfg.MaxPatterns
	if maxPatterns <= 0 {
		maxPatterns = 1000
	}
	maxSamples := cfg.SamplesPerPattern
	if maxSamples <= 0 {
		maxSamples = defaultMaxSamples
	}
	return &Digest{
		patterns:    make(map[string]*patternStats),
		maxPatterns: maxPatterns,
		maxSamples:  maxSamples,
	}
}

// Record normalizes the query and records its execution duration.
func (d *Digest) Record(query string, dur time.Duration) {
	normalized, err := pg_query.Normalize(query)
	if err != nil {
		normalized = query
	}

	d.mu.RLock()
	ps, ok := d.patterns[normalized]
	d.mu.RUnlock()

	if !ok {
		d.mu.Lock()
		ps, ok = d.patterns[normalized]
		if !ok {
			if len(d.patterns) >= d.maxPatterns {
				d.mu.Unlock()
				return // drop — max patterns reached
			}
			ps = newPatternStats(d.maxSamples)
			d.patterns[normalized] = ps
		}
		d.mu.Unlock()
	}

	ps.record(dur)
}

// TopN returns the top N query patterns sorted by total execution count (descending).
func (d *Digest) TopN(n int) []QueryDigestStats {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]QueryDigestStats, 0, len(d.patterns))
	for pattern, ps := range d.patterns {
		qs := ps.snapshot()
		qs.Pattern = pattern
		result = append(result, qs)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})

	if n > 0 && n < len(result) {
		result = result[:n]
	}

	return result
}

// PatternCount returns the number of unique query patterns tracked.
func (d *Digest) PatternCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.patterns)
}

// Reset clears all collected statistics.
func (d *Digest) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.patterns = make(map[string]*patternStats)
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func durationMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}
