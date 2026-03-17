package redact

import (
	"strings"
	"testing"
)

func TestSQL_PolicyNone(t *testing.T) {
	query := "SELECT * FROM users WHERE id = 42 AND name = 'alice'"
	got := SQL(query, PolicyNone)
	if got != query {
		t.Errorf("PolicyNone: got %q, want %q", got, query)
	}
}

func TestSQL_PolicyLiterals(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string // substring that should NOT appear
	}{
		{"string literal", "SELECT * FROM users WHERE name = 'alice'", "alice"},
		{"numeric literal", "SELECT * FROM orders WHERE id = 42", "42"},
		{"multiple literals", "INSERT INTO t(a,b) VALUES('x', 123)", "123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SQL(tt.query, PolicyLiterals)
			if strings.Contains(got, tt.want) {
				t.Errorf("PolicyLiterals: result %q still contains %q", got, tt.want)
			}
			// Should contain $1 (pg_query.Normalize output)
			if !strings.Contains(got, "$1") {
				t.Errorf("PolicyLiterals: result %q missing $1 placeholder", got)
			}
		})
	}
}

func TestSQL_PolicyLiterals_Fallback(t *testing.T) {
	// Unparseable SQL should use regex fallback
	query := "THIS IS NOT VALID SQL 'secret' 42"
	got := SQL(query, PolicyLiterals)
	if strings.Contains(got, "secret") {
		t.Errorf("fallback: result %q still contains 'secret'", got)
	}
	if strings.Contains(got, "42") {
		t.Errorf("fallback: result %q still contains '42'", got)
	}
}

func TestSQL_PolicyFull(t *testing.T) {
	query := "SELECT * FROM users WHERE id = 1"
	got := SQL(query, PolicyFull)
	if !strings.HasPrefix(got, "[fingerprint:") || !strings.HasSuffix(got, "]") {
		t.Errorf("PolicyFull: got %q, want [fingerprint:...]", got)
	}
	// Must not contain any part of the original query
	if strings.Contains(got, "users") {
		t.Errorf("PolicyFull: result %q still contains table name", got)
	}
}

func TestSQL_PolicyFull_Unparseable(t *testing.T) {
	got := SQL("THIS IS NOT VALID SQL", PolicyFull)
	if got != "[unparseable query]" {
		t.Errorf("PolicyFull unparseable: got %q, want %q", got, "[unparseable query]")
	}
}

func TestSQLTruncated(t *testing.T) {
	query := "SELECT * FROM users WHERE id = 1"
	got := SQLTruncated(query, PolicyNone, 10)
	if len(got) > 13 { // 10 + "..."
		t.Errorf("SQLTruncated: got %q (len %d), want max 13", got, len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("SQLTruncated: got %q, want ... suffix", got)
	}
}

func TestForLog(t *testing.T) {
	query := "SELECT * FROM users WHERE name = 'alice'"
	got := ForLog(query, PolicyLiterals)
	if strings.Contains(got, "alice") {
		t.Errorf("ForLog: result %q still contains 'alice'", got)
	}
	if len(got) > 203 { // 200 + "..."
		t.Errorf("ForLog: result too long (%d chars)", len(got))
	}
}

func TestSQL_SameStructureSameFingerprint(t *testing.T) {
	q1 := "SELECT * FROM users WHERE id = 1"
	q2 := "SELECT * FROM users WHERE id = 999"
	fp1 := SQL(q1, PolicyFull)
	fp2 := SQL(q2, PolicyFull)
	if fp1 != fp2 {
		t.Errorf("same structure should have same fingerprint: %q vs %q", fp1, fp2)
	}
}

func TestSQL_EmptyQuery(t *testing.T) {
	got := SQL("", PolicyLiterals)
	// Empty query should not panic
	if got == "" {
		// Acceptable — empty in, empty out (or fallback result)
	}
}
