package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
)

func TestWithAppHub(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"entries":[{"logName":"projects/p/logs/a","resource":{"type":"pubsub_topic","labels":{"project_id":"p","topic_id":"t"}},"jsonPayload":{"n":12345678901234567890}},`+
			`{"logName":"projects/p/logs/b","resource":{"type":"global","labels":{"project_id":"p"}}}],"nextPageToken":"x"}`)
	})
	h := WithAppHub(next, func(typ string, labels map[string]string) map[string]any {
		if typ == "pubsub_topic" && labels["topic_id"] == "t" {
			return map[string]any{"application": map[string]any{"container": "projects/p", "location": "global", "id": "app"},
				"service": map[string]any{"id": "topic"}}
		}
		return nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/entries:list", strings.NewReader(`{}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "12345678901234567890") {
		t.Errorf("a payload's integer was not kept as written: %s", body)
	}
	var got struct {
		Entries []struct {
			AppHub map[string]any `json:"apphub"`
		} `json:"entries"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].AppHub == nil || got.Entries[1].AppHub != nil || got.NextPageToken != "x" {
		t.Fatalf("entries.list = %s", body)
	}
	// Any other call is passed through.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/projects/p/logs", nil))
	if strings.Contains(rec.Body.String(), "apphub") {
		t.Errorf("another call was changed: %s", rec.Body.String())
	}
}

// Filters match an entry's apphub (#2096): entries.list with
// apphub.application.id="app" returns the registered resource's entries.
func TestFilterAppHub(t *testing.T) {
	SetAppHub(func(typ string, labels map[string]string) map[string]any {
		if typ == "pubsub_topic" && labels["topic_id"] == "t" {
			return map[string]any{"application": map[string]any{"container": "projects/p", "location": "global", "id": "app"},
				"service": map[string]any{"id": "topic"}}
		}
		return nil
	})
	t.Cleanup(func() { SetAppHub(nil) })
	in := &loggingpb.LogEntry{LogName: "projects/p/logs/a", Resource: &monitoredres.MonitoredResource{Type: "pubsub_topic", Labels: map[string]string{"project_id": "p", "topic_id": "t"}}}
	out := &loggingpb.LogEntry{LogName: "projects/p/logs/a", Resource: &monitoredres.MonitoredResource{Type: "pubsub_topic", Labels: map[string]string{"project_id": "p", "topic_id": "u"}}}
	for filter, want := range map[string][2]bool{
		`apphub.application.id="app"`:              {true, false},
		`apphub.service.id="topic"`:                {true, false},
		`apphub.application.location="global"`:     {true, false},
		`apphub.workload.id:*`:                     {false, false},
		`NOT apphub.application.id="app"`:          {false, true},
		`"projects/p" AND apphub.application.id:*`: {true, false},
	} {
		match, err := compile(filter)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		if got := [2]bool{match(in), match(out)}; got != want {
			t.Errorf("%s matches %v; want %v", filter, got, want)
		}
	}
}
