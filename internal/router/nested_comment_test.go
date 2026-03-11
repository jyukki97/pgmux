package router

import (
	"strings"
	"testing"
)

func TestNestedComments_Bypass(t *testing.T) {
	// Postgres allows nested block comments: /* /* */ */
	// If stripped by non-nested regex like `/\*.*?\*/`, it strips `/* /* */`, leaving ` */`.
	// Vulnerability scenario:
	// A user submits:  SELECT 1 /* /* */ ; UPDATE admin SET foo='bar' ; /* */
	// If the parser thinks the UPDATE is active, it classifies it as QueryWrite!

	input := `SELECT /* /* */ UPDATE admin SET foo='bar' /* */`
	got := stripComments(input)

	if strings.Contains(got, "UPDATE admin") {
		t.Errorf("VULNERABLE: Nested comment reveals hidden write queries!")
	}
}
