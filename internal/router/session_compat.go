package router

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// SessionFeature identifies a session-dependent SQL feature
// that is incompatible with transaction pooling.
type SessionFeature string

const (
	FeatureListen       SessionFeature = "listen"
	FeatureUnlisten     SessionFeature = "unlisten"
	FeatureSessionSet   SessionFeature = "session_set"
	FeatureDeclare      SessionFeature = "declare_cursor"
	FeaturePrepare      SessionFeature = "prepare"
	FeatureCreateTemp   SessionFeature = "create_temp"
	FeatureAdvisoryLock SessionFeature = "advisory_lock"
)

// SessionDependencyResult contains the outcome of a session dependency check.
type SessionDependencyResult struct {
	Detected bool
	Feature  SessionFeature
}

// DetectSessionDependency checks if a query uses session-dependent features
// that are incompatible with transaction pooling.
// Uses fast string-based keyword matching.
func DetectSessionDependency(query string) SessionDependencyResult {
	// Try single-statement fast path first
	result := detectSingleStmtDependency(query)
	if result.Detected {
		return result
	}

	// Check for advisory lock functions (substring match)
	if containsSessionAdvisoryLock(query) {
		return SessionDependencyResult{Detected: true, Feature: FeatureAdvisoryLock}
	}

	// Multi-statement: split and check each
	if !isSingleStatement(query) {
		stmts := splitStatements(query)
		for _, stmt := range stmts {
			if r := detectSingleStmtDependency(stmt); r.Detected {
				return r
			}
		}
	}

	return SessionDependencyResult{}
}

// DetectSessionDependencyAST checks for session-dependent features using
// a pre-parsed AST tree. Falls back to string-based detection for advisory locks.
func DetectSessionDependencyAST(pq *ParsedQuery, query string) SessionDependencyResult {
	if pq != nil && pq.Tree != nil {
		for _, rawStmt := range pq.Tree.GetStmts() {
			stmt := rawStmt.GetStmt()
			if stmt == nil {
				continue
			}
			if result := detectNodeDependency(stmt); result.Detected {
				return result
			}
		}
	}

	// Advisory lock: always use string-based detection
	// (function calls require walking the full expression tree)
	if containsSessionAdvisoryLock(query) {
		return SessionDependencyResult{Detected: true, Feature: FeatureAdvisoryLock}
	}

	return SessionDependencyResult{}
}

// detectSingleStmtDependency checks a single statement for session-dependent features.
func detectSingleStmtDependency(query string) SessionDependencyResult {
	// Skip leading whitespace
	i := 0
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r') {
		i++
	}
	if i >= len(query) {
		return SessionDependencyResult{}
	}

	rest := query[i:]
	n := len(rest)
	ch := rest[0] | 0x20 // lowercase first char

	switch ch {
	case 'l': // LISTEN
		if n >= 6 && eqFold6(rest, "LISTEN") && (n == 6 || rest[6] == ' ' || rest[6] == '\t') {
			return SessionDependencyResult{Detected: true, Feature: FeatureListen}
		}
	case 'u': // UNLISTEN
		if n >= 8 && eqFoldN(rest[:8], "UNLISTEN") && (n == 8 || rest[8] == ' ' || rest[8] == '\t' || rest[8] == ';') {
			return SessionDependencyResult{Detected: true, Feature: FeatureUnlisten}
		}
	case 's': // SET (but not SET LOCAL / SET TRANSACTION)
		if n >= 4 && eqFold3(rest, "SET") && (rest[3] == ' ' || rest[3] == '\t') {
			j := 4
			for j < n && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j+5 < n && eqFold5(rest[j:], "LOCAL") && (rest[j+5] == ' ' || rest[j+5] == '\t') {
				return SessionDependencyResult{} // SET LOCAL — transaction-scoped
			}
			if j+11 < n && eqFoldN(rest[j:j+11], "TRANSACTION") {
				return SessionDependencyResult{} // SET TRANSACTION — transaction-scoped
			}
			return SessionDependencyResult{Detected: true, Feature: FeatureSessionSet}
		}
	case 'd': // DECLARE
		if n >= 7 && eqFoldN(rest[:7], "DECLARE") && (n == 7 || rest[7] == ' ' || rest[7] == '\t') {
			return SessionDependencyResult{Detected: true, Feature: FeatureDeclare}
		}
	case 'p': // PREPARE
		if n >= 7 && eqFoldN(rest[:7], "PREPARE") && (n == 7 || rest[7] == ' ' || rest[7] == '\t') {
			return SessionDependencyResult{Detected: true, Feature: FeaturePrepare}
		}
	case 'c': // CREATE TEMP / CREATE TEMPORARY
		if n >= 6 && eqFold6(rest, "CREATE") {
			j := 6
			for j < n && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j+4 <= n && eqFoldN(rest[j:j+4], "TEMP") {
				return SessionDependencyResult{Detected: true, Feature: FeatureCreateTemp}
			}
		}
	}
	return SessionDependencyResult{}
}

// containsSessionAdvisoryLock checks if the query contains session-scoped advisory lock functions.
// Transaction-scoped variants (pg_advisory_xact_lock, pg_try_advisory_xact_lock) are safe
// for connection pooling and are not flagged.
//
// Key insight: "advisory_lock" is a substring of "pg_advisory_lock" and "pg_try_advisory_lock"
// but NOT of "pg_advisory_xact_lock" or "pg_try_advisory_xact_lock".
// This makes a simple Contains check sufficient to distinguish session vs. transaction-scoped.
func containsSessionAdvisoryLock(query string) bool {
	lower := strings.ToLower(query)
	return strings.Contains(lower, "advisory_lock") || strings.Contains(lower, "advisory_unlock")
}

// detectNodeDependency checks a single AST node for session-dependent features.
func detectNodeDependency(node *pg_query.Node) SessionDependencyResult {
	switch n := node.GetNode().(type) {
	case *pg_query.Node_ListenStmt:
		return SessionDependencyResult{Detected: true, Feature: FeatureListen}

	case *pg_query.Node_UnlistenStmt:
		return SessionDependencyResult{Detected: true, Feature: FeatureUnlisten}

	case *pg_query.Node_VariableSetStmt:
		// SET LOCAL and SET TRANSACTION are transaction-scoped (safe)
		if n.VariableSetStmt.GetIsLocal() {
			return SessionDependencyResult{}
		}
		return SessionDependencyResult{Detected: true, Feature: FeatureSessionSet}

	case *pg_query.Node_DeclareCursorStmt:
		return SessionDependencyResult{Detected: true, Feature: FeatureDeclare}

	case *pg_query.Node_PrepareStmt:
		return SessionDependencyResult{Detected: true, Feature: FeaturePrepare}

	case *pg_query.Node_CreateStmt:
		if rel := n.CreateStmt.GetRelation(); rel != nil {
			if rel.GetRelpersistence() == "t" {
				return SessionDependencyResult{Detected: true, Feature: FeatureCreateTemp}
			}
		}
	}
	return SessionDependencyResult{}
}
