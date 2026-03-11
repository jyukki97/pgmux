package router

import "testing"

func TestClassifyAST(t *testing.T) {
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
		{"ALTER TABLE foo ADD COLUMN col int", QueryWrite},
		{"DROP TABLE foo", QueryWrite},
		{"TRUNCATE users", QueryWrite},
		// Hint comments
		{"/* route:writer */ SELECT * FROM users", QueryWrite},
		{"/* route:reader */ INSERT INTO users VALUES (1)", QueryRead},
		{"/*route:writer*/ SELECT 1", QueryWrite},
		{"/* route:reader */ SELECT 1", QueryRead},
		// Regular comments should not trigger writes
		{"-- comment\nSELECT 1", QueryRead},
		{"/* normal comment */ SELECT 1", QueryRead},
		// GRANT/REVOKE
		{"GRANT SELECT ON users TO readonly", QueryWrite},
		// CREATE INDEX
		{"CREATE INDEX idx ON users (name)", QueryWrite},
		// CREATE VIEW
		{"CREATE VIEW v AS SELECT * FROM users", QueryWrite},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := ClassifyAST(tt.query)
			if got != tt.want {
				t.Errorf("ClassifyAST(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestClassifyAST_MultiStatement(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  QueryType
	}{
		{"insert in multi", "SELECT 1; INSERT INTO users VALUES(1);", QueryWrite},
		{"CTE with update", "WITH x AS (UPDATE users SET score=0 RETURNING id) SELECT * FROM x", QueryWrite},
		{"CTE with delete", "WITH d AS (DELETE FROM old_logs RETURNING id) INSERT INTO archive SELECT * FROM d", QueryWrite},
		{"pure CTE read", "WITH x AS (SELECT * FROM users) SELECT * FROM x", QueryRead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyAST(tt.query)
			if got != tt.want {
				t.Errorf("ClassifyAST(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestClassifyAST_DollarQuoting(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  QueryType
	}{
		{
			"keyword inside $$ should not trigger write",
			"SELECT $$ INSERT INTO users VALUES(1) $$",
			QueryRead,
		},
		{
			"keyword inside $tag$ should not trigger write",
			"SELECT $body$ DELETE FROM users $body$",
			QueryRead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyAST(tt.query)
			if got != tt.want {
				t.Errorf("ClassifyAST(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestClassifyAST_NestedComments(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  QueryType
	}{
		{
			"nested comment hides UPDATE",
			"SELECT /* /* UPDATE admin SET foo='bar' */ */ 1",
			QueryRead,
		},
		{
			"triple nested comment",
			"SELECT /* /* /* */ */ */ 1",
			QueryRead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyAST(tt.query)
			if got != tt.want {
				t.Errorf("ClassifyAST(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestExtractTablesAST(t *testing.T) {
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
			tables := ExtractTablesAST(tt.query)
			if len(tables) == 0 {
				t.Fatalf("ExtractTablesAST(%q) returned empty", tt.query)
			}
			if tables[0] != tt.want {
				t.Errorf("ExtractTablesAST(%q) = %q, want %q", tt.query, tables[0], tt.want)
			}
		})
	}
}

func TestExtractTablesAST_MultiTable(t *testing.T) {
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
			nil, // order may vary, just check count
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
			tables := ExtractTablesAST(tt.query)
			if len(tables) != tt.wantCount {
				t.Errorf("ExtractTablesAST(%q) got %d tables %v, want %d", tt.query, len(tables), tables, tt.wantCount)
				return
			}
			if tt.wantAll != nil {
				for i, want := range tt.wantAll {
					if i >= len(tables) {
						break
					}
					if tables[i] != want {
						t.Errorf("tables[%d] = %q, want %q", i, tables[i], want)
					}
				}
			}
		})
	}
}

func TestExtractTablesAST_QuotedNames(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			"double-quoted table",
			`INSERT INTO "Order Items" VALUES (1)`,
			"order items",
		},
		{
			"quoted schema.table",
			`UPDATE public."my table" SET a=1`,
			"my table",
		},
		{
			"DELETE FROM quoted table",
			`DELETE FROM "user data" WHERE id=1`,
			"user data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tables := ExtractTablesAST(tt.query)
			if len(tables) == 0 {
				t.Fatalf("ExtractTablesAST(%q) returned empty", tt.query)
			}
			if tables[0] != tt.want {
				t.Errorf("ExtractTablesAST(%q) = %q, want %q", tt.query, tables[0], tt.want)
			}
		})
	}
}
