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

// TestCloudSQLConsoleHighAvailability (#1408): Create and Edit send the
// Zonal availability choice as settings.availabilityType, a REGIONAL
// instance's page shows its zones and offers Failover, which calls
// instances.failover with the instance's settingsVersion.
func TestCloudSQLConsoleHighAvailability(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.Method+" "+r.URL.Path] = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/ha") {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "ha", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"gceZone": "us-central1-a", "secondaryGceZone": "us-central1-b",
				"settings": map[string]any{"tier": "db-custom-1-3840", "availabilityType": "REGIONAL", "settingsVersion": "4"}})
			return
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17"}}
	ctx := context.Background()

	_, fields := p.CreateForm()
	found := false
	for _, f := range fields {
		if f.Name == "availabilityType" && f.Default == "ZONAL" && len(f.Options) == 2 {
			found = true
		}
	}
	if !found {
		t.Error("Create instance has no Zonal availability choice")
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"name": "ha", "databaseVersion": "POSTGRES_17", "tier": "t",
		"availabilityType": "REGIONAL"}); err != nil {
		t.Fatal(err)
	}
	if b := bodies["POST /v1/projects/proj/instances"]; !strings.Contains(b, `"availabilityType":"REGIONAL"`) {
		t.Errorf("insert body %s", b)
	}

	d, err := p.Detail(ctx, "proj", []string{"ha"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, pr := range d.Summary {
		got[pr.Label] = pr.Value
	}
	if got["Zone"] != "us-central1-a" || got["Secondary zone"] != "us-central1-b" || got["Zonal availability"] != "REGIONAL" {
		t.Errorf("summary %v", got)
	}
	hasFailover := false
	for _, a := range d.Actions {
		if a.ID == "failover" && a.Confirm != "" {
			hasFailover = true
		}
	}
	if !hasFailover {
		t.Error("a REGIONAL instance has no Failover action with a confirmation")
	}
	if err := p.ActAt(ctx, "proj", []string{"ha"}, "failover", nil); err != nil {
		t.Fatal(err)
	}
	if b := bodies["POST /v1/projects/proj/instances/ha/failover"]; !strings.Contains(b, `"settingsVersion":"4"`) {
		t.Errorf("failover body %s", b)
	}

	if err := p.Edit(ctx, "proj", []string{"ha"}, map[string]string{"tier": "t", "availabilityType": "ZONAL"}); err != nil {
		t.Fatal(err)
	}
	if b := bodies["PATCH /v1/projects/proj/instances/ha"]; !strings.Contains(b, `"availabilityType":"ZONAL"`) {
		t.Errorf("patch body %s", b)
	}
}
