package router

import (
	"log/slog"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// ClassifyAST determines whether a query is a read or write operation using AST parsing.
// Falls back to string-based Classify on parse errors.
func ClassifyAST(query string) QueryType {
	// 1. Check for routing hint (same logic as string parser)
	if hint := extractHint(query); hint != "" {
		if hint == "writer" {
			return QueryWrite
		}
		return QueryRead
	}

	// 2. Parse SQL to AST
	tree, err := ParseSQL(query)
	if err != nil {
		slog.Debug("AST parse failed, fallback to string parser", "error", err)
		return Classify(query)
	}

	// 3. Check each statement
	for _, rawStmt := range tree.GetStmts() {
		stmt := rawStmt.GetStmt()
		if stmt == nil {
			continue
		}
		if isWriteNode(stmt) {
			return QueryWrite
		}
	}

	return QueryRead
}

// isWriteNode checks if a node represents a write operation.
func isWriteNode(node *pg_query.Node) bool {
	switch n := node.GetNode().(type) {
	case *pg_query.Node_InsertStmt:
		return true
	case *pg_query.Node_UpdateStmt:
		return true
	case *pg_query.Node_DeleteStmt:
		return true
	case *pg_query.Node_CreateStmt:
		return true
	case *pg_query.Node_AlterTableStmt:
		return true
	case *pg_query.Node_DropStmt:
		return true
	case *pg_query.Node_TruncateStmt:
		return true
	case *pg_query.Node_GrantStmt:
		return true
	case *pg_query.Node_GrantRoleStmt:
		return true
	case *pg_query.Node_CreateSchemaStmt:
		return true
	case *pg_query.Node_IndexStmt:
		return true
	case *pg_query.Node_CreateSeqStmt:
		return true
	case *pg_query.Node_AlterSeqStmt:
		return true
	case *pg_query.Node_ViewStmt:
		return true
	case *pg_query.Node_CreateFunctionStmt:
		return true
	case *pg_query.Node_CreateTrigStmt:
		return true
	case *pg_query.Node_RenameStmt:
		return true
	case *pg_query.Node_CommentStmt:
		return true
	case *pg_query.Node_SelectStmt:
		// CTE with write operations: WITH ... AS (INSERT/UPDATE/DELETE ...)
		s := n.SelectStmt
		if s.GetWithClause() != nil {
			for _, cte := range s.GetWithClause().GetCtes() {
				if ce := cte.GetCommonTableExpr(); ce != nil {
					if ce.GetCtequery() != nil && isWriteNode(ce.GetCtequery()) {
						return true
					}
				}
			}
		}
		return false
	default:
		return false
	}
}

// ExtractTablesAST extracts table names from write queries using AST parsing.
// Falls back to string-based ExtractTables on parse errors.
func ExtractTablesAST(query string) []string {
	tree, err := ParseSQL(query)
	if err != nil {
		slog.Debug("AST parse failed for table extraction, fallback", "error", err)
		return ExtractTables(query)
	}

	seen := make(map[string]bool)
	var tables []string

	for _, rawStmt := range tree.GetStmts() {
		stmt := rawStmt.GetStmt()
		if stmt == nil {
			continue
		}

		extractWriteTables(stmt, func(table string) {
			t := strings.ToLower(table)
			if t != "" && !seen[t] {
				seen[t] = true
				tables = append(tables, t)
			}
		})
	}

	return tables
}

// extractWriteTables collects table names from write operations.
func extractWriteTables(node *pg_query.Node, add func(string)) {
	switch n := node.GetNode().(type) {
	case *pg_query.Node_InsertStmt:
		extractCTEWriteTables(n.InsertStmt.GetWithClause(), add)
		if rel := n.InsertStmt.GetRelation(); rel != nil {
			add(rel.GetRelname())
		}
	case *pg_query.Node_UpdateStmt:
		extractCTEWriteTables(n.UpdateStmt.GetWithClause(), add)
		if rel := n.UpdateStmt.GetRelation(); rel != nil {
			add(rel.GetRelname())
		}
	case *pg_query.Node_DeleteStmt:
		extractCTEWriteTables(n.DeleteStmt.GetWithClause(), add)
		if rel := n.DeleteStmt.GetRelation(); rel != nil {
			add(rel.GetRelname())
		}
	case *pg_query.Node_TruncateStmt:
		for _, arg := range n.TruncateStmt.GetRelations() {
			if rv := arg.GetRangeVar(); rv != nil {
				add(rv.GetRelname())
			}
		}
	case *pg_query.Node_DropStmt:
		for _, obj := range n.DropStmt.GetObjects() {
			if list := obj.GetList(); list != nil {
				for _, item := range list.GetItems() {
					if s := item.GetString_(); s != nil {
						add(s.GetSval())
					}
				}
			}
		}
	case *pg_query.Node_SelectStmt:
		extractCTEWriteTables(n.SelectStmt.GetWithClause(), add)
	}
}

// extractCTEWriteTables extracts write table names from CTE clauses.
func extractCTEWriteTables(wc *pg_query.WithClause, add func(string)) {
	if wc == nil {
		return
	}
	for _, cte := range wc.GetCtes() {
		if ce := cte.GetCommonTableExpr(); ce != nil {
			if ce.GetCtequery() != nil {
				extractWriteTables(ce.GetCtequery(), add)
			}
		}
	}
}
