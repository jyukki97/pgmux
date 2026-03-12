package mirror

import (
	"sort"
	"sync"
	"time"
)

const maxSamples = 1000

// QueryStats holds aggregated latency comparison for a normalized query pattern.
type QueryStats struct {
	Pattern    string  `json:"query_pattern"`
	Count      int64   `json:"count"`
	PrimaryP50 float64 `json:"primary_p50_ms"`
	PrimaryP99 float64 `json:"primary_p99_ms"`
	MirrorP50  float64 `json:"mirror_p50_ms"`
	MirrorP99  float64 `json:"mirror_p99_ms"`
	Regression bool    `json:"regression"`
}

type patternStats struct {
	mu          sync.Mutex
	count       int64
	primaryDurs []time.Duration
	mirrorDurs  []time.Duration
	idx         int
}

func (ps *patternStats) record(primaryDur, mirrorDur time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.count++

	if len(ps.primaryDurs) < maxSamples {
		ps.primaryDurs = append(ps.primaryDurs, primaryDur)
		ps.mirrorDurs = append(ps.mirrorDurs, mirrorDur)
	} else {
		ps.primaryDurs[ps.idx] = primaryDur
		ps.mirrorDurs[ps.idx] = mirrorDur
		ps.idx = (ps.idx + 1) % maxSamples
	}
}

func (ps *patternStats) snapshot() QueryStats {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	n := len(ps.primaryDurs)
	if n == 0 {
		return QueryStats{Count: ps.count}
	}

	primary := make([]time.Duration, n)
	mirror := make([]time.Duration, n)
	copy(primary, ps.primaryDurs)
	copy(mirror, ps.mirrorDurs)

	sort.Slice(primary, func(i, j int) bool { return primary[i] < primary[j] })
	sort.Slice(mirror, func(i, j int) bool { return mirror[i] < mirror[j] })

	pp50 := percentile(primary, 0.50)
	pp99 := percentile(primary, 0.99)
	mp50 := percentile(mirror, 0.50)
	mp99 := percentile(mirror, 0.99)

	return QueryStats{
		Count:      ps.count,
		PrimaryP50: durationMs(pp50),
		PrimaryP99: durationMs(pp99),
		MirrorP50:  durationMs(mp50),
		MirrorP99:  durationMs(mp99),
		Regression: mp50 > pp50*2,
	}
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

type statsCollector struct {
	mu       sync.RWMutex
	patterns map[string]*patternStats
}

func newStatsCollector() *statsCollector {
	return &statsCollector{
		patterns: make(map[string]*patternStats),
	}
}

func (sc *statsCollector) record(normalized string, primaryDur, mirrorDur time.Duration) {
	sc.mu.RLock()
	ps, ok := sc.patterns[normalized]
	sc.mu.RUnlock()

	if !ok {
		sc.mu.Lock()
		ps, ok = sc.patterns[normalized]
		if !ok {
			ps = &patternStats{}
			sc.patterns[normalized] = ps
		}
		sc.mu.Unlock()
	}

	ps.record(primaryDur, mirrorDur)
}

func (sc *statsCollector) snapshot() []QueryStats {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	result := make([]QueryStats, 0, len(sc.patterns))
	for pattern, ps := range sc.patterns {
		qs := ps.snapshot()
		qs.Pattern = pattern
		result = append(result, qs)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})

	return result
}
