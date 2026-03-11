package router

import (
	"testing"
	"time"
)

func TestSession_BasicRouting(t *testing.T) {
	s := NewSession(0)

	if got := s.Route("SELECT * FROM users"); got != RouteReader {
		t.Errorf("SELECT → %d, want RouteReader", got)
	}
	if got := s.Route("INSERT INTO users VALUES (1)"); got != RouteWriter {
		t.Errorf("INSERT → %d, want RouteWriter", got)
	}
}

func TestSession_Transaction(t *testing.T) {
	s := NewSession(0)

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
	s := NewSession(100 * time.Millisecond)

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
	s := NewSession(0)

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
	s := NewSession(0)

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
	s := NewSession(0)

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
	s := NewSession(0)

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
