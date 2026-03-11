package router

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// FirewallRule identifies which rule blocked a query.
type FirewallRule string

const (
	RuleDeleteNoWhere FirewallRule = "delete_no_where"
	RuleUpdateNoWhere FirewallRule = "update_no_where"
	RuleDropTable     FirewallRule = "drop_table"
	RuleTruncate      FirewallRule = "truncate"
)

// FirewallConfig mirrors config.FirewallConfig for the router package.
type FirewallConfig struct {
	Enabled                 bool
	BlockDeleteWithoutWhere bool
	BlockUpdateWithoutWhere bool
	BlockDropTable          bool
	BlockTruncate           bool
}

// FirewallResult contains the outcome of a firewall check.
type FirewallResult struct {
	Blocked bool
	Rule    FirewallRule
	Message string
}

// CheckFirewall inspects a query against firewall rules using AST parsing.
// Returns a FirewallResult indicating if the query is blocked.
func CheckFirewall(query string, cfg FirewallConfig) FirewallResult {
	if !cfg.Enabled {
		return FirewallResult{}
	}

	tree, err := ParseSQL(query)
	if err != nil {
		// Can't parse → allow (fail-open for firewall; safety is in the DB)
		return FirewallResult{}
	}

	for _, rawStmt := range tree.GetStmts() {
		stmt := rawStmt.GetStmt()
		if stmt == nil {
			continue
		}

		if result := checkNode(stmt, cfg); result.Blocked {
			return result
		}
	}

	return FirewallResult{}
}

func checkNode(node *pg_query.Node, cfg FirewallConfig) FirewallResult {
	switch n := node.GetNode().(type) {
	case *pg_query.Node_DeleteStmt:
		if cfg.BlockDeleteWithoutWhere && n.DeleteStmt.GetWhereClause() == nil {
			table := ""
			if rel := n.DeleteStmt.GetRelation(); rel != nil {
				table = rel.GetRelname()
			}
			return FirewallResult{
				Blocked: true,
				Rule:    RuleDeleteNoWhere,
				Message: fmt.Sprintf("DELETE without WHERE clause on table %q is blocked by firewall", table),
			}
		}

	case *pg_query.Node_UpdateStmt:
		if cfg.BlockUpdateWithoutWhere && n.UpdateStmt.GetWhereClause() == nil {
			table := ""
			if rel := n.UpdateStmt.GetRelation(); rel != nil {
				table = rel.GetRelname()
			}
			return FirewallResult{
				Blocked: true,
				Rule:    RuleUpdateNoWhere,
				Message: fmt.Sprintf("UPDATE without WHERE clause on table %q is blocked by firewall", table),
			}
		}

	case *pg_query.Node_DropStmt:
		if cfg.BlockDropTable {
			return FirewallResult{
				Blocked: true,
				Rule:    RuleDropTable,
				Message: "DROP statement is blocked by firewall",
			}
		}

	case *pg_query.Node_TruncateStmt:
		if cfg.BlockTruncate {
			return FirewallResult{
				Blocked: true,
				Rule:    RuleTruncate,
				Message: "TRUNCATE statement is blocked by firewall",
			}
		}
	}

	return FirewallResult{}
}
