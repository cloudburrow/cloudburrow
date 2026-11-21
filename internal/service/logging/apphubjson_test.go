package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
