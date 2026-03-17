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

	// Prepared statement write tracking: statement name → is write query
	stmtWrite map[string]bool

	// Session pinning: when true, all queries are routed to writer
	// and the backend connection is held for the session lifetime.
	pinned       bool
	pinnedReason string
}

func NewSession(readAfterWriteDelay time.Duration, causalConsistency bool, astParser bool) *Session {
	return &Session{
		readAfterWriteDelay: readAfterWriteDelay,
		causalConsistency:   causalConsistency,
		astParser:           astParser,
		stmtRoutes:          make(map[string]Route),
		stmtWrite:           make(map[string]bool),
	}
}

// RouteWithTxState determines the route and returns before/after transaction state in a single lock.
// Eliminates the 2 extra InTransaction() calls (3 lock acquisitions → 1).
func (s *Session) RouteWithTxState(query string) (route Route, wasInTx, nowInTx bool) {
	s.mu.Lock()
	wasInTx = s.inTransaction
	route = s.routeQueryLocked(query)
	nowInTx = s.inTransaction
	s.mu.Unlock()
	return
}

// Route determines where to send the query based on session state and query type.
// Handles semicolon-separated multi-statement queries by scanning all statements.
func (s *Session) Route(query string) Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routeQueryLocked(query)
}

// routeQueryLocked is the lock-free inner routing logic. Caller must hold s.mu.
func (s *Session) routeQueryLocked(query string) Route {

	// Fast path for simple queries: check first keyword without splitting.
	// Most queries are single-statement without transaction control.
	// Handle trailing semicolons (common: "SELECT ... WHERE aid = 123;")
	// Zero-allocation: use hasTxPrefix instead of strings.ToUpper.
	hasTxKeyword := false
	if isSingleStatement(query) {
		if tx := hasTxPrefix(query); tx > 0 {
			if tx == 1 { // BEGIN/START TRANSACTION
				s.inTransaction = true
			} else { // COMMIT/ROLLBACK/END
				s.inTransaction = false
			}
			hasTxKeyword = true
		}
	} else {
		// Multi-statement: scan all for transaction control
		wasTx := s.inTransaction
		s.updateTransactionState(query)
		if s.pinned || wasTx || s.inTransaction {
			return RouteWriter
		}
		if containsTransactionKeyword(query) {
			return RouteWriter
		}
	}

	// Pinned session or transaction control → always writer
	if s.pinned || hasTxKeyword || s.inTransaction {
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

// splitStatements splits a query string by semicolons, respecting quoted strings,
// dollar quoting ($$...$$, $tag$...$tag$), line comments (-- ...), and
// block comments (/* ... */ including nested).
func splitStatements(query string) []string {
	var stmts []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(query); i++ {
		ch := query[i]

		// --- Dollar quoting (only outside regular quotes) ---
		if ch == '$' && !inSingleQuote && !inDoubleQuote {
			tag, ok := parseDollarTag(query, i)
			if ok {
				// Write opening tag
				current.WriteString(tag)
				// Find closing tag
				end := strings.Index(query[i+len(tag):], tag)
				if end >= 0 {
					// Write body + closing tag
					current.WriteString(query[i+len(tag) : i+len(tag)+end])
					current.WriteString(tag)
					i += len(tag) + end + len(tag) - 1
				} else {
					// No closing tag — rest of query is dollar-quoted
					current.WriteString(query[i+len(tag):])
					i = len(query) - 1
				}
				continue
			}
		}

		// --- Line comment: -- (only outside quotes) ---
		if ch == '-' && !inSingleQuote && !inDoubleQuote &&
			i+1 < len(query) && query[i+1] == '-' {
			// Consume until end of line (or end of query)
			for i < len(query) && query[i] != '\n' {
				current.WriteByte(query[i])
				i++
			}
			if i < len(query) {
				current.WriteByte(query[i]) // write the '\n'
			}
			continue
		}

		// --- Block comment: /* ... */ with nesting (only outside quotes) ---
		if ch == '/' && !inSingleQuote && !inDoubleQuote &&
			i+1 < len(query) && query[i+1] == '*' {
			depth := 1
			current.WriteByte('/')
			current.WriteByte('*')
			i += 2
			for i < len(query) && depth > 0 {
				if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
					depth++
					current.WriteByte('/')
					current.WriteByte('*')
					i += 2
				} else if i+1 < len(query) && query[i] == '*' && query[i+1] == '/' {
					depth--
					current.WriteByte('*')
					current.WriteByte('/')
					i += 2
				} else {
					current.WriteByte(query[i])
					i++
				}
			}
			i-- // outer loop will i++
			continue
		}

		switch {
		case ch == '\'' && !inDoubleQuote:
			current.WriteByte(ch)
			if inSingleQuote {
				// Check for escaped quote ('')
				if i+1 < len(query) && query[i+1] == '\'' {
					current.WriteByte('\'')
					i++
				} else {
					inSingleQuote = false
				}
			} else {
				inSingleQuote = true
			}
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

// hasTxPrefix checks if a (single-statement) query starts with a transaction
// control keyword. Returns 0 if no match, 1 for BEGIN/START TRANSACTION,
// 2 for COMMIT/ROLLBACK/END. Zero-allocation: uses byte-level comparison
// instead of strings.ToUpper.
func hasTxPrefix(query string) int {
	// Skip leading whitespace
	i := 0
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r') {
		i++
	}
	if i >= len(query) {
		return 0
	}

	rest := query[i:]
	n := len(rest)

	// Check first byte (case-insensitive) to branch quickly
	ch := rest[0] | 0x20 // lowercase
	switch ch {
	case 'b': // BEGIN
		if n >= 5 && eqFold5(rest, "BEGIN") && (n == 5 || isSpace(rest[5]) || rest[5] == ';') {
			return 1
		}
	case 's': // START TRANSACTION
		if n >= 17 && eqFoldN(rest[:17], "START TRANSACTION") && (n == 17 || isSpace(rest[17]) || rest[17] == ';') {
			return 1
		}
	case 'c': // COMMIT
		if n >= 6 && eqFold6(rest, "COMMIT") && (n == 6 || isSpace(rest[6]) || rest[6] == ';') {
			return 2
		}
	case 'r': // ROLLBACK
		if n >= 8 && eqFoldN(rest[:8], "ROLLBACK") && (n == 8 || isSpace(rest[8]) || rest[8] == ';') {
			return 2
		}
	case 'e': // END
		if n >= 3 && eqFold3(rest, "END") && (n == 3 || isSpace(rest[3]) || rest[3] == ';') {
			return 2
		}
	}
	return 0
}

// eqFold3 checks case-insensitive equality for 3 bytes.
func eqFold3(s string, target string) bool {
	return (s[0]|0x20) == (target[0]|0x20) &&
		(s[1]|0x20) == (target[1]|0x20) &&
		(s[2]|0x20) == (target[2]|0x20)
}

// eqFold5 checks case-insensitive equality for 5 bytes.
func eqFold5(s string, target string) bool {
	return (s[0]|0x20) == (target[0]|0x20) &&
		(s[1]|0x20) == (target[1]|0x20) &&
		(s[2]|0x20) == (target[2]|0x20) &&
		(s[3]|0x20) == (target[3]|0x20) &&
		(s[4]|0x20) == (target[4]|0x20)
}

// eqFold6 checks case-insensitive equality for 6 bytes.
func eqFold6(s string, target string) bool {
	return (s[0]|0x20) == (target[0]|0x20) &&
		(s[1]|0x20) == (target[1]|0x20) &&
		(s[2]|0x20) == (target[2]|0x20) &&
		(s[3]|0x20) == (target[3]|0x20) &&
		(s[4]|0x20) == (target[4]|0x20) &&
		(s[5]|0x20) == (target[5]|0x20)
}

// eqFoldN checks case-insensitive equality for N bytes.
func eqFoldN(s string, target string) bool {
	if len(s) < len(target) {
		return false
	}
	for i := 0; i < len(target); i++ {
		if (s[i] | 0x20) != (target[i] | 0x20) {
			return false
		}
	}
	return true
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// isSingleStatement returns true if the query contains at most one statement.
// A trailing semicolon (e.g., "SELECT 1;") is treated as a single statement.
// This avoids expensive splitStatements for the common case.
func isSingleStatement(query string) bool {
	idx := strings.IndexByte(query, ';')
	if idx < 0 {
		return true // no semicolon at all
	}
	// Check if the semicolon is at the end (only trailing whitespace after it)
	rest := strings.TrimSpace(query[idx+1:])
	return rest == ""
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

	// Track write classification for read-only mode enforcement
	var qtype QueryType
	if s.astParser {
		qtype = ClassifyAST(query)
	} else {
		qtype = Classify(query)
	}
	s.stmtWrite[name] = (qtype == QueryWrite)

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

// StatementIsWrite returns whether a previously registered prepared statement
// is a write query. Returns true for unknown statements (safe default).
func (s *Session) StatementIsWrite(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if isWrite, ok := s.stmtWrite[name]; ok {
		return isWrite
	}
	return true // safe default: treat unknown as write
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

// Pin marks the session as pinned to the writer backend.
// Once pinned, all subsequent queries are routed to writer and the backend
// connection is held for the session lifetime.
func (s *Session) Pin(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pinned {
		s.pinned = true
		s.pinnedReason = reason
	}
}

// Pinned returns whether the session is pinned to the writer.
func (s *Session) Pinned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned
}

// PinnedReason returns the feature that caused the session to be pinned.
func (s *Session) PinnedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinnedReason
}

// CloseStatement removes a prepared statement from the routing map.
func (s *Session) CloseStatement(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stmtRoutes, name)
	delete(s.stmtWrite, name)
}

// routeLocked determines the route without locking (caller must hold mu).
func (s *Session) routeLocked(query string) Route {
	upper := strings.ToUpper(strings.TrimSpace(query))

	if strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION") ||
		strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") {
		return RouteWriter
	}

	if s.pinned || s.inTransaction {
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
