package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// An AlloyDB server runs Google's AlloyDB Omni image only when its
// cluster's record names who accepted the terms and when (#2207); every
// other server, and every POSTGRES_14 one, runs PostgreSQL as before.
func TestAlloyDBBackendOmniOnlyWhenAccepted(t *testing.T) {
	e := newAlloyDBEngine(config.Config{}, nil)
	spec := alloydb.ServerSpec{Server: "alloydb-abc", Version: "POSTGRES_16", Flags: map[string]string{"google_columnar_engine.enabled": "on"}}
	for _, a := range []*alloydb.OmniAcceptance{nil, {}, {AcceptedBy: "dev"}, {AcceptedAt: "now"}} {
		spec.Omni = a
		b := e.backend(spec)
		if strings.Contains(b.Image, "alloydbomni") || b.Image != components.CloudSQLPostgres16Image {
			t.Errorf("acceptance %+v: image %s, want PostgreSQL 16", a, b.Image)
		}
		if m := b.Manifest("ns", "inst"); strings.Contains(m, "alloydbomni") || strings.Contains(m, "/dev/shm") {
			t.Errorf("acceptance %+v: the manifest names Omni or its shared memory", a)
		}
	}
	spec.Omni = &alloydb.OmniAcceptance{AcceptedBy: "dev", AcceptedAt: "2026-11-24T00:00:00Z", Via: alloydb.OmniViaFlag}
	spec.Flags["google_columnar_engine.memory_size_in_mb"] = "2048"
	b := e.backend(spec)
	if b.Image != components.AlloyDBOmni16Image || b.MemoryRequest != "2Gi" || b.ShmSize != "2304Mi" {
		t.Errorf("accepted: image %s, memory %s, shm %s", b.Image, b.MemoryRequest, b.ShmSize)
	}
	args := strings.Join(b.Args, " ")
	if strings.Contains(args, "shared_preload_libraries") || !strings.Contains(args, "-c google_columnar_engine.enabled=on") {
		t.Errorf("accepted: args %q keep Omni's preloads and pass its flags", args)
	}
	m := b.Manifest("ns", "inst")
	for _, want := range []string{"google/alloydbomni:16.14.0@sha256:", "mountPath: /dev/shm", "medium: Memory", "sizeLimit: 2304Mi", "PGDATA"} {
		if !strings.Contains(m, want) {
			t.Errorf("accepted manifest lacks %q", want)
		}
	}
	// A read pool replicates through the same image.
	spec.ReplicaOf = "alloydb-primary"
	if b := e.backend(spec); b.Image != components.AlloyDBOmni16Image {
		t.Errorf("an Omni read pool: %s", b.Image)
	}
	spec.ReplicaOf, spec.Version = "", "POSTGRES_14"
	if b := e.backend(spec); b.Image != components.CloudSQLPostgres14Image {
		t.Errorf("POSTGRES_14 with the terms accepted: %s, want PostgreSQL 14", b.Image)
	}
}

// up's acceptance is recorded as the user's, with how it was given; none
// is recorded without one; status reports both and each Omni cluster.
func TestOmniTermsOfAndStatus(t *testing.T) {
	if a := omniTermsOf(config.Config{}, "now"); a != nil {
		t.Errorf("no acceptance: %+v", a)
	}
	for src, via := range map[string]string{"flag": alloydb.OmniViaFlag, "env": alloydb.OmniViaEnv, "file": alloydb.OmniViaFile} {
		cfg := config.Config{}
		cfg.AlloyDB.AcceptOmniTerms = true
		cfg.Source.OmniTerms = src
		if a := omniTermsOf(cfg, "2026-11-24T00:00:00Z"); a == nil || a.Via != via || a.AcceptedBy == "" || a.AcceptedAt != "2026-11-24T00:00:00Z" {
			t.Errorf("%s: %+v", src, a)
		}
	}
	cfg := config.Config{Name: "t", StateDir: t.TempDir()}
	if got := omniTermsLines(readOmniTermsStatus(cfg)); len(got) != 1 || !strings.Contains(got[0], "not accepted") ||
		!strings.Contains(got[0], "--accept-alloydb-omni-terms") {
		t.Errorf("no acceptance: %q", got)
	}
	if omniTermsReport(cfg) != nil {
		t.Error("a report with nothing to report")
	}
	a := &alloydb.OmniAcceptance{AcceptedBy: "dev", AcceptedAt: "2026-11-24T00:00:00Z", Via: alloydb.OmniViaFlag}
	if err := recordOmniTerms(cfg, a); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.InstanceDir(), "alloydb.json"),
		[]byte(`{"omni": {"projects/p/locations/us-central1/clusters/c1": {"acceptedBy": "dev", "acceptedAt": "t1", "via": "v"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := omniTermsLines(readOmniTermsStatus(cfg))
	if len(lines) != 2 || !strings.Contains(lines[0], "accepted by dev at 2026-11-24T00:00:00Z via "+alloydb.OmniViaFlag) ||
		!strings.Contains(lines[1], "clusters/c1: runs AlloyDB Omni, accepted by dev at t1 via v") {
		t.Errorf("status: %q", lines)
	}
	if r := omniTermsReport(cfg); r == nil || r.Up == nil || len(r.Clusters) != 1 {
		t.Errorf("json: %+v", r)
	}
	// `up` without one removes the record.
	if err := recordOmniTerms(cfg, nil); err != nil {
		t.Fatal(err)
	}
	if st := readOmniTermsStatus(cfg); st.Up != nil {
		t.Errorf("after an up with no acceptance: %+v", st.Up)
	}
}

// The console's Create cluster page has the terms' checkbox, unchecked by
// default, linking Google's terms; checked, the cluster runs Omni under
// the user's acceptance; unchecked, PostgreSQL.
func TestConsoleAlloyDBOmniCheckbox(t *testing.T) {
	_, fields := alloyDBProvider{}.CreateForm()
	var box *console.Field
	for i := range fields {
		if fields[i].Name == "omniTerms" {
			box = &fields[i]
		}
	}
	if box == nil || box.Type != "checkbox" || box.Default == "true" || len(box.Links) == 0 || box.Links[0].URL != alloydb.OmniTermsURL ||
		!strings.Contains(box.Label, "AlloyDB Omni licence terms") {
		t.Fatalf("the checkbox: %+v", box)
	}
	api, err := alloydb.NewServer("", "", alloydb.Options{ConnectorHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	p := alloyDBProvider{svc: &alloyDBService{api: api, engine: newAlloyDBEngine(config.Config{}, nil)}}
	values := map[string]string{"clusterId": "ticked", "password": "pw", "version": "POSTGRES_17", "region": "us-central1",
		"network": "default", "instanceId": "primary", "cpu": "2", "omniTerms": "true"}
	// The primary's creation needs an engine; the cluster is made first.
	_, _ = p.Create(context.Background(), "p1", values)
	if a := api.OmniOf("projects/p1/locations/us-central1/clusters/ticked"); !a.Accepted() || a.Via != alloydb.OmniViaConsole {
		t.Errorf("checked: %+v", a)
	}
	values["clusterId"], values["omniTerms"] = "unticked", ""
	_, _ = p.Create(context.Background(), "p1", values)
	if a := api.OmniOf("projects/p1/locations/us-central1/clusters/unticked"); a != nil {
		t.Errorf("unchecked: %+v", a)
	}
	if got := omniEngineLabel(nil); !strings.HasPrefix(got, "PostgreSQL") {
		t.Errorf("the PostgreSQL label: %q", got)
	}
}
