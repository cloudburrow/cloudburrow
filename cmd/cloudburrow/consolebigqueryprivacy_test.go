package main

import (
	"strconv"
	"testing"
)

// TestAnalysisRuleProps (#1580): a privacy_policy, the DDL's JSON or the
// REST resource's, is one property per analysis rule, its settings by name.
func TestAnalysisRuleProps(t *testing.T) {
	policy := `{"join_restriction_policy": {"join_condition": "JOIN_ALL", "join_allowed_columns": ["id", "name"]},
		"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"},
		"differential_privacy_policy": {"max_epsilon_per_query": 10.01, "epsilon_budget": 25.6,
			"privacy_unit_column": "last_name", "delta_per_query": 0.01, "delta_budget": 0.1, "max_groups_contributed": 2}}`
	got := analysisRuleProps(unquoteOption(strconv.Quote(policy)))
	want := [][2]string{
		{"Aggregation threshold", "privacy_unit_column: last_name; threshold: 3"},
		{"Differential privacy", "delta_budget: 0.1; delta_per_query: 0.01; epsilon_budget: 25.6; max_epsilon_per_query: 10.01; " +
			"max_groups_contributed: 2; privacy_unit_column: last_name"},
		{"Join restriction", "join_allowed_columns: id, name; join_condition: JOIN_ALL"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, w := range want {
		if got[i].Label != w[0] || got[i].Value != w[1] {
			t.Errorf("rule %d: %q %q, want %q %q", i, got[i].Label, got[i].Value, w[0], w[1])
		}
	}
	rest := analysisRuleProps(`{"aggregationThresholdPolicy": {"threshold": "5", "privacyUnitColumns": ["u"]}}`)
	if len(rest) != 1 || rest[0].Label != "Aggregation threshold" || rest[0].Value != "privacyUnitColumns: u; threshold: 5" {
		t.Errorf("REST form: %v", rest)
	}
	if bad := analysisRuleProps("not json"); len(bad) != 1 || bad[0].Value != "not json" {
		t.Errorf("not JSON: %v", bad)
	}
	if got := unquoteOption("'{}'"); got != "{}" {
		t.Errorf("unquoteOption: %q", got)
	}
}
