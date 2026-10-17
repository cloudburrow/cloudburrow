package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stopped instance still gives a bundle: configuration and doctor output,
// with every cluster step recorded as skipped and why.
func TestDiagnoseStoppedInstance(t *testing.T) {
	cfg := formatsConfig(t, "storage,pubsub")
	b := collectDiagnostics(context.Background(), cfg, newKubectl(cfg.KubeconfigPath()), tokenFileAdmin(cfg))
	if b.m.ClusterUp || b.m.Running {
		t.Fatalf("a stopped instance reported cluster_up=%v running=%v", b.m.ClusterUp, b.m.Running)
	}
	for _, f := range []string{"version.txt", "config.json", "doctor.txt"} {
		if len(b.files[f]) == 0 {
			t.Errorf("no %s", f)
		}
	}
	skipped := map[string]string{}
	for _, s := range b.m.Steps {
		if s.Status == "skipped" {
			skipped[s.Step] = s.Reason
		}
	}
	for _, step := range []string{"pods", "events", "logs", "readiness", "status"} {
		if skipped[step] == "" {
			t.Errorf("step %q was not recorded as skipped with a reason: %v", step, b.m.Steps)
		}
	}
	if !strings.Contains(skipped["pods"], "not running") {
		t.Errorf("pods skip reason does not say the cluster is down: %q", skipped["pods"])
	}

	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := b.write(out); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode: %v %v", fi.Mode(), err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" {
		t.Fatalf("first entry is %v (%v), want manifest.json", hdr, err)
	}
	raw, _ := io.ReadAll(tr)
	var m diagnoseManifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Format != "cloudburrow-diagnose" || len(m.Excluded) == 0 {
		t.Fatalf("manifest: %v %+v", err, m)
	}
}

func TestWithoutEnvValues(t *testing.T) {
	in := `{"items":[{"spec":{"containers":[{"name":"app","env":[
		{"name":"API_KEY","value":"sk-live-123"},
		{"name":"FROM_SECRET","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]}]}}]}`
	out := string(withoutEnvValues([]byte(in)))
	if strings.Contains(out, "sk-live-123") {
		t.Fatalf("env value survived: %s", out)
	}
	for _, want := range []string{"API_KEY", "FROM_SECRET", "secretKeyRef", `"name": "app"`} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %s: %s", want, out)
		}
	}
	if got := string(withoutEnvValues([]byte("not json sk-live-123"))); strings.Contains(got, "sk-live") {
		t.Errorf("unparseable input passed through: %s", got)
	}
}

// TestDiagnoseGetArgsPutTheVerbFirst (#1583): `kubectl -A -l ... get ksvc`
// is refused as a plugin name, so the knative services step always failed.
func TestDiagnoseGetArgsPutTheVerbFirst(t *testing.T) {
	for _, ns := range []string{"", "cloudburrow"} {
		args := diagnoseGetArgs(ns, "ksvc", "demo")
		if args[0] != "get" || args[1] != "ksvc" {
			t.Errorf("diagnoseGetArgs(%q) = %q, want the get verb and kind first", ns, args)
		}
	}
	if got := strings.Join(diagnoseGetArgs("", "ksvc", "demo"), " "); got != "get ksvc -o json -A -l cloudburrow.dev/instance=demo" {
		t.Errorf("all namespaces: %s", got)
	}
}
