package mirror

import (
	"testing"
	"time"
)

func TestStatsCollector_RecordAndSnapshot(t *testing.T) {
	sc := newStatsCollector()

	sc.record("SELECT $1", 10*time.Millisecond, 15*time.Millisecond)
	sc.record("SELECT $1", 12*time.Millisecond, 14*time.Millisecond)
	sc.record("INSERT INTO t VALUES ($1)", 5*time.Millisecond, 8*time.Millisecond)

	snap := sc.snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 patterns, got %d", len(snap))
	}

	// Sorted by count descending — SELECT should be first
	if snap[0].Pattern != "SELECT $1" {
		t.Errorf("first pattern = %q, want SELECT $1", snap[0].Pattern)
	}
	if snap[0].Count != 2 {
		t.Errorf("count = %d, want 2", snap[0].Count)
	}
	if snap[1].Count != 1 {
		t.Errorf("count = %d, want 1", snap[1].Count)
	}
}

func TestStatsCollector_Percentiles(t *testing.T) {
	sc := newStatsCollector()

	// Insert 100 samples with increasing durations
	for i := 1; i <= 100; i++ {
		primary := time.Duration(i) * time.Millisecond
		mirror := time.Duration(i*2) * time.Millisecond
		sc.record("SELECT $1", primary, mirror)
	}

	snap := sc.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(snap))
	}

	qs := snap[0]
	if qs.Count != 100 {
		t.Errorf("count = %d, want 100", qs.Count)
	}

	// P50 should be around 50ms for primary
	if qs.PrimaryP50 < 40 || qs.PrimaryP50 > 60 {
		t.Errorf("primary P50 = %.1f ms, want ~50", qs.PrimaryP50)
	}
	// P99 should be around 99ms for primary
	if qs.PrimaryP99 < 90 || qs.PrimaryP99 > 100 {
		t.Errorf("primary P99 = %.1f ms, want ~99", qs.PrimaryP99)
	}
	// Mirror is 2x primary, so P50 ~100ms
	if qs.MirrorP50 < 80 || qs.MirrorP50 > 120 {
		t.Errorf("mirror P50 = %.1f ms, want ~100", qs.MirrorP50)
	}
}

func TestStatsCollector_RegressionDetection(t *testing.T) {
	sc := newStatsCollector()

	// Mirror is 3x slower than primary → regression
	for i := 0; i < 50; i++ {
		sc.record("SELECT $1", 10*time.Millisecond, 31*time.Millisecond)
	}

	snap := sc.snapshot()
	if !snap[0].Regression {
		t.Error("expected regression to be detected (mirror P50 > primary P50 × 2)")
	}
}

func TestStatsCollector_NoRegression(t *testing.T) {
	sc := newStatsCollector()

	// Mirror is roughly equal to primary → no regression
	for i := 0; i < 50; i++ {
		sc.record("SELECT $1", 10*time.Millisecond, 12*time.Millisecond)
	}

	snap := sc.snapshot()
	if snap[0].Regression {
		t.Error("expected no regression when mirror latency is close to primary")
	}
}

func TestStatsCollector_CircularBuffer(t *testing.T) {
	sc := newStatsCollector()

	// Fill beyond maxSamples to trigger circular buffer wrap
	for i := 0; i < maxSamples+500; i++ {
		sc.record("SELECT $1", 10*time.Millisecond, 20*time.Millisecond)
	}

	snap := sc.snapshot()
	if snap[0].Count != int64(maxSamples+500) {
		t.Errorf("count = %d, want %d", snap[0].Count, maxSamples+500)
	}

	// P50 should still be correct (all same value)
	if snap[0].PrimaryP50 != 10.0 {
		t.Errorf("primary P50 = %.1f, want 10.0", snap[0].PrimaryP50)
	}
}

func TestStatsCollector_EmptySnapshot(t *testing.T) {
	sc := newStatsCollector()
	snap := sc.snapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot, got %d patterns", len(snap))
	}
}

func TestPercentile_Empty(t *testing.T) {
	result := percentile(nil, 0.5)
	if result != 0 {
		t.Errorf("expected 0, got %v", result)
	}
}

func TestPercentile_SingleElement(t *testing.T) {
	result := percentile([]time.Duration{5 * time.Millisecond}, 0.99)
	if result != 5*time.Millisecond {
		t.Errorf("expected 5ms, got %v", result)
	}
}

func TestDurationMs(t *testing.T) {
	d := 1500 * time.Microsecond
	ms := durationMs(d)
	if ms != 1.5 {
		t.Errorf("durationMs(1500µs) = %f, want 1.5", ms)
	}
}
