package logfilter

import (
	"strings"
	"testing"
)

// entry is a LogEntry in its JSON form, as protojson writes it.
func entry() Map {
	return Map{
		"logName":          "projects/p1/logs/cloudaudit.googleapis.com%2Factivity",
		"insertId":         "abc123",
		"timestamp":        "2023-11-29T23:15:00.5Z",
		"receiveTimestamp": "2023-11-29T23:15:01Z",
		"severity":         "ERROR",
		"trace":            "projects/p1/traces/t1",
		"resource": map[string]any{"type": "gce_instance",
			"labels": map[string]any{"instance_id": "12345", "zone": "us-central1-a"}},
		"labels": map[string]any{"env_name": "prod", "compute.googleapis.com/resource_name": "vm-1"},
		"httpRequest": map[string]any{"requestMethod": "GET", "status": 404, "remoteIp": "10.1.2.3",
			"latency": "1.500s", "cacheHit": true, "requestSize": "1024", "userAgent": "curl/8.0"},
		"jsonPayload": map[string]any{"message": "Hello World from the worker", "code": 7.0, "ok": false,
			"nested": map[string]any{"x": map[string]any{"y": "deep value"}},
			"items":  []any{map[string]any{"name": "alpha"}, map[string]any{"name": "beta"}},
			"tags":   []any{"a", "b"}, "nothing": nil, "latency": "12.5ms", "when": "2023-11-29T10:00:00Z"},
		"protoPayload": map[string]any{"@type": "type.googleapis.com/google.cloud.audit.AuditLog",
			"methodName": "v1.compute.instances.insert", "status": map[string]any{"code": 7.0}},
		"operation": map[string]any{"id": "op-1", "first": true},
	}
}

// TestLanguage is each construct the Logging query language reference shows
// (docs.cloud.google.com/logging/docs/view/logging-query-language), against
// one entry.
func TestLanguage(t *testing.T) {
	for _, c := range []struct {
		q    string
		want bool
	}{
		// Comparisons.
		{`resource.type = "gce_instance"`, true},
		{`resource.type = gce_instance`, true},
		{`resource.type = "GCE_INSTANCE"`, true}, // strings ignore case
		{`resource.type != gce_instance`, false},
		{`resource.type:"gce"`, true},
		{`resource.type:"GCE"`, true},
		{`resource.type:"cloudsql"`, false},
		{`httpRequest.status >= 400`, true},
		{`httpRequest.status < 400`, false},
		{`httpRequest.status = 404`, true},
		{`httpRequest.status > 404`, false},
		{`httpRequest.status <= 404`, true},
		{`httpRequest.requestSize > 1000`, true}, // int64, a string in JSON
		{`jsonPayload.code = 7`, true},
		{`jsonPayload.code > 6.5`, true},
		{`jsonPayload.code >= 7e0`, true},
		{`jsonPayload.ok = false`, true},
		{`jsonPayload.ok = FALSE`, true},
		{`httpRequest.cacheHit = true`, true},
		{`httpRequest.cacheHit > false`, true}, // true is greater than false
		{`httpRequest.latency > "1s"`, true},
		{`httpRequest.latency < 2s`, true},
		{`httpRequest.latency = "1500ms"`, true},
		{`jsonPayload.code = "seven"`, false}, // a failed conversion fails the comparison
		{`jsonPayload.nothing = NULL_VALUE`, true},
		{`jsonPayload.message = NULL_VALUE`, false},
		{`jsonPayload.message != NULL_VALUE`, true},
		// Has and presence.
		{`jsonPayload.message:"world"`, true},
		{`jsonPayload.message:world`, true},
		{`jsonPayload:"deep"`, true}, // some subfield has it
		{`jsonPayload.nested:*`, true},
		{`operation.id:*`, true},
		{`operation.producer:*`, false}, // defaulted, not present
		{`jsonPayload.absent:*`, false},
		{`trace:*`, true},
		// Missing and defaulted fields.
		{`jsonPayload.absent = foo`, false},
		{`jsonPayload.absent != foo`, false},
		{`NOT jsonPayload.absent = foo`, true},
		{`labels.missing != x`, false},
		{`operation.producer = ""`, true}, // defaulted: compared as its default
		{`operation.last = false`, true},
		{`sourceLocation.line = 0`, true},
		{`spanId != "x"`, true},
		// Severity ordering.
		{`severity = ERROR`, true},
		{`severity = error`, true},
		{`severity >= WARNING`, true},
		{`severity > ERROR`, false},
		{`severity < CRITICAL`, true},
		{`severity <= DEBUG`, false},
		{`severity >= 500`, true},
		{`severity = 500`, true},
		{`severity != DEFAULT`, true},
		{`severity:ERROR`, true},
		// Timestamps.
		{`timestamp >= "2023-11-29T23:00:00Z"`, true},
		{`timestamp <= "2023-11-29T23:30:00Z"`, true},
		{`timestamp >= "2023-11-29T23:00:00Z" AND timestamp <= "2023-11-29T23:30:00Z"`, true},
		{`timestamp > "2023-11-29"`, true}, // a date is midnight UTC
		{`timestamp < "2023-11-29"`, false},
		{`timestamp > "2023-11-30T00:15:00+01:00"`, true},
		{`timestamp = "2023-11-29T23:15:00.500000000Z"`, true},
		{`receiveTimestamp > timestamp`, false}, // "timestamp" is a string here, not a time
		{`timestamp > "yesterday"`, false},
		// Boolean operators, precedence and implicit AND.
		{`severity = ERROR AND resource.type = gce_instance`, true},
		{`severity = ERROR resource.type = gce_instance`, true},
		{"severity = ERROR\nresource.type = cloud_run_revision", false},
		{`severity = INFO OR resource.type = gce_instance`, true},
		{`severity = INFO OR resource.type = k8s_container`, false},
		{`NOT severity = INFO`, true},
		{`-severity = INFO`, true},
		{`-severity = ERROR`, false},
		{`NOT NOT severity = ERROR`, true},
		// NOT binds tightest, then OR, then AND:
		// "a" OR NOT "b" AND NOT "c" OR "d" is ("a" OR (NOT "b")) AND ((NOT "c") OR "d").
		{`"nomatch" OR NOT "Hello" AND NOT "worker" OR "alpha"`, false},
		{`"nomatch" OR NOT "nomatch2" AND NOT "worker" OR "alpha"`, true},
		{`severity = INFO OR severity = ERROR AND resource.type = gce_instance`, true},
		{`severity = ERROR OR severity = INFO AND resource.type = k8s_container`, false},
		// Parentheses.
		{`(severity = INFO OR severity = ERROR) AND resource.type = gce_instance`, true},
		{`severity = INFO OR (severity = ERROR AND resource.type = k8s_container)`, false},
		{`NOT (severity = INFO OR jsonPayload.code = 8)`, true},
		{`((severity = ERROR))`, true},
		// A parenthesized value list applies the field and operator to each.
		{`severity = (INFO OR ERROR)`, true},
		{`severity = (INFO OR WARNING)`, false},
		{`jsonPayload.message:("hello" AND "worker")`, true},
		{`jsonPayload.message:("hello" AND "nope")`, false},
		{`jsonPayload.message:(NOT "nope")`, true},
		{`jsonPayload.code = (-1 OR 7)`, true},
		// Field paths.
		{`labels.env_name = prod`, true},
		{`labels.env_name = PROD`, true},
		{`labels.envName = prod`, false}, // map keys are case-sensitive
		{`labels."compute.googleapis.com/resource_name" = "vm-1"`, true},
		{`resource.labels.instance_id = "12345"`, true},
		{`resource.labels.zone:"central1"`, true},
		{`jsonPayload.nested.x.y = "deep value"`, true},
		{`jsonPayload."nested".x.y = "deep value"`, true},
		{`jsonPayload.items.name = beta`, true}, // repeated: any element
		{`jsonPayload.tags = b`, true},
		{`protoPayload.@type = "type.googleapis.com/google.cloud.audit.AuditLog"`, true},
		{`protoPayload.methodname:"instances.insert"`, true}, // protoPayload names ignore case
		{`protoPayload.status.code = 7`, true},
		{`http_request.status = 404`, true}, // proto names
		{`logName = "projects/p1/logs/cloudaudit.googleapis.com%2Factivity"`, true},
		{`logName:"cloudaudit"`, true},
		// Regular expressions.
		{`jsonPayload.message =~ "World"`, true},
		{`jsonPayload.message =~ "world"`, false}, // case-sensitive
		{`jsonPayload.message =~ "(?i)world"`, true},
		{`jsonPayload.message !~ "world"`, true},
		{`logName =~ "^projects/p1"`, true},
		{`logName =~ "activity$"`, true},
		{`labels.env_name =~ ("^prod" OR "^staging")`, true},
		{`httpRequest.status =~ "404"`, false}, // only strings match
		{`jsonPayload.message =~ "Hello\\s+World"`, true},
		// Global restrictions.
		{`"Hello World"`, true},
		{`hello`, true},
		{`worker AND 12345`, true},
		{`unicorn`, false},
		{`and`, false}, // lower-case keywords are search terms
		{`"10.1.2.3"`, true},
		// SEARCH.
		{`SEARCH("hello")`, true},
		{`SEARCH("world hello")`, true}, // order does not matter
		{`SEARCH("wor")`, false},        // whole tokens, not substrings
		{"SEARCH(\"`hello world`\")", true},
		{"SEARCH(\"`world hello`\")", false},
		{`SEARCH(jsonPayload.message, "worker")`, true},
		{`SEARCH(jsonPayload.message, "alpha")`, false},
		{`SEARCH(jsonPayload, "alpha deep")`, true}, // tokens may span fields
		{`search("HELLO")`, true},
		// log_id.
		{`log_id("cloudaudit.googleapis.com/activity")`, true},
		{`LOG_ID("cloudaudit.googleapis.com/activity")`, true},
		{`log_id("cloudaudit.googleapis.com%2Factivity")`, false}, // URL-encoded IDs match nothing
		{`NOT log_id("stdout")`, true},
		// sample.
		{`sample(insertId, 1)`, true},
		{`sample(jsonPayload.absent, 1)`, false},
		{`jsonPayload.code:* AND sample(jsonPayload.code, 1.0)`, true},
		// ip_in_net.
		{`ip_in_net(httpRequest.remoteIp, "10.0.0.0/8")`, true},
		{`ip_in_net(httpRequest.remoteIp, "10.1.2.3")`, true},
		{`ip_in_net(httpRequest.remoteIp, "192.168.0.0/16")`, false},
		{`ip_in_net(httpRequest.remoteIp, "2001:db8::/32")`, false},
		{`ip_in_net(jsonPayload.message, "10.0.0.0/8")`, false}, // not an address
		{`ip_in_net(jsonPayload.absent, "10.0.0.0/8")`, false},
		// cast and regexp_extract.
		{`cast(httpRequest.status, STRING) = "404"`, true},
		{`cast(httpRequest.status, STRING):"40"`, true},
		{`cast(jsonPayload.code, INT64) = 7`, true},
		{`cast(resource.labels.instance_id, INT64) > 12000`, true},
		{`cast(resource.labels.instance_id, FLOAT64) = 12345`, true},
		{`cast(jsonPayload.when, TIMESTAMP) < "2023-11-29T12:00:00Z"`, true},
		{`cast(jsonPayload.latency, DURATION) < "1s"`, true},
		{`cast(jsonPayload.ok, BOOL) = false`, true},
		{`cast(timestamp, STRING) =~ "^2023-11-29T23"`, true},
		{`cast(timestamp, STRING, TIME_ZONE("America/New_York")) =~ "^2023-11-29T18"`, true},
		{`CAST(REGEXP_EXTRACT(CAST(timestamp, STRING), "\\d+:\\d+:(\\d+)"), INT64) < 30`, true},
		{`CAST(REGEXP_EXTRACT(CAST(timestamp, STRING), "\\d+:(\\d+):\\d+"), INT64) = 15`, true},
		{`regexp_extract(jsonPayload.latency, "([0-9.]+)ms") = "12.5"`, true},
		{`regexp_extract(jsonPayload.message, "(nomatch)") = "nomatch"`, false},
		// Comments.
		{"-- the worker's errors\nseverity = ERROR -- and nothing else\n", true},
		{"severity = INFO\n-- resource.type = gce_instance\n", false},
	} {
		f, err := Parse(c.q, Options{})
		if err != nil {
			t.Errorf("%q refused: %v", c.q, err)
			continue
		}
		if got := f.Match(entry()); got != c.want {
			t.Errorf("%q = %v, want %v", c.q, got, c.want)
		}
	}
}

// TestEmpty: an empty or comment-only query matches every entry.
func TestEmpty(t *testing.T) {
	for _, q := range []string{"", "  \n", "-- nothing"} {
		f, err := Parse(q, Options{})
		if err != nil || !f.Match(entry()) {
			t.Errorf("%q = %v, %v", q, f, err)
		}
	}
}

// TestErrors are the documented refusals: syntax errors, undefined fields,
// unquoted or invalid regular expressions, and bad function arguments, each
// with the language's message.
func TestErrors(t *testing.T) {
	for q, want := range map[string]string{
		`severity = ERROR OR`:                     "Unparseable filter: syntax error at line 1, column 20, token '<EOF>'",
		`OR severity = ERROR`:                     "Unparseable filter: syntax error at line 1, column 1, token 'OR'",
		`severity = ERROR AND AND x`:              "token 'AND'",
		`(severity = ERROR`:                       "token '<EOF>'",
		`severity = ERROR)`:                       "token ')'",
		"severity = ERROR\n  AND )":               "line 2, column 7, token ')'",
		`severity =`:                              "token '<EOF>'",
		`severity == ERROR`:                       "token '=='",
		`= ERROR`:                                 "token '='",
		`severity = ERROR NOT`:                    "token '<EOF>'",
		`jsonPayload.message:"open`:               "unterminated string",
		`foo = bar`:                               `field "foo" is not defined in LogEntry`,
		`resource.lables.zone = a`:                `field "resource.lables.zone" is not defined`,
		`httpRequest.code = 1`:                    `"code" is not a field of httpRequest`,
		`logName.x = 1`:                           `"x" is not a field of logName`,
		`jsonPayload.message =~ world`:            "must be double-quoted",
		`jsonPayload.message =~ "("`:              "invalid regular expression",
		`SEARCH()`:                                "SEARCH takes a query string",
		`SEARCH(jsonPayload)`:                     "SEARCH takes a query string",
		`log_id(stdout)`:                          "log_id takes one double-quoted log ID",
		`sample(insertId, 0)`:                     "must be greater than 0 and at most 1",
		`sample(insertId, 1.5)`:                   "must be greater than 0 and at most 1",
		`ip_in_net(httpRequest.remoteIp, "10/x")`: "is not an IP address or subnet",
		`cast(httpRequest.status, NUMBER) = 1`:    "CAST to NUMBER is not supported",
		`cast(timestamp, STRING, TIME_ZONE("Mars/Base")) = 1`: "unknown time zone",
		`regexp_extract(jsonPayload.message, "(a)(b)") = "a"`: "exactly one capture group",
		`regexp_extract(jsonPayload.message, "a") = "a"`:      "exactly one capture group",
		`cast(httpRequest.status, STRING)`:                    "yields a value",
		`nosuchfn("x")`:                                       `unknown function "nosuchfn"`,
		`source("projects/p1")`:                               "source() is not supported here",
		`labels."a/b`:                                         "unterminated string",
		`labels.."a" = 1`:                                     "syntax error",
		strings.Repeat("a ", MaxLen/2) + "b":                  "longer than 20000 characters",
	} {
		_, err := Parse(q, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60q = %v, want an error with %q", q, err, want)
		}
		if _, ok := err.(*Error); err != nil && !ok {
			t.Errorf("%.60q: %T is not a *Error", q, err)
		}
	}
}

// TestSource is source() through the caller's hook.
func TestSource(t *testing.T) {
	opts := Options{Source: func(arg string) (func(Record) bool, error) {
		return func(r Record) bool {
			v, _ := r.Field("logName")
			return strings.HasPrefix(v.(string), arg+"/")
		}, nil
	}}
	for q, want := range map[string]bool{`source("projects/p1")`: true, `source("projects/p2")`: false,
		`NOT source("projects/p2") AND severity = ERROR`: true} {
		f, err := Parse(q, opts)
		if err != nil || f.Match(entry()) != want {
			t.Errorf("%q = %v, want %v", q, err, want)
		}
	}
}

// TestCheckView: log views take AND, OR, NOT on one term, comparisons,
// source() and log_id(); not NOT on a compound statement or SEARCH.
func TestCheckView(t *testing.T) {
	opts := Options{Source: func(string) (func(Record) bool, error) { return func(Record) bool { return true }, nil }}
	for q, ok := range map[string]bool{
		`source("projects/p1") AND log_id("stdout")`:          true,
		`NOT resource.type = "gce_instance"`:                  true,
		`log_id("a") OR log_id("b")`:                          true,
		`resource.labels.zone:"us" AND labels.env =~ "^prod"`: true,
		`severity >= ERROR`:                                   true,
		`NOT (log_id("a") AND resource.type = "global")`:      false,
		`NOT (log_id("a") OR log_id("b"))`:                    false,
		`SEARCH("x")`:                                         false,
		`hello`:                                               false,
		`sample(insertId, 0.5)`:                               false,
	} {
		f, err := Parse(q, opts)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if err := f.CheckView(); (err == nil) != ok {
			t.Errorf("%q: CheckView = %v, want ok %v", q, err, ok)
		}
	}
}

// TestSampleIsDeterministic: sample keeps about the fraction asked, and the
// same entries each time.
func TestSampleIsDeterministic(t *testing.T) {
	f, err := Parse(`sample(insertId, 0.25)`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for i := range 4000 {
		e := Map{"insertId": "id-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)}
		a, b := f.Match(e), f.Match(e)
		if a != b {
			t.Fatal("sample is not deterministic")
		}
		if a {
			kept++
		}
	}
	if kept < 800 || kept > 1200 {
		t.Errorf("sample(insertId, 0.25) kept %d of 4000", kept)
	}
}
