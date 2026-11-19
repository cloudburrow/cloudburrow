package console

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCompileQuery (#1863): the Logs Explorer's query editor takes the
// Logging query language over the LogEntry fields an entry carries, and
// refuses what it cannot evaluate rather than ignoring it.
func TestCompileQuery(t *testing.T) {
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	app := Entry{ID: 7, Timestamp: at, Severity: SeverityError, Source: "logging/app", Project: "p1",
		Resource: "projects/p1/logs/app", Message: "payment failed for order 12"}
	pod := Entry{ID: 8, Timestamp: at.Add(time.Minute), Severity: SeverityInfo, Source: "run/api",
		Resource: "api-00001", Message: "listening on 8080"}
	for _, c := range []struct {
		query    string
		app, pod bool
	}{
		{"", true, true},
		{"severity>=WARNING", true, false},
		{"severity=INFO", false, true},
		{"severity>=400", true, false},
		{`logName="projects/p1/logs/app"`, true, false},
		{"resource.type=run", false, true},
		{`textPayload:"PAYMENT"`, true, false},
		{"payment", true, false},
		{`"8080"`, false, true},
		{"NOT severity=ERROR", false, true},
		{"-severity=ERROR", false, true},
		{"severity=ERROR OR resource.type=run", true, true},
		{"severity>=INFO AND resource.labels.project_id=p1", true, false},
		{"-- a comment\nresource.labels.name=api-00001", false, true},
		{`timestamp>="2026-11-19T10:00:30Z"`, false, true},
		{"operation.id=x", false, false},
	} {
		pred, err := CompileQuery(c.query)
		if err != nil {
			t.Errorf("%q refused: %v", c.query, err)
			continue
		}
		match := func(e Entry) bool { return pred == nil || pred(e) }
		if match(app) != c.app || match(pod) != c.pod {
			t.Errorf("%q: app %v pod %v, want %v %v", c.query, match(app), match(pod), c.app, c.pod)
		}
	}
	for _, bad := range []string{"(severity=ERROR)", "textPayload=~\"a.*\"", "jsonPayload.x=1", "severity>=LOUD",
		"severity=", `"open`, "a OR"} {
		if _, err := CompileQuery(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	b, err := json.Marshal(app)
	if err != nil || !strings.Contains(string(b), `"logEntry":{`) || !strings.Contains(string(b), `"logName":"projects/p1/logs/app"`) {
		t.Errorf("the entry's JSON does not carry its LogEntry form: %s %v", b, err)
	}
}
