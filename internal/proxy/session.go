package proxy

import (
	"sync"
	"time"
)

// SessionInfo holds observable state for a single client session.
// All mutable fields are protected by mu for concurrent admin reads.
type SessionInfo struct {
	mu             sync.RWMutex
	ID             uint32
	ClientAddr     string
	User           string
	Database       string
	ConnectedAt    time.Time
	currentQuery   string
	queryStartedAt time.Time
	backendAddr    string
	inTransaction  bool
	pinned         bool
	pinnedReason   string
	ct             *cancelTarget
}

// SetQueryState updates the current query and backend info atomically.
func (si *SessionInfo) SetQueryState(query, backendAddr string) {
	si.mu.Lock()
	si.currentQuery = query
	si.queryStartedAt = time.Now()
	si.backendAddr = backendAddr
	si.mu.Unlock()
}

// ClearQueryState clears the current query state after execution.
func (si *SessionInfo) ClearQueryState() {
	si.mu.Lock()
	si.currentQuery = ""
	si.queryStartedAt = time.Time{}
	si.backendAddr = ""
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
	ID             uint32    `json:"id"`
	ClientAddr     string    `json:"client_addr"`
	User           string    `json:"user"`
	Database       string    `json:"database"`
	ConnectedAt    time.Time `json:"connected_at"`
	CurrentQuery   string    `json:"current_query,omitempty"`
	QueryStartedAt string    `json:"query_started_at,omitempty"`
	BackendAddr    string    `json:"backend_addr,omitempty"`
	InTransaction  bool      `json:"in_transaction"`
	Pinned         bool      `json:"pinned"`
	PinnedReason   string    `json:"pinned_reason,omitempty"`
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
		BackendAddr:   si.backendAddr,
		InTransaction: si.inTransaction,
		Pinned:        si.pinned,
		PinnedReason:  si.pinnedReason,
	}
	if !si.queryStartedAt.IsZero() {
		snap.QueryStartedAt = si.queryStartedAt.Format(time.RFC3339)
	}
	si.mu.RUnlock()
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

// Sessions returns a snapshot of all active sessions.
func (s *Server) Sessions() []SessionSnapshot {
	var result []SessionSnapshot
	s.sessions.Range(func(_, val any) bool {
		si := val.(*SessionInfo)
		result = append(result, si.Snapshot())
		return true
	})
	return result
}

// CancelSession cancels the active query on the given session.
// Returns true if a cancel was forwarded, false if no active query.
// Returns an error string if the session was not found.
func (s *Server) CancelSession(id uint32) (found bool, cancelled bool) {
	val, ok := s.sessions.Load(id)
	if !ok {
		return false, false
	}
	si := val.(*SessionInfo)
	si.mu.RLock()
	ct := si.ct
	si.mu.RUnlock()
	if ct == nil {
		return true, false
	}
	addr, bPID, bSecret := ct.get()
	if addr == "" || bPID == 0 {
		return true, false
	}
	if err := forwardCancel(addr, bPID, bSecret); err != nil {
		return true, false
	}
	return true, true
}
