package router

import (
	"log/slog"
	"regexp"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// RewriteActionType identifies the kind of rewrite action.
type RewriteActionType string

const (
	ActionTableRename   RewriteActionType = "table_rename"
	ActionColumnRename  RewriteActionType = "column_rename"
	ActionAddWhere      RewriteActionType = "add_where"
	ActionSchemaQualify RewriteActionType = "schema_qualify"
)

// RewriteRuleMatch defines when a rewrite rule should be applied.
type RewriteRuleMatch struct {
	Tables        []string // table names to match (case-insensitive)
	StatementType []string // SELECT, INSERT, UPDATE, DELETE (empty = all)
	QueryPattern  string   // regex pattern against raw SQL (optional)
}

// RewriteAction defines a single transformation to apply.
type RewriteAction struct {
	Type      RewriteActionType
	From      string // source name (table_rename, column_rename)
	To        string // target name (table_rename, column_rename, schema_qualify)
	Condition string // SQL condition for add_where
}

// RewriteRule is a named, toggleable rewrite rule.
type RewriteRule struct {
	Name    string
	Enabled bool
	Match   RewriteRuleMatch
	Actions []RewriteAction
}

// RewriteResult holds the outcome of applying rewrite rules.
type RewriteResult struct {
	Rewritten    bool
	RewrittenSQL string
	AppliedRules []string
}

// compiledRule caches the compiled regex and pre-parsed add_where conditions.
type compiledRule struct {
	rule    RewriteRule
	pattern *regexp.Regexp // nil if no QueryPattern
}

// Rewriter applies query rewrite rules. It is immutable after construction
// and safe for concurrent use without a mutex (managed via atomic.Pointer in proxy).
type Rewriter struct {
	rules []compiledRule
}

// NewRewriter compiles the given rules and returns a Rewriter.
// Invalid regex patterns are logged and skipped.
func NewRewriter(rules []RewriteRule) *Rewriter {
	compiled := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{rule: r}
		if r.Match.QueryPattern != "" {
			re, err := regexp.Compile(r.Match.QueryPattern)
			if err != nil {
				slog.Warn("rewrite rule regex compile failed, skipping",
					"rule", r.Name, "pattern", r.Match.QueryPattern, "error", err)
				continue
			}
			cr.pattern = re
		}
		compiled = append(compiled, cr)
	}
	return &Rewriter{rules: compiled}
}

// Rules returns the list of active rule names.
func (rw *Rewriter) Rules() []string {
	names := make([]string, len(rw.rules))
	for i, cr := range rw.rules {
		names[i] = cr.rule.Name
	}
	return names
}

// Apply applies all matching rewrite rules to the query.
// The query is always re-parsed internally to avoid mutating a shared AST.
// Fail-open: returns the original query on any error.
func (rw *Rewriter) Apply(query string, pq *ParsedQuery) RewriteResult {
	if len(rw.rules) == 0 {
		return RewriteResult{}
	}

	// Always parse a fresh tree to avoid mutating a shared ParsedQuery's AST.
	// The cost of re-parsing is acceptable since rewriting is not on every query
	// (only when rules are configured) and AST mutation safety is critical.
	tree, err := ParseSQL(query)
	if err != nil {
		return RewriteResult{}
	}

	var appliedRules []string
	modified := false

	for _, cr := range rw.rules {
		if !ruleMatches(query, tree, cr) {
			continue
		}

		ruleModified := false
		for _, action := range cr.rule.Actions {
			if applyAction(tree, action) {
				ruleModified = true
				modified = true
			}
		}

		if ruleModified {
			appliedRules = append(appliedRules, cr.rule.Name)
		}
	}

	if !modified {
		return RewriteResult{}
	}

	deparsed, err := pg_query.Deparse(tree)
	if err != nil {
		slog.Warn("rewrite deparse failed, using original query", "error", err)
		return RewriteResult{}
	}

	return RewriteResult{
		Rewritten:    true,
		RewrittenSQL: deparsed,
		AppliedRules: appliedRules,
	}
}

// ruleMatches checks whether a rule's match conditions are satisfied.
func ruleMatches(query string, tree *pg_query.ParseResult, cr compiledRule) bool {
	m := cr.rule.Match

	// 1. Regex pattern
	if cr.pattern != nil && !cr.pattern.MatchString(query) {
		return false
	}

	// 2. Statement type filter
	if len(m.StatementType) > 0 {
		if !stmtTypeMatches(tree, m.StatementType) {
			return false
		}
	}

	// 3. Table filter
	if len(m.Tables) > 0 {
		if !tablesMatchRule(tree, m.Tables) {
			return false
		}
	}

	return true
}

// stmtTypeMatches checks if the tree contains any of the listed statement types.
func stmtTypeMatches(tree *pg_query.ParseResult, types []string) bool {
	for _, rawStmt := range tree.GetStmts() {
		stmt := rawStmt.GetStmt()
		if stmt == nil {
			continue
		}
		stmtType := nodeStmtType(stmt)
		for _, t := range types {
			if strings.EqualFold(stmtType, t) {
				return true
			}
		}
	}
	return false
}

// nodeStmtType returns the statement type name for a top-level node.
func nodeStmtType(node *pg_query.Node) string {
	switch node.GetNode().(type) {
	case *pg_query.Node_SelectStmt:
		return "SELECT"
	case *pg_query.Node_InsertStmt:
		return "INSERT"
	case *pg_query.Node_UpdateStmt:
		return "UPDATE"
	case *pg_query.Node_DeleteStmt:
		return "DELETE"
	default:
		return ""
	}
}

// tablesMatchRule checks if any table referenced in the tree matches the rule's table list.
func tablesMatchRule(tree *pg_query.ParseResult, tables []string) bool {
	tableSet := make(map[string]bool, len(tables))
	for _, t := range tables {
		tableSet[strings.ToLower(t)] = true
	}

	found := false
	WalkNodes(tree, func(node *pg_query.Node) bool {
		if found {
			return false
		}
		if rv := node.GetRangeVar(); rv != nil {
			name := strings.ToLower(rv.GetRelname())
			if tableSet[name] {
				found = true
				return false
			}
			// Check schema-qualified match: "schema.table"
			if rv.GetSchemaname() != "" {
				qualified := strings.ToLower(rv.GetSchemaname()) + "." + name
				if tableSet[qualified] {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// applyAction applies a single rewrite action to the AST in-place.
// Returns true if the tree was modified.
func applyAction(tree *pg_query.ParseResult, action RewriteAction) bool {
	switch action.Type {
	case ActionTableRename:
		return rewriteTableName(tree, action.From, action.To)
	case ActionColumnRename:
		return rewriteColumnName(tree, action.From, action.To)
	case ActionAddWhere:
		return rewriteAddWhere(tree, action.Condition)
	case ActionSchemaQualify:
		return rewriteSchemaQualify(tree, action.From, action.To)
	default:
		slog.Warn("unknown rewrite action type", "type", action.Type)
		return false
	}
}

// rewriteTableName renames all occurrences of oldName to newName in RangeVar nodes.
func rewriteTableName(tree *pg_query.ParseResult, oldName, newName string) bool {
	oldLower := strings.ToLower(oldName)
	changed := false

	WalkNodes(tree, func(node *pg_query.Node) bool {
		if rv := node.GetRangeVar(); rv != nil {
			if strings.ToLower(rv.GetRelname()) == oldLower {
				rv.Relname = newName
				changed = true
			}
		}
		return true
	})

	return changed
}

// rewriteColumnName renames all occurrences of oldName to newName in ColumnRef nodes.
// Note: This is a global rename — it does not scope to specific tables.
func rewriteColumnName(tree *pg_query.ParseResult, oldName, newName string) bool {
	oldLower := strings.ToLower(oldName)
	changed := false

	WalkNodes(tree, func(node *pg_query.Node) bool {
		if cr := node.GetColumnRef(); cr != nil {
			for _, field := range cr.GetFields() {
				if s := field.GetString_(); s != nil {
					if strings.ToLower(s.GetSval()) == oldLower {
						s.Sval = newName
						changed = true
					}
				}
			}
		}
		return true
	})

	return changed
}

// rewriteAddWhere adds an AND condition to every SELECT/UPDATE/DELETE WHERE clause.
// If no WHERE clause exists, it becomes the sole WHERE condition.
// The condition is re-parsed for each statement to avoid shared AST node references.
func rewriteAddWhere(tree *pg_query.ParseResult, condition string) bool {
	changed := false
	for _, rawStmt := range tree.GetStmts() {
		stmt := rawStmt.GetStmt()
		if stmt == nil {
			continue
		}

		// Parse a fresh condition node per statement to avoid shared references.
		condNode, err := parseCondition(condition)
		if err != nil {
			slog.Warn("rewrite add_where: condition parse failed", "condition", condition, "error", err)
			return false
		}

		switch n := stmt.GetNode().(type) {
		case *pg_query.Node_SelectStmt:
			n.SelectStmt.WhereClause = andWhere(n.SelectStmt.GetWhereClause(), condNode)
			changed = true
		case *pg_query.Node_UpdateStmt:
			n.UpdateStmt.WhereClause = andWhere(n.UpdateStmt.GetWhereClause(), condNode)
			changed = true
		case *pg_query.Node_DeleteStmt:
			n.DeleteStmt.WhereClause = andWhere(n.DeleteStmt.GetWhereClause(), condNode)
			changed = true
		}
	}

	return changed
}

// parseCondition parses a SQL condition expression and returns the WhereClause node.
func parseCondition(condition string) (*pg_query.Node, error) {
	condTree, err := pg_query.Parse("SELECT 1 WHERE " + condition)
	if err != nil {
		return nil, err
	}
	for _, rawStmt := range condTree.GetStmts() {
		if sel := rawStmt.GetStmt().GetSelectStmt(); sel != nil {
			if wc := sel.GetWhereClause(); wc != nil {
				return wc, nil
			}
		}
	}
	return nil, err
}

// andWhere combines an existing WHERE clause with a new condition using AND.
// If existing is nil, returns the new condition directly.
func andWhere(existing, newCond *pg_query.Node) *pg_query.Node {
	if existing == nil {
		return newCond
	}
	return &pg_query.Node{
		Node: &pg_query.Node_BoolExpr{
			BoolExpr: &pg_query.BoolExpr{
				Boolop: pg_query.BoolExprType_AND_EXPR,
				Args:   []*pg_query.Node{existing, newCond},
			},
		},
	}
}

// rewriteSchemaQualify adds a schema prefix to unqualified table references matching tableName.
func rewriteSchemaQualify(tree *pg_query.ParseResult, tableName, schemaName string) bool {
	tableLower := strings.ToLower(tableName)
	changed := false

	WalkNodes(tree, func(node *pg_query.Node) bool {
		if rv := node.GetRangeVar(); rv != nil {
			if strings.ToLower(rv.GetRelname()) == tableLower && rv.GetSchemaname() == "" {
				rv.Schemaname = schemaName
				changed = true
			}
		}
		return true
	})

	return changed
}
