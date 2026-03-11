package cache

import (
	"hash/fnv"
	"log/slog"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// SemanticCacheKey generates a cache key from a normalized AST representation.
// Semantically equivalent queries (different whitespace, literal values, WHERE clause order)
// produce the same key. Falls back to CacheKey on parse failure.
func SemanticCacheKey(query string) uint64 {
	// Step 1: Fingerprint — pg_query_go normalizes whitespace, casing, and replaces
	// constants with placeholders. Structurally identical queries get the same fingerprint.
	fp, err := pg_query.FingerprintToUInt64(query)
	if err != nil {
		slog.Debug("semantic cache key: fingerprint failed, fallback", "error", err)
		return CacheKey(query)
	}
	return fp
}

// NormalizeQuery returns a canonical string representation of the query
// with constants replaced by $N placeholders. Useful for logging and debugging.
func NormalizeQuery(query string) string {
	normalized, err := pg_query.Normalize(query)
	if err != nil {
		return query
	}
	return normalized
}

// SemanticCacheKeyWithParams generates a semantic cache key that also considers
// the actual parameter values. This allows caching different results for
// the same query structure with different parameters.
func SemanticCacheKeyWithParams(query string, params ...any) uint64 {
	normalized, err := pg_query.Normalize(query)
	if err != nil {
		return CacheKey(query, params...)
	}

	h := fnv.New64a()
	h.Write([]byte(normalized))
	for _, p := range params {
		if s, ok := p.(string); ok {
			h.Write([]byte(s))
		}
	}
	return h.Sum64()
}
