package router

import (
	"strings"
	"testing"
)

func TestDollarQuoting_Strip(t *testing.T) {
	input := `SELECT * FROM foo WHERE note = $$ /* route:writer */ $$`
	got := stripStringLiterals(input)

	// If dollar quoting is NOT handled, `got` will still contain the comment.
	hasHint := strings.Contains(got, "/* route:writer */")
	if hasHint {
		t.Errorf("VULNERABLE: Dollar quoting bypasses string literal stripper!")
	}
}

func TestClassify_DollarQuoting(t *testing.T) {
	query := "SELECT * FROM users WHERE data = $tag$ UPDATE admin $tag$"
	if Classify(query) == QueryWrite {
		t.Errorf("VULNERABLE: Dollar quoted string triggers QueryWrite!")
	}
}
