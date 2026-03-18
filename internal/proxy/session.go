package proxy

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// SessionInfo holds observable state for a single client session.
// Mutable fields (currentQuery, queryStartedAt, inTransaction, pinned,
// pinnedReason) are protected by mu for concurrent admin reads.
// ct is immutable after construction and does not require locking.
// The backend address is derived from ct at snapshot time (ct tracks it
// via setFromConn/clear during query execution).
type SessionInfo struct {
	mu             sync.RWMutex
	ID             uint32
	ClientAddr     string
	User           string
	Database       string
	ConnectedAt    time.Time
	currentQuery   string
	queryStartedAt time.Time
	inTransaction  bool
	pinned         bool
	pinnedReason   string
	ct             *cancelTarget // immutable after construction
}

// SetQueryState updates the current query text atomically.
func (si *SessionInfo) SetQueryState(query string) {
	si.mu.Lock()
	si.currentQuery = query
	si.queryStartedAt = time.Now()
	si.mu.Unlock()
}

// ClearQueryState clears the current query state after execution.
func (si *SessionInfo) ClearQueryState() {
	si.mu.Lock()
	si.currentQuery = ""
	si.queryStartedAt = time.Time{}
	si.mu.Unlock()
}

// SetTransactionState updates the transaction and pin state.
func (si *SessionInfo) SetTransactionState(inTx, pinned bool, pinnedReason string) {
	si.mu.Lock()
	si.inTransaction = inTx
	si.pinned = pinned
	si.pinnedReason = pinnedReason
	si.mu.Unlock()
}

// SessionSnapshot is a point-in-time copy of SessionInfo for serialization.
type SessionSnapshot struct {
	ID             uint32     `json:"id"`
	ClientAddr     string     `json:"client_addr"`
	User           string     `json:"user"`
	Database       string     `json:"database"`
	ConnectedAt    time.Time  `json:"connected_at"`
	CurrentQuery   string     `json:"current_query,omitempty"`
	QueryStartedAt *time.Time `json:"query_started_at,omitempty"`
	BackendAddr    string     `json:"backend_addr,omitempty"`
	InTransaction  bool       `json:"in_transaction"`
	Pinned         bool       `json:"pinned"`
	PinnedReason   string     `json:"pinned_reason,omitempty"`
}

// Snapshot returns a read-consistent copy of the session state.
func (si *SessionInfo) Snapshot() SessionSnapshot {
	si.mu.RLock()
	snap := SessionSnapshot{
		ID:            si.ID,
		ClientAddr:    si.ClientAddr,
		User:          si.User,
		Database:      si.Database,
		ConnectedAt:   si.ConnectedAt,
		CurrentQuery:  si.currentQuery,
		InTransaction: si.inTransaction,
		Pinned:        si.pinned,
		PinnedReason:  si.pinnedReason,
	}
	if !si.queryStartedAt.IsZero() {
		t := si.queryStartedAt
		snap.QueryStartedAt = &t
	}
	si.mu.RUnlock()

	// Derive backend address from cancel target (tracks it via setFromConn/clear).
	// ct is immutable — no session lock needed; ct.get() has its own lock.
	if si.ct != nil {
		addr, _, _ := si.ct.get()
		snap.BackendAddr = addr
	}

	return snap
}

// registerSession adds a session to the server registry.
func (s *Server) registerSession(si *SessionInfo) {
	s.sessions.Store(si.ID, si)
}

// unregisterSession removes a session from the server registry.
func (s *Server) unregisterSession(id uint32) {
	s.sessions.Delete(id)
}

// Sessions returns a snapshot of all active sessions, sorted by ID.
func (s *Server) Sessions() []SessionSnapshot {
	var result []SessionSnapshot
	s.sessions.Range(func(_, val any) bool {
		si := val.(*SessionInfo)
		result = append(result, si.Snapshot())
		return true
	})
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

// CancelSession cancels the active query on the given session.
// Returns found=true if the session exists, cancelled=true if a cancel was forwarded.
func (s *Server) CancelSession(id uint32) (found bool, cancelled bool) {
	val, ok := s.sessions.Load(id)
	if !ok {
		return false, false
	}
	si := val.(*SessionInfo)
	ct := si.ct // immutable — no lock needed
	if ct == nil {
		return true, false
	}
	addr, bPID, bSecret := ct.get()
	if addr == "" || bPID == 0 {
		return true, false
	}
	if err := forwardCancel(addr, bPID, bSecret); err != nil {
		slog.Warn("session cancel forward failed", "session_id", id, "error", err)
		return true, false
	}
	return true, true
}
