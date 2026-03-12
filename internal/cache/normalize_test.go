package cache

import (
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

func TestSemanticCacheKey_WhitespaceInsensitive(t *testing.T) {
	k1 := SemanticCacheKey("SELECT * FROM users WHERE id = 1")
	k2 := SemanticCacheKey("SELECT  *  FROM  users  WHERE  id  =  1")
	k3 := SemanticCacheKey("select * from users where id = 1")

	if k1 != k2 {
		t.Errorf("whitespace: key1 (%d) != key2 (%d)", k1, k2)
	}
	if k1 != k3 {
		t.Errorf("case: key1 (%d) != key3 (%d)", k1, k3)
	}
}

func TestSemanticCacheKey_LiteralSensitive(t *testing.T) {
	// Same structure, different literal values → different keys (prevents cache collision)
	k1 := SemanticCacheKey("SELECT * FROM users WHERE id = 1")
	k2 := SemanticCacheKey("SELECT * FROM users WHERE id = 999")

	if k1 == k2 {
		t.Errorf("different literals must produce different keys to prevent cache collision: %d", k1)
	}
}

func TestSemanticCacheKey_DifferentStructure(t *testing.T) {
	k1 := SemanticCacheKey("SELECT * FROM users WHERE id = 1")
	k2 := SemanticCacheKey("SELECT * FROM orders WHERE id = 1")

	if k1 == k2 {
		t.Error("different tables should produce different keys")
	}
}

func TestSemanticCacheKey_FallbackOnError(t *testing.T) {
	// Invalid SQL should fallback to CacheKey
	k := SemanticCacheKey("SELECTT INVALID SQL")
	if k == 0 {
		t.Error("fallback key should be non-zero")
	}
}

func TestSemanticCacheKeyWithTree_MatchesSemanticCacheKey(t *testing.T) {
	queries := []string{
		"SELECT * FROM users WHERE id = 1",
		"SELECT  *  FROM  users  WHERE  id  =  1",
		"select * from users where id = 1",
		"SELECT * FROM users WHERE id = 999",
		"SELECT * FROM orders WHERE id = 1",
		"INSERT INTO users VALUES (1)",
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			original := SemanticCacheKey(query)
			tree, err := pg_query.Parse(query)
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			withTree := SemanticCacheKeyWithTree(tree, query)
			if original != withTree {
				t.Errorf("SemanticCacheKey=%d, SemanticCacheKeyWithTree=%d", original, withTree)
			}
		})
	}
}

func BenchmarkSemanticCacheKey(b *testing.B) {
	query := "SELECT * FROM users WHERE id = 1 AND name = 'alice' ORDER BY created_at"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SemanticCacheKey(query)
	}
}

func BenchmarkSemanticCacheKeyWithTree(b *testing.B) {
	query := "SELECT * FROM users WHERE id = 1 AND name = 'alice' ORDER BY created_at"
	tree, _ := pg_query.Parse(query)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SemanticCacheKeyWithTree(tree, query)
	}
}
