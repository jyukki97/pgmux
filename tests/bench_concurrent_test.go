package tests

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/router"
)

// === Concurrent Cache Benchmarks ===
// Tests mutex contention under parallel goroutines

func BenchmarkCacheGetHit_Parallel(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	key := cache.CacheKey("SELECT * FROM users WHERE id = 1")
	c.Set(key, []byte(`[{"id":1,"name":"alice"}]`), []string{"users"})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get(key)
		}
	})
}

func BenchmarkCacheSet_Parallel(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	result := []byte(`[{"id":1,"name":"alice"}]`)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := cache.CacheKey("SELECT * FROM users WHERE id = ?", fmt.Sprintf("%d", i))
			c.Set(key, result, []string{"users"})
			i++
		}
	})
}

func BenchmarkCacheMixedReadWrite_Parallel(b *testing.B) {
	// 90% read, 10% write — typical read-heavy workload
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	// Pre-fill 1000 entries
	for i := 0; i < 1000; i++ {
		key := cache.CacheKey("SELECT * FROM users WHERE id = ?", fmt.Sprintf("%d", i))
		c.Set(key, []byte(`[{"id":1}]`), []string{"users"})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%10 == 0 {
				// 10% writes
				key := cache.CacheKey("SELECT * FROM users WHERE id = ?", fmt.Sprintf("w%d", i))
				c.Set(key, []byte(`[{"id":1}]`), []string{"users"})
			} else {
				// 90% reads
				key := cache.CacheKey("SELECT * FROM users WHERE id = ?", fmt.Sprintf("%d", i%1000))
				c.Get(key)
			}
			i++
		}
	})
}

// === Concurrent Routing Benchmarks ===

func BenchmarkRoundRobin_Next_Parallel(b *testing.B) {
	rb := router.NewRoundRobin([]string{
		"reader1:5432",
		"reader2:5432",
		"reader3:5432",
	})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rb.Next()
		}
	})
}

func BenchmarkSessionRoute_Parallel(b *testing.B) {
	// Each goroutine gets its own session (as in real per-connection usage)
	b.RunParallel(func(pb *testing.PB) {
		s := router.NewSession(500*time.Millisecond, false, false)
		for pb.Next() {
			s.Route("SELECT * FROM users WHERE id = 1")
		}
	})
}

// === Full Pipeline Concurrent Benchmark ===
// Simulates: classify → cache lookup → route → (cache miss) → cache set

func BenchmarkFullPipeline_Parallel(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 50000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})
	rb := router.NewRoundRobin([]string{
		"reader1:5432",
		"reader2:5432",
		"reader3:5432",
	})

	queries := []string{
		"SELECT * FROM users WHERE id = 1",
		"SELECT * FROM orders WHERE user_id = 42",
		"SELECT name, email FROM users WHERE active = true LIMIT 10",
		"SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id",
		"INSERT INTO logs (msg) VALUES ('test')",
	}

	// Pre-fill cache for some queries (simulate ~60% hit rate)
	for i := 0; i < 3; i++ {
		key := cache.CacheKey(queries[i])
		c.Set(key, []byte(`[{"id":1}]`), []string{"users"})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := router.NewSession(500*time.Millisecond, false, false)
		i := 0
		for pb.Next() {
			q := queries[i%len(queries)]
			qType := router.Classify(q)

			if qType == router.QueryRead {
				key := cache.CacheKey(q)
				if hit := c.Get(key); hit != nil {
					_ = hit
				} else {
					_ = rb.Next()
					// simulate cache miss → set
					c.Set(key, []byte(`[{"id":1}]`), []string{"users"})
				}
			} else {
				_ = s.Route(q)
			}
			i++
		}
	})
}

// === AST Pipeline Concurrent Benchmark ===

func BenchmarkASTPipeline_Parallel(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 50000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})
	rb := router.NewRoundRobin([]string{
		"reader1:5432",
		"reader2:5432",
		"reader3:5432",
	})
	fwCfg := router.FirewallConfig{
		Enabled:                 true,
		BlockDeleteWithoutWhere: true,
		BlockUpdateWithoutWhere: true,
		BlockDropTable:          true,
		BlockTruncate:           true,
	}

	queries := []string{
		"SELECT * FROM users WHERE id = 1",
		"SELECT * FROM orders WHERE user_id = 42",
		"SELECT name, email FROM users WHERE active = true LIMIT 10",
		"SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id WHERE o.total > 100",
		"INSERT INTO logs (msg) VALUES ('test')",
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			q := queries[i%len(queries)]

			// Full AST pipeline: parse → classify → firewall → cache key
			pq, err := router.NewParsedQuery(q)
			if err != nil {
				continue
			}
			_ = router.ClassifyASTWithTree(q, pq)
			_ = router.CheckFirewallWithTree(pq, fwCfg)
			_ = cache.SemanticCacheKeyWithTree(pq.Tree, q)

			key := cache.CacheKey(q)
			if hit := c.Get(key); hit == nil {
				_ = rb.Next()
				tables := router.ExtractTablesASTWithTree(pq)
				c.Set(key, []byte(`[{"id":1}]`), tables)
			}
			i++
		}
	})
}

// === GC Pressure Test ===
// Measures how allocation-heavy paths affect throughput under sustained load

func BenchmarkGCPressure_ASTHeavy(b *testing.B) {
	queries := []string{
		"SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id WHERE o.total > 100 AND u.active = true ORDER BY o.total DESC LIMIT 10",
		"WITH active AS (SELECT * FROM users WHERE active = true) SELECT * FROM active JOIN orders ON active.id = orders.user_id",
		"SELECT * FROM users WHERE id IN (SELECT user_id FROM orders WHERE total > 100)",
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			q := queries[i%len(queries)]
			pq, err := router.NewParsedQuery(q)
			if err != nil {
				i++
				continue
			}
			_ = router.ClassifyASTWithTree(q, pq)
			_ = router.ExtractTablesASTWithTree(pq)
			_ = cache.SemanticCacheKeyWithTree(pq.Tree, q)
			i++
		}
	})
}

// === Contention Benchmark ===
// Cache invalidation while reads are happening (write path vs read path contention)

func BenchmarkCacheContention_InvalidateDuringReads(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	// Pre-fill
	for i := 0; i < 1000; i++ {
		key := cache.CacheKey(fmt.Sprintf("SELECT * FROM users WHERE id = %d", i))
		c.Set(key, []byte(`[{"id":1}]`), []string{"users"})
	}
	for i := 0; i < 1000; i++ {
		key := cache.CacheKey(fmt.Sprintf("SELECT * FROM orders WHERE id = %d", i))
		c.Set(key, []byte(`[{"id":1}]`), []string{"orders"})
	}

	// Background invalidation every 100μs
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				c.InvalidateTable("users")
				// Re-fill so reads keep hitting
				for i := 0; i < 100; i++ {
					key := cache.CacheKey(fmt.Sprintf("SELECT * FROM users WHERE id = %d", i))
					c.Set(key, []byte(`[{"id":1}]`), []string{"users"})
				}
			}
		}
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// Read from orders (not being invalidated) and users (being invalidated)
			if i%2 == 0 {
				key := cache.CacheKey(fmt.Sprintf("SELECT * FROM orders WHERE id = %d", i%1000))
				c.Get(key)
			} else {
				key := cache.CacheKey(fmt.Sprintf("SELECT * FROM users WHERE id = %d", i%1000))
				c.Get(key)
			}
			i++
		}
	})
	b.StopTimer()

	close(done)
	wg.Wait()
}
