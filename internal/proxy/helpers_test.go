package proxy

import (
	"testing"

	"github.com/jyukki97/pgmux/internal/router"
)

func TestIsSessionModifying(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		// SET -> true (session-scoped)
		{"SET search_path TO public", true},
		{"set search_path to public", true},   // case insensitive
		{"  SET search_path TO public", true}, // leading whitespace
		{"SET statement_timeout = 5000", true},

		// SET LOCAL -> false (transaction-scoped)
		{"SET LOCAL search_path TO public", false},
		{"set local search_path to public", false},

		// SET TRANSACTION -> false (transaction-scoped)
		{"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE", false},
		{"set transaction read only", false},

		// SET CONSTRAINTS -> false (transaction-scoped)
		{"SET CONSTRAINTS ALL DEFERRED", false},

		// PREPARE -> true
		{"PREPARE stmt AS SELECT 1", true},
		{"prepare stmt as select 1", true},

		// DECLARE -> true
		{"DECLARE cur CURSOR FOR SELECT 1", true},
		{"declare cur cursor for select 1", true},

		// DEALLOCATE -> true
		{"DEALLOCATE stmt", true},
		{"DEALLOCATE ALL", true},

		// LISTEN -> true
		{"LISTEN my_channel", true},
		{"listen my_channel", true},

		// UNLISTEN -> true
		{"UNLISTEN my_channel", true},
		{"UNLISTEN *", true},

		// LOAD -> true
		{"LOAD 'my_library'", true},

		// CREATE TEMP -> true
		{"CREATE TEMP TABLE t (id int)", true},
		{"CREATE TEMPORARY TABLE t (id int)", true},
		{"create temp table t (id int)", true},

		// Regular queries -> false
		{"SELECT 1", false},
		{"INSERT INTO t VALUES (1)", false},
		{"UPDATE t SET x = 1", false},
		{"DELETE FROM t", false},
		{"BEGIN", false},
		{"COMMIT", false},
		{"ROLLBACK", false},
		{"CREATE TABLE t (id int)", false}, // not TEMP
		{"", false},

		// Comments before keyword
		{"/* comment */ SET search_path TO public", true},
		{"-- comment\nSET search_path TO public", true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := isSessionModifying(tt.query)
			if got != tt.want {
				t.Errorf("isSessionModifying(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestRouteName(t *testing.T) {
	tests := []struct {
		route router.Route
		want  string
	}{
		{router.RouteWriter, "writer"},
		{router.RouteReader, "reader"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := routeName(tt.route)
			if got != tt.want {
				t.Errorf("routeName(%v) = %q, want %q", tt.route, got, tt.want)
			}
		})
	}
}
