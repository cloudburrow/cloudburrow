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

// TestCloudSQLConsoleBackupLocation (#1956): the instance's page shows the
// backups' location and, for a CMEK instance's backups, the key version
// that encrypted each; Edit offers Backup location, as Google's Edit
// backup settings does under Advanced options, and sends it as
// settings.backupConfiguration.location.
func TestCloudSQLConsoleBackupLocation(t *testing.T) {
	const key = "projects/proj/locations/us-central1/keyRings/r/cryptoKeys/k"
	var mu sync.Mutex
	var patch map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/loc"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "loc", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"settings": map[string]any{"tier": "db-custom-1-3840",
					"backupConfiguration": map[string]any{"enabled": true, "location": "europe-west1"}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/loc/backupRuns"):
			_, _ = io.WriteString(w, `{"items":[{"id":"7","status":"SUCCESSFUL","type":"ON_DEMAND","location":"us",`+
				`"diskEncryptionStatus":{"kmsKeyVersionName":"`+key+`/cryptoKeyVersions/2"}}]}`)
		case r.Method == http.MethodPatch:
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&patch)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17"}}
	ctx := context.Background()
	d, _, _, err := p.detailAll(ctx, "proj", []string{"loc"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := p.Detail(ctx, "proj", []string{"loc"})
	if err != nil {
		t.Fatal(err)
	}
	var summary string
	for _, pr := range d.Summary {
		if pr.Label == "Backup location" {
			summary = pr.Value
		}
	}
	if summary != "Region: europe-west1" {
		t.Errorf("Backup location %q, want Region: europe-west1", summary)
	}
	var row map[string]string
	for _, s := range g.Sections {
		if s.ID == "backups" && len(s.Listing.Items) == 1 {
			row = s.Listing.Items[0].Fields
		}
	}
	if row["Location"] != "Multi-region: us" || !strings.HasSuffix(row["Encryption key version"], "/cryptoKeyVersions/2") {
		t.Errorf("the backup's row: %v", row)
	}
	if d.Edit == nil {
		t.Fatal("no Edit form")
	}
	found := false
	for _, f := range d.Edit.Fields {
		if f.Name == "backupLocation" {
			found = f.Default == "europe-west1" && strings.Contains(strings.Join(f.Options, ","), "asia-northeast1") &&
				f.OptionLabels[""] != ""
		}
	}
	if !found {
		t.Error("Edit has no Backup location select defaulting to europe-west1")
	}
	if err := p.Edit(ctx, "proj", []string{"loc"}, map[string]string{"tier": "db-custom-1-3840", "availabilityType": "ZONAL",
		"backupsEnabled": "true", "backupLocation": "asia-northeast1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	st, _ := patch["settings"].(map[string]any)
	bc, _ := st["backupConfiguration"].(map[string]any)
	if bc["location"] != "asia-northeast1" {
		t.Errorf("the patch's backupConfiguration: %v", bc)
	}
}
