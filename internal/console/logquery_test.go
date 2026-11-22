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
		{"(severity=ERROR)", true, false},
		{`textPayload=~"^pay.*\\d+$"`, true, false},
		{"jsonPayload.x=1", false, false},
		{"severity>=LOUD", false, false},
		{`severity=ERROR OR resource.type=run AND textPayload:listening`, false, true},
		{`SEARCH("order")`, true, false},
		{`log_id("app")`, true, false},
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
	for _, bad := range []string{"(severity=ERROR", "textPayload=~\"(\"", "nosuch.x=1", "severity=", `"open`, "a OR"} {
		if _, err := CompileQuery(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	b, err := json.Marshal(app)
	if err != nil || !strings.Contains(string(b), `"logEntry":{`) || !strings.Contains(string(b), `"logName":"projects/p1/logs/app"`) {
		t.Errorf("the entry's JSON does not carry its LogEntry form: %s %v", b, err)
	}
}

// TestQueryMatchesEntryLabels (#2032): an entry's labels are one map in
// its LogEntry, a key with dots or a slash staying one key, which the
// query editor matches as labels."KEY" and the Fields pane's label
// dimensions count.
func TestQueryMatchesEntryLabels(t *testing.T) {
	e := Entry{ID: 1, Timestamp: time.Now(), Severity: SeverityInfo, Source: "logging/app", Message: "hi",
		Labels: map[string]string{"k8s-pod/app": "frontend", "service.name": "checkout"}}
	j := logEntryJSON(e)
	labels, ok := j["labels"].(map[string]any)
	if !ok || labels["k8s-pod/app"] != "frontend" || labels["service.name"] != "checkout" {
		t.Fatalf("the LogEntry's labels = %#v", j["labels"])
	}
	if _, nested := j["service"]; nested {
		t.Error("a label key's dots nested it as a path")
	}
	for q, want := range map[string]bool{
		`labels."k8s-pod/app"="frontend"`: true,
		`labels."service.name"=checkout`:  true,
		`labels."k8s-pod/app"="backend"`:  false,
		`labels.missing:*`:                false,
	} {
		m, err := CompileQuery(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := m(e); got != want {
			t.Errorf("%s matched %v, want %v", q, got, want)
		}
	}
	b, _ := json.Marshal(e)
	if !strings.Contains(string(b), `"labels":{"k8s-pod/app":"frontend"`) {
		t.Errorf("the entry's JSON = %s", b)
	}
}

// A Cloud Logging entry's monitored resource is queried as Google's Logs
// Explorer queries it (#1849): resource.type is its type and each label is
// resource.labels.<key>; a label the entry lacks matches nothing.
func TestCompileQueryMonitoredResource(t *testing.T) {
	job := Entry{Source: LoggingSourcePrefix + "cloudscheduler.googleapis.com%2Fexecutions", Project: "p1",
		Resource: "projects/p1/logs/cloudscheduler.googleapis.com%2Fexecutions", ResourceType: "cloud_scheduler_job",
		ResourceLabels: map[string]string{"project_id": "p1", "location": "us-central1", "job_id": "nightly"}}
	pod := Entry{Source: "run/web", Project: "p1", Message: "hello"}
	for q, want := range map[string][2]bool{
		`resource.type="cloud_scheduler_job"` + "\n" + `resource.labels.job_id="nightly"`: {true, false},
		`resource.labels.location="us-central1"`:                                          {true, false},
		`resource.labels.job_id="other"`:                                                  {false, false},
		`resource.type="run"`:                                                             {false, true},
	} {
		match, err := CompileQuery(q)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if got := [2]bool{match(job), match(pod)}; got != want {
			t.Errorf("%q matches the job's entry and the pod's line %v, want %v", q, got, want)
		}
	}
}

// An entry's App Hub metadata (#2096) is matched and shown under apphub.
func TestCompileQueryAppHub(t *testing.T) {
	SetAppHubFields(func(typ string, labels map[string]string) map[string]string {
		if typ == "pubsub_topic" && labels["topic_id"] == "orders" {
			return map[string]string{"apphub.application.id": "shop", "apphub.service.id": "orders-topic"}
		}
		return nil
	})
	t.Cleanup(func() { SetAppHubFields(nil) })
	e := Entry{Project: "p1", ResourceType: "pubsub_topic", ResourceLabels: map[string]string{"project_id": "p1", "topic_id": "orders"}}
	other := Entry{Project: "p1", ResourceType: "pubsub_topic", ResourceLabels: map[string]string{"project_id": "p1", "topic_id": "audit"}}
	match, err := CompileQuery(`apphub.application.id="shop"`)
	if err != nil {
		t.Fatal(err)
	}
	if !match(e) || match(other) {
		t.Errorf("apphub.application.id matches %v and %v; want true and false", match(e), match(other))
	}
	app, _ := logEntryJSON(e)["apphub"].(map[string]any)
	if app == nil || app["application"].(map[string]any)["id"] != "shop" {
		t.Errorf("logEntry apphub = %v", logEntryJSON(e)["apphub"])
	}
}
