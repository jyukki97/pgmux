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
func containsWriteKeyword(query string) bool {
	upper := strings.ToUpper(query)
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
	matches := hintRegex.FindStringSubmatch(query)
	if len(matches) >= 2 {
		return matches[1]
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
	// Remove /* ... */ comments
	re := regexp.MustCompile(`/\*.*?\*/`)
	q := re.ReplaceAllString(query, "")

	// Remove -- line comments
	re2 := regexp.MustCompile(`--[^\n]*`)
	q = re2.ReplaceAllString(q, "")

	return q
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
func extractCTETables(query string) []string {
	upper := strings.ToUpper(query)
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
			sub := query[idx:]
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

func extractAfter(query, upper, keyword string) string {
	rest := strings.TrimSpace(query[len(keyword):])
	// Handle optional keywords like "TABLE"
	upperRest := strings.ToUpper(rest)
	if strings.HasPrefix(upperRest, "TABLE ") {
		rest = strings.TrimSpace(rest[6:])
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	// Remove schema prefix and clean up
	name := strings.TrimRight(fields[0], "(;,")
	parts := strings.Split(name, ".")
	return strings.ToLower(parts[len(parts)-1])
}
