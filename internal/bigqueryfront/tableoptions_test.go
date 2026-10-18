package bigqueryfront

import (
	"strconv"
	"testing"
)

// TestPrivacyPolicyTableOption (#1580): TABLE_OPTIONS lists privacy_policy
// as a STRING of the DDL's JSON, whether DDL or tables.insert set it.
func TestPrivacyPolicyTableOption(t *testing.T) {
	ddl := `{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}}`
	find := func(tb infoTable) (string, string) {
		for _, o := range tableOptions(tb) {
			if o[0] == "privacy_policy" {
				return o[1], o[2]
			}
		}
		return "", ""
	}
	typ, val := find(infoTable{"tableId": "v", settingsKey: tableSetting{PrivacyPolicy: &ddl}})
	if typ != "STRING" || val != strconv.Quote(ddl) {
		t.Errorf("DDL policy: %s %s", typ, val)
	}
	rest := infoTable{"tableId": "v", "view": map[string]any{"query": "SELECT 1", "privacyPolicy": map[string]any{
		"joinRestrictionPolicy": map[string]any{"joinCondition": "JOIN_ALL", "joinAllowedColumns": []any{"id"}}}}}
	typ, val = find(rest)
	want := strconv.Quote(`{"join_restriction_policy":{"join_allowed_columns":["id"],"join_condition":"JOIN_ALL"}}`)
	if typ != "STRING" || val != want {
		t.Errorf("REST policy: %s %s, want %s", typ, val, want)
	}
	if typ, _ := find(infoTable{"tableId": "t"}); typ != "" {
		t.Errorf("a table with no policy lists privacy_policy")
	}
}
