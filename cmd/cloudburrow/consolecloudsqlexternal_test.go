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

// TestCloudSQLConsoleExternalPrimary (#1501): Create instance with an
// external server host:port inserts a source representation instance;
// its page shows the external server and offers Create read replica; a
// replica of it offers Verify external sync settings, which lists what the
// server lacks, and Start external sync.
func TestCloudSQLConsoleExternalPrimary(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.Method+" "+r.URL.Path] = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/src"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "src", "state": "RUNNABLE", "databaseVersion": "MYSQL_8_4",
				"instanceType": "ON_PREMISES_INSTANCE", "replicaNames": []string{"rep"},
				"onPremisesConfiguration": map[string]any{"hostPort": "10.0.0.6:3306", "username": "repl"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/rep"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "rep", "state": "RUNNABLE", "databaseVersion": "MYSQL_8_4",
				"instanceType": "READ_REPLICA_INSTANCE", "masterInstanceName": "proj:src",
				"replicaConfiguration": map[string]any{"mysqlReplicaConfiguration": map[string]any{"username": "repl"}},
				"settings":             map[string]any{"tier": "db-custom-1-3840", "databaseReplicationEnabled": true}})
		case strings.HasSuffix(r.URL.Path, "/verifyExternalSyncSettings"):
			_, _ = io.WriteString(w, `{"kind":"sql#migrationSettingErrorList","errors":[{"type":"UNSUPPORTED_GTID_MODE","detail":"gtid_mode is OFF"}]}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"MYSQL_8_4"}}
	ctx := context.Background()

	if _, err := p.Create(ctx, "proj", map[string]string{"name": "src", "databaseVersion": "MYSQL_8_4",
		"sourceHostPort": "10.0.0.6:3306", "sourceUser": "repl", "sourcePassword": "pw"}); err != nil {
		t.Fatal(err)
	}
	if b := bodies["POST /v1/projects/proj/instances"]; !strings.Contains(b, `"instanceType":"ON_PREMISES_INSTANCE"`) ||
		!strings.Contains(b, `"hostPort":"10.0.0.6:3306"`) || !strings.Contains(b, `"username":"repl"`) || strings.Contains(b, `"settings"`) {
		t.Errorf("insert body %s", b)
	}

	d, err := p.Detail(ctx, "proj", []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, pr := range d.Summary {
		got[pr.Label] = pr.Value
	}
	if got["External server"] != "10.0.0.6:3306" || got["Replication user"] != "repl" || got["Read replicas"] != "rep" {
		t.Errorf("source representation summary %v", got)
	}
	if len(d.Actions) != 1 || d.Actions[0].ID != "create-replica" {
		t.Errorf("source representation actions %+v", d.Actions)
	}

	d, err = p.Detail(ctx, "proj", []string{"rep"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range d.Actions {
		ids[a.ID] = true
	}
	if !ids["verify-external-sync"] || !ids["start-external-sync"] || !ids["promote-replica"] {
		t.Errorf("an external replica's actions %v", ids)
	}
	res, err := p.ActAtResult(ctx, "proj", []string{"rep"}, "verify-external-sync", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Items) != 1 || res.Items[0].Name != "UNSUPPORTED_GTID_MODE" {
		t.Errorf("verify result %+v", res)
	}
	if err := p.ActAt(ctx, "proj", []string{"rep"}, "start-external-sync", map[string]string{"skipVerification": "true"}); err != nil {
		t.Fatal(err)
	}
	if b := bodies["POST /v1/projects/proj/instances/rep/startExternalSync"]; !strings.Contains(b, `"syncMode":"ONLINE"`) ||
		!strings.Contains(b, `"skipVerification":true`) {
		t.Errorf("startExternalSync body %s", b)
	}
}
