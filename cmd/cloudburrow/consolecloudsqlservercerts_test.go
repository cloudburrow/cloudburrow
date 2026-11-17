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

// TestCloudSQLConsoleServerCertificates (#1614): Create instance and Edit
// instance offer Server CA mode and send it in ipConfiguration; a
// GOOGLE_MANAGED_CAS_CA instance's page lists its server certificates and
// offers Add server certificate, Rotate server certificate and Resolve DNS
// name (answered with connect.resolve's settings) instead of the server CA
// actions, which an internal-CA instance keeps.
func TestCloudSQLConsoleServerCertificates(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/cas"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "cas", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"region": "us-central1", "dnsName": "cas.proj.us-central1.sql.cloudburrow.internal.",
				"settings": map[string]any{"tier": "db-custom-1-3840", "ipConfiguration": map[string]any{
					"serverCaMode": "GOOGLE_MANAGED_CAS_CA", "ipv4Enabled": true}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/plain"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "plain", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"settings": map[string]any{"tier": "db-custom-1-3840", "ipConfiguration": map[string]any{"ipv4Enabled": true}}})
		case strings.HasSuffix(r.URL.Path, "/listServerCertificates"):
			_, _ = io.WriteString(w, `{"activeVersion":"aaa","serverCerts":[`+
				`{"sha1Fingerprint":"aaa","createTime":"2026-11-17T10:00:00.1Z"},{"sha1Fingerprint":"bbb","createTime":"2026-11-17T10:00:00.2Z"}]}`)
		case strings.HasSuffix(r.URL.Path, ":resolveConnectSettings"):
			_, _ = io.WriteString(w, `{"connectionName":"proj:us-central1:cas","databaseVersion":"POSTGRES_17","serverCaMode":"GOOGLE_MANAGED_CAS_CA"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17"}}
	ctx := context.Background()

	_, fields := p.CreateForm()
	found := false
	for _, f := range fields {
		found = found || f.Name == "serverCaMode"
	}
	if !found {
		t.Error("Create instance has no Server CA mode")
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"name": "new", "databaseVersion": "POSTGRES_17", "tier": "t",
		"serverCaMode": "GOOGLE_MANAGED_CAS_CA"}); err != nil {
		t.Fatal(err)
	}

	actions := func(name string) (map[string]console.Action, console.Detail) {
		d, err := p.Detail(ctx, "proj", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]console.Action{}
		for _, a := range d.Actions {
			out[a.ID] = a
		}
		return out, d
	}
	cas, d := actions("cas")
	for _, id := range []string{"add-server-cert", "rotate-server-cert", "resolve-dns"} {
		if _, ok := cas[id]; !ok {
			t.Errorf("a CAS instance has no %s", id)
		}
	}
	if _, ok := cas["add-server-ca"]; ok {
		t.Error("a CAS instance offers Add server CA")
	}
	if got := cas["rotate-server-cert"].Fields[0]; got.Default != "bbb" {
		t.Errorf("Rotate server certificate defaults to %q, want the upcoming bbb", got.Default)
	}
	summary := ""
	for _, pr := range d.Summary {
		summary += pr.Label + "=" + pr.Value + "\n"
	}
	if !strings.Contains(summary, "bbb (upcoming)") || !strings.Contains(summary, "aaa (active)") ||
		!strings.Contains(summary, "Server CA mode=GOOGLE_MANAGED_CAS_CA") {
		t.Errorf("summary:\n%s", summary)
	}
	plain, _ := actions("plain")
	if _, ok := plain["add-server-ca"]; !ok {
		t.Error("an internal-CA instance lost Add server CA")
	}
	if _, ok := plain["add-server-cert"]; ok {
		t.Error("an internal-CA instance offers Add server certificate")
	}

	for _, act := range []struct {
		id string
		v  map[string]string
	}{{"add-server-cert", nil}, {"rotate-server-cert", map[string]string{"nextVersion": "bbb"}}} {
		if err := p.ActAt(ctx, "proj", []string{"cas"}, act.id, act.v); err != nil {
			t.Fatalf("%s: %v", act.id, err)
		}
	}
	res, err := p.ActAtResult(ctx, "proj", []string{"cas"}, "resolve-dns",
		map[string]string{"dnsName": "cas.proj.us-central1.sql.cloudburrow.internal.", "location": "us-central1"})
	if err != nil || res == nil || len(res.Items) == 0 || res.Items[0].Fields["Value"] != "proj:us-central1:cas" {
		t.Fatalf("resolve: %v %+v", err, res)
	}
	if err := p.Edit(ctx, "proj", []string{"plain"}, map[string]string{"tier": "t", "availabilityType": "ZONAL",
		"serverCaMode": "GOOGLE_MANAGED_CAS_CA"}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(calls, "\n")
	for _, want := range []string{
		`POST /v1/projects/proj/instances {`, `"serverCaMode":"GOOGLE_MANAGED_CAS_CA"`,
		"POST /v1/projects/proj/instances/cas/addServerCertificate",
		`POST /v1/projects/proj/instances/cas/rotateServerCertificate {"rotateServerCertificateContext":{"nextVersion":"bbb"}}`,
		"GET /v1/locations/us-central1/dns/cas.proj.us-central1.sql.cloudburrow.internal.:resolveConnectSettings",
		`"ipConfiguration":{"ipv4Enabled":true,"serverCaMode":"GOOGLE_MANAGED_CAS_CA"}`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no call with %s:\n%s", want, all)
		}
	}
}
