package iamcond

import (
	"strings"
	"testing"
	"time"
)

func TestEval(t *testing.T) {
	at := time.Date(2026, 4, 5, 14, 30, 15, 250e6, time.UTC) // a Sunday
	a := Attributes{ResourceName: "projects/p1/databases/db1", ResourceType: "firestore.googleapis.com/Database",
		ResourceService: "firestore.googleapis.com", RequestTime: at}
	for _, c := range []struct {
		src  string
		want bool
	}{
		{`resource.name == "projects/p1/databases/db1"`, true},
		{`resource.name == "projects/p1/databases/db2"`, false},
		{`resource.name != 'projects/p1/databases/db2'`, true},
		{`resource.name.startsWith("projects/p1/")`, true},
		{`resource.name.endsWith("/db1") && resource.type == "firestore.googleapis.com/Database"`, true},
		{`resource.service == "storage.googleapis.com" || resource.service == "firestore.googleapis.com"`, true},
		{`resource.name.extract("projects/{project}/") == "p1"`, true},
		{`resource.name.extract("databases/{db}") == "db1"`, true},
		{`resource.name.extract("buckets/{b}/") == ""`, true},
		{`!(resource.name == "x")`, true},
		{`request.time < timestamp("2027-01-01T00:00:00Z")`, true},
		{`request.time > timestamp("2027-01-01T00:00:00Z")`, false},
		{`request.time < timestamp("bad")`, false},
		{`request.time >= date("2026-04-05") && request.time < date("2026-04-05") + duration("86400s")`, true},
		{`request.time - duration("3600s") < timestamp("2026-04-05T14:00:00Z")`, true},
		{`request.time.getDayOfWeek() == 0`, true},
		{`request.time.getHours() == 14 && request.time.getMinutes() == 30 && request.time.getSeconds() == 15`, true},
		{`request.time.getHours("America/New_York") == 10`, true},
		{`request.time.getHours("+05:30") == 20`, true},
		{`request.time.getHours("Not/AZone") == 10`, false},
		{`request.time.getMonth() == 3 && request.time.getDate() == 5 && request.time.getDayOfMonth() == 4`, true},
		{`request.time.getDayOfYear() == 94 && request.time.getFullYear() == 2026 && request.time.getMilliseconds() == 250`, true},
		{`resource.type in ["a", "firestore.googleapis.com/Database"]`, true},
		{`true`, true},
		{`false || (resource.name == "projects/p1/databases/db1")`, true},
	} {
		e, err := Compile(c.src)
		if err != nil {
			t.Errorf("Compile(%s): %v", c.src, err)
			continue
		}
		if got := e.Eval(a); got != c.want {
			t.Errorf("Eval(%s) = %v, want %v", c.src, got, c.want)
		}
	}
	// An attribute the resource lacks grants nothing, unless the other side decides.
	if Eval(`resource.name == "x"`, Attributes{}) {
		t.Error("an unset resource.name granted")
	}
	if !Eval(`resource.name == "x" || true`, Attributes{}) {
		t.Error("|| true did not absorb the error")
	}
}

func TestCompileErrors(t *testing.T) {
	for src, want := range map[string]string{
		``:                    "unexpected",
		`resource.name ==`:    "unexpected",
		`resource.name == "a`: "unterminated",
		`resource.name`:       "must be a boolean",
		`principal.type == "iam.googleapis.com/ServiceAccount"`: "Principal Access Boundary",
		`principal.subject.startsWith("a")`:                     "Principal Access Boundary",
		`request.foo == "x"`:                                    "undefined field",
		`resource.hasTagKey(1)`:                                 "no matching overload",
		`resource.matchTag("k")`:                                "no matching overload",
		`api.getAttribute("x") == "y"`:                          "no matching overload",
		`api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', ['a']).hasOnly(['a'])`: "must be []",
		`compute.isForwardingRuleCreationOperation(1)`:                                      "no matching overload",
		`destination.port == "22"`:                                                          "no matching overload",
		`request.auth == "x"`:                                                               "request.auth.access_levels",
		`resource.unknownTagFn("a")`:                                                        "undeclared",
		`foo == 1`:                                                                          "undeclared",
		`resource.name == 1`:                                                                "no matching overload",
		`resource.name.contains("a")`:                                                       "no matching overload",
		`request.time < "2026"`:                                                             "no matching overload",
		`resource.name.extract("a/b") == ""`:                                                "placeholder",
		`unknownFn("a")`:                                                                    "undeclared",
		`resource.name == "a" $`:                                                            "unexpected character",
		strings.Repeat(`true && `, 13) + `true`:                                             "logical operators",
	} {
		_, err := Compile(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%q) = %v, want an error containing %q", src, err, want)
		}
	}
	if _, err := Compile(strings.Repeat(`true && `, 12) + `true`); err != nil {
		t.Errorf("12 logical operators: %v", err)
	}
}

func TestEvalRemainingAttributes(t *testing.T) {
	a := Attributes{
		Tags:          []Tag{{NamespacedKey: "123456789012/env", ShortValue: "prod", KeyID: "tagKeys/111", ValueID: "tagValues/222"}},
		TagsKnown:     true,
		RequestPath:   "/admin/users",
		RequestHost:   "app.example.com",
		AccessLevels:  []string{"accessPolicies/1/accessLevels/corp"},
		DestinationIP: "10.0.0.5", DestinationPort: 22,
		API:            map[string]any{"iam.googleapis.com/modifiedGrantsByRole": []string{"roles/pubsub.editor"}},
		ForwardingRule: &ForwardingRule{Creation: true, Schemes: []string{"INTERNAL"}},
	}
	for _, c := range []struct {
		src  string
		want bool
	}{
		{`resource.hasTagKey("123456789012/env")`, true},
		{`resource.hasTagKey("123456789012/team")`, false},
		{`resource.hasTagKeyId("tagKeys/111")`, true},
		{`resource.matchTag("123456789012/env", "prod")`, true},
		{`resource.matchTag("123456789012/env", "dev")`, false},
		{`resource.matchTagId("tagKeys/111", "tagValues/222")`, true},
		{`request.path.startsWith("/admin/")`, true},
		{`request.path == "/admin"`, false},
		{`request.host.endsWith(".example.com")`, true},
		{`"accessPolicies/1/accessLevels/corp" in request.auth.access_levels`, true},
		{`"accessPolicies/1/accessLevels/home" in request.auth.access_levels`, false},
		{`destination.ip == "10.0.0.5" && destination.port < 1024`, true},
		{`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", []).hasOnly(["roles/pubsub.editor", "roles/pubsub.publisher"])`, true},
		{`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", []).hasOnly(["roles/pubsub.publisher"])`, false},
		{`api.getAttribute("mcp.googleapis.com/tool.name", "") == ""`, true},
		{`api.getAttribute("mcp.googleapis.com/tool.isReadOnly", false)`, false},
		{`!compute.isForwardingRuleCreationOperation() || (compute.isForwardingRuleCreationOperation() && compute.matchLoadBalancingSchemes(["INTERNAL", "INTERNAL_MANAGED"]))`, true},
		{`compute.matchLoadBalancingSchemes(["EXTERNAL"])`, false},
	} {
		e, err := Compile(c.src)
		if err != nil {
			t.Errorf("Compile(%s): %v", c.src, err)
			continue
		}
		if got := e.Eval(a); got != c.want {
			t.Errorf("Eval(%s) = %v, want %v", c.src, got, c.want)
		}
	}
	// Where the check does not supply an attribute, its part grants nothing.
	for _, src := range []string{
		`resource.hasTagKey("123456789012/env")`,
		`!resource.hasTagKey("123456789012/env")`,
		`request.path.startsWith("/")`,
		`"x" in request.auth.access_levels || !("x" in request.auth.access_levels)`,
		`destination.port > 0`,
		`compute.isForwardingRuleCreationOperation()`,
	} {
		if Eval(src, Attributes{}) {
			t.Errorf("Eval(%s) without the attribute granted", src)
		}
	}
	if !Eval(`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", []).hasOnly([])`, Attributes{}) {
		t.Error("an undefined API attribute did not return its default")
	}
}

// TestMapDefault is api.getAttribute with a map default (#1748): every
// expression Google's "Control access to Memory Bank with IAM Conditions"
// gives for aiplatform.googleapis.com/memoryScope.
func TestMapDefault(t *testing.T) {
	const scope = `api.getAttribute('aiplatform.googleapis.com/memoryScope', {})`
	userA := Attributes{API: map[string]any{"aiplatform.googleapis.com/memoryScope": map[string]string{"userId": "userA"}}}
	other := Attributes{API: map[string]any{"aiplatform.googleapis.com/memoryScope": map[string]string{"user_id": "userA", "admin_override": "1"}}}
	none := Attributes{}
	for _, c := range []struct {
		src               string
		userA, other, nil bool
	}{
		{scope + ` == {'userId': 'userA'}`, true, false, false},
		{scope + ` != {'userId': 'userA'}`, false, true, true},
		{scope + `['userId'] == 'userA'`, true, false, false},
		{`('admin_override' in ` + scope + `) || ('public_access_flag' in ` + scope + `)`, false, true, false},
		{scope + `['userId'].startsWith('user')`, true, false, false},
		{scope + `['userId'] in ['userA', 'userB']`, true, false, false},
		{`'user_id' in ` + scope + ` && ` + scope + `['user_id'] == 'userA'`, false, true, false},
		{scope + `['user_id'] == 'userA'`, false, true, false},
		{scope + ` == {}`, false, false, true},
		{`{'a': 1, 'b': 2}['b'] == 2`, true, true, true},
		{`{'a': 1}['b'] == 2 || true`, true, true, true},
		{`['x', 'y'][1] == 'y'`, true, true, true},
		{`api.getAttribute('aiplatform.googleapis.com/sessionUserId', '') == ''`, true, true, true},
	} {
		e, err := Compile(c.src)
		if err != nil {
			t.Errorf("Compile(%s): %v", c.src, err)
			continue
		}
		for _, w := range []struct {
			a    Attributes
			want bool
		}{{userA, c.userA}, {other, c.other}, {none, c.nil}} {
			if got := e.Eval(w.a); got != w.want {
				t.Errorf("Eval(%s) on %v = %v, want %v", c.src, w.a.API, got, w.want)
			}
		}
	}
	for src, want := range map[string]string{
		`api.getAttribute('aiplatform.googleapis.com/memoryScope', '') == ''`:        "the attribute is a map(string, string)",
		`api.getAttribute('aiplatform.googleapis.com/memoryScope', {})[1] == 'a'`:    "no matching overload",
		`api.getAttribute('aiplatform.googleapis.com/memoryScope', {})['a'] == 1`:    "no matching overload",
		`api.getAttribute('aiplatform.googleapis.com/sessionUserId', {}) == {}`:      "the attribute is a string",
		`api.getAttribute('aiplatform.googleapis.com/memoryScope', {}) == {'a' 'b'}`: "expected \":\"",
		`{'a': 'b'}`:              "must be a boolean",
		`{'a': 'b'} == {'a': 1}`:  "no matching overload",
		`resource.name['a'] == 1`: "no matching overload",
	} {
		_, err := Compile(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%q) = %v, want an error containing %q", src, err, want)
		}
	}
}
