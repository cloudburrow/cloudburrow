package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The storage screen says what its backend does (#518): fake-gcs-server
// ignores the project and reports no retention or lifecycle, so the screen
// notes both; the builtin server scopes by project and reports both, so the
// screen shows them and notes neither.
func TestConsoleStorageDescribesItsBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/storage/v1/b":
			_, _ = w.Write([]byte(`{"items":[{"name":"b","location":"US","storageClass":"STANDARD"}]}`))
		case "/storage/v1/b/b":
			_, _ = w.Write([]byte(`{"name":"b","location":"US","locationType":"multi-region","storageClass":"STANDARD",
				"retentionPolicy":{"retentionPeriod":"3600","isLocked":true},
				"lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":30}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()

	props := func(sec console.Section) map[string]string {
		out := map[string]string{}
		for _, g := range sec.Groups {
			for _, p := range g.Properties {
				out[p.Label] = p.Value
			}
		}
		return out
	}

	fake := storageProvider{endpoint: endpoint}
	l, err := fake.List(ctx, "p")
	if err != nil || !strings.Contains(l.Note, "does not scope buckets by project") {
		t.Errorf("fake-gcs listing note = %q (%v)", l.Note, err)
	}
	sec, err := fake.bucketConfig(ctx, "b")
	if err != nil || !strings.Contains(sec.Note, "fake-gcs-server does not implement retention") {
		t.Errorf("fake-gcs bucket note = %q (%v)", sec.Note, err)
	}
	if _, ok := props(sec)["Retention policy"]; ok {
		t.Error("the fake-gcs screen shows a retention policy the backend does not report")
	}

	builtin := storageProvider{endpoint: endpoint, builtin: true}
	if l, err = builtin.List(ctx, "p"); err != nil || l.Note != "" || len(l.Items) != 1 {
		t.Errorf("builtin listing = %+v (%v), want one bucket and no note", l, err)
	}
	if sec, err = builtin.bucketConfig(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sec.Note, "fake-gcs") {
		t.Errorf("builtin bucket note = %q", sec.Note)
	}
	got := props(sec)
	if got["Retention policy"] != "3600 s, locked" || got["Lifecycle rules"] != "1" {
		t.Errorf("builtin bucket properties = %v", got)
	}
}
