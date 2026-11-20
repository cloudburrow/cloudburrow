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

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestCloudSQLConsoleRound2 (#1860): Stop beside a running instance's title
// and Start beside a stopped one's, which the list and Overview show
// STOPPED, each an instances.patch of activationPolicy; Connections has
// Google's Summary, Networking, Security and Connectivity tests tabs, the
// client certificates on Security; Backups is one list, each backup's
// location and expiry from the project's Backups API; the create page has
// Enable password policies after the password and Data cache, whose on
// choice is not available locally; Edit sets the policy.
func TestCloudSQLConsoleRound2(t *testing.T) {
	var mu sync.Mutex
	policy := "ALWAYS"
	var patches, inserts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			patches = append(patches, string(b))
			_, _ = io.WriteString(w, `{"name":"op","status":"DONE"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instances"):
			inserts = append(inserts, string(b))
			_, _ = io.WriteString(w, `{"name":"op","status":"DONE"}`)
		case strings.HasSuffix(r.URL.Path, "/instances/pg"), strings.HasSuffix(r.URL.Path, "/instances"):
			in := map[string]any{"name": "pg", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"connectionName": "proj:us-central1:pg", "region": "us-central1",
				"ipAddresses": []any{map[string]any{"type": "PRIMARY", "ipAddress": "127.0.0.1"}},
				"settings": map[string]any{"tier": "db-custom-1-3840", "activationPolicy": policy,
					"ipConfiguration": map[string]any{"sslMode": "ENCRYPTED_ONLY",
						"authorizedNetworks": []any{map[string]any{"name": "office", "value": "203.0.113.0/24"}}},
					"passwordValidationPolicy": map[string]any{"enablePasswordPolicy": true, "minLength": 12}}}
			if strings.HasSuffix(r.URL.Path, "/instances") {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{in}})
			} else {
				_ = json.NewEncoder(w).Encode(in)
			}
		case strings.HasSuffix(r.URL.Path, "/instances/pg/backupRuns"):
			_, _ = io.WriteString(w, `{"items":[{"id":"7","status":"SUCCESSFUL","type":"ON_DEMAND","enqueuedTime":"2026-11-18T10:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/instances/pg/sslCerts"):
			_, _ = io.WriteString(w, `{"items":[{"commonName":"app","sha1Fingerprint":"ab"}]}`)
		case strings.HasSuffix(r.URL.Path, "/projects/proj/backups"):
			_, _ = io.WriteString(w, `{"backups":[{"name":"projects/proj/backups/7","instance":"pg","type":"ON_DEMAND","state":"SUCCESSFUL",`+
				`"location":"us-central1","expiryTime":"2026-12-18T10:00:00Z"}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17", "MYSQL_8_0"}}
	ctx := context.Background()
	ids := func(acts []console.Action) string {
		var out []string
		for _, a := range acts {
			out = append(out, a.ID)
		}
		return "," + strings.Join(out, ",") + ","
	}
	d, err := p.Detail(ctx, "proj", []string{"pg"})
	if err != nil {
		t.Fatal(err)
	}
	if a := ids(d.Actions); !strings.Contains(a, ",stop,") || strings.Contains(a, ",start,") {
		t.Errorf("a running instance's title actions %s; want Stop", a)
	}
	secs := map[string]console.Section{}
	for _, s := range d.Sections {
		secs[s.ID] = s
	}
	var tabs []string
	var security console.Section
	for _, s := range secs["connections"].Tabs {
		tabs = append(tabs, s.Label)
		if s.Label == "Security" {
			security = s
		}
	}
	if strings.Join(tabs, ",") != "Summary,Networking,Security,Connectivity tests" {
		t.Errorf("Connections tabs %v", tabs)
	}
	if len(security.Listing.Items) != 1 || !strings.Contains(security.Note, "Allow only SSL connections") ||
		!strings.Contains(ids(security.Actions), ",create-client-cert,") {
		t.Errorf("Security tab %+v", security)
	}
	if net := secs["connections"].Tabs[1]; len(net.Listing.Items) != 1 || net.Listing.Items[0].Fields["Network"] != "203.0.113.0/24" {
		t.Errorf("Networking tab %+v", net)
	}
	if _, ok := secs["project-backups"]; ok {
		t.Error("Project backups is still a menu page of its own")
	}
	b := secs["backups"].Listing
	if len(b.Items) != 1 || b.Items[0].Fields["Location"] != "Region: us-central1" || b.Items[0].Fields["Expires"] != "2026-12-18T10:00:00Z" ||
		!strings.Contains(ids(secs["backups"].Actions), ",create-project-backup,") {
		t.Errorf("merged Backups %+v %s", b, ids(secs["backups"].Actions))
	}
	var edit bool
	for _, f := range d.Edit.Fields {
		if f.Name == "enablePasswordPolicy" {
			edit = f.Default == "true"
		}
	}
	if !edit {
		t.Error("Edit lacks Enable password policies, on")
	}
	if err := p.ActAt(ctx, "proj", []string{"pg"}, "stop", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Edit(ctx, "proj", []string{"pg"}, map[string]string{"tier": "db-custom-1-3840", "enablePasswordPolicy": "true",
		"passwordMinLength": "14", "passwordComplexity": "true", "passwordChangeInterval": "2"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(patches) != 2 || !strings.Contains(patches[0], `"activationPolicy":"NEVER"`) ||
		!strings.Contains(patches[1], `"minLength":14`) || !strings.Contains(patches[1], `"complexity":"COMPLEXITY_DEFAULT"`) ||
		!strings.Contains(patches[1], `"passwordChangeInterval":"7200s"`) {
		t.Errorf("patches %q", patches)
	}
	policy = "NEVER"
	mu.Unlock()
	d, err = p.Detail(ctx, "proj", []string{"pg"})
	if err != nil {
		t.Fatal(err)
	}
	if a := ids(d.Actions); !strings.Contains(a, ",start,") || strings.Contains(a, ",stop,") {
		t.Errorf("a stopped instance's title actions %s; want Start", a)
	}
	if l, _ := p.List(ctx, "proj"); len(l.Items) == 0 || l.Items[len(l.Items)-1].Status != "STOPPED" {
		t.Errorf("list status %+v", l.Items)
	}

	_, fields := p.CreateForm()
	var order []string
	by := map[string]console.Field{}
	for _, f := range fields {
		order = append(order, f.Name)
		by[f.Name] = f
	}
	if got := strings.Join(order, ","); !strings.Contains(got, "rootPassword,enablePasswordPolicy,passwordMinLength,passwordComplexity,passwordReuseInterval,passwordDisallowUsername") {
		t.Errorf("create page order %s", got)
	}
	if f := by["dataCache"]; strings.Join(f.DisabledOptions, ",") != "on" || f.VisibleWhen["edition"] != "ENTERPRISE_PLUS" {
		t.Errorf("Data cache %+v", f)
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "PostgreSQL", "databaseVersion": "POSTGRES_17", "name": "pw",
		"enablePasswordPolicy": "true", "passwordMinLength": "10", "passwordReuseInterval": "3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "PostgreSQL", "databaseVersion": "POSTGRES_17", "name": "dc",
		"dataCache": "on"}); err == nil || !strings.Contains(err.Error(), "not available locally") {
		t.Errorf("data cache on: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inserts) != 1 || !strings.Contains(inserts[0], `"enablePasswordPolicy":true`) || !strings.Contains(inserts[0], `"reuseInterval":3`) {
		t.Errorf("inserts %q", inserts)
	}
}
