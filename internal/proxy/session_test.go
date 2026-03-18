package proxy

import (
	"sync"
	"testing"
	"time"
)

func TestSessionInfo_SetAndClearQueryState(t *testing.T) {
	si := &SessionInfo{
		ID:          1,
		ClientAddr:  "127.0.0.1:1234",
		User:        "test",
		Database:    "testdb",
		ConnectedAt: time.Now(),
	}

	// Initially no query
	snap := si.Snapshot()
	if snap.CurrentQuery != "" {
		t.Errorf("expected empty query, got %q", snap.CurrentQuery)
	}
	if snap.QueryStartedAt != nil {
		t.Errorf("expected nil query_started_at, got %v", snap.QueryStartedAt)
	}

	// Set query state
	si.SetQueryState("SELECT 1")
	snap = si.Snapshot()
	if snap.CurrentQuery != "SELECT 1" {
		t.Errorf("current_query = %q, want SELECT 1", snap.CurrentQuery)
	}
	if snap.QueryStartedAt == nil {
		t.Error("expected query_started_at to be set")
	}

	// Clear query state
	si.ClearQueryState()
	snap = si.Snapshot()
	if snap.CurrentQuery != "" {
		t.Errorf("expected empty query after clear, got %q", snap.CurrentQuery)
	}
	if snap.QueryStartedAt != nil {
		t.Errorf("expected nil query_started_at after clear, got %v", snap.QueryStartedAt)
	}
}

func TestSessionInfo_BackendAddrFromCancelTarget(t *testing.T) {
	ct := &cancelTarget{proxyPID: 1, proxySecret: 42}
	si := &SessionInfo{
		ID:          1,
		ConnectedAt: time.Now(),
		ct:          ct,
	}

	// No active backend → empty addr
	snap := si.Snapshot()
	if snap.BackendAddr != "" {
		t.Errorf("expected empty backend_addr, got %q", snap.BackendAddr)
	}

	// Set backend via cancelTarget (simulates query execution)
	ct.mu.Lock()
	ct.backendAddr = "10.0.0.1:5432"
	ct.backendPID = 100
	ct.backendSecret = 200
	ct.mu.Unlock()

	snap = si.Snapshot()
	if snap.BackendAddr != "10.0.0.1:5432" {
		t.Errorf("backend_addr = %q, want 10.0.0.1:5432", snap.BackendAddr)
	}

	// Clear backend (simulates query completion)
	ct.clear()
	snap = si.Snapshot()
	if snap.BackendAddr != "" {
		t.Errorf("expected empty backend_addr after clear, got %q", snap.BackendAddr)
	}
}

func TestSessionInfo_SetTransactionState(t *testing.T) {
	si := &SessionInfo{
		ID:          1,
		ConnectedAt: time.Now(),
	}

	si.SetTransactionState(true, false, "")
	snap := si.Snapshot()
	if !snap.InTransaction {
		t.Error("expected in_transaction = true")
	}
	if snap.Pinned {
		t.Error("expected pinned = false")
	}

	si.SetTransactionState(false, true, "LISTEN")
	snap = si.Snapshot()
	if snap.InTransaction {
		t.Error("expected in_transaction = false")
	}
	if !snap.Pinned {
		t.Error("expected pinned = true")
	}
	if snap.PinnedReason != "LISTEN" {
		t.Errorf("pinned_reason = %q, want LISTEN", snap.PinnedReason)
	}
}

func TestSessionSnapshot_OmitsEmptyFields(t *testing.T) {
	si := &SessionInfo{
		ID:          1,
		ClientAddr:  "127.0.0.1:5432",
		User:        "user",
		Database:    "db",
		ConnectedAt: time.Now(),
	}

	snap := si.Snapshot()
	if snap.CurrentQuery != "" {
		t.Error("expected empty current_query")
	}
	if snap.QueryStartedAt != nil {
		t.Error("expected nil query_started_at")
	}
	if snap.BackendAddr != "" {
		t.Error("expected empty backend_addr")
	}
	if snap.PinnedReason != "" {
		t.Error("expected empty pinned_reason")
	}
}

func TestServer_SessionRegistry(t *testing.T) {
	s := &Server{}

	si1 := &SessionInfo{ID: 1, User: "user1", ConnectedAt: time.Now()}
	si2 := &SessionInfo{ID: 2, User: "user2", ConnectedAt: time.Now()}

	// Register sessions
	s.registerSession(si1)
	s.registerSession(si2)

	sessions := s.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions count = %d, want 2", len(sessions))
	}
	// Verify sorted by ID
	if sessions[0].ID != 1 || sessions[1].ID != 2 {
		t.Errorf("sessions not sorted: [%d, %d]", sessions[0].ID, sessions[1].ID)
	}

	// Unregister one
	s.unregisterSession(1)
	sessions = s.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions count = %d, want 1", len(sessions))
	}
	if sessions[0].ID != 2 {
		t.Errorf("remaining session ID = %d, want 2", sessions[0].ID)
	}

	// Unregister all
	s.unregisterSession(2)
	sessions = s.Sessions()
	if len(sessions) != 0 {
		t.Fatalf("sessions count = %d, want 0", len(sessions))
	}
}

func TestServer_CancelSession_NotFound(t *testing.T) {
	s := &Server{}
	found, cancelled := s.CancelSession(999)
	if found {
		t.Error("expected found = false for non-existent session")
	}
	if cancelled {
		t.Error("expected cancelled = false for non-existent session")
	}
}

func TestServer_CancelSession_NoActiveQuery(t *testing.T) {
	s := &Server{}
	ct := &cancelTarget{proxyPID: 1, proxySecret: 42}
	si := &SessionInfo{ID: 1, ct: ct, ConnectedAt: time.Now()}
	s.registerSession(si)
	defer s.unregisterSession(1)

	found, cancelled := s.CancelSession(1)
	if !found {
		t.Error("expected found = true")
	}
	if cancelled {
		t.Error("expected cancelled = false (no active backend query)")
	}
}

func TestSessionInfo_ConcurrentAccess(t *testing.T) {
	si := &SessionInfo{
		ID:          1,
		ClientAddr:  "127.0.0.1:1234",
		User:        "test",
		Database:    "testdb",
		ConnectedAt: time.Now(),
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			si.SetQueryState("SELECT 1")
		}()
		go func() {
			defer wg.Done()
			si.ClearQueryState()
		}()
		go func() {
			defer wg.Done()
			_ = si.Snapshot()
		}()
	}
	wg.Wait()
}
