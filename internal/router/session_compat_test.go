package router

import (
	"testing"
)

func TestDetectSessionDependency(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    bool
		feature SessionFeature
	}{
		// LISTEN
		{"listen basic", "LISTEN foo", true, FeatureListen},
		{"listen lowercase", "listen channel_name", true, FeatureListen},
		{"listen leading space", "  LISTEN bar", true, FeatureListen},

		// UNLISTEN
		{"unlisten basic", "UNLISTEN foo", true, FeatureUnlisten},
		{"unlisten all", "UNLISTEN *", true, FeatureUnlisten},

		// SET (session-scoped)
		{"set basic", "SET search_path TO public", true, FeatureSessionSet},
		{"set statement_timeout", "SET statement_timeout = '5s'", true, FeatureSessionSet},
		{"set lowercase", "set work_mem = '256MB'", true, FeatureSessionSet},

		// SET LOCAL (transaction-scoped — safe)
		{"set local", "SET LOCAL search_path TO public", false, ""},
		{"set local lowercase", "set local work_mem = '256MB'", false, ""},

		// SET TRANSACTION (transaction-scoped — safe)
		{"set transaction", "SET TRANSACTION ISOLATION LEVEL READ COMMITTED", false, ""},
		{"set transaction lowercase", "set transaction read only", false, ""},

		// DECLARE CURSOR
		{"declare cursor", "DECLARE my_cursor CURSOR FOR SELECT * FROM t", true, FeatureDeclare},
		{"declare lowercase", "declare c CURSOR FOR SELECT 1", true, FeatureDeclare},

		// PREPARE
		{"prepare basic", "PREPARE plan1 AS SELECT * FROM t WHERE id = $1", true, FeaturePrepare},
		{"prepare lowercase", "prepare stmt AS INSERT INTO t VALUES ($1)", true, FeaturePrepare},

		// CREATE TEMP
		{"create temp table", "CREATE TEMP TABLE tmp AS SELECT 1", true, FeatureCreateTemp},
		{"create temporary table", "CREATE TEMPORARY TABLE tmp (id int)", true, FeatureCreateTemp},
		{"create temp lowercase", "create temp table tmp (id int)", true, FeatureCreateTemp},

		// CREATE (non-temp — not flagged)
		{"create table", "CREATE TABLE t (id int)", false, ""},

		// Advisory lock (session-scoped)
		{"advisory lock", "SELECT pg_advisory_lock(123)", true, FeatureAdvisoryLock},
		{"try advisory lock", "SELECT pg_try_advisory_lock(1, 2)", true, FeatureAdvisoryLock},
		{"advisory unlock", "SELECT pg_advisory_unlock(123)", true, FeatureAdvisoryLock},
		{"advisory unlock all", "SELECT pg_advisory_unlock_all()", true, FeatureAdvisoryLock},
		{"advisory lock shared", "SELECT pg_advisory_lock_shared(123)", true, FeatureAdvisoryLock},

		// Advisory lock (transaction-scoped — safe)
		{"advisory xact lock", "SELECT pg_advisory_xact_lock(123)", false, ""},
		{"try advisory xact lock", "SELECT pg_try_advisory_xact_lock(1, 2)", false, ""},
		{"advisory xact lock shared", "SELECT pg_advisory_xact_lock_shared(123)", false, ""},

		// Safe queries (not flagged)
		{"select", "SELECT 1", false, ""},
		{"insert", "INSERT INTO t VALUES (1)", false, ""},
		{"update", "UPDATE t SET x = 1 WHERE id = 1", false, ""},
		{"delete", "DELETE FROM t WHERE id = 1", false, ""},
		{"begin", "BEGIN", false, ""},
		{"commit", "COMMIT", false, ""},

		// Multi-statement
		{"multi with listen", "SELECT 1; LISTEN foo", true, FeatureListen},
		{"multi with set", "BEGIN; SET search_path TO public", true, FeatureSessionSet},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectSessionDependency(tt.query)
			if result.Detected != tt.want {
				t.Errorf("DetectSessionDependency(%q).Detected = %v, want %v", tt.query, result.Detected, tt.want)
			}
			if tt.want && result.Feature != tt.feature {
				t.Errorf("DetectSessionDependency(%q).Feature = %q, want %q", tt.query, result.Feature, tt.feature)
			}
		})
	}
}

func TestDetectSessionDependencyAST(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    bool
		feature SessionFeature
	}{
		// LISTEN
		{"ast listen", "LISTEN foo", true, FeatureListen},
		{"ast unlisten", "UNLISTEN foo", true, FeatureUnlisten},

		// SET
		{"ast set", "SET search_path TO public", true, FeatureSessionSet},
		{"ast set local", "SET LOCAL search_path TO public", false, ""},

		// DECLARE
		{"ast declare", "DECLARE c CURSOR FOR SELECT 1", true, FeatureDeclare},

		// PREPARE
		{"ast prepare", "PREPARE s AS SELECT 1", true, FeaturePrepare},

		// CREATE TEMP
		{"ast create temp", "CREATE TEMP TABLE tmp (id int)", true, FeatureCreateTemp},
		{"ast create table", "CREATE TABLE t (id int)", false, ""},

		// Advisory lock (string-based fallback)
		{"ast advisory lock", "SELECT pg_advisory_lock(1)", true, FeatureAdvisoryLock},
		{"ast advisory xact lock", "SELECT pg_advisory_xact_lock(1)", false, ""},

		// Safe
		{"ast select", "SELECT 1", false, ""},
		{"ast insert", "INSERT INTO t VALUES (1)", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pq, _ := NewParsedQuery(tt.query)
			result := DetectSessionDependencyAST(pq, tt.query)
			if result.Detected != tt.want {
				t.Errorf("DetectSessionDependencyAST(%q).Detected = %v, want %v", tt.query, result.Detected, tt.want)
			}
			if tt.want && result.Feature != tt.feature {
				t.Errorf("DetectSessionDependencyAST(%q).Feature = %q, want %q", tt.query, result.Feature, tt.feature)
			}
		})
	}
}

func TestContainsSessionAdvisoryLock(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		// Session-scoped (unsafe)
		{"SELECT pg_advisory_lock(1)", true},
		{"SELECT pg_try_advisory_lock(1, 2)", true},
		{"SELECT pg_advisory_unlock(1)", true},
		{"SELECT pg_advisory_unlock_all()", true},
		{"SELECT pg_advisory_lock_shared(1)", true},
		{"SELECT PG_ADVISORY_LOCK(1)", true},

		// Transaction-scoped (safe)
		{"SELECT pg_advisory_xact_lock(1)", false},
		{"SELECT pg_try_advisory_xact_lock(1)", false},
		{"SELECT pg_advisory_xact_lock_shared(1)", false},
		{"SELECT pg_try_advisory_xact_lock_shared(1)", false},

		// No advisory lock
		{"SELECT 1", false},
		{"INSERT INTO t VALUES (1)", false},

		// QA6: False positives from comments/literals (#245)
		{"SELECT 'pg_advisory_lock'", false},
		{"SELECT 'pg_advisory_unlock'", false},
		{"/* advisory_lock */ SELECT 1", false},
		{"/* advisory_unlock */ SELECT 1", false},
		{"SELECT $$ pg_advisory_lock $$ FROM t", false},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := containsSessionAdvisoryLock(tt.query)
			if got != tt.want {
				t.Errorf("containsSessionAdvisoryLock(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// === QA6: Leading comments bypass session detection (#243) ===

func TestDetectSessionDependency_LeadingComment(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    bool
		feature SessionFeature
	}{
		{"comment SET", "/*x*/ SET search_path TO public", true, FeatureSessionSet},
		{"comment LISTEN", "/*x*/ LISTEN foo", true, FeatureListen},
		{"comment UNLISTEN", "/*x*/ UNLISTEN *", true, FeatureUnlisten},
		{"comment PREPARE", "/*x*/ PREPARE s AS SELECT 1", true, FeaturePrepare},
		{"comment DECLARE", "/*x*/ DECLARE c CURSOR FOR SELECT 1", true, FeatureDeclare},
		{"comment CREATE TEMP", "/*x*/ CREATE TEMP TABLE t (id int)", true, FeatureCreateTemp},
		{"line comment SET", "-- c\nSET search_path TO public", true, FeatureSessionSet},
		{"nested comment LISTEN", "/* /* n */ */ LISTEN foo", true, FeatureListen},
		// SET LOCAL with comment — still safe
		{"comment SET LOCAL", "/*x*/ SET LOCAL search_path TO public", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectSessionDependency(tt.query)
			if result.Detected != tt.want {
				t.Errorf("DetectSessionDependency(%q).Detected = %v, want %v", tt.query, result.Detected, tt.want)
			}
			if tt.want && result.Feature != tt.feature {
				t.Errorf("DetectSessionDependency(%q).Feature = %q, want %q", tt.query, result.Feature, tt.feature)
			}
		})
	}
}

func TestSessionPin(t *testing.T) {
	s := NewSession(0, false, false)

	if s.Pinned() {
		t.Error("new session should not be pinned")
	}

	s.Pin("listen")
	if !s.Pinned() {
		t.Error("session should be pinned after Pin()")
	}
	if s.PinnedReason() != "listen" {
		t.Errorf("PinnedReason() = %q, want %q", s.PinnedReason(), "listen")
	}

	// Pinned session should always route to writer
	route := s.Route("SELECT 1")
	if route != RouteWriter {
		t.Errorf("pinned session Route(SELECT) = %d, want RouteWriter", route)
	}

	route, _, _ = s.RouteWithTxState("SELECT 1")
	if route != RouteWriter {
		t.Errorf("pinned session RouteWithTxState(SELECT) = %d, want RouteWriter", route)
	}
}
