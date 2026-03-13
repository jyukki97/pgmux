package proxy

import (
	"fmt"
	"sync"

	"github.com/jyukki97/pgmux/internal/config"
)

// ConnTracker tracks active client connections per user and per database.
// Thread-safe via sync.Mutex (TryAcquire always writes, critical section is tiny).
type ConnTracker struct {
	mu         sync.Mutex
	byUser     map[string]int // username → active count
	byDB       map[string]int // database → active count
	userLimits map[string]int // username → max (from auth.users[].max_connections)
	dbLimits   map[string]int // database → max (from databases[].max_connections)
	defaultUser int           // default_max_connections_per_user (0 = unlimited)
	defaultDB   int           // default_max_connections_per_database (0 = unlimited)
}

// ConnLimitStats is a snapshot of connection limit state for the admin API.
type ConnLimitStats struct {
	ByUser   map[string]ConnStat `json:"by_user"`
	ByDB     map[string]ConnStat `json:"by_database"`
	Defaults struct {
		PerUser int `json:"per_user"`
		PerDB   int `json:"per_database"`
	} `json:"defaults"`
}

// ConnStat represents active count and limit for a single user or database.
type ConnStat struct {
	Active int `json:"active"`
	Limit  int `json:"limit"`
}

// NewConnTracker creates a ConnTracker from the current config.
func NewConnTracker(cfg *config.Config) *ConnTracker {
	ct := &ConnTracker{
		byUser:      make(map[string]int),
		byDB:        make(map[string]int),
		userLimits:  make(map[string]int),
		dbLimits:    make(map[string]int),
		defaultUser: cfg.ConnectionLimits.DefaultMaxConnectionsPerUser,
		defaultDB:   cfg.ConnectionLimits.DefaultMaxConnectionsPerDB,
	}
	for _, u := range cfg.Auth.Users {
		if u.MaxConnections > 0 {
			ct.userLimits[u.Username] = u.MaxConnections
		}
	}
	for name, db := range cfg.ResolvedDatabases() {
		if db.MaxConnections > 0 {
			ct.dbLimits[name] = db.MaxConnections
		}
	}
	return ct
}

// TryAcquire checks user and DB limits atomically. Returns true if allowed.
// On rejection, returns false with a reason string.
func (ct *ConnTracker) TryAcquire(user, db string) (bool, string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Check user limit
	userLimit := ct.defaultUser
	if l, ok := ct.userLimits[user]; ok {
		userLimit = l
	}
	if userLimit > 0 && ct.byUser[user] >= userLimit {
		return false, fmt.Sprintf("too many connections for user %q (limit: %d)", user, userLimit)
	}

	// Check DB limit
	dbLimit := ct.defaultDB
	if l, ok := ct.dbLimits[db]; ok {
		dbLimit = l
	}
	if dbLimit > 0 && ct.byDB[db] >= dbLimit {
		return false, fmt.Sprintf("too many connections for database %q (limit: %d)", db, dbLimit)
	}

	ct.byUser[user]++
	ct.byDB[db]++
	return true, ""
}

// Release decrements counters when a client disconnects.
func (ct *ConnTracker) Release(user, db string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if ct.byUser[user] > 0 {
		ct.byUser[user]--
	}
	if ct.byDB[db] > 0 {
		ct.byDB[db]--
	}
}

// UpdateLimits refreshes limit thresholds from a new config without resetting counters.
func (ct *ConnTracker) UpdateLimits(cfg *config.Config) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	ct.defaultUser = cfg.ConnectionLimits.DefaultMaxConnectionsPerUser
	ct.defaultDB = cfg.ConnectionLimits.DefaultMaxConnectionsPerDB

	ct.userLimits = make(map[string]int)
	for _, u := range cfg.Auth.Users {
		if u.MaxConnections > 0 {
			ct.userLimits[u.Username] = u.MaxConnections
		}
	}

	ct.dbLimits = make(map[string]int)
	for name, db := range cfg.ResolvedDatabases() {
		if db.MaxConnections > 0 {
			ct.dbLimits[name] = db.MaxConnections
		}
	}
}

// Stats returns a snapshot of current connection counts and limits.
func (ct *ConnTracker) Stats() ConnLimitStats {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	s := ConnLimitStats{
		ByUser: make(map[string]ConnStat),
		ByDB:   make(map[string]ConnStat),
	}
	s.Defaults.PerUser = ct.defaultUser
	s.Defaults.PerDB = ct.defaultDB

	for user, count := range ct.byUser {
		limit := ct.defaultUser
		if l, ok := ct.userLimits[user]; ok {
			limit = l
		}
		s.ByUser[user] = ConnStat{Active: count, Limit: limit}
	}
	// Include users with explicit limits but no active connections
	for user, limit := range ct.userLimits {
		if _, ok := s.ByUser[user]; !ok {
			s.ByUser[user] = ConnStat{Active: 0, Limit: limit}
		}
	}

	for db, count := range ct.byDB {
		limit := ct.defaultDB
		if l, ok := ct.dbLimits[db]; ok {
			limit = l
		}
		s.ByDB[db] = ConnStat{Active: count, Limit: limit}
	}
	for db, limit := range ct.dbLimits {
		if _, ok := s.ByDB[db]; !ok {
			s.ByDB[db] = ConnStat{Active: 0, Limit: limit}
		}
	}

	return s
}
