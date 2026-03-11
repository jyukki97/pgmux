package router

import (
	pg_query "github.com/pganalyze/pg_query_go/v5"
)

// ParseSQL parses a SQL query string into a PostgreSQL AST.
func ParseSQL(query string) (*pg_query.ParseResult, error) {
	return pg_query.Parse(query)
}

// WalkNodes traverses all nodes in the parse tree depth-first.
// The callback fn is called for each node. If fn returns false, traversal stops.
func WalkNodes(tree *pg_query.ParseResult, fn func(node *pg_query.Node) bool) {
	for _, stmt := range tree.GetStmts() {
		if stmt.GetStmt() != nil {
			if !walkNode(stmt.GetStmt(), fn) {
				return
			}
		}
	}
}

// walkNode recursively visits a node and its children.
func walkNode(node *pg_query.Node, fn func(*pg_query.Node) bool) bool {
	if node == nil {
		return true
	}
	if !fn(node) {
		return false
	}

	// Visit child nodes based on the node type
	switch n := node.GetNode().(type) {
	case *pg_query.Node_SelectStmt:
		s := n.SelectStmt
		for _, target := range s.GetTargetList() {
			if !walkNode(target, fn) {
				return false
			}
		}
		for _, from := range s.GetFromClause() {
			if !walkNode(from, fn) {
				return false
			}
		}
		if !walkNode(s.GetWhereClause(), fn) {
			return false
		}
		for _, w := range s.GetWindowClause() {
			if !walkNode(w, fn) {
				return false
			}
		}
		for _, v := range s.GetValuesLists() {
			if !walkNode(v, fn) {
				return false
			}
		}
		for _, g := range s.GetGroupClause() {
			if !walkNode(g, fn) {
				return false
			}
		}
		if !walkNode(s.GetHavingClause(), fn) {
			return false
		}
		if s.GetLarg() != nil {
			if !walkSelectStmt(s.GetLarg(), fn) {
				return false
			}
		}
		if s.GetRarg() != nil {
			if !walkSelectStmt(s.GetRarg(), fn) {
				return false
			}
		}
		for _, cte := range s.GetWithClause().GetCtes() {
			if !walkNode(cte, fn) {
				return false
			}
		}

	case *pg_query.Node_InsertStmt:
		s := n.InsertStmt
		if s.GetRelation() != nil {
			rv := &pg_query.Node{Node: &pg_query.Node_RangeVar{RangeVar: s.GetRelation()}}
			if !walkNode(rv, fn) {
				return false
			}
		}
		if s.GetSelectStmt() != nil {
			if !walkNode(s.GetSelectStmt(), fn) {
				return false
			}
		}
		for _, cte := range s.GetWithClause().GetCtes() {
			if !walkNode(cte, fn) {
				return false
			}
		}

	case *pg_query.Node_UpdateStmt:
		s := n.UpdateStmt
		if s.GetRelation() != nil {
			rv := &pg_query.Node{Node: &pg_query.Node_RangeVar{RangeVar: s.GetRelation()}}
			if !walkNode(rv, fn) {
				return false
			}
		}
		for _, target := range s.GetTargetList() {
			if !walkNode(target, fn) {
				return false
			}
		}
		if !walkNode(s.GetWhereClause(), fn) {
			return false
		}
		for _, from := range s.GetFromClause() {
			if !walkNode(from, fn) {
				return false
			}
		}
		for _, cte := range s.GetWithClause().GetCtes() {
			if !walkNode(cte, fn) {
				return false
			}
		}

	case *pg_query.Node_DeleteStmt:
		s := n.DeleteStmt
		if s.GetRelation() != nil {
			rv := &pg_query.Node{Node: &pg_query.Node_RangeVar{RangeVar: s.GetRelation()}}
			if !walkNode(rv, fn) {
				return false
			}
		}
		if !walkNode(s.GetWhereClause(), fn) {
			return false
		}
		for _, u := range s.GetUsingClause() {
			if !walkNode(u, fn) {
				return false
			}
		}
		for _, cte := range s.GetWithClause().GetCtes() {
			if !walkNode(cte, fn) {
				return false
			}
		}

	case *pg_query.Node_CommonTableExpr:
		cte := n.CommonTableExpr
		if !walkNode(cte.GetCtequery(), fn) {
			return false
		}

	case *pg_query.Node_JoinExpr:
		j := n.JoinExpr
		if !walkNode(j.GetLarg(), fn) {
			return false
		}
		if !walkNode(j.GetRarg(), fn) {
			return false
		}
		if !walkNode(j.GetQuals(), fn) {
			return false
		}

	case *pg_query.Node_SubLink:
		sl := n.SubLink
		if !walkNode(sl.GetSubselect(), fn) {
			return false
		}
		if !walkNode(sl.GetTestexpr(), fn) {
			return false
		}

	case *pg_query.Node_BoolExpr:
		for _, arg := range n.BoolExpr.GetArgs() {
			if !walkNode(arg, fn) {
				return false
			}
		}

	case *pg_query.Node_AExpr:
		ae := n.AExpr
		if !walkNode(ae.GetLexpr(), fn) {
			return false
		}
		if !walkNode(ae.GetRexpr(), fn) {
			return false
		}

	case *pg_query.Node_ResTarget:
		if !walkNode(n.ResTarget.GetVal(), fn) {
			return false
		}

	case *pg_query.Node_FuncCall:
		for _, arg := range n.FuncCall.GetArgs() {
			if !walkNode(arg, fn) {
				return false
			}
		}

	case *pg_query.Node_List:
		for _, item := range n.List.GetItems() {
			if !walkNode(item, fn) {
				return false
			}
		}
	}

	return true
}

// walkSelectStmt wraps a SelectStmt as a Node and walks it.
func walkSelectStmt(s *pg_query.SelectStmt, fn func(*pg_query.Node) bool) bool {
	node := &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: s}}
	return walkNode(node, fn)
}
