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
		``:                                      "unexpected",
		`resource.name ==`:                      "unexpected",
		`resource.name == "a`:                   "unterminated",
		`resource.name`:                         "must be a boolean",
		`request.path == "/x"`:                  "not supported",
		`api.getAttribute("x", "") == "y"`:      "not supported",
		`foo == 1`:                              "undeclared",
		`resource.name == 1`:                    "no matching overload",
		`resource.name.contains("a")`:           "no matching overload",
		`request.time < "2026"`:                 "no matching overload",
		`resource.name.extract("a/b") == ""`:    "placeholder",
		`unknownFn("a")`:                        "undeclared",
		`resource.name == "a" $`:                "unexpected character",
		strings.Repeat(`true && `, 13) + `true`: "logical operators",
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
