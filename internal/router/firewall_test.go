package router

import "testing"

func TestCheckFirewall_DeleteWithoutWhere(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:                 true,
		BlockDeleteWithoutWhere: true,
	}

	// Blocked: DELETE without WHERE
	result := CheckFirewall("DELETE FROM users", cfg)
	if !result.Blocked {
		t.Error("expected DELETE without WHERE to be blocked")
	}
	if result.Rule != RuleDeleteNoWhere {
		t.Errorf("rule = %q, want %q", result.Rule, RuleDeleteNoWhere)
	}

	// Allowed: DELETE with WHERE
	result = CheckFirewall("DELETE FROM users WHERE id = 1", cfg)
	if result.Blocked {
		t.Error("expected DELETE with WHERE to be allowed")
	}

	// Allowed: SELECT
	result = CheckFirewall("SELECT * FROM users", cfg)
	if result.Blocked {
		t.Error("expected SELECT to be allowed")
	}
}

func TestCheckFirewall_UpdateWithoutWhere(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:                 true,
		BlockUpdateWithoutWhere: true,
	}

	// Blocked: UPDATE without WHERE
	result := CheckFirewall("UPDATE users SET active = false", cfg)
	if !result.Blocked {
		t.Error("expected UPDATE without WHERE to be blocked")
	}
	if result.Rule != RuleUpdateNoWhere {
		t.Errorf("rule = %q, want %q", result.Rule, RuleUpdateNoWhere)
	}

	// Allowed: UPDATE with WHERE
	result = CheckFirewall("UPDATE users SET active = false WHERE id = 1", cfg)
	if result.Blocked {
		t.Error("expected UPDATE with WHERE to be allowed")
	}
}

func TestCheckFirewall_DropTable(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:        true,
		BlockDropTable: true,
	}

	result := CheckFirewall("DROP TABLE users", cfg)
	if !result.Blocked {
		t.Error("expected DROP TABLE to be blocked")
	}
	if result.Rule != RuleDropTable {
		t.Errorf("rule = %q, want %q", result.Rule, RuleDropTable)
	}

	// DROP INDEX should also be blocked (it's a DropStmt)
	result = CheckFirewall("DROP INDEX idx_users_name", cfg)
	if !result.Blocked {
		t.Error("expected DROP INDEX to be blocked")
	}
}

func TestCheckFirewall_Truncate(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:       true,
		BlockTruncate: true,
	}

	result := CheckFirewall("TRUNCATE users", cfg)
	if !result.Blocked {
		t.Error("expected TRUNCATE to be blocked")
	}
	if result.Rule != RuleTruncate {
		t.Errorf("rule = %q, want %q", result.Rule, RuleTruncate)
	}
}

func TestCheckFirewall_Disabled(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:                 false,
		BlockDeleteWithoutWhere: true,
	}

	result := CheckFirewall("DELETE FROM users", cfg)
	if result.Blocked {
		t.Error("expected firewall disabled to allow all queries")
	}
}

func TestCheckFirewall_MultiStatement(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:                 true,
		BlockDeleteWithoutWhere: true,
	}

	// First statement is fine, second is blocked
	result := CheckFirewall("SELECT 1; DELETE FROM users;", cfg)
	if !result.Blocked {
		t.Error("expected multi-statement with DELETE without WHERE to be blocked")
	}
}

func TestCheckFirewall_AllRulesEnabled(t *testing.T) {
	cfg := FirewallConfig{
		Enabled:                 true,
		BlockDeleteWithoutWhere: true,
		BlockUpdateWithoutWhere: true,
		BlockDropTable:          true,
		BlockTruncate:           true,
	}

	// All these should be blocked
	blocked := []struct {
		query string
		rule  FirewallRule
	}{
		{"DELETE FROM users", RuleDeleteNoWhere},
		{"UPDATE users SET x=1", RuleUpdateNoWhere},
		{"DROP TABLE users", RuleDropTable},
		{"TRUNCATE users", RuleTruncate},
	}

	for _, tt := range blocked {
		result := CheckFirewall(tt.query, cfg)
		if !result.Blocked {
			t.Errorf("expected %q to be blocked", tt.query)
		}
		if result.Rule != tt.rule {
			t.Errorf("%q: rule = %q, want %q", tt.query, result.Rule, tt.rule)
		}
	}

	// All these should be allowed
	allowed := []string{
		"SELECT * FROM users",
		"INSERT INTO users VALUES (1)",
		"DELETE FROM users WHERE id = 1",
		"UPDATE users SET x=1 WHERE id = 1",
	}

	for _, q := range allowed {
		result := CheckFirewall(q, cfg)
		if result.Blocked {
			t.Errorf("expected %q to be allowed, blocked by %q", q, result.Rule)
		}
	}
}
