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

// TestCloudSQLConsoleReencrypt (#1614): a CMEK instance's page offers
// Re-encrypt, which calls instances.reencrypt; an instance without a key
// does not.
func TestCloudSQLConsoleReencrypt(t *testing.T) {
	const key = "projects/proj/locations/us-central1/keyRings/r/cryptoKeys/k"
	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/enc"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "enc", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"settings":                    map[string]any{"tier": "db-custom-1-3840"},
				"diskEncryptionConfiguration": map[string]any{"kmsKeyName": key},
				"diskEncryptionStatus":        map[string]any{"kmsKeyVersionName": key + "/cryptoKeyVersions/1"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/plain"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "plain", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"settings": map[string]any{"tier": "db-custom-1-3840"}})
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17"}}
	ctx := context.Background()
	has := func(name string) bool {
		d, err := p.Detail(ctx, "proj", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range d.Actions {
			if a.ID == "reencrypt" && a.Confirm != "" {
				return true
			}
		}
		return false
	}
	if !has("enc") {
		t.Error("a CMEK instance has no Re-encrypt action")
	}
	if has("plain") {
		t.Error("an instance without a key offers Re-encrypt")
	}
	if err := p.ActAt(ctx, "proj", []string{"enc"}, "reencrypt", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(calls, "\n"), "POST /v1/projects/proj/instances/enc/reencrypt") {
		t.Errorf("no reencrypt call: %v", calls)
	}
}
