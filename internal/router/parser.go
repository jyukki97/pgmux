package router

import (
	"regexp"
	"strings"
	"time"
)

type QueryType int

const (
	QueryRead  QueryType = iota
	QueryWrite
)

var writeKeywords = map[string]bool{
	"INSERT":   true,
	"UPDATE":   true,
	"DELETE":   true,
	"CREATE":   true,
	"ALTER":    true,
	"DROP":     true,
	"TRUNCATE": true,
	"GRANT":    true,
	"REVOKE":   true,
	"MERGE":    true,
	"COPY":     true,
	"CALL":     true,
	"COMMENT":  true,
}

var hintRegex = regexp.MustCompile(`/\*\s*route:(writer|reader)\s*\*/`)
var timeoutHintRegex = regexp.MustCompile(`/\*\s*timeout:(\d+(?:\.\d+)?(?:s|ms|m))\s*\*/`)

// Classify determines whether a query is a read or write operation.
// For multi-statement queries (semicolon-separated), returns QueryWrite if any statement is a write.
func Classify(query string) QueryType {
	// Fast path for simple single-statement queries without hints or comments.
	// This avoids expensive string operations (stripStringLiterals, splitStatements, etc.)
	// for the common case of "SELECT ..." queries.
	if qt, ok := classifyFast(query); ok {
		return qt
	}

	// 1. Check for routing hint
	if hint := extractHint(query); hint != "" {
		if hint == "writer" {
			return QueryWrite
		}
		return QueryRead
	}

	// 2. Check all statements — if any is a write, the whole query is a write
	stmts := splitStatements(query)
	for _, stmt := range stmts {
		keyword := firstKeyword(stmt)
		if writeKeywords[keyword] {
			return QueryWrite
		}
		// CTE: WITH ... AS (UPDATE/INSERT/DELETE ...)
		if keyword == "WITH" && containsWriteKeyword(stmt) {
			return QueryWrite
		}
		// Side-effectful SELECT: FOR UPDATE/SHARE, nextval(), set_config(), etc.
		if keyword == "SELECT" && isSideEffectfulSelect(stmt) {
			return QueryWrite
		}
		// EXPLAIN ANALYZE actually executes the query
		if keyword == "EXPLAIN" && isExplainAnalyzeWrite(stmt) {
			return QueryWrite
		}
	}
	return QueryRead
}

// classifyFast is a fast path for simple single-statement queries.
// Returns (QueryType, true) if the query can be classified without expensive parsing.
// Returns (0, false) if the full parser is needed.
func classifyFast(query string) (QueryType, bool) {
	// Skip leading whitespace
	i := 0
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r') {
		i++
	}
	if i >= len(query) {
		return QueryRead, true
	}

	// If contains comments (potential hints), fall through to full parser
	if strings.Contains(query, "/*") || strings.Contains(query, "--") {
		return 0, false
	}

	// If contains multiple statements, need full parser
	// A single trailing semicolon is fine (e.g., "SELECT 1;")
	if idx := strings.IndexByte(query, ';'); idx >= 0 {
		if strings.TrimSpace(query[idx+1:]) != "" {
			return 0, false // multiple statements
		}
	}

	// Extract first keyword (uppercase the first word only)
	j := i
	for j < len(query) && query[j] != ' ' && query[j] != '\t' && query[j] != '\n' && query[j] != '(' {
		j++
	}
	if j == i {
		return QueryRead, true
	}

	kw := strings.ToUpper(query[i:j])
	if writeKeywords[kw] {
		return QueryWrite, true
	}
	if kw == "WITH" || kw == "EXPLAIN" {
		return 0, false // CTE / EXPLAIN ANALYZE needs full parser
	}
	if kw == "SELECT" {
		// Check for side-effectful patterns that need full analysis
		upper := strings.ToUpper(query)
		if strings.Contains(upper, "FOR UPDATE") || strings.Contains(upper, "FOR SHARE") ||
			strings.Contains(upper, "FOR NO KEY UPDATE") || strings.Contains(upper, "FOR KEY SHARE") ||
			hasSideEffectFunc(upper) {
			return 0, false // need full parser
		}
	}
	return QueryRead, true
}

// containsWriteKeyword checks if a WITH/CTE query contains write operations.
// String literals are stripped first to avoid false positives from keywords inside quotes.
func containsWriteKeyword(query string) bool {
	upper := strings.ToUpper(stripComments(stripStringLiterals(query)))
	for kw := range writeKeywords {
		// Look for write keywords that aren't just substrings of table/column names
		idx := strings.Index(upper, kw)
		for idx >= 0 {
			// Check it's a word boundary (preceded by space, paren, or start)
			if idx == 0 || upper[idx-1] == ' ' || upper[idx-1] == '(' || upper[idx-1] == '\n' {
				end := idx + len(kw)
				if end >= len(upper) || upper[end] == ' ' || upper[end] == '\n' || upper[end] == '(' {
					return true
				}
			}
			next := strings.Index(upper[idx+1:], kw)
			if next < 0 {
				break
			}
			idx = idx + 1 + next
		}
	}
	return false
}

func extractHint(query string) string {
	sanitized := stripStringLiterals(query)
	// Only match hints in top-level block comments (depth 0).
	// Nested comments like /* /* route:writer */ */ should NOT be treated as hints.
	i := 0
	for i < len(sanitized) {
		if i+1 < len(sanitized) && sanitized[i] == '/' && sanitized[i+1] == '*' {
			start := i
			depth := 1
			i += 2
			for i < len(sanitized) && depth > 0 {
				if i+1 < len(sanitized) && sanitized[i] == '/' && sanitized[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < len(sanitized) && sanitized[i] == '*' && sanitized[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			// Only check for hint if this was a non-nested comment (max depth was 1)
			comment := sanitized[start:i]
			if !strings.Contains(comment, "/* ") || strings.Count(comment, "/*") == 1 {
				matches := hintRegex.FindStringSubmatch(comment)
				if len(matches) >= 2 {
					return matches[1]
				}
			}
		} else {
			i++
		}
	}
	return ""
}

func firstKeyword(query string) string {
	q := stripComments(query)
	q = strings.TrimSpace(q)
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

func stripComments(query string) string {
	var result strings.Builder
	result.Grow(len(query))
	i := 0
	for i < len(query) {
		// Block comment: handle nested /* ... */
		if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
			depth := 1
			i += 2
			for i < len(query) && depth > 0 {
				if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < len(query) && query[i] == '*' && query[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			continue
		}
		// Line comment: --
		if i+1 < len(query) && query[i] == '-' && query[i+1] == '-' {
			for i < len(query) && query[i] != '\n' {
				i++
			}
			continue
		}
		result.WriteByte(query[i])
		i++
	}
	return result.String()
}

// ExtractReadTables extracts table names from read queries (SELECT ... FROM).
// Handles multi-statement queries, subqueries in FROM clause, and JOINs.
func ExtractReadTables(query string) []string {
	seen := make(map[string]bool)
	var tables []string

	stmts := splitStatements(query)
	for _, stmt := range stmts {
		for _, t := range extractReadTablesFromStmt(stmt) {
			if t != "" && !seen[t] {
				seen[t] = true
				tables = append(tables, t)
			}
		}
	}

	return tables
}

func extractReadTablesFromStmt(stmt string) []string {
	q := strings.TrimSpace(stmt)
	upper := strings.ToUpper(q)

	// Only process SELECT or WITH ... SELECT (pure read CTE)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return nil
	}

	sanitized := stripStringLiterals(q)
	upperSanitized := strings.ToUpper(sanitized)
	var tables []string

	// Find all FROM and JOIN table references
	for _, kw := range []string{"FROM", "JOIN"} {
		idx := strings.Index(upperSanitized, kw)
		for idx >= 0 {
			// Check word boundary before keyword
			if idx > 0 {
				prev := upperSanitized[idx-1]
				if prev != ' ' && prev != '\n' && prev != '\t' && prev != '(' && prev != ',' {
					next := strings.Index(upperSanitized[idx+len(kw):], kw)
					if next < 0 {
						break
					}
					idx = idx + len(kw) + next
					continue
				}
			}

			// Check word boundary after keyword
			end := idx + len(kw)
			if end < len(upperSanitized) {
				next := upperSanitized[end]
				if next != ' ' && next != '\n' && next != '\t' && next != '(' {
					nextIdx := strings.Index(upperSanitized[end:], kw)
					if nextIdx < 0 {
						break
					}
					idx = end + nextIdx
					continue
				}
			}

			rest := strings.TrimSpace(sanitized[end:])
			if len(rest) == 0 {
				break
			}

			// Skip subqueries: FROM (SELECT ...)
			if rest[0] == '(' {
				next := strings.Index(upperSanitized[end:], kw)
				if next < 0 {
					break
				}
				idx = end + next
				continue
			}

			name := extractIdentifier(rest)
			if name == "" {
				next := strings.Index(upperSanitized[end:], kw)
				if next < 0 {
					break
				}
				idx = end + next
				continue
			}

			// Remove schema prefix
			parts := strings.Split(name, ".")
			final := parts[len(parts)-1]
			final = stripQuotes(final)
			t := strings.ToLower(final)

			// Skip SQL keywords that may follow FROM (e.g., FROM (SELECT ...))
			upperT := strings.ToUpper(t)
			if upperT != "SELECT" && upperT != "LATERAL" && upperT != "UNNEST" && upperT != "GENERATE_SERIES" {
				tables = append(tables, t)
			}

			next := strings.Index(upperSanitized[end:], kw)
			if next < 0 {
				break
			}
			idx = end + next
		}
	}

	return tables
}

// ExtractTables extracts table names from write queries.
// Handles multi-statement queries, CTE (WITH ... AS (UPDATE ...)), and subqueries.
func ExtractTables(query string) []string {
	seen := make(map[string]bool)
	var tables []string

	stmts := splitStatements(query)
	for _, stmt := range stmts {
		for _, t := range extractTablesFromStmt(stmt) {
			if t != "" && !seen[t] {
				seen[t] = true
				tables = append(tables, t)
			}
		}
	}

	return tables
}

func extractTablesFromStmt(stmt string) []string {
	q := strings.TrimSpace(stmt)
	upper := strings.ToUpper(q)
	var tables []string

	switch {
	case strings.HasPrefix(upper, "INSERT INTO"):
		tables = append(tables, extractAfter(q, upper, "INSERT INTO"))
	case strings.HasPrefix(upper, "UPDATE"):
		tables = append(tables, extractAfter(q, upper, "UPDATE"))
	case strings.HasPrefix(upper, "DELETE FROM"):
		tables = append(tables, extractAfter(q, upper, "DELETE FROM"))
	case strings.HasPrefix(upper, "TRUNCATE"):
		tables = append(tables, extractAfter(q, upper, "TRUNCATE"))
	case strings.HasPrefix(upper, "MERGE INTO"):
		tables = append(tables, extractAfter(q, upper, "MERGE INTO"))
	case strings.HasPrefix(upper, "MERGE"):
		tables = append(tables, extractAfter(q, upper, "MERGE"))
	case strings.HasPrefix(upper, "COPY"):
		tables = append(tables, extractCopyTable(q, upper)...)
	case strings.HasPrefix(upper, "EXPLAIN"):
		// EXPLAIN ANALYZE may execute writes — delegate to inner statement
		tables = append(tables, extractExplainTables(q, upper)...)
	case strings.HasPrefix(upper, "WITH"):
		// CTE: scan for write keywords inside the CTE body
		tables = append(tables, extractCTETables(q)...)
	}

	return tables
}

// extractCTETables extracts table names from CTE (WITH ... AS (...)) queries
// that contain write operations (UPDATE, INSERT, DELETE).
// String literals are stripped first to avoid false positives.
func extractCTETables(query string) []string {
	sanitized := stripStringLiterals(query)
	upper := strings.ToUpper(sanitized)
	var tables []string

	// Find all write keywords and extract table names after them
	for _, kw := range []struct {
		keyword string
		prefix  string
	}{
		{"INSERT INTO", "INSERT INTO"},
		{"UPDATE", "UPDATE"},
		{"DELETE FROM", "DELETE FROM"},
	} {
		idx := strings.Index(upper, kw.keyword)
		for idx >= 0 {
			sub := sanitized[idx:]
			subUpper := upper[idx:]
			t := extractAfter(sub, subUpper, kw.prefix)
			if t != "" {
				tables = append(tables, t)
			}
			next := strings.Index(upper[idx+len(kw.keyword):], kw.keyword)
			if next < 0 {
				break
			}
			idx = idx + len(kw.keyword) + next
		}
	}

	return tables
}

// stripStringLiterals replaces content inside single/double-quoted strings with empty strings.
// Handles PostgreSQL escaped quotes ('') correctly.
// Example: "SELECT * FROM t WHERE x = 'INSERT INTO foo'" → "SELECT * FROM t WHERE x = ''"
func stripStringLiterals(query string) string {
	var result strings.Builder
	result.Grow(len(query))
	inSingle := false
	inDouble := false

	for i := 0; i < len(query); i++ {
		ch := query[i]

		// Dollar quoting: $$ or $tag$ (only outside other quotes)
		if ch == '$' && !inSingle && !inDouble {
			tag, ok := parseDollarTag(query, i)
			if ok {
				// Write the opening tag, skip content, write the closing tag
				result.WriteString(tag)
				end := strings.Index(query[i+len(tag):], tag)
				if end >= 0 {
					i += len(tag) + end + len(tag) - 1
					result.WriteString(tag)
				} else {
					// No closing tag — treat rest as dollar-quoted (skip all)
					i = len(query) - 1
					result.WriteString(tag)
				}
				continue
			}
		}

		switch {
		case ch == '\'' && !inDouble:
			result.WriteByte(ch)
			if inSingle {
				// Check for escaped quote ('')
				if i+1 < len(query) && query[i+1] == '\'' {
					result.WriteByte('\'')
					i++
				} else {
					inSingle = false
				}
			} else {
				inSingle = true
			}
		case ch == '"' && !inSingle:
			result.WriteByte(ch)
			if inDouble {
				inDouble = false
			} else {
				inDouble = true
			}
		case inSingle || inDouble:
			// skip content inside quotes
		default:
			result.WriteByte(ch)
		}
	}
	return result.String()
}

// parseDollarTag checks if position i in query starts a dollar-quote tag ($$ or $tag$).
// Returns the tag string and true if valid, or ("", false) otherwise.
func parseDollarTag(query string, i int) (string, bool) {
	if i >= len(query) || query[i] != '$' {
		return "", false
	}
	// $$ case
	if i+1 < len(query) && query[i+1] == '$' {
		return "$$", true
	}
	// $tag$ case: tag must be [A-Za-z0-9_] and not start with digit
	j := i + 1
	if j >= len(query) {
		return "", false
	}
	// Tag must start with letter or underscore
	if !isTagStart(query[j]) {
		return "", false
	}
	j++
	for j < len(query) && isTagChar(query[j]) {
		j++
	}
	if j < len(query) && query[j] == '$' {
		return query[i : j+1], true
	}
	return "", false
}

func isTagStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isTagChar(ch byte) bool {
	return isTagStart(ch) || (ch >= '0' && ch <= '9')
}

// ExtractTimeoutHint extracts a per-query timeout override from a SQL hint comment.
// Example: /* timeout:5s */ SELECT ... → 5s
// Returns 0 if no hint is found.
func ExtractTimeoutHint(query string) time.Duration {
	sanitized := stripStringLiterals(query)
	matches := timeoutHintRegex.FindStringSubmatch(sanitized)
	if len(matches) >= 2 {
		d, err := time.ParseDuration(matches[1])
		if err == nil && d > 0 {
			return d
		}
	}
	return 0
}

func extractAfter(query, upper, keyword string) string {
	rest := strings.TrimSpace(query[len(keyword):])
	// Handle optional keywords like "TABLE"
	upperRest := strings.ToUpper(rest)
	if strings.HasPrefix(upperRest, "TABLE ") {
		rest = strings.TrimSpace(rest[6:])
	}
	if len(rest) == 0 {
		return ""
	}
	name := extractIdentifier(rest)
	if name == "" {
		return ""
	}
	// Remove schema prefix — take the last part after '.'
	parts := strings.Split(name, ".")
	final := parts[len(parts)-1]
	// Strip surrounding quotes if present
	final = stripQuotes(final)
	return strings.ToLower(final)
}

// extractIdentifier extracts a possibly quoted (schema.table) identifier from the start of s.
// Handles: tablename, "quoted name", schema."quoted name", "schema"."table"
func extractIdentifier(s string) string {
	var result strings.Builder
	i := 0
	for {
		if i >= len(s) {
			break
		}
		if s[i] == '"' {
			// Quoted identifier: read until closing "
			result.WriteByte('"')
			i++
			for i < len(s) {
				if s[i] == '"' {
					// Check for escaped "" inside identifier
					if i+1 < len(s) && s[i+1] == '"' {
						result.WriteString(`""`)
						i += 2
						continue
					}
					result.WriteByte('"')
					i++
					break
				}
				result.WriteByte(s[i])
				i++
			}
		} else {
			// Unquoted identifier: read until whitespace or special char
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' &&
				s[i] != '(' && s[i] != ';' && s[i] != ',' && s[i] != '.' && s[i] != '"' {
				result.WriteByte(s[i])
				i++
			}
		}
		// Check for dot (schema.table)
		if i < len(s) && s[i] == '.' {
			result.WriteByte('.')
			i++
			continue
		}
		break
	}
	return result.String()
}

// sideEffectFuncs lists function name prefixes (uppercased) that indicate side effects.
var sideEffectFuncs = []string{
	"NEXTVAL(",
	"SETVAL(",
	"CURRVAL(",
	"SET_CONFIG(",
	"PG_ADVISORY_LOCK(",
	"PG_ADVISORY_XACT_LOCK(",
	"PG_ADVISORY_UNLOCK(",
	"PG_ADVISORY_UNLOCK_ALL(",
	"PG_TRY_ADVISORY_LOCK(",
	"PG_TRY_ADVISORY_XACT_LOCK(",
	"LO_CREATE(",
	"LO_UNLINK(",
	"PG_NOTIFY(",
	"TXID_CURRENT(",
}

// hasSideEffectFunc checks if an uppercased query contains a known side-effectful function call.
func hasSideEffectFunc(upperQuery string) bool {
	for _, fn := range sideEffectFuncs {
		if strings.Contains(upperQuery, fn) {
			return true
		}
	}
	return false
}

// lockingClauses lists locking clause patterns (uppercased) to check at word boundaries.
var lockingClauses = []string{
	"FOR UPDATE",
	"FOR NO KEY UPDATE",
	"FOR SHARE",
	"FOR KEY SHARE",
}

// isSideEffectfulSelect checks if a SELECT statement contains locking clauses or
// side-effectful function calls. String literals are stripped first to avoid false positives.
func isSideEffectfulSelect(query string) bool {
	upper := strings.ToUpper(stripComments(stripStringLiterals(query)))

	// Check for locking clauses
	for _, clause := range lockingClauses {
		idx := strings.Index(upper, clause)
		if idx >= 0 {
			// Check word boundary before
			if idx == 0 || upper[idx-1] == ' ' || upper[idx-1] == '\n' || upper[idx-1] == '\t' || upper[idx-1] == ')' {
				end := idx + len(clause)
				// Check word boundary after
				if end >= len(upper) || upper[end] == ' ' || upper[end] == '\n' || upper[end] == '\t' || upper[end] == ';' {
					return true
				}
			}
		}
	}

	// Check for side-effectful function calls
	if hasSideEffectFunc(upper) {
		return true
	}

	return false
}

// ContainsCopyStatement checks if the query contains a COPY statement.
// Handles leading comments, multi-statement queries, and other SQL formatting.
func ContainsCopyStatement(query string) bool {
	stmts := splitStatements(query)
	for _, stmt := range stmts {
		if firstKeyword(stmt) == "COPY" {
			return true
		}
	}
	return false
}

// extractCopyTable extracts the table name from COPY ... FROM statements.
func extractCopyTable(q, upper string) []string {
	// COPY tablename FROM ... — extract tablename
	// COPY (query) TO ... — subquery, skip
	rest := strings.TrimSpace(q[4:])
	if len(rest) > 0 && rest[0] == '(' {
		return nil // COPY (query) TO — no direct table
	}
	name := extractIdentifier(rest)
	if name == "" {
		return nil
	}
	parts := strings.Split(name, ".")
	final := strings.ToLower(stripQuotes(parts[len(parts)-1]))
	if final != "" {
		return []string{final}
	}
	return nil
}

// extractExplainTables extracts write-target tables from EXPLAIN ANALYZE statements.
func extractExplainTables(q, upper string) []string {
	if !strings.Contains(upper, "ANALYZE") {
		return nil
	}
	// Find the inner statement after EXPLAIN [ANALYZE] [VERBOSE] [options]
	// Simple heuristic: find the first write keyword
	for _, kw := range []struct {
		keyword string
		prefix  string
	}{
		{"INSERT INTO", "INSERT INTO"},
		{"UPDATE", "UPDATE"},
		{"DELETE FROM", "DELETE FROM"},
		{"MERGE INTO", "MERGE INTO"},
		{"MERGE", "MERGE"},
	} {
		idx := strings.Index(upper, kw.keyword)
		if idx >= 0 {
			sub := q[idx:]
			subUpper := upper[idx:]
			t := extractAfter(sub, subUpper, kw.prefix)
			if t != "" {
				return []string{t}
			}
		}
	}
	return nil
}

// isExplainAnalyzeWrite checks if an EXPLAIN statement has ANALYZE and a write sub-query.
// EXPLAIN ANALYZE INSERT/UPDATE/DELETE ... actually executes the query.
func isExplainAnalyzeWrite(stmt string) bool {
	upper := strings.ToUpper(stripComments(stripStringLiterals(stmt)))
	if !strings.Contains(upper, "ANALYZE") {
		return false
	}
	// Check if any write keyword appears after ANALYZE
	for kw := range writeKeywords {
		if kw == "COPY" || kw == "COMMENT" {
			continue // EXPLAIN ANALYZE COPY / COMMENT unlikely
		}
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}

// stripQuotes removes surrounding double quotes from a quoted identifier.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		return strings.ReplaceAll(inner, `""`, `"`)
	}
	return s
}
