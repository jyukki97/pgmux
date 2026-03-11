package router

import (
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v5"
)

func TestParseSQL(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{"simple select", "SELECT 1", false},
		{"insert", "INSERT INTO users (name) VALUES ('alice')", false},
		{"update", "UPDATE users SET name = 'bob' WHERE id = 1", false},
		{"delete", "DELETE FROM users WHERE id = 1", false},
		{"create table", "CREATE TABLE t (id INT)", false},
		{"multi-statement", "SELECT 1; SELECT 2", false},
		{"complex join", "SELECT u.name FROM users u JOIN orders o ON u.id = o.user_id WHERE o.total > 100", false},
		{"CTE", "WITH cte AS (SELECT 1) SELECT * FROM cte", false},
		{"subquery", "SELECT * FROM (SELECT 1 AS n) sub", false},
		{"dollar quoting", "SELECT $$hello world$$", false},
		{"invalid sql", "SELECTT", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, err := ParseSQL(tt.query)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseSQL(%q) error = %v, wantErr %v", tt.query, err, tt.wantErr)
			}
			if !tt.wantErr && len(tree.GetStmts()) == 0 {
				t.Errorf("ParseSQL(%q) returned no statements", tt.query)
			}
		})
	}
}

func TestWalkNodes_CollectsRangeVars(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"SELECT * FROM users", []string{"users"}},
		{"SELECT * FROM users u JOIN orders o ON u.id = o.user_id", []string{"users", "orders"}},
		{"INSERT INTO users (name) VALUES ('alice')", []string{"users"}},
		{"UPDATE users SET name = 'bob' WHERE id = 1", []string{"users"}},
		{"DELETE FROM users WHERE id = 1", []string{"users"}},
		{"SELECT * FROM (SELECT 1) sub", []string{}}, // subquery, no RangeVar
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			tree, err := ParseSQL(tt.query)
			if err != nil {
				t.Fatalf("ParseSQL: %v", err)
			}

			var tables []string
			WalkNodes(tree, func(node *pg_query.Node) bool {
				if rv := node.GetRangeVar(); rv != nil {
					tables = append(tables, rv.GetRelname())
				}
				return true
			})

			if len(tables) != len(tt.want) {
				t.Errorf("tables = %v, want %v", tables, tt.want)
				return
			}
			for i, got := range tables {
				if got != tt.want[i] {
					t.Errorf("tables[%d] = %q, want %q", i, got, tt.want[i])
				}
			}
		})
	}
}

func TestWalkNodes_StopsEarly(t *testing.T) {
	tree, err := ParseSQL("SELECT * FROM a JOIN b ON true JOIN c ON true")
	if err != nil {
		t.Fatal(err)
	}

	var visited int
	WalkNodes(tree, func(node *pg_query.Node) bool {
		if rv := node.GetRangeVar(); rv != nil {
			visited++
			if visited >= 2 {
				return false // stop after 2 RangeVars
			}
		}
		return true
	})

	if visited != 2 {
		t.Errorf("visited = %d, want 2 (early stop)", visited)
	}
}

func TestWalkNodes_CTE(t *testing.T) {
	tree, err := ParseSQL("WITH cte AS (SELECT * FROM users) SELECT * FROM cte")
	if err != nil {
		t.Fatal(err)
	}

	var tables []string
	WalkNodes(tree, func(node *pg_query.Node) bool {
		if rv := node.GetRangeVar(); rv != nil {
			tables = append(tables, rv.GetRelname())
		}
		return true
	})

	// Should find "users" from the CTE body and "cte" from the main query
	found := map[string]bool{}
	for _, t := range tables {
		found[t] = true
	}
	if !found["users"] {
		t.Error("missing 'users' table from CTE body")
	}
	if !found["cte"] {
		t.Error("missing 'cte' table reference")
	}
}

func TestParseSQL_MultiStatement(t *testing.T) {
	tree, err := ParseSQL("SELECT 1; INSERT INTO users VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}

	stmts := tree.GetStmts()
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(stmts))
	}

	// First should be SelectStmt
	if stmts[0].GetStmt().GetSelectStmt() == nil {
		t.Error("first statement should be SelectStmt")
	}

	// Second should be InsertStmt
	if stmts[1].GetStmt().GetInsertStmt() == nil {
		t.Error("second statement should be InsertStmt")
	}
}
