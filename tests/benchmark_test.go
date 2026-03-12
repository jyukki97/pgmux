package tests

import (
	"testing"
	"time"

	"github.com/jyukki97/pgmux/internal/cache"
	"github.com/jyukki97/pgmux/internal/router"
)

func BenchmarkClassify_SELECT(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.Classify("SELECT * FROM users WHERE id = 1")
	}
}

func BenchmarkClassify_INSERT(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.Classify("INSERT INTO users (name, email) VALUES ('alice', 'alice@example.com')")
	}
}

func BenchmarkClassify_WithHint(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.Classify("/* route:writer */ SELECT * FROM users")
	}
}

func BenchmarkExtractTables(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.ExtractTables("INSERT INTO users (name) VALUES ('alice')")
	}
}

func BenchmarkSessionRoute(b *testing.B) {
	s := router.NewSession(500*time.Millisecond, false, false)
	for i := 0; i < b.N; i++ {
		s.Route("SELECT * FROM users WHERE id = 1")
	}
}

func BenchmarkCacheKey(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cache.CacheKey("SELECT * FROM users WHERE id = $1", "123")
	}
}

func BenchmarkCacheGetHit(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	key := cache.CacheKey("SELECT * FROM users WHERE id = 1")
	c.Set(key, []byte(`[{"id":1,"name":"alice"}]`), []string{"users"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(key)
	}
}

func BenchmarkCacheGetMiss(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(uint64(i))
	}
}

func BenchmarkCacheSet(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	result := []byte(`[{"id":1,"name":"alice"}]`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := cache.CacheKey("SELECT * FROM users WHERE id = ?", string(rune(i)))
		c.Set(key, result, []string{"users"})
	}
}

func BenchmarkCacheInvalidateTable(b *testing.B) {
	c := cache.New(cache.Config{
		MaxEntries: 10000,
		TTL:        time.Minute,
		MaxSize:    4096,
	})

	// Pre-fill
	for i := 0; i < 100; i++ {
		key := cache.CacheKey("SELECT * FROM users LIMIT 1 OFFSET " + string(rune(i)))
		c.Set(key, []byte("result"), []string{"users"})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Refill
		for j := 0; j < 100; j++ {
			key := cache.CacheKey("SELECT * FROM users LIMIT 1 OFFSET " + string(rune(j)))
			c.Set(key, []byte("result"), []string{"users"})
		}
		b.StartTimer()
		c.InvalidateTable("users")
	}
}

// === AST Parser Benchmarks ===

func BenchmarkClassifyAST_SELECT(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.ClassifyAST("SELECT * FROM users WHERE id = 1")
	}
}

func BenchmarkClassifyAST_INSERT(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.ClassifyAST("INSERT INTO users (name, email) VALUES ('alice', 'alice@example.com')")
	}
}

func BenchmarkClassifyAST_ComplexJoin(b *testing.B) {
	q := "SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id WHERE o.total > 100 AND u.active = true ORDER BY o.total DESC LIMIT 10"
	for i := 0; i < b.N; i++ {
		router.ClassifyAST(q)
	}
}

func BenchmarkClassifyAST_CTE(b *testing.B) {
	q := "WITH active AS (SELECT * FROM users WHERE active = true) SELECT * FROM active JOIN orders ON active.id = orders.user_id"
	for i := 0; i < b.N; i++ {
		router.ClassifyAST(q)
	}
}

func BenchmarkExtractTablesAST(b *testing.B) {
	for i := 0; i < b.N; i++ {
		router.ExtractTablesAST("INSERT INTO users (name) VALUES ('alice')")
	}
}

func BenchmarkSemanticCacheKey(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cache.SemanticCacheKey("SELECT * FROM users WHERE id = 1")
	}
}

func BenchmarkCheckFirewall(b *testing.B) {
	cfg := router.FirewallConfig{
		Enabled:                 true,
		BlockDeleteWithoutWhere: true,
		BlockUpdateWithoutWhere: true,
		BlockDropTable:          true,
		BlockTruncate:           true,
	}
	for i := 0; i < b.N; i++ {
		router.CheckFirewall("DELETE FROM users WHERE id = 1", cfg)
	}
}

func BenchmarkRoundRobin_Next(b *testing.B) {
	rb := router.NewRoundRobin([]string{
		"reader1:5432",
		"reader2:5432",
		"reader3:5432",
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Next()
	}
}
