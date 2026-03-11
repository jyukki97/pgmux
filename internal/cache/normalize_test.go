package cache

import "testing"

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

func TestSemanticCacheKey_LiteralInsensitive(t *testing.T) {
	// Same structure, different literal values → same fingerprint
	k1 := SemanticCacheKey("SELECT * FROM users WHERE id = 1")
	k2 := SemanticCacheKey("SELECT * FROM users WHERE id = 999")

	if k1 != k2 {
		t.Errorf("different literals should produce same fingerprint: key1 (%d) != key2 (%d)", k1, k2)
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

func TestNormalizeQuery(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"SELECT * FROM users WHERE id = 1", "SELECT * FROM users WHERE id = $1"},
		{"SELECT * FROM users WHERE name = 'alice'", "SELECT * FROM users WHERE name = $1"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := NormalizeQuery(tt.query)
			if got != tt.want {
				t.Errorf("NormalizeQuery(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestSemanticCacheKeyWithParams(t *testing.T) {
	// Same normalized query + same params → same key
	k1 := SemanticCacheKeyWithParams("SELECT * FROM users WHERE id = 1")
	k2 := SemanticCacheKeyWithParams("SELECT * FROM users WHERE id = 2")

	// Normalized form is the same (both become "SELECT * FROM users WHERE id = $1")
	// but without explicit params, the keys should be the same
	if k1 != k2 {
		t.Errorf("same normalized form without params: key1 (%d) != key2 (%d)", k1, k2)
	}

	// With explicit params, different values → different keys
	k3 := SemanticCacheKeyWithParams("SELECT * FROM users WHERE id = $1", "1")
	k4 := SemanticCacheKeyWithParams("SELECT * FROM users WHERE id = $1", "2")

	if k3 == k4 {
		t.Error("different params should produce different keys")
	}
}
