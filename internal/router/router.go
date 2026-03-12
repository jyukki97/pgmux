package router

import (
	"strings"
	"sync"
	"time"
)

type Route int

const (
	RouteWriter Route = iota
	RouteReader
)

type Session struct {
	mu                  sync.Mutex
	inTransaction       bool
	lastWriteTime       time.Time
	readAfterWriteDelay time.Duration
	causalConsistency   bool
	lastWriteLSN        LSN
	astParser           bool

	// Prepared statement routing: statement name → route
	stmtRoutes map[string]Route
}

func NewSession(readAfterWriteDelay time.Duration, causalConsistency bool, astParser bool) *Session {
	return &Session{
		readAfterWriteDelay: readAfterWriteDelay,
		causalConsistency:   causalConsistency,
		astParser:           astParser,
		stmtRoutes:          make(map[string]Route),
	}
}

// Route determines where to send the query based on session state and query type.
// Handles semicolon-separated multi-statement queries by scanning all statements.
func (s *Session) Route(query string) Route {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Scan all statements in the query for transaction control
	wasTx := s.inTransaction
	s.updateTransactionState(query)

	// Transaction control statements always go to writer
	if wasTx || s.inTransaction {
		return RouteWriter
	}

	// Check if the query contains any transaction control keywords → writer
	if containsTransactionKeyword(query) {
		return RouteWriter
	}

	var qtype QueryType
	if s.astParser {
		qtype = ClassifyAST(query)
	} else {
		qtype = Classify(query)
	}

	// Write query
	if qtype == QueryWrite {
		if !s.causalConsistency {
			s.lastWriteTime = time.Now()
		}
		// LSN is set externally via SetLastWriteLSN after the write completes
		return RouteWriter
	}

	// Read-after-write protection
	if s.causalConsistency {
		// LSN-based: handled by the caller via LastWriteLSN() + LSN-aware balancer
		// Route returns RouteReader; the server uses session LSN for balancer selection
		return RouteReader
	}

	// Timer-based: send to writer within delay window
	if s.readAfterWriteDelay > 0 && !s.lastWriteTime.IsZero() &&
		time.Since(s.lastWriteTime) < s.readAfterWriteDelay {
		return RouteWriter
	}

	return RouteReader
}

// updateTransactionState scans all statements in a (possibly multi-statement) query
// and updates inTransaction accordingly. Handles "SELECT 1; COMMIT;" correctly.
func (s *Session) updateTransactionState(query string) {
	stmts := splitStatements(query)
	for _, stmt := range stmts {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") {
			s.inTransaction = true
		}
		if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") ||
			strings.HasPrefix(upper, "END") {
			s.inTransaction = false
		}
	}
}

// containsTransactionKeyword checks if any statement starts with BEGIN/COMMIT/ROLLBACK/END.
func containsTransactionKeyword(query string) bool {
	stmts := splitStatements(query)
	for _, stmt := range stmts {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") ||
			strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") ||
			strings.HasPrefix(upper, "END") {
			return true
		}
	}
	return false
}

// splitStatements splits a query string by semicolons, respecting quoted strings.
func splitStatements(query string) []string {
	var stmts []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch {
		case ch == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
			current.WriteByte(ch)
		case ch == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
			current.WriteByte(ch)
		case ch == ';' && !inSingleQuote && !inDoubleQuote:
			s := strings.TrimSpace(current.String())
			if s != "" {
				stmts = append(stmts, s)
			}
			current.Reset()
		default:
			current.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// InTransaction returns whether the session is currently in a transaction.
func (s *Session) InTransaction() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inTransaction
}

// SetInTransaction explicitly sets the transaction state.
// Used by the Extended Query path where transaction control (BEGIN/COMMIT)
// comes via Parse messages rather than Simple Query.
func (s *Session) SetInTransaction(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inTransaction = v
}

// RegisterStatement records the route for a prepared statement.
// The unnamed statement ("") is also tracked and overwritten on each Parse.
func (s *Session) RegisterStatement(name, query string) Route {
	s.mu.Lock()
	defer s.mu.Unlock()

	route := s.routeLocked(query)
	s.stmtRoutes[name] = route
	return route
}

// StatementRoute returns the route for a previously registered prepared statement.
// Returns RouteWriter if the statement is unknown (safe default).
func (s *Session) StatementRoute(name string) Route {
	s.mu.Lock()
	defer s.mu.Unlock()

	if route, ok := s.stmtRoutes[name]; ok {
		return route
	}
	return RouteWriter
}

// SetLastWriteLSN records the WAL LSN after a write query.
func (s *Session) SetLastWriteLSN(lsn LSN) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWriteLSN = lsn
}

// LastWriteLSN returns the last recorded write LSN for LSN-aware routing.
func (s *Session) LastWriteLSN() LSN {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastWriteLSN
}

// CloseStatement removes a prepared statement from the routing map.
func (s *Session) CloseStatement(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stmtRoutes, name)
}

// routeLocked determines the route without locking (caller must hold mu).
func (s *Session) routeLocked(query string) Route {
	upper := strings.ToUpper(strings.TrimSpace(query))

	if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") ||
		strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") {
		return RouteWriter
	}

	if s.inTransaction {
		return RouteWriter
	}

	var qtype QueryType
	if s.astParser {
		qtype = ClassifyAST(query)
	} else {
		qtype = Classify(query)
	}
	if qtype == QueryWrite {
		return RouteWriter
	}

	if s.causalConsistency {
		return RouteReader
	}

	if s.readAfterWriteDelay > 0 && !s.lastWriteTime.IsZero() &&
		time.Since(s.lastWriteTime) < s.readAfterWriteDelay {
		return RouteWriter
	}

	return RouteReader
}
