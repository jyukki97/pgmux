package digest

import (
	"sync"
	"testing"
	"time"
)

func TestDigestRecord(t *testing.T) {
	d := New(Config{MaxPatterns: 100, SamplesPerPattern: 100})

	// Record some queries
	d.Record("SELECT id, name FROM users WHERE id = 1", 10*time.Millisecond)
	d.Record("SELECT id, name FROM users WHERE id = 2", 20*time.Millisecond)
	d.Record("SELECT id, name FROM users WHERE id = 3", 30*time.Millisecond)
	d.Record("INSERT INTO logs (msg) VALUES ('hello')", 5*time.Millisecond)

	// Same normalized pattern should be grouped
	stats := d.TopN(0)
	if len(stats) != 2 {
		t.Fatalf("expected 2 patterns, got %d", len(stats))
	}

	// Top pattern should be the SELECT (3 executions)
	top := stats[0]
	if top.Count != 3 {
		t.Errorf("expected count 3, got %d", top.Count)
	}
	if top.AvgMs < 15 || top.AvgMs > 25 {
		t.Errorf("expected avg ~20ms, got %.2f", top.AvgMs)
	}
	if top.MinMs < 9 || top.MinMs > 11 {
		t.Errorf("expected min ~10ms, got %.2f", top.MinMs)
	}
	if top.MaxMs < 29 || top.MaxMs > 31 {
		t.Errorf("expected max ~30ms, got %.2f", top.MaxMs)
	}
}

func TestDigestTopN(t *testing.T) {
	d := New(Config{MaxPatterns: 100, SamplesPerPattern: 100})

	// Create 5 distinct patterns with different counts
	for i := 0; i < 50; i++ {
		d.Record("SELECT * FROM a WHERE id = 1", time.Millisecond)
	}
	for i := 0; i < 30; i++ {
		d.Record("SELECT * FROM b WHERE id = 1", time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		d.Record("SELECT * FROM c WHERE id = 1", time.Millisecond)
	}

	// TopN(2) should return only top 2
	stats := d.TopN(2)
	if len(stats) != 2 {
		t.Fatalf("expected 2 results, got %d", len(stats))
	}
	if stats[0].Count != 50 {
		t.Errorf("expected first count 50, got %d", stats[0].Count)
	}
	if stats[1].Count != 30 {
		t.Errorf("expected second count 30, got %d", stats[1].Count)
	}
}

func TestDigestMaxPatterns(t *testing.T) {
	d := New(Config{MaxPatterns: 2, SamplesPerPattern: 100})

	d.Record("SELECT * FROM a WHERE id = 1", time.Millisecond)
	d.Record("SELECT * FROM b WHERE id = 1", time.Millisecond)
	d.Record("SELECT * FROM c WHERE id = 1", time.Millisecond) // should be dropped

	if d.PatternCount() != 2 {
		t.Errorf("expected 2 patterns, got %d", d.PatternCount())
	}
}

func TestDigestP50P99(t *testing.T) {
	d := New(Config{MaxPatterns: 100, SamplesPerPattern: 1000})

	// Record 100 queries with increasing durations
	for i := 1; i <= 100; i++ {
		d.Record("SELECT * FROM t WHERE id = 1", time.Duration(i)*time.Millisecond)
	}

	stats := d.TopN(1)
	if len(stats) != 1 {
		t.Fatalf("expected 1 result, got %d", len(stats))
	}

	s := stats[0]
	// P50 should be around 50ms
	if s.P50Ms < 45 || s.P50Ms > 55 {
		t.Errorf("expected P50 ~50ms, got %.2f", s.P50Ms)
	}
	// P99 should be around 99-100ms
	if s.P99Ms < 95 || s.P99Ms > 105 {
		t.Errorf("expected P99 ~99ms, got %.2f", s.P99Ms)
	}
}

func TestDigestReset(t *testing.T) {
	d := New(Config{MaxPatterns: 100, SamplesPerPattern: 100})

	d.Record("SELECT 1", time.Millisecond)
	if d.PatternCount() != 1 {
		t.Fatalf("expected 1 pattern, got %d", d.PatternCount())
	}

	d.Reset()
	if d.PatternCount() != 0 {
		t.Errorf("expected 0 patterns after reset, got %d", d.PatternCount())
	}
}

func TestDigestConcurrency(t *testing.T) {
	d := New(Config{MaxPatterns: 100, SamplesPerPattern: 100})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Record("SELECT * FROM users WHERE id = 1", time.Millisecond)
		}()
	}
	wg.Wait()

	stats := d.TopN(1)
	if len(stats) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(stats))
	}
	if stats[0].Count != 100 {
		t.Errorf("expected count 100, got %d", stats[0].Count)
	}
}

func TestDigestCircularBuffer(t *testing.T) {
	d := New(Config{MaxPatterns: 100, SamplesPerPattern: 10})

	// Record more than maxSamples
	for i := 0; i < 20; i++ {
		d.Record("SELECT 1", time.Duration(i+1)*time.Millisecond)
	}

	stats := d.TopN(1)
	if len(stats) != 1 {
		t.Fatalf("expected 1 result, got %d", len(stats))
	}

	s := stats[0]
	if s.Count != 20 {
		t.Errorf("expected count 20, got %d", s.Count)
	}
	// Min should reflect all-time min (1ms), not just buffer
	if s.MinMs < 0.5 || s.MinMs > 1.5 {
		t.Errorf("expected min ~1ms, got %.2f", s.MinMs)
	}
}
