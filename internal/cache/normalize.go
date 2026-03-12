package cache

import (
	"hash/fnv"
	"log/slog"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// SemanticCacheKey generates a cache key from a normalized AST representation.
// Semantically equivalent queries (different whitespace, casing) produce the same key
// while preserving literal values to prevent cross-query cache collisions.
// Falls back to CacheKey on parse failure.
func SemanticCacheKey(query string) uint64 {
	// Parse → Deparse normalizes whitespace and casing while preserving literal values.
	// Unlike FingerprintToUInt64 which strips all constants, Deparse keeps them intact.
	tree, err := pg_query.Parse(query)
	if err != nil {
		slog.Debug("semantic cache key: parse failed, fallback", "error", err)
		return CacheKey(query)
	}
	return semanticCacheKeyFromTree(tree, query)
}

// SemanticCacheKeyWithTree generates a cache key using a pre-parsed AST tree,
// avoiding a redundant pg_query.Parse() call.
func SemanticCacheKeyWithTree(tree *pg_query.ParseResult, query string) uint64 {
	return semanticCacheKeyFromTree(tree, query)
}

// semanticCacheKeyFromTree generates a cache key from a pre-parsed tree.
func semanticCacheKeyFromTree(tree *pg_query.ParseResult, query string) uint64 {
	deparsed, err := pg_query.Deparse(tree)
	if err != nil {
		slog.Debug("semantic cache key: deparse failed, fallback", "error", err)
		return CacheKey(query)
	}
	h := fnv.New64a()
	h.Write([]byte(deparsed))
	return h.Sum64()
}

