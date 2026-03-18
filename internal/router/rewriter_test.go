package router

import (
	"strings"
	"testing"
)

func TestRewriteTableRename(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "rename_old_users",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"old_users"}},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "old_users", To: "users_v2"},
			},
		},
	}
	rw := NewRewriter(rules)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "select",
			query: "SELECT id, name FROM old_users WHERE active = true",
			want:  "users_v2",
		},
		{
			name:  "insert",
			query: "INSERT INTO old_users (name) VALUES ('test')",
			want:  "users_v2",
		},
		{
			name:  "update",
			query: "UPDATE old_users SET name = 'new' WHERE id = 1",
			want:  "users_v2",
		},
		{
			name:  "delete",
			query: "DELETE FROM old_users WHERE id = 1",
			want:  "users_v2",
		},
		{
			name:  "join",
			query: "SELECT u.id FROM old_users u JOIN orders o ON u.id = o.user_id",
			want:  "users_v2",
		},
		{
			name:  "no_match",
			query: "SELECT * FROM other_table",
			want:  "", // not rewritten
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := rw.Apply(tt.query, nil)
			if tt.want == "" {
				if result.Rewritten {
					t.Errorf("expected no rewrite, got: %s", result.RewrittenSQL)
				}
				return
			}
			if !result.Rewritten {
				t.Fatal("expected rewrite, got none")
			}
			if !strings.Contains(strings.ToLower(result.RewrittenSQL), strings.ToLower(tt.want)) {
				t.Errorf("expected %q in result, got: %s", tt.want, result.RewrittenSQL)
			}
			if strings.Contains(strings.ToLower(result.RewrittenSQL), "old_users") {
				t.Errorf("old table name should not appear in result: %s", result.RewrittenSQL)
			}
		})
	}
}

func TestRewriteColumnRename(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "rename_user_name",
			Enabled: true,
			Match:   RewriteRuleMatch{},
			Actions: []RewriteAction{
				{Type: ActionColumnRename, From: "user_name", To: "name"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT user_name FROM users WHERE user_name = 'test'", nil)
	if !result.Rewritten {
		t.Fatal("expected rewrite")
	}
	lower := strings.ToLower(result.RewrittenSQL)
	if strings.Contains(lower, "user_name") {
		t.Errorf("old column name should not appear: %s", result.RewrittenSQL)
	}
}

func TestRewriteAddWhere(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "tenant_filter",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"orders"}},
			Actions: []RewriteAction{
				{Type: ActionAddWhere, Condition: "tenant_id = 1"},
			},
		},
	}
	rw := NewRewriter(rules)

	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "select_no_where",
			query: "SELECT * FROM orders",
		},
		{
			name:  "select_with_where",
			query: "SELECT * FROM orders WHERE status = 'active'",
		},
		{
			name:  "update",
			query: "UPDATE orders SET status = 'shipped' WHERE id = 1",
		},
		{
			name:  "delete",
			query: "DELETE FROM orders WHERE id = 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := rw.Apply(tt.query, nil)
			if !result.Rewritten {
				t.Fatal("expected rewrite")
			}
			lower := strings.ToLower(result.RewrittenSQL)
			if !strings.Contains(lower, "tenant_id") {
				t.Errorf("expected tenant_id in result: %s", result.RewrittenSQL)
			}
		})
	}
}

func TestRewriteSchemaQualify(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "qualify_users",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"users"}},
			Actions: []RewriteAction{
				{Type: ActionSchemaQualify, From: "users", To: "public"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM users", nil)
	if !result.Rewritten {
		t.Fatal("expected rewrite")
	}
	lower := strings.ToLower(result.RewrittenSQL)
	if !strings.Contains(lower, "public.") {
		t.Errorf("expected schema qualifier in result: %s", result.RewrittenSQL)
	}
}

func TestRewriteSchemaQualifySkipsAlreadyQualified(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "qualify_users",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"users"}},
			Actions: []RewriteAction{
				{Type: ActionSchemaQualify, From: "users", To: "public"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM myschema.users", nil)
	if result.Rewritten {
		// Should not be rewritten because it already has a schema qualifier
		if strings.Contains(strings.ToLower(result.RewrittenSQL), "public.") {
			t.Errorf("should not overwrite existing schema: %s", result.RewrittenSQL)
		}
	}
}

func TestRewriteStatementTypeFilter(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "select_only_filter",
			Enabled: true,
			Match: RewriteRuleMatch{
				Tables:        []string{"orders"},
				StatementType: []string{"SELECT"},
			},
			Actions: []RewriteAction{
				{Type: ActionAddWhere, Condition: "tenant_id = 1"},
			},
		},
	}
	rw := NewRewriter(rules)

	// SELECT should match
	result := rw.Apply("SELECT * FROM orders", nil)
	if !result.Rewritten {
		t.Error("expected SELECT to be rewritten")
	}

	// INSERT should NOT match
	result = rw.Apply("INSERT INTO orders (name) VALUES ('test')", nil)
	if result.Rewritten {
		t.Error("expected INSERT to NOT be rewritten")
	}
}

func TestRewriteRegexFilter(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "regex_match",
			Enabled: true,
			Match: RewriteRuleMatch{
				QueryPattern: "(?i)FROM\\s+legacy_",
			},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "legacy_users", To: "users"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM legacy_users WHERE id = 1", nil)
	if !result.Rewritten {
		t.Fatal("expected rewrite")
	}

	result = rw.Apply("SELECT * FROM users WHERE id = 1", nil)
	if result.Rewritten {
		t.Error("should not rewrite non-matching query")
	}
}

func TestRewriteDisabledRule(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "disabled_rule",
			Enabled: false,
			Match:   RewriteRuleMatch{},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "users", To: "nobody"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM users", nil)
	if result.Rewritten {
		t.Error("disabled rule should not be applied")
	}
}

func TestRewriteMultipleRules(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "rename_table",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"old_orders"}},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "old_orders", To: "orders"},
			},
		},
		{
			Name:    "add_tenant",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"orders"}},
			Actions: []RewriteAction{
				{Type: ActionAddWhere, Condition: "tenant_id = 1"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM old_orders", nil)
	if !result.Rewritten {
		t.Fatal("expected rewrite")
	}
	lower := strings.ToLower(result.RewrittenSQL)
	if strings.Contains(lower, "old_orders") {
		t.Errorf("old table name should be replaced: %s", result.RewrittenSQL)
	}
	// Note: The second rule matches "orders" which was just renamed from "old_orders".
	// Since we apply rules sequentially on the same tree, the second rule
	// sees the already-renamed table. We need to re-check match after modifications.
}

func TestRewriteWithParsedQuery(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "rename",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"old_users"}},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "old_users", To: "users"},
			},
		},
	}
	rw := NewRewriter(rules)

	pq, err := NewParsedQuery("SELECT * FROM old_users")
	if err != nil {
		t.Fatal(err)
	}

	result := rw.Apply("SELECT * FROM old_users", pq)
	if !result.Rewritten {
		t.Fatal("expected rewrite with pre-parsed query")
	}
	if !strings.Contains(result.AppliedRules[0], "rename") {
		t.Errorf("expected rule name in applied rules: %v", result.AppliedRules)
	}
}

func TestRewriteFailOpen(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "test",
			Enabled: true,
			Match:   RewriteRuleMatch{},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "x", To: "y"},
			},
		},
	}
	rw := NewRewriter(rules)

	// Invalid SQL should fail-open (return original query, no rewrite)
	result := rw.Apply("THIS IS NOT VALID SQL!!!", nil)
	if result.Rewritten {
		t.Error("invalid SQL should not be rewritten")
	}
}

func TestRewriteAddWhereInvalidCondition(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "bad_condition",
			Enabled: true,
			Match:   RewriteRuleMatch{},
			Actions: []RewriteAction{
				{Type: ActionAddWhere, Condition: "INVALID SQL !!!((("},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM users", nil)
	if result.Rewritten {
		t.Error("invalid condition should not result in rewrite")
	}
}

func TestRewriteRulesMethod(t *testing.T) {
	rules := []RewriteRule{
		{Name: "rule_a", Enabled: true, Actions: []RewriteAction{{Type: ActionTableRename, From: "a", To: "b"}}},
		{Name: "rule_b", Enabled: true, Actions: []RewriteAction{{Type: ActionTableRename, From: "c", To: "d"}}},
		{Name: "rule_c", Enabled: false, Actions: []RewriteAction{{Type: ActionTableRename, From: "e", To: "f"}}},
	}
	rw := NewRewriter(rules)

	names := rw.Rules()
	if len(names) != 2 {
		t.Errorf("expected 2 active rules, got %d", len(names))
	}
	if names[0] != "rule_a" || names[1] != "rule_b" {
		t.Errorf("unexpected rule names: %v", names)
	}
}

func TestRewriteSubquery(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "rename_in_subquery",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"old_users"}},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "old_users", To: "users"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("SELECT * FROM orders WHERE user_id IN (SELECT id FROM old_users)", nil)
	if !result.Rewritten {
		t.Fatal("expected rewrite in subquery")
	}
	if strings.Contains(strings.ToLower(result.RewrittenSQL), "old_users") {
		t.Errorf("old table name should be replaced in subquery: %s", result.RewrittenSQL)
	}
}

func TestRewriteCTE(t *testing.T) {
	rules := []RewriteRule{
		{
			Name:    "rename_in_cte",
			Enabled: true,
			Match:   RewriteRuleMatch{Tables: []string{"old_users"}},
			Actions: []RewriteAction{
				{Type: ActionTableRename, From: "old_users", To: "users"},
			},
		},
	}
	rw := NewRewriter(rules)

	result := rw.Apply("WITH active AS (SELECT id FROM old_users WHERE active = true) SELECT * FROM active", nil)
	if !result.Rewritten {
		t.Fatal("expected rewrite in CTE")
	}
	if strings.Contains(strings.ToLower(result.RewrittenSQL), "old_users") {
		t.Errorf("old table name should be replaced in CTE: %s", result.RewrittenSQL)
	}
}
