package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCloudSQLConsoleProjectBackups: an instance's page lists the
// project's backups of it in Project backups (ListBackups filtered by the
// instance), offers Create project backup (Backups.CreateBackup with the
// instance, description and days), and Edit project backup sends only what
// it was given, so changing the days keeps the description.
func TestCloudSQLConsoleProjectBackups(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls[r.Method+" "+r.URL.Path] += r.URL.RawQuery + " " + string(b) + "\n"
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/pg"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "pg", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"settings": map[string]any{"tier": "db-custom-1-3840"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/proj/backups":
			_ = json.NewEncoder(w).Encode(map[string]any{"backups": []any{map[string]any{
				"name": "projects/proj/backups/b1", "instance": "pg", "type": "ON_DEMAND", "state": "SUCCESSFUL",
				"description": "nightly", "backupInterval": map[string]any{"startTime": "2026-10-16T12:00:00Z"}}}})
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17"}}
	ctx := context.Background()

	d, err := p.Detail(ctx, "proj", []string{"pg"})
	if err != nil {
		t.Fatal(err)
	}
	var section bool
	for _, s := range d.Sections {
		if s.ID == "project-backups" {
			section = len(s.Listing.Items) == 1 && s.Listing.Items[0].Name == "b1" && s.Listing.Items[0].Fields["Description"] == "nightly" &&
				s.Listing.Items[0].Fields["Created"] == "2026-10-16T12:00:00Z"
		}
	}
	if !section {
		t.Errorf("no Project backups tab listing b1: %+v", d.Sections)
	}
	if q := calls["GET /v1/projects/proj/backups"]; !strings.Contains(q, "filter=instance%3D%22pg%22") {
		t.Errorf("ListBackups query %q; want the instance's filter", q)
	}
	var create bool
	for _, a := range d.Actions {
		create = create || a.ID == "create-project-backup"
	}
	if !create {
		t.Fatal("a RUNNABLE instance's page has no Create project backup")
	}

	if err := p.ActAt(ctx, "proj", []string{"pg"}, "create-project-backup", map[string]string{"description": "before upgrade", "ttlDays": "3"}); err != nil {
		t.Fatal(err)
	}
	if b := calls["POST /v1/projects/proj/backups"]; !strings.Contains(b, `"instance":"pg"`) || !strings.Contains(b, `"description":"before upgrade"`) ||
		!strings.Contains(b, `"ttlDays":"3"`) {
		t.Errorf("CreateBackup body %s", b)
	}
	if err := p.ActAt(ctx, "proj", []string{"pg"}, "create-project-backup", map[string]string{"ttlDays": "soon"}); err == nil {
		t.Error("Create project backup took days \"soon\"")
	}

	if err := p.ActAt(ctx, "proj", []string{"pg"}, "edit-project-backup", map[string]string{"backup": "projects/proj/backups/b1", "ttlDays": "9"}); err != nil {
		t.Fatal(err)
	}
	if q := calls["PATCH /v1/projects/proj/backups/b1"]; !strings.Contains(q, "updateMask=ttl_days ") {
		t.Errorf("UpdateBackup %q; want only ttl_days in the mask, keeping the description", q)
	}
	if err := p.ActAt(ctx, "proj", []string{"pg"}, "edit-project-backup", map[string]string{"backup": "projects/proj/backups/b1"}); err == nil ||
		!strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("an Edit project backup that changes nothing: %v", err)
	}
}
