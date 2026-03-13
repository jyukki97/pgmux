package router

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		query string
		want  QueryType
	}{
		{"SELECT * FROM users", QueryRead},
		{"select * from users", QueryRead},
		{"  SELECT 1", QueryRead},
		{"SHOW tables", QueryRead},
		{"EXPLAIN SELECT 1", QueryRead},
		{"INSERT INTO users VALUES (1)", QueryWrite},
		{"insert into users values (1)", QueryWrite},
		{"UPDATE users SET name = 'a'", QueryWrite},
		{"DELETE FROM users WHERE id = 1", QueryWrite},
		{"CREATE TABLE foo (id int)", QueryWrite},
		{"ALTER TABLE foo ADD col int", QueryWrite},
		{"DROP TABLE foo", QueryWrite},
		{"TRUNCATE users", QueryWrite},
		// Hint comments
		{"/* route:writer */ SELECT * FROM users", QueryWrite},
		{"/* route:reader */ INSERT INTO users VALUES (1)", QueryRead},
		{"/*route:writer*/ SELECT 1", QueryWrite},
		{"/* route:reader */ SELECT 1", QueryRead},
		// Regular comments should be stripped
		{"-- comment\nSELECT 1", QueryRead},
		{"/* normal comment */ SELECT 1", QueryRead},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := Classify(tt.query)
			if got != tt.want {
				t.Errorf("Classify(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestClassify_MultiStatement(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  QueryType
	}{
		{"select then commit", "SELECT 1; COMMIT;", QueryRead},
		{"insert in multi", "SELECT 1; INSERT INTO users VALUES(1);", QueryWrite},
		{"CTE with update", "WITH x AS (UPDATE users SET score=0 RETURNING id) SELECT * FROM x", QueryWrite},
		{"CTE with delete", "WITH d AS (DELETE FROM old_logs RETURNING id) INSERT INTO archive SELECT * FROM d", QueryWrite},
		{"pure CTE read", "WITH x AS (SELECT * FROM users) SELECT * FROM x", QueryRead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.query)
			if got != tt.want {
				t.Errorf("Classify(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestExtractTables_MultiTable(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantCount int
		wantAll   []string
	}{
		{
			"CTE with two writes",
			"WITH x AS (UPDATE users SET score=0) UPDATE ranking SET total=0",
			2,
			[]string{"users", "ranking"},
		},
		{
			"multi-statement insert+delete",
			"INSERT INTO users VALUES(1); DELETE FROM logs WHERE id=1;",
			2,
			[]string{"users", "logs"},
		},
		{
			"CTE delete into insert",
			"WITH d AS (DELETE FROM old_logs RETURNING id) INSERT INTO archive SELECT * FROM d",
			2,
			[]string{"archive", "old_logs"}, // INSERT INTO found before DELETE FROM in keyword scan
		},
		{
			"single table",
			"UPDATE orders SET status='done'",
			1,
			[]string{"orders"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tables := ExtractTables(tt.query)
			if len(tables) != tt.wantCount {
				t.Errorf("ExtractTables(%q) got %d tables %v, want %d", tt.query, len(tables), tables, tt.wantCount)
				return
			}
			for i, want := range tt.wantAll {
				if i >= len(tables) {
					break
				}
				if tables[i] != want {
					t.Errorf("tables[%d] = %q, want %q", i, tables[i], want)
				}
			}
		})
	}
}

func TestExtractTables(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"INSERT INTO users VALUES (1)", "users"},
		{"insert into users values (1)", "users"},
		{"UPDATE orders SET status = 'done'", "orders"},
		{"DELETE FROM products WHERE id = 1", "products"},
		{"TRUNCATE TABLE logs", "logs"},
		{"INSERT INTO public.users VALUES (1)", "users"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			tables := ExtractTables(tt.query)
			if len(tables) == 0 {
				t.Fatalf("ExtractTables(%q) returned empty", tt.query)
			}
			if tables[0] != tt.want {
				t.Errorf("ExtractTables(%q) = %q, want %q", tt.query, tables[0], tt.want)
			}
		})
	}
}

func TestExtractReadTables(t *testing.T) {
	tests := []struct {
		name string
		query string
		want []string
	}{
		{
			"simple select",
			"SELECT * FROM users",
			[]string{"users"},
		},
		{
			"lowercase select",
			"select * from orders",
			[]string{"orders"},
		},
		{
			"select with schema",
			"SELECT * FROM public.users",
			[]string{"users"},
		},
		{
			"select with join",
			"SELECT * FROM users JOIN orders ON users.id = orders.user_id",
			[]string{"users", "orders"},
		},
		{
			"select with left join",
			"SELECT * FROM users LEFT JOIN orders ON users.id = orders.user_id",
			[]string{"users", "orders"},
		},
		{
			"select with multiple joins",
			"SELECT * FROM users JOIN orders ON users.id = orders.user_id JOIN products ON orders.product_id = products.id",
			[]string{"users", "orders", "products"},
		},
		{
			"write query returns empty",
			"INSERT INTO users VALUES (1)",
			nil,
		},
		{
			"update query returns empty",
			"UPDATE users SET name = 'a'",
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tables := ExtractReadTables(tt.query)
			if len(tables) != len(tt.want) {
				t.Errorf("ExtractReadTables(%q) got %d tables %v, want %d %v", tt.query, len(tables), tables, len(tt.want), tt.want)
				return
			}
			for i, w := range tt.want {
				if tables[i] != w {
					t.Errorf("tables[%d] = %q, want %q", i, tables[i], w)
				}
			}
		})
	}
}

// === QA Report Regression Tests (extended cases) ===

// #4: Dollar Quoting — additional cases beyond dollar_quote_test.go
func TestClassify_DollarQuoting_Extended(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  QueryType
	}{
		{
			"hint inside $tag$ should be ignored",
			"SELECT * FROM users WHERE note = $body$ /* route:writer */ $body$",
			QueryRead,
		},
		{
			"real hint outside $$ should still work",
			"/* route:writer */ SELECT * FROM users WHERE note = $$ harmless $$",
			QueryWrite,
		},
		{
			"$$ with no closing tag",
			"SELECT $$ open-ended",
			QueryRead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.query)
			if got != tt.want {
				t.Errorf("Classify(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

// #5: Nested Block Comments — additional cases beyond nested_comment_test.go
func TestClassify_NestedComments_Extended(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  QueryType
	}{
		{
			"nested comment hides UPDATE",
			"SELECT /* /* */ UPDATE admin SET foo='bar' */ 1",
			QueryRead,
		},
		{
			"triple nested comment",
			"SELECT /* /* /* */ */ */ 1",
			QueryRead,
		},
		{
			"nested comment with hint inside should be ignored",
			"/* /* route:writer */ */ SELECT 1",
			QueryRead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.query)
			if got != tt.want {
				t.Errorf("Classify(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

// #6: Quoted table names — additional cases beyond quoted_table_test.go
func TestExtractTables_QuotedNames_Extended(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			"double-quoted table with schema",
			`UPDATE public."my table" SET a=1`,
			"my table",
		},
		{
			"INSERT INTO quoted table",
			`INSERT INTO "Order Items" VALUES (1)`,
			"order items",
		},
		{
			"DELETE FROM quoted table",
			`DELETE FROM "user data" WHERE id=1`,
			"user data",
		},
		{
			"TRUNCATE quoted table",
			`TRUNCATE TABLE "audit log"`,
			"audit log",
		},
		{
			"quoted schema and quoted table",
			`UPDATE "my schema"."my table" SET a=1`,
			"my table",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tables := ExtractTables(tt.query)
			if len(tables) == 0 {
				t.Fatalf("ExtractTables(%q) returned empty", tt.query)
			}
			if tables[0] != tt.want {
				t.Errorf("ExtractTables(%q) = %q, want %q", tt.query, tables[0], tt.want)
			}
		})
	}
}

func TestExtractTimeoutHint(t *testing.T) {
	tests := []struct {
		query string
		want  time.Duration
	}{
		// Basic hints
		{"/* timeout:5s */ SELECT * FROM users", 5 * time.Second},
		{"/* timeout:500ms */ SELECT 1", 500 * time.Millisecond},
		{"/* timeout:1m */ SELECT 1", time.Minute},
		{"/*timeout:10s*/ SELECT 1", 10 * time.Second},
		{"/* timeout:2s */ SELECT 1", 2 * time.Second},
		// No hint
		{"SELECT * FROM users", 0},
		{"/* route:writer */ SELECT 1", 0},
		{"-- timeout:5s\nSELECT 1", 0},
		// Combined with route hint
		{"/* timeout:3s */ /* route:writer */ SELECT 1", 3 * time.Second},
		// Inside string literal — should be ignored
		{"SELECT '/* timeout:5s */' FROM users", 0},
		// Invalid duration
		{"/* timeout:abc */ SELECT 1", 0},
		// Zero is not useful
		{"/* timeout:0s */ SELECT 1", 0},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := ExtractTimeoutHint(tt.query)
			if got != tt.want {
				t.Errorf("ExtractTimeoutHint(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}
