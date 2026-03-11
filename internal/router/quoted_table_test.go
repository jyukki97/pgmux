package router

import (
	"testing"
)

func TestExtractTables_QuotedSpaces(t *testing.T) {
	query := `UPDATE "my table" SET a = 1`
	got := ExtractTables(query)

	// If it splits by space, it will return `"my` instead of `"my table"` or `my table`.
	if len(got) == 0 || (got[0] != `"my table"` && got[0] != `my table`) {
		t.Errorf("VULNERABLE: Extracted wrong table name for quoted identifier! Got: %v", got)
	}
}
