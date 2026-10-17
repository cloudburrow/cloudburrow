package bigqueryfront

import (
	"reflect"
	"strings"
	"testing"
)

func TestDataPolicyNames(t *testing.T) {
	toks, _ := lex(`["{'name':'p1.region-us.dp1'}", "{\"name\":\"p1.region-EU.dp2\"}"]`)
	names, msg := dataPolicyNames(toks)
	want := []string{"projects/p1/locations/us/dataPolicies/dp1", "projects/p1/locations/eu/dataPolicies/dp2"}
	if msg != "" || !reflect.DeepEqual(names, want) {
		t.Errorf("names %v, %q", names, msg)
	}
	for _, bad := range []string{`'x'`, `["dp1"]`, `["{'name':'p1.us.dp1'}"]`, `["{'name':'dp1'}"]`, `[1]`} {
		toks, _ := lex(bad)
		if _, msg := dataPolicyNames(toks); !strings.Contains(msg, "data") {
			t.Errorf("%s: %q, want a refusal", bad, msg)
		}
	}
	f := map[string]any{"name": "a"}
	toks, _ = lex(`["{'name':'p.region-us.a'}"]`)
	setDataPolicies(f, alterOption{name: "data_policies", val: toks})
	toks, _ = lex(`["{'name':'p.region-us.a'}", "{'name':'p.region-us.b'}"]`)
	setDataPolicies(f, alterOption{name: "data_policies", val: toks, add: true})
	if got := f["dataPolicies"].([]any); len(got) != 2 {
		t.Errorf("+= gave %v", got)
	}
	toks, _ = lex(`NULL`)
	setDataPolicies(f, alterOption{name: "data_policies", val: toks})
	if _, ok := f["dataPolicies"]; ok {
		t.Errorf("NULL left %v", f["dataPolicies"])
	}
}
