package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The storage screen shows what the server keeps (#518): buckets scoped by
// project with no caveat, and a bucket's retention policy and lifecycle
// rule count in its configuration.
func TestConsoleStorageShowsRetentionAndLifecycle(t *testing.T) {
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
	p := storageProvider{endpoint: strings.TrimPrefix(srv.URL, "http://")}
	ctx := context.Background()

	l, err := p.List(ctx, "p")
	if err != nil || l.Note != "" || len(l.Items) != 1 {
		t.Errorf("listing = %+v (%v), want one bucket and no note", l, err)
	}
	sec, err := p.bucketConfig(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, g := range sec.Groups {
		for _, prop := range g.Properties {
			got[prop.Label] = prop.Value
		}
	}
	if got["Retention policy"] != "3600 s, locked" || got["Lifecycle rules"] != "1" {
		t.Errorf("bucket properties = %v", got)
	}
	var _ console.Section = sec
}
