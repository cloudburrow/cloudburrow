package prefetch

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/images"
)

func TestCanonicalName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"mysql:8.4@sha256:0744":                    "docker.io/library/mysql@sha256:0744",
		"valkey/valkey:8.1-alpine@sha256:081c":     "docker.io/valkey/valkey@sha256:081c",
		"kindest/node:v1.36.4@sha256:099e":         "docker.io/kindest/node@sha256:099e",
		"gcr.io/cloud-spanner-emulator/e@sha256:c": "gcr.io/cloud-spanner-emulator/e@sha256:c",
		"docker.io/envoyproxy/envoy:v1.37-latest":  "docker.io/envoyproxy/envoy:v1.37-latest",
		"localhost:5000/x:1":                       "localhost:5000/x:1",
		"dev.local/cloudburrow-storage:abc":        "dev.local/cloudburrow-storage:abc",
		"registry:5000/team/app:1@sha256:d":        "registry:5000/team/app@sha256:d",
	} {
		if got := CanonicalName(in); got != want {
			t.Errorf("CanonicalName(%q) = %q, want %q", in, got, want)
		}
	}
}

// archive builds a `docker save`-shaped archive. Its index names no image,
// as Docker's does for a reference pinned by digest.
func archive(t *testing.T, withLayer bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("blobs/sha256/cfg", "{}")
	if withLayer {
		add("blobs/sha256/layer", "layer bytes")
	}
	add("index.json", `{"schemaVersion":2,"manifests":[{"digest":"sha256:idx","annotations":{"containerd.io/distribution.source.docker.io":"x/y"}}]}`)
	add("manifest.json", `[{"Config":"blobs/sha256/cfg","RepoTags":null,"Layers":["blobs/sha256/layer"]}]`)
	add("oci-layout", `{"imageLayoutVersion":"1.0.0"}`)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func entries(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
}

// The cache names the image in the archive's index, which is what makes a
// load find it by the pinned reference again (#604), and changes nothing
// else.
func TestNameAnnotatesTheIndexOnly(t *testing.T) {
	t.Parallel()
	in := archive(t, true)
	var out bytes.Buffer
	if err := Name(bytes.NewReader(in), &out, "docker.io/x/y@sha256:idx"); err != nil {
		t.Fatal(err)
	}
	got, want := entries(t, out.Bytes()), entries(t, in)
	for name, body := range want {
		if name == "index.json" {
			continue
		}
		if got[name] != body {
			t.Errorf("%s changed: %q, want %q", name, got[name], body)
		}
	}
	var index struct {
		Manifests []struct {
			Annotations map[string]string
		}
	}
	if err := json.Unmarshal([]byte(got["index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	ann := index.Manifests[0].Annotations
	if ann[annotationName] != "docker.io/x/y@sha256:idx" {
		t.Errorf("annotations = %v, want the image named", ann)
	}
	if ann["containerd.io/distribution.source.docker.io"] != "x/y" {
		t.Errorf("an existing annotation was lost: %v", ann)
	}
}

// Docker 29 saved an image as its index and manifest alone and exited 0
// (measured, the Spanner emulator's). Such an archive must not be cached.
func TestNameRefusesAnIncompleteArchive(t *testing.T) {
	t.Parallel()
	err := Name(bytes.NewReader(archive(t, false)), io.Discard, "docker.io/x/y@sha256:idx")
	if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "blobs/sha256/layer") {
		t.Errorf("Name() = %v, want the missing layer named", err)
	}
}

const coreYAML = `apiVersion: v1
kind: ConfigMap
data:
  queue-sidecar-image: gcr.io/knative-releases/knative.dev/serving/cmd/queue@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
---
spec:
  template:
    spec:
      containers:
        - name: controller
          image: gcr.io/knative-releases/knative.dev/serving/cmd/controller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
        - image: "docker.io/envoyproxy/envoy:v1.37-latest"
          name: gateway
        - name: again
          image: gcr.io/knative-releases/knative.dev/serving/cmd/controller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
---
# a CRD schema names the field with no value
properties:
  image:
    description: Container image name.
`

func TestImagesIn(t *testing.T) {
	t.Parallel()
	got := ImagesIn([]byte(coreYAML))
	want := []string{
		"gcr.io/knative-releases/knative.dev/serving/cmd/queue@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gcr.io/knative-releases/knative.dev/serving/cmd/controller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"docker.io/envoyproxy/envoy:v1.37-latest",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ImagesIn = %q, want %q", got, want)
	}
}

func testPlan() Plan {
	return Plan{
		NodeImage:    "kindest/node:v1@sha256:node",
		Storage:      true,
		StorageImage: "dev.local/cloudburrow-storage:abc",
		Arch:         "arm64",
		Images: map[string][]string{
			"gcr.io/sdk@sha256:sdk":      {"pubsub", "bigtable"},
			"ghcr.io/bq@sha256:bigquery": {"bigquery"},
		},
		Knative: []KnativeManifest{{Name: "serving-core.yaml", URL: "https://example.test/serving-core.yaml", SHA256: sha256Hex([]byte(coreYAML))}},
	}
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The artifacts come in the order `up` needs them, so the first missing is
// the first `up --offline` would have fetched; Knative's images join the
// list once the YAML naming them is cached.
func TestArtifactsAndFirstMissing(t *testing.T) {
	t.Parallel()
	c := Cache{Dir: t.TempDir()}
	p := testPlan()
	arts := c.Artifacts(p)
	var refs []string
	for _, a := range arts {
		refs = append(refs, a.Ref)
	}
	want := []string{"kindest/node:v1@sha256:node", "dev.local/cloudburrow-storage:abc", "gcr.io/sdk@sha256:sdk",
		"ghcr.io/bq@sha256:bigquery", "https://example.test/serving-core.yaml"}
	if strings.Join(refs, " ") != strings.Join(want, " ") {
		t.Fatalf("artifacts = %q, want %q", refs, want)
	}
	first, missing := c.FirstMissing(arts)
	if !missing || first.What != "kind node image" {
		t.Fatalf("first missing = %+v, want the node image", first)
	}
	msg := c.MissingError(first).Error()
	for _, s := range []string{"kind node image", "kindest/node:v1@sha256:node", "cloudburrow prefetch", c.Path(first)} {
		if !strings.Contains(msg, s) {
			t.Errorf("refusal %q does not name %q", msg, s)
		}
	}
	if !errors.Is(c.MissingError(first), ErrMissing) {
		t.Error("the refusal is not ErrMissing")
	}

	for _, a := range arts[:4] {
		write(t, c.Path(a), []byte("archive"))
	}
	if first, _ := c.FirstMissing(c.Artifacts(p)); first.Name != "serving-core.yaml" {
		t.Fatalf("first missing = %+v, want serving-core.yaml", first)
	}
	// A YAML that does not match its pin is missing, not cached.
	write(t, c.Path(arts[4]), []byte(coreYAML+"# edited\n"))
	if first, _ := c.FirstMissing(c.Artifacts(p)); first.Name != "serving-core.yaml" {
		t.Fatalf("an edited YAML counted as cached: first missing = %+v", first)
	}
	write(t, c.Path(arts[4]), []byte(coreYAML))
	arts = c.Artifacts(p)
	if len(arts) != 8 {
		t.Fatalf("with the YAML cached, %d artifacts, want its 3 images added: %+v", len(arts), arts)
	}
	if first, _ := c.FirstMissing(arts); !strings.Contains(first.Ref, "cmd/queue@") {
		t.Errorf("first missing = %+v, want the queue image the YAML names", first)
	}
	if !arts[7].Unpinned || arts[5].Unpinned {
		t.Errorf("only the tag-named Envoy is unpinned: %+v", arts[5:])
	}
}

// fakeRunner records every command and answers from a script.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	present map[string]bool // images the daemon or node has
	archive []byte          // what docker save or ctr export writes
	// loadAs names the image each cached file holds, as a load would.
	loadAs map[string]string
}

func (f *fakeRunner) record(name string, args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, c)
	return c
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	c := f.record(name, args)
	switch {
	case strings.HasPrefix(c, "docker image inspect "), strings.Contains(c, " crictl inspecti -q "):
		if f.present[args[len(args)-1]] {
			return "{}", nil
		}
		return "", errors.New("No such image")
	case strings.HasPrefix(c, "docker load "):
		f.mu.Lock()
		f.present[f.loadAs[args[len(args)-1]]] = true
		f.mu.Unlock()
		return "", nil
	case strings.HasPrefix(c, "kind get nodes"):
		return "node-1\n", nil
	}
	return "", nil
}

func (f *fakeRunner) RunStdout(_ context.Context, w io.Writer, name string, args ...string) error {
	f.record(name, args)
	_, err := w.Write(f.archive)
	return err
}

func (f *fakeRunner) RunStdin(_ context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	f.record(name, args)
	b, _ := io.ReadAll(stdin)
	f.mu.Lock()
	f.present[strings.TrimPrefix(string(b), "archive:")] = true
	f.mu.Unlock()
	return "", nil
}

func (f *fakeRunner) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

// Prefetch pulls what the daemon lacks, pulls node images in a throwaway
// node, checks and names every archive, and a second run finds everything
// cached and runs nothing.
func TestPrefetcherStoresEverythingOnce(t *testing.T) {
	t.Parallel()
	c := Cache{Dir: t.TempDir()}
	r := &fakeRunner{present: map[string]bool{"kindest/node:v1@sha256:node": true}, archive: archive(t, true)}
	fetched := 0
	nodeStarted, nodeStopped := 0, 0
	p := &Prefetcher{
		Cache:  c,
		Runner: r,
		Fetch: func(_ context.Context, url string) ([]byte, error) {
			fetched++
			return []byte(coreYAML), nil
		},
		BuildStorage: func(context.Context) (string, error) { return "dev.local/cloudburrow-storage:abc", nil },
		Node: func(context.Context) (string, func(), error) {
			nodeStarted++
			return "helper-node", func() { nodeStopped++ }, nil
		},
		Platform: "linux/arm64",
	}
	stored, err := p.Run(context.Background(), testPlan())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 8 || fetched != 1 || nodeStarted != 1 || nodeStopped != 1 {
		t.Fatalf("stored %d, fetched %d, node started %d stopped %d; want 8, 1, 1, 1", len(stored), fetched, nodeStarted, nodeStopped)
	}
	calls := r.joined()
	for _, want := range []string{
		"docker save kindest/node:v1@sha256:node",
		"docker save dev.local/cloudburrow-storage:abc",
		"docker exec helper-node ctr --namespace=k8s.io images pull --platform linux/arm64 gcr.io/sdk@sha256:sdk",
		"docker exec helper-node ctr --namespace=k8s.io images export --platform linux/arm64 - docker.io/envoyproxy/envoy:v1.37-latest",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "docker pull") {
		t.Errorf("pulled an image the daemon had:\n%s", calls)
	}
	for _, s := range stored {
		if !s.Fresh || !c.Has(s.Artifact) {
			t.Errorf("%s: fresh %v, cached %v", s.Ref, s.Fresh, c.Has(s.Artifact))
		}
	}

	r2 := &fakeRunner{present: map[string]bool{}}
	p.Runner, p.Fetch = r2, func(context.Context, string) ([]byte, error) { t.Fatal("fetched again"); return nil, nil }
	p.Node = func(context.Context) (string, func(), error) { t.Fatal("started a node again"); return "", nil, nil }
	stored, err = p.Run(context.Background(), testPlan())
	if err != nil {
		t.Fatal(err)
	}
	if calls := r2.joined(); calls != "" {
		t.Errorf("a second prefetch ran commands:\n%s", calls)
	}
	for _, s := range stored {
		if s.Fresh {
			t.Errorf("%s reported fresh on the second run", s.Ref)
		}
	}
}

// A YAML that does not match its pin is refused and not cached.
func TestPrefetcherRefusesAManifestThatDoesNotMatchItsPin(t *testing.T) {
	t.Parallel()
	c := Cache{Dir: t.TempDir()}
	p := &Prefetcher{Cache: c, Runner: &fakeRunner{present: map[string]bool{}},
		Fetch: func(context.Context, string) ([]byte, error) { return []byte("tampered"), nil }}
	_, err := p.Run(context.Background(), testPlan())
	if err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("Run() = %v, want the pin mismatch", err)
	}
	if _, err := os.Stat(c.Path(Artifact{Kind: Manifest, Name: "serving-core.yaml", SHA256: testPlan().Knative[0].SHA256})); err == nil {
		t.Error("the tampered YAML was cached")
	}
}

// `up` with a cache loads the host images into Docker and imports the node
// images into the node, and pulls nothing: no `docker pull`, no ctr pull,
// no download. What the daemon or node already has is left alone.
func TestLoaderUsesTheCacheAndNeverPulls(t *testing.T) {
	t.Parallel()
	c := Cache{Dir: t.TempDir()}
	p := testPlan()
	write(t, c.Path(Artifact{Kind: Manifest, Name: "serving-core.yaml", SHA256: p.Knative[0].SHA256}), []byte(coreYAML))
	arts := c.Artifacts(p)
	for _, a := range arts {
		if a.Kind != Manifest {
			write(t, c.Path(a), []byte("archive:"+a.Ref))
		}
	}
	r := &fakeRunner{present: map[string]bool{"dev.local/cloudburrow-storage:abc": true, "ghcr.io/bq@sha256:bigquery": true}}
	r.loadAs = map[string]string{}
	for _, a := range arts {
		r.loadAs[c.Path(a)] = a.Ref
	}
	l := &Loader{Cache: c, Runner: r, Nodes: &images.Loader{ClusterName: "cloudburrow-x", Runner: r}}
	if err := l.LoadHost(context.Background(), arts); err != nil {
		t.Fatal(err)
	}
	if err := l.LoadNodes(context.Background(), arts); err != nil {
		t.Fatal(err)
	}
	calls := r.joined()
	if strings.Contains(calls, " pull") || strings.Contains(calls, "curl") {
		t.Errorf("up pulled with a complete cache:\n%s", calls)
	}
	if !strings.Contains(calls, "docker load --quiet --input "+c.Path(arts[0])) {
		t.Errorf("the node image was not loaded from the cache:\n%s", calls)
	}
	if strings.Contains(calls, "docker load --quiet --input "+c.Path(arts[1])) {
		t.Errorf("the storage image the daemon had was loaded again:\n%s", calls)
	}
	imports := strings.Count(calls, "ctr --namespace=k8s.io images import")
	// The node lacked the SDK image and Knative's three; it had BigQuery's.
	if imports != 4 {
		t.Errorf("%d imports, want 4:\n%s", imports, calls)
	}

	fetch := c.Fetcher(p, func(context.Context, string) ([]byte, error) {
		t.Fatal("downloaded a YAML that is cached")
		return nil, nil
	}, true)
	b, err := fetch(context.Background(), p.Knative[0].URL)
	if err != nil || string(b) != coreYAML {
		t.Errorf("cached fetch = %q, %v", b, err)
	}
	if _, err := fetch(context.Background(), "https://example.test/other.yaml"); !errors.Is(err, ErrMissing) {
		t.Errorf("offline fetch of an uncached URL = %v, want ErrMissing", err)
	}
}

func TestFetcherFallsBackOnlineOnly(t *testing.T) {
	t.Parallel()
	c := Cache{Dir: t.TempDir()}
	p := testPlan()
	online := c.Fetcher(p, func(_ context.Context, url string) ([]byte, error) { return []byte("from " + url), nil }, false)
	if b, err := online(context.Background(), p.Knative[0].URL); err != nil || !strings.HasPrefix(string(b), "from ") {
		t.Errorf("online, uncached: %q, %v; want the download", b, err)
	}
	offline := c.Fetcher(p, func(context.Context, string) ([]byte, error) {
		t.Fatal("downloaded offline")
		return nil, nil
	}, true)
	_, err := offline(context.Background(), p.Knative[0].URL)
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "serving-core.yaml") {
		t.Errorf("offline, uncached: %v; want ErrMissing naming the YAML", err)
	}
}

// The console terminal's image is prefetched like any node image, pulled in
// the throwaway node, but it is optional (#824): `up --offline` does not
// need it, LoadNodes leaves it to LoadOptional, which imports it when
// cached and does nothing when not.
func TestTheTerminalImageIsPrefetchedAndOptional(t *testing.T) {
	t.Parallel()
	c := Cache{Dir: t.TempDir()}
	p := testPlan()
	p.Knative = nil
	p.Terminal = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:term"
	arts := c.Artifacts(p)
	last := arts[len(arts)-1]
	if last.Ref != p.Terminal || !last.Optional || last.Kind != NodeImage || last.What != TerminalWhat {
		t.Fatalf("last artifact = %+v, want the optional terminal image", last)
	}
	for _, a := range arts[:len(arts)-1] {
		write(t, c.Path(a), []byte("archive:"+a.Ref))
	}
	if first, missing := c.FirstMissing(arts); missing {
		t.Errorf("up --offline would refuse for %+v, which is optional", first)
	}

	r := &fakeRunner{present: map[string]bool{}, archive: archive(t, true)}
	pf := &Prefetcher{Cache: c, Runner: r, Platform: "linux/amd64",
		Node: func(context.Context) (string, func(), error) { return "helper-node", func() {}, nil }}
	stored, err := pf.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	calls := r.joined()
	if !strings.Contains(calls, "docker exec helper-node ctr --namespace=k8s.io images pull --platform linux/amd64 "+p.Terminal) {
		t.Errorf("the terminal image was not pulled in the node:\n%s", calls)
	}
	if s := stored[len(stored)-1]; s.Ref != p.Terminal || !s.Fresh || !c.Has(s.Artifact) {
		t.Errorf("the terminal image was not stored: %+v", s)
	}

	// The fake import takes the image a stand-in archive names.
	write(t, c.Path(last), []byte("archive:"+last.Ref))
	lr := &fakeRunner{present: map[string]bool{}}
	l := &Loader{Cache: c, Runner: lr, Nodes: &images.Loader{ClusterName: "cloudburrow-x", Runner: lr}}
	if err := l.LoadNodes(context.Background(), arts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lr.joined(), p.Terminal) {
		t.Errorf("LoadNodes imported the optional terminal image before anything started:\n%s", lr.joined())
	}
	done, err := l.LoadOptional(context.Background(), last)
	if err != nil || !done || !lr.present[p.Terminal] {
		t.Errorf("LoadOptional = %v, %v; imported %v", done, err, lr.present[p.Terminal])
	}
	empty := &Loader{Cache: Cache{Dir: t.TempDir()}, Runner: lr, Nodes: l.Nodes}
	if done, err := empty.LoadOptional(context.Background(), last); done || err != nil {
		t.Errorf("LoadOptional of an uncached image = %v, %v; want nothing done", done, err)
	}
}
