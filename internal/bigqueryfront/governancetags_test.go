package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestGovernanceTags (#1456): data_governance_tags's value read as the
// field's dataGovernanceTagsInfo, what BigQuery refuses, and a schema's
// tags read and kept.
func TestGovernanceTags(t *testing.T) {
	for val, want := range map[string]string{
		`[("p/k", "v")]`:               `{"dataGovernanceTags":{"p/k":"v"}}`,
		`ARRAY[STRUCT('p/k', 'v')]`:    `{"dataGovernanceTags":{"p/k":"v"}}`,
		`NULL`:                         `{}`,
		`[]`:                           `{"dataGovernanceTags":{}}`,
		`[("p/k", "v"), ("p/l", "w")]`: "Only one",
		`[("k", "v")]`:                 "namespaced",
		`[("p/k", 1)]`:                 "must be an array",
		`("p/k", "v")`:                 "must be an array",
		`[("p/k", "v") ("p/l", "w")]`:  "must be an array",
	} {
		toks, _ := lex(val)
		info, msg := governanceTagsOption(toks)
		got := msg
		if msg == "" {
			b, _ := json.Marshal(info)
			got = string(b)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: %s, want %s", val, got, want)
		}
	}
	tags, msg, nested := governanceTagsOf(json.RawMessage(`{"fields":[{"name":"A","dataGovernanceTagsInfo":{"dataGovernanceTags":{"p/k":"v"}}},` +
		`{"name":"b","dataGovernanceTagsInfo":{}},{"name":"c"}]}`))
	if fmt.Sprint(tags) != "map[a:map[p/k:v] b:map[]]" || msg != "" || nested != "" {
		t.Errorf("governanceTagsOf = %v %q %q", tags, msg, nested)
	}
	if _, _, nested := governanceTagsOf(json.RawMessage(`{"fields":[{"name":"s","fields":[{"name":"x","dataGovernanceTagsInfo":{}}]}]}`)); !strings.Contains(nested, "s.x") {
		t.Errorf("a RECORD's field: %q, want a 501 naming s.x", nested)
	}
	s := &tableSettings{}
	s.setGovernanceTags("k", "1", tags, true, nil)
	s.setGovernanceTags("k", "1", map[string]map[string]string{"c": {"p/c": "x"}}, false, nil)
	meta := map[string]any{"tableReference": map[string]any{"tableId": "t"}, "creationTime": "1",
		"schema": map[string]any{"fields": []any{map[string]any{"name": "a"}, map[string]any{"name": "C"}}}}
	s.m[streamKey("p", "d", "t")] = s.m["k"]
	if !s.withGovernanceTags("p", "d", meta) {
		t.Fatal("withGovernanceTags put none")
	}
	if b, _ := json.Marshal(meta["schema"]); string(b) != `{"fields":[{"dataGovernanceTagsInfo":{"dataGovernanceTags":{"p/k":"v"}},"name":"a"},`+
		`{"dataGovernanceTagsInfo":{"dataGovernanceTags":{"p/c":"x"}},"name":"C"}]}` {
		t.Errorf("schema = %s", b)
	}
	s.setGovernanceTags("k", "1", nil, false, map[string]bool{"c": true})
	if got := fmt.Sprint(s.m["k"].GovernanceTags); got != "map[c:map[p/c:x]]" {
		t.Errorf("after a's drop: %s", got)
	}
}
