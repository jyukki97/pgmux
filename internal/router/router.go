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

	// Prepared statement routing: statement name → route
	stmtRoutes map[string]Route
}

func NewSession(readAfterWriteDelay time.Duration) *Session {
	return &Session{
		readAfterWriteDelay: readAfterWriteDelay,
		stmtRoutes:          make(map[string]Route),
	}
}

// Route determines where to send the query based on session state and query type.
func (s *Session) Route(query string) Route {
	s.mu.Lock()
	defer s.mu.Unlock()

	upper := strings.ToUpper(strings.TrimSpace(query))

	// Track transaction state
	if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") {
		s.inTransaction = true
		return RouteWriter
	}
	if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") {
		s.inTransaction = false
		return RouteWriter
	}

	// All queries in a transaction go to writer
	if s.inTransaction {
		return RouteWriter
	}

	qtype := Classify(query)

	// Write query
	if qtype == QueryWrite {
		s.lastWriteTime = time.Now()
		return RouteWriter
	}

	// Read-after-write: send to writer within delay window
	if s.readAfterWriteDelay > 0 && !s.lastWriteTime.IsZero() &&
		time.Since(s.lastWriteTime) < s.readAfterWriteDelay {
		return RouteWriter
	}

	return RouteReader
}

// InTransaction returns whether the session is currently in a transaction.
func (s *Session) InTransaction() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inTransaction
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

	if Classify(query) == QueryWrite {
		return RouteWriter
	}

	if s.readAfterWriteDelay > 0 && !s.lastWriteTime.IsZero() &&
		time.Since(s.lastWriteTime) < s.readAfterWriteDelay {
		return RouteWriter
	}

	return RouteReader
}
