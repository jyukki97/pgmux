package router

import (
	"regexp"
	"strings"
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
}

var hintRegex = regexp.MustCompile(`/\*\s*route:(writer|reader)\s*\*/`)

// Classify determines whether a query is a read or write operation.
// For multi-statement queries (semicolon-separated), returns QueryWrite if any statement is a write.
func Classify(query string) QueryType {
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
	}
	return QueryRead
}

// containsWriteKeyword checks if a WITH/CTE query contains write operations.
// String literals are stripped first to avoid false positives from keywords inside quotes.
func containsWriteKeyword(query string) bool {
	upper := strings.ToUpper(stripStringLiterals(query))
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

// stripQuotes removes surrounding double quotes from a quoted identifier.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		return strings.ReplaceAll(inner, `""`, `"`)
	}
	return s
}
