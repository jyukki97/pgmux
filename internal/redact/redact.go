package redact

import (
	"regexp"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// Policy controls how SQL statements are redacted for external observability surfaces.
type Policy string

const (
	// PolicyNone passes SQL through unchanged. Use only for development/debugging.
	PolicyNone Policy = "none"
	// PolicyLiterals replaces all literal values with positional parameters ($1, $2, ...).
	// This is the safe default for production — preserves query structure for debugging
	// while removing potentially sensitive data.
	PolicyLiterals Policy = "literals"
	// PolicyFull replaces the entire SQL with a structural fingerprint hash.
	// Maximum privacy — only reveals query shape, not table names or column names.
	PolicyFull Policy = "full"
)

// regexFallback strips string literals and numeric constants when pg_query fails.
var regexFallback = regexp.MustCompile(`'[^']*'|"[^"]*"|\b\d+(\.\d+)?\b`)

// SQL applies the redaction policy to a SQL string.
// Thread-safe, stateless, safe to call on hot path.
func SQL(query string, policy Policy) string {
	switch policy {
	case PolicyNone:
		return query
	case PolicyFull:
		fp, err := pg_query.Fingerprint(query)
		if err != nil {
			return "[unparseable query]"
		}
		return "[fingerprint:" + fp + "]"
	default: // PolicyLiterals
		normalized, err := pg_query.Normalize(query)
		if err != nil {
			return regexFallback.ReplaceAllString(query, "?")
		}
		return normalized
	}
}

// SQLTruncated applies redaction then truncates to maxLen.
func SQLTruncated(query string, policy Policy, maxLen int) string {
	result := SQL(query, policy)
	if len(result) > maxLen {
		return result[:maxLen] + "..."
	}
	return result
}

// ForLog returns a redacted, truncated SQL suitable for slog attributes (max 200 chars).
func ForLog(query string, policy Policy) string {
	return SQLTruncated(query, policy, 200)
}
