package router

import (
	"testing"
	"time"
)

func TestSession_BasicRouting(t *testing.T) {
	s := NewSession(0, false, false)

	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT → %d, want RouteReader", got)
	}
	if got := s.Route("INSERT INTO users VALUES (1)"); got != RouteWriter {
		t.Errorf("INSERT → %d, want RouteWriter", got)
	}
}

func TestSession_Transaction(t *testing.T) {
	s := NewSession(0, false, false)

	// BEGIN → all queries go to writer
	if got := s.Route("BEGIN"); got != RouteWriter {
		t.Errorf("BEGIN → %d, want RouteWriter", got)
	}
	if got := s.Route("SELECT * FROM users"); got != RouteWriter {
		t.Errorf("SELECT in tx → %d, want RouteWriter", got)
	}
	if got := s.Route("INSERT INTO users VALUES (1)"); got != RouteWriter {
		t.Errorf("INSERT in tx → %d, want RouteWriter", got)
	}

	// COMMIT → back to normal routing
	if got := s.Route("COMMIT"); got != RouteWriter {
		t.Errorf("COMMIT → %d, want RouteWriter", got)
	}
	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT after commit → %d, want RouteReader", got)
	}
}

func TestSession_ReadAfterWriteDelay(t *testing.T) {
	s := NewSession(100*time.Millisecond, false, false)

	// Write
	s.Route("INSERT INTO users VALUES (1)")

	// Read immediately after write → writer
	if got := s.Route("SELECT * FROM users"); got != RouteWriter {
		t.Errorf("SELECT after write → %d, want RouteWriter", got)
	}

	// Wait for delay to expire
	time.Sleep(150 * time.Millisecond)

	// Read after delay → reader
	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT after delay → %d, want RouteReader", got)
	}
}

func TestSession_Rollback(t *testing.T) {
	s := NewSession(0, false, false)

	s.Route("BEGIN")
	if !s.InTransaction() {
		t.Error("expected InTransaction=true after BEGIN")
	}

	s.Route("ROLLBACK")
	if s.InTransaction() {
		t.Error("expected InTransaction=false after ROLLBACK")
	}

	if got := s.Route("SELECT 1"); got != RouteReader {
		t.Errorf("SELECT after rollback → %d, want RouteReader", got)
	}
}

func TestSession_PreparedStatements(t *testing.T) {
	s := NewSession(0, false, false)

	// Register a SELECT prepared statement → reader
	route := s.RegisterStatement("stmt_read", "SELECT * FROM users WHERE id = $1")
	if route != RouteReader {
		t.Errorf("RegisterStatement SELECT → %d, want RouteReader", route)
	}

	// Register an INSERT prepared statement → writer
	route = s.RegisterStatement("stmt_write", "INSERT INTO users (name) VALUES ($1)")
	if route != RouteWriter {
		t.Errorf("RegisterStatement INSERT → %d, want RouteWriter", route)
	}

	// Look up routes
	if got := s.StatementRoute("stmt_read"); got != RouteReader {
		t.Errorf("StatementRoute(stmt_read) → %d, want RouteReader", got)
	}
	if got := s.StatementRoute("stmt_write"); got != RouteWriter {
		t.Errorf("StatementRoute(stmt_write) → %d, want RouteWriter", got)
	}

	// Unknown statement → writer (safe default)
	if got := s.StatementRoute("unknown"); got != RouteWriter {
		t.Errorf("StatementRoute(unknown) → %d, want RouteWriter", got)
	}

	// Close statement
	s.CloseStatement("stmt_read")
	if got := s.StatementRoute("stmt_read"); got != RouteWriter {
		t.Errorf("StatementRoute after close → %d, want RouteWriter", got)
	}
}

func TestSession_PreparedStatement_InTransaction(t *testing.T) {
	s := NewSession(0, false, false)

	// Start transaction
	s.Route("BEGIN")

	// Even a SELECT prepared statement should route to writer in transaction
	route := s.RegisterStatement("tx_stmt", "SELECT * FROM users")
	if route != RouteWriter {
		t.Errorf("RegisterStatement in tx → %d, want RouteWriter", route)
	}

	s.Route("COMMIT")

	// After commit, new registration should route to reader
	route = s.RegisterStatement("post_tx", "SELECT * FROM users")
	if route != RouteReader {
		t.Errorf("RegisterStatement after commit → %d, want RouteReader", route)
	}
}

func TestSession_UnnamedStatement(t *testing.T) {
	s := NewSession(0, false, false)

	// Unnamed statement (empty string) — overwritten on each Parse
	s.RegisterStatement("", "SELECT 1")
	if got := s.StatementRoute(""); got != RouteReader {
		t.Errorf("unnamed SELECT → %d, want RouteReader", got)
	}

	// Overwrite unnamed with a write query
	s.RegisterStatement("", "INSERT INTO t VALUES (1)")
	if got := s.StatementRoute(""); got != RouteWriter {
		t.Errorf("unnamed INSERT → %d, want RouteWriter", got)
	}
}

func TestSession_MultiStatementCommit(t *testing.T) {
	s := NewSession(0, false, false)

	// Start transaction
	s.Route("BEGIN")
	if !s.InTransaction() {
		t.Fatal("expected InTransaction=true")
	}

	// Multi-statement with COMMIT embedded
	s.Route("SELECT 1; COMMIT;")
	if s.InTransaction() {
		t.Error("expected InTransaction=false after 'SELECT 1; COMMIT;'")
	}

	// After commit, reads should go to reader
	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT after multi-stmt COMMIT → %d, want RouteReader", got)
	}
}

func TestSession_MultiStatementBegin(t *testing.T) {
	s := NewSession(0, false, false)

	// Multi-statement with BEGIN embedded
	s.Route("SELECT 1; BEGIN;")
	if !s.InTransaction() {
		t.Error("expected InTransaction=true after 'SELECT 1; BEGIN;'")
	}

	// All subsequent queries should go to writer
	if got := s.Route("SELECT * FROM users"); got != RouteWriter {
		t.Errorf("SELECT in tx → %d, want RouteWriter", got)
	}
}

func TestSession_CausalConsistency_LSNTracking(t *testing.T) {
	s := NewSession(0, true, false)

	// Initially no LSN
	if lsn := s.LastWriteLSN(); !lsn.IsZero() {
		t.Errorf("initial LSN should be zero, got %v", lsn)
	}

	// Write query — timer should NOT be set (causal mode)
	s.Route("INSERT INTO users VALUES (1)")

	// Read after write in causal mode → RouteReader (caller handles LSN-aware routing)
	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT in causal mode → %d, want RouteReader", got)
	}

	// Set LSN externally (simulating server behavior)
	lsn, _ := ParseLSN("0/16B3748")
	s.SetLastWriteLSN(lsn)

	if got := s.LastWriteLSN(); got != lsn {
		t.Errorf("LastWriteLSN = %v, want %v", got, lsn)
	}

	// Read still returns RouteReader (LSN-aware balancer handles fallback)
	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT with LSN set → %d, want RouteReader", got)
	}
}

func TestSession_CausalConsistency_SkipsTimerDelay(t *testing.T) {
	// With causal consistency ON, read_after_write_delay should be ignored
	s := NewSession(100*time.Millisecond, true, false)

	s.Route("INSERT INTO users VALUES (1)")

	// In causal mode, reads should NOT be routed to writer by timer
	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT in causal mode → %d, want RouteReader (timer should be skipped)", got)
	}
}

func TestSession_ASTParser_CTEWithInsert(t *testing.T) {
	// With AST mode on, CTE containing INSERT routes to writer
	s := NewSession(0, false, true)

	query := "WITH ins AS (INSERT INTO users (name) VALUES ('alice') RETURNING id) SELECT * FROM ins"
	if got := s.Route(query); got != RouteWriter {
		t.Errorf("AST mode: CTE with INSERT → %d, want RouteWriter", got)
	}
}

func TestSession_ASTParser_CTEWithUpdate(t *testing.T) {
	// With AST mode on, CTE containing UPDATE routes to writer
	s := NewSession(0, false, true)

	query := "WITH upd AS (UPDATE users SET name = 'bob' WHERE id = 1 RETURNING id) SELECT * FROM upd"
	if got := s.Route(query); got != RouteWriter {
		t.Errorf("AST mode: CTE with UPDATE → %d, want RouteWriter", got)
	}
}

func TestSession_StringParser_CTEWithInsert(t *testing.T) {
	// With AST mode off, same CTE query still uses string-based classification
	s := NewSession(0, false, false)

	query := "WITH ins AS (INSERT INTO users (name) VALUES ('alice') RETURNING id) SELECT * FROM ins"
	if got := s.Route(query); got != RouteWriter {
		t.Errorf("String mode: CTE with INSERT → %d, want RouteWriter", got)
	}
}

func TestSession_ASTParser_SelectRouteReader(t *testing.T) {
	// Standard SELECT routes to reader in both modes
	queryAST := NewSession(0, false, true)
	queryString := NewSession(0, false, false)

	query := "SELECT * FROM users WHERE id = 1"
	if got := queryAST.Route(query); got != RouteReader {
		t.Errorf("AST mode: SELECT → %d, want RouteReader", got)
	}
	if got := queryString.Route(query); got != RouteReader {
		t.Errorf("String mode: SELECT → %d, want RouteReader", got)
	}
}

func TestSession_ASTParser_PreparedStatement(t *testing.T) {
	// Prepared statement routing also respects AST parser setting
	s := NewSession(0, false, true)

	route := s.RegisterStatement("stmt_cte",
		"WITH ins AS (INSERT INTO users (name) VALUES ($1) RETURNING id) SELECT * FROM ins")
	if route != RouteWriter {
		t.Errorf("AST mode: RegisterStatement CTE with INSERT → %d, want RouteWriter", route)
	}

	route = s.RegisterStatement("stmt_select", "SELECT * FROM users WHERE id = $1")
	if route != RouteReader {
		t.Errorf("AST mode: RegisterStatement SELECT → %d, want RouteReader", route)
	}
}

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		query string
		want  int
	}{
		{"SELECT 1", 1},
		{"SELECT 1; SELECT 2;", 2},
		{"INSERT INTO t VALUES ('a;b'); SELECT 1;", 2}, // semicolon inside quotes
		{"", 0},
	}
	for _, tt := range tests {
		got := splitStatements(tt.query)
		if len(got) != tt.want {
			t.Errorf("splitStatements(%q) = %d stmts %v, want %d", tt.query, len(got), got, tt.want)
		}
	}
}
