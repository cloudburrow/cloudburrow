package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/prefetch"
)

// fakeTools puts docker, kind and kubectl stand-ins first on PATH. Each
// logs its arguments; docker answers `image inspect` and `crictl inspecti`
// from files of present images, and a load or import adds the image the
// archive names (a test archive's content is "archive:<ref>").
func fakeTools(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	host, node := filepath.Join(dir, "host"), filepath.Join(dir, "node")
	for _, f := range []string{log, host, node} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	docker := `#!/bin/sh
echo "docker $*" >> "` + log + `"
case "$*" in
  "version --format"*) echo arm64 ;;
  "image inspect "*) grep -qxF "$3" "` + host + `" || exit 1 ;;
  "load --quiet --input "*) sed 's/^archive://' "$4" >> "` + host + `" ;;
  *" crictl inspecti -q "*) for last; do :; done; grep -qxF "$last" "` + node + `" || exit 1 ;;
  *" images import "*) sed 's/^archive://' >> "` + node + `" ;;
  *) echo "unexpected: docker $*" >&2; exit 97 ;;
esac
`
	kind := `#!/bin/sh
echo "kind $*" >> "` + log + `"
case "$*" in
  "get nodes --name "*) echo node-1 ;;
  *) echo "unexpected: kind $*" >&2; exit 97 ;;
esac
`
	other := `#!/bin/sh
echo "$(basename "$0") $*" >> "` + log + `"
exit 97
`
	for name, body := range map[string]string{"docker": docker, "kind": kind, "kubectl": other, "curl": other, "wget": other} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func calls(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// `up --offline` with nothing cached refuses before it creates anything,
// naming the first artifact it would have downloaded (#604): no cluster, no
// pull, and only `docker version` asked of Docker, for the architecture.
func TestUpOfflineRefusesBeforeCreatingAnything(t *testing.T) {
	log := fakeTools(t)
	stateDir := t.TempDir()
	args := append([]string{"--offline", "--state-dir", stateDir, "--name", "offline-refusal"}, osAssignedPortsBut("")...)
	var out bytes.Buffer
	err := runUp(context.Background(), args, &out, io.Discard)
	if !errors.Is(err, prefetch.ErrMissing) {
		t.Fatalf("runUp() = %v, want ErrMissing", err)
	}
	for _, want := range []string{"up --offline", "kind node image", config.DefaultNodeImage, "cloudburrow prefetch",
		filepath.Join(stateDir, "cache")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if got := strings.TrimSpace(calls(t, log)); got != "docker version --format {{.Server.Arch}}" {
		t.Errorf("before refusing, up ran:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "offline-refusal")); err == nil {
		t.Error("the refusal created the instance directory")
	}
}

// fillCache writes a stand-in archive for every artifact of cfg.
func fillCache(t *testing.T, cfg config.Config) []prefetch.Artifact {
	t.Helper()
	cache := prefetch.Cache{Dir: prefetch.CacheDir(cfg.StateDir)}
	arts := cache.Artifacts(offlinePlan(context.Background(), cfg, prefetch.ExecRunner{}))
	for _, a := range arts {
		p := cache.Path(a)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("archive:"+a.Ref+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return arts
}

// With a complete cache, `up --offline` passes its check, loads the node
// image into Docker, imports the backend image into the node, and never
// pulls: no `docker pull`, no pull inside the node, no curl or wget.
// (Without Cloud Run and storage, so the stand-in cache needs no real
// Knative YAML and no embedded storage server; the YAML path is
// TestUpReadsKnativeFromTheCache.)
func TestUpOfflineNeverPullsOrDownloads(t *testing.T) {
	log := fakeTools(t)
	cfg, err := config.Load(config.Options{Args: []string{"--state-dir", t.TempDir(), "--name", "offline-cached",
		"--services", "pubsub,bigtable"}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	arts := fillCache(t, cfg)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %+v, want the node image and the Cloud SDK image", arts)
	}

	ctx := context.Background()
	oc, err := newOfflineCache(ctx, cfg, true, prefetch.ExecRunner{}, io.Discard)
	if err != nil {
		t.Fatalf("a complete cache was refused: %v", err)
	}
	oc.use(components0(cfg), cfg)
	if err := (offlineHostComponent{oc}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (offlineNodesComponent{oc}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	got := calls(t, log)
	for _, bad := range []string{"docker pull", "images pull", "curl", "wget", "kind create", "kind load"} {
		if strings.Contains(got, bad) {
			t.Errorf("up --offline ran %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{
		"docker load --quiet --input " + prefetch.Cache{Dir: prefetch.CacheDir(cfg.StateDir)}.Path(arts[0]),
		"docker exec --privileged -i node-1 ctr --namespace=k8s.io images import --snapshotter=overlayfs -",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

// The installer `up` builds reads Knative's YAMLs from the cache, checked
// against the pin, and offline downloads nothing.
func TestUpReadsKnativeFromTheCache(t *testing.T) {
	cfg, err := config.Load(config.Options{Args: []string{"--state-dir", t.TempDir(), "--services", "run"}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("kind: ConfigMap\n")
	sum := sha256.Sum256(body)
	m := prefetch.KnativeManifest{Name: "serving-core.yaml", URL: "https://127.0.0.1:1/serving-core.yaml", SHA256: hex.EncodeToString(sum[:])}
	cache := prefetch.Cache{Dir: prefetch.CacheDir(cfg.StateDir)}
	a := prefetch.Artifact{Kind: prefetch.Manifest, Name: m.Name, SHA256: m.SHA256}
	if err := os.MkdirAll(filepath.Dir(cache.Path(a)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.Path(a), body, 0o644); err != nil {
		t.Fatal(err)
	}
	oc := &offlineCache{cache: cache, plan: prefetch.Plan{Knative: []prefetch.KnativeManifest{m}}, offline: true}
	comps := components0(cfg)
	oc.use(comps, cfg)
	got, err := comps.Installer().Fetch(context.Background(), m.URL)
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("installer fetch = %q, %v; want the cached YAML", got, err)
	}
	if _, err := comps.Installer().Fetch(context.Background(), "https://127.0.0.1:1/kourier.yaml"); !errors.Is(err, prefetch.ErrMissing) {
		t.Errorf("offline fetch of an uncached YAML = %v, want ErrMissing", err)
	}
}

func TestOfflineFlag(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		offline bool
		rest    string
	}{
		{[]string{"--name", "a"}, false, "--name a"},
		{[]string{"--offline", "--name", "a"}, true, "--name a"},
		{[]string{"-offline=false"}, false, ""},
		{[]string{"--name", "a", "--offline=true"}, true, "--name a"},
	} {
		offline, rest, err := offlineFlag(tt.args)
		if err != nil || offline != tt.offline || strings.Join(rest, " ") != tt.rest {
			t.Errorf("offlineFlag(%q) = %v %q %v, want %v %q", tt.args, offline, rest, err, tt.offline, tt.rest)
		}
	}
	if _, _, err := offlineFlag([]string{"--offline=maybe"}); err == nil {
		t.Error("offlineFlag accepted --offline=maybe")
	}
}

func components0(cfg config.Config) *components.LifecycleComponent {
	return components.NewLifecycleComponent(cfg.KubeconfigPath(), cfg, io.Discard)
}
