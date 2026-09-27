// Package prefetch keeps the artifacts a first `up` downloads in the state
// directory, so `up` can run where the network cannot reach them (#604).
//
// A first `up` needs Docker Hub for the kind node image, the registries of
// every enabled backend, gcr.io for Knative's images and GitHub for
// Knative's release YAMLs. `cloudburrow prefetch` stores each of them under
// <state dir>/cache: images as archives written by `docker save`, and the
// YAMLs after checking them against their pinned sha256. `up` prefers a
// cached copy to a download, and `up --offline` refuses before creating a
// cluster when anything it needs is not cached, naming the first missing
// artifact.
//
// Nothing here resolves a reference: every image is the reference
// CloudBurrow already pins (dependencies.json), or the one a checksummed
// Knative YAML names.
package prefetch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kind is where an artifact is used.
type Kind int

const (
	// HostImage is an image the Docker daemon itself needs: the kind node
	// image, and the builtin storage server's locally built image.
	HostImage Kind = iota
	// NodeImage is an image the cluster's nodes run: a backend's or
	// Knative's. It is imported into the nodes rather than pulled by them.
	NodeImage
	// Manifest is a Knative release YAML, pinned by sha256.
	Manifest
)

func (k Kind) String() string {
	switch k {
	case HostImage, NodeImage:
		return "image"
	default:
		return "manifest"
	}
}

// Artifact is one thing `up` would otherwise download.
type Artifact struct {
	Kind Kind
	// What says what it is for, for a person: "kind node image".
	What string
	// Ref is the image reference, or the manifest's URL.
	Ref string
	// SHA256 is a manifest's pinned hash.
	SHA256 string
	// Name is a manifest's file name.
	Name string
	// Unpinned marks an image named by a tag alone. The one today is
	// Kourier's Envoy, which the checksummed kourier.yaml names by tag.
	Unpinned bool
	// Built marks the image this CLI builds rather than pulls: the builtin
	// storage server's.
	Built bool
}

// ErrMissing means an artifact `up --offline` needs is not in the cache.
var ErrMissing = errors.New("not in the offline cache")

// Cache is the artifact store under the state directory.
type Cache struct {
	Dir string
}

// CacheDir is where an instance's state directory keeps the cache. It is
// shared by every instance in that state directory, since the pins are the
// CLI's, not an instance's.
func CacheDir(stateDir string) string { return filepath.Join(stateDir, "cache") }

// Path is where an artifact is kept.
func (c Cache) Path(a Artifact) string {
	if a.Kind == Manifest {
		return filepath.Join(c.Dir, "manifests", a.SHA256[:12]+"-"+a.Name)
	}
	return filepath.Join(c.Dir, "images", fileName(a.Ref)+".tar")
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// fileName is a reference as a file name. A digest is kept whole, so two
// pins of one repository never share a file.
func fileName(ref string) string {
	return strings.Trim(unsafeChars.ReplaceAllString(ref, "_"), "_")
}

// Has reports whether the artifact is cached. A manifest is also checked
// against its pin, so a truncated or edited file counts as missing.
func (c Cache) Has(a Artifact) bool {
	if a.Ref == "" {
		return false
	}
	if a.Kind == Manifest {
		_, err := c.ReadManifest(a)
		return err == nil
	}
	info, err := os.Stat(c.Path(a))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// ReadManifest returns a cached manifest after checking its sha256.
func (c Cache) ReadManifest(a Artifact) ([]byte, error) {
	b, err := os.ReadFile(c.Path(a))
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(b); got != a.SHA256 {
		return nil, fmt.Errorf("cached %s has sha256 %s, not the pinned %s", a.Name, got, a.SHA256)
	}
	return b, nil
}

// FirstMissing returns the first artifact not in the cache.
func (c Cache) FirstMissing(arts []Artifact) (Artifact, bool) {
	for _, a := range arts {
		if !c.Has(a) {
			return a, true
		}
	}
	return Artifact{}, false
}

// MissingError is the refusal `up --offline` prints.
func (c Cache) MissingError(a Artifact) error {
	ref := a.Ref
	if ref == "" {
		ref = "(reference unknown)"
	}
	return fmt.Errorf("%w: %s %s (expected at %s). Run `cloudburrow prefetch` with the same flags "+
		"on a machine that can reach the network, and copy its state directory here; see "+
		"\"Offline and air-gapped use\" in docs/install.md", ErrMissing, a.What, ref, c.Path(a))
}

// Plan is what an instance needs, before the cache is consulted.
type Plan struct {
	// NodeImage is the pinned kind node image.
	NodeImage string
	// Storage is whether the builtin storage server is enabled.
	Storage bool
	// StorageImage is its image as this CLI builds it for Arch; empty when
	// this CLI has no storage server for Arch.
	StorageImage string
	// Arch is the Docker daemon's architecture, which is a kind node's.
	Arch string
	// Images are the backend images, by the service names that use each.
	Images map[string][]string
	// Knative lists the release YAMLs when Cloud Run is enabled.
	Knative []KnativeManifest
}

// KnativeManifest is a pinned release YAML.
type KnativeManifest struct {
	Name, URL, SHA256 string
}

// Artifacts lists what the plan needs, in the order `up` needs it: the node
// image, the storage image, the backends, then Knative. The images a
// Knative YAML names are known only from the YAML, so they are listed when
// it is cached; until then the YAML itself is the first thing missing.
func (c Cache) Artifacts(p Plan) []Artifact {
	var out []Artifact
	out = append(out, Artifact{Kind: HostImage, What: "kind node image", Ref: p.NodeImage})
	if p.Storage {
		out = append(out, Artifact{Kind: HostImage, Built: true,
			What: "builtin Cloud Storage server image (built by this CLI for linux/" + p.Arch + ")",
			Ref:  p.StorageImage})
	}
	refs := make([]string, 0, len(p.Images))
	for ref := range p.Images {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		out = append(out, Artifact{Kind: NodeImage,
			What: "backend image for " + strings.Join(p.Images[ref], ", "), Ref: ref})
	}
	var named []string
	for _, m := range p.Knative {
		a := Artifact{Kind: Manifest, What: "Knative " + m.Name, Ref: m.URL, SHA256: m.SHA256, Name: m.Name}
		out = append(out, a)
		if b, err := c.ReadManifest(a); err == nil {
			named = append(named, ImagesIn(b)...)
		}
	}
	seen := map[string]bool{}
	for _, ref := range named {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, Artifact{Kind: NodeImage, What: "Knative image", Ref: ref,
			Unpinned: !strings.Contains(ref, "@sha256:")})
	}
	return out
}

var (
	// digestRef is any image reference pinned by digest, wherever it
	// appears: Knative names its queue-proxy image in a ConfigMap value,
	// not in an image: field.
	digestRef = regexp.MustCompile(`[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[A-Za-z0-9._-]+)+(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[0-9a-f]{64}`)
	// imageField is a container's image: field with a value.
	imageField = regexp.MustCompile(`(?m)^[ \t]*(?:-[ \t]*)?image:[ \t]*["']?([^\s"'#]+)`)
)

// ImagesIn returns the image references a manifest names, in order of first
// appearance.
func ImagesIn(manifest []byte) []string {
	type hit struct {
		at  int
		ref string
	}
	var hits []hit
	for _, m := range digestRef.FindAllIndex(manifest, -1) {
		hits = append(hits, hit{m[0], string(manifest[m[0]:m[1]])})
	}
	for _, m := range imageField.FindAllSubmatchIndex(manifest, -1) {
		hits = append(hits, hit{m[2], string(manifest[m[2]:m[3]])})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	var out []string
	seen := map[string]bool{}
	for _, h := range hits {
		if !seen[h.ref] {
			seen[h.ref] = true
			out = append(out, h.ref)
		}
	}
	return out
}

// Runner runs docker and kind. RunStdout streams a command's output, an
// image archive, to w.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunStdout(ctx context.Context, w io.Writer, name string, args ...string) error
}

// Fetcher downloads a manifest.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// Prefetcher fills a cache.
type Prefetcher struct {
	Cache  Cache
	Runner Runner
	Fetch  Fetcher
	// BuildStorage builds the builtin storage image and returns its tag.
	BuildStorage func(ctx context.Context) (string, error)
	// Node starts a throwaway kind node from the pinned node image and
	// returns its container and a function that removes it. Node images
	// are pulled and exported by that node's containerd, the runtime that
	// imports them at `up`, rather than by `docker save`: Docker 29 saved
	// the Spanner emulator's image as its index and manifest with no
	// config or layers, and exited 0 (measured).
	Node func(ctx context.Context) (node string, stop func(), err error)
	// Platform is the nodes' platform, linux/<arch>.
	Platform string
	Out      io.Writer
}

// Stored is one artifact in the cache after a prefetch.
type Stored struct {
	Artifact
	Path  string
	Bytes int64
	// Fresh is false when the artifact was already cached.
	Fresh bool
}

// Run stores every artifact of the plan: the Knative YAMLs first, since the
// images they name are part of the plan, then the Docker daemon's images,
// then the nodes'.
func (p *Prefetcher) Run(ctx context.Context, plan Plan) ([]Stored, error) {
	// had records what the cache held before this run wrote anything, by
	// path. Fresh comes from it, not from comparing a file's mtime with the
	// start time: a filesystem's mtime can be coarser than, or behind, the
	// clock time.Now reads, so a file written just now could read as older
	// than the run and be reported as already cached (#668).
	had := map[string]bool{}
	for _, m := range plan.Knative {
		a := Artifact{Kind: Manifest, What: "Knative " + m.Name, Ref: m.URL, SHA256: m.SHA256, Name: m.Name}
		if p.Cache.Has(a) {
			had[p.Cache.Path(a)] = true
			continue
		}
		p.logf("  fetching %s\n", m.URL)
		b, err := p.Fetch(ctx, m.URL)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", m.Name, err)
		}
		if got := sha256Hex(b); got != m.SHA256 {
			return nil, fmt.Errorf("%s has sha256 %s, not the pinned %s; not caching it", m.Name, got, m.SHA256)
		}
		if err := writeAtomic(p.Cache.Path(a), func(w io.Writer) error { _, err := w.Write(b); return err }); err != nil {
			return nil, err
		}
	}
	arts := p.Cache.Artifacts(plan)
	for _, a := range arts {
		// The manifests were recorded above, before they were fetched.
		if a.Kind != Manifest && p.Cache.Has(a) {
			had[p.Cache.Path(a)] = true
		}
	}
	for _, a := range arts {
		if a.Kind == HostImage && !p.Cache.Has(a) {
			if err := p.storeHost(ctx, a); err != nil {
				return nil, err
			}
		}
	}
	if err := p.storeNodeImages(ctx, arts); err != nil {
		return nil, err
	}
	var stored []Stored
	for _, a := range arts {
		info, err := os.Stat(p.Cache.Path(a))
		if err != nil {
			return nil, err
		}
		stored = append(stored, Stored{Artifact: a, Path: p.Cache.Path(a), Bytes: info.Size(),
			Fresh: !had[p.Cache.Path(a)]})
	}
	return stored, nil
}

// storeHost saves an image the Docker daemon needs, pulling it first if
// the daemon lacks it.
func (p *Prefetcher) storeHost(ctx context.Context, a Artifact) error {
	if a.Ref == "" {
		return fmt.Errorf("%s: no reference to store", a.What)
	}
	if a.Built {
		p.logf("  building %s\n", a.Ref)
		ref, err := p.BuildStorage(ctx)
		if err != nil {
			return fmt.Errorf("build the storage image: %w", err)
		}
		if ref != a.Ref {
			return fmt.Errorf("built %s, expected %s", ref, a.Ref)
		}
	} else if _, err := p.Runner.Run(ctx, "docker", "image", "inspect", a.Ref); err != nil {
		p.logf("  pulling %s\n", a.Ref)
		if _, err := p.Runner.Run(ctx, "docker", "pull", "--quiet", a.Ref); err != nil {
			return fmt.Errorf("pull %s: %w", a.Ref, err)
		}
	}
	p.logf("  saving %s\n", a.Ref)
	return p.save(ctx, a, "docker", "save", a.Ref)
}

// storeNodeImages pulls each missing node image in a throwaway node and
// exports it from there.
func (p *Prefetcher) storeNodeImages(ctx context.Context, arts []Artifact) error {
	var missing []Artifact
	for _, a := range arts {
		if a.Kind == NodeImage && !p.Cache.Has(a) {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	p.logf("  starting a throwaway kind node to pull %d image(s) with\n", len(missing))
	node, stop, err := p.Node(ctx)
	if err != nil {
		return fmt.Errorf("start a node to pull images with: %w", err)
	}
	defer stop()
	for _, a := range missing {
		// ctr takes only a fully qualified name.
		name := CanonicalName(a.Ref)
		p.logf("  pulling %s\n", a.Ref)
		if _, err := p.Runner.Run(ctx, "docker", "exec", node, "ctr", "--namespace=k8s.io", "images", "pull",
			"--platform", p.Platform, name); err != nil {
			return fmt.Errorf("pull %s: %w", a.Ref, err)
		}
		if err := p.save(ctx, a, "docker", "exec", node, "ctr", "--namespace=k8s.io", "images", "export",
			"--platform", p.Platform, "-", name); err != nil {
			return err
		}
	}
	return nil
}

// save streams an archive from a command into the cache, named and checked.
func (p *Prefetcher) save(ctx context.Context, a Artifact, name string, args ...string) error {
	return writeAtomic(p.Cache.Path(a), func(w io.Writer) error {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := Name(pr, w, CanonicalName(a.Ref))
			// Drain, so the command never blocks on a reader that stopped.
			_, _ = io.Copy(io.Discard, pr)
			done <- err
		}()
		err := p.Runner.RunStdout(ctx, pw, name, args...)
		_ = pw.CloseWithError(err)
		if nerr := <-done; err == nil {
			err = nerr
		}
		if err != nil {
			return fmt.Errorf("archive %s: %w", a.Ref, err)
		}
		return nil
	})
}

func (p *Prefetcher) logf(format string, a ...any) {
	if p.Out != nil {
		fmt.Fprintf(p.Out, format, a...)
	}
}

// CanonicalName is the fully qualified name an archive gives its image:
// docker.io/library/ spelled out, and a tag dropped when a digest is
// present, which is how containerd names an image pulled by digest and
// how the CRI resolves the reference a pod names.
func CanonicalName(ref string) string {
	name, digest, hasDigest := strings.Cut(ref, "@")
	first, _, hasSlash := strings.Cut(name, "/")
	switch {
	case !hasSlash:
		name = "docker.io/library/" + name
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		name = "docker.io/" + name
	}
	if hasDigest {
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name = name[:i]
		}
		return name + "@" + digest
	}
	return name
}

// annotationName is the index annotation containerd and Docker name an
// imported image by.
const annotationName = "io.containerd.image.name"

// Name copies a `docker save` archive from r to w with its image named.
//
// `docker save` of a reference pinned by digest writes an OCI index with no
// name: loaded back, the image has none, so neither `docker image inspect`
// by the pinned reference nor a pod naming it finds it (measured on Docker
// 29 with the containerd image store). Naming it in index.json is what
// makes both find it again.
func Name(r io.Reader, w io.Writer, name string) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	sawIndex := false
	entries := map[string]bool{}
	var legacy []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entries[h.Name] = true
		if h.Name != "index.json" && h.Name != "manifest.json" {
			// Layers are streamed: one can be hundreds of megabytes.
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return err
		}
		if h.Name == "index.json" {
			sawIndex = true
			if body, err = nameIndex(body, name); err != nil {
				return err
			}
			h.Size = int64(len(body))
		} else {
			legacy = body
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	if !sawIndex {
		return errors.New("the archive has no OCI index.json; the offline cache needs a Docker that writes one (25 or later)")
	}
	if err := complete(legacy, entries); err != nil {
		return err
	}
	return tw.Close()
}

// complete checks that the archive holds the config and every layer its
// manifest.json lists: an archive without them imports as an image that
// cannot run, and `docker save` has written one and exited 0.
func complete(legacy []byte, entries map[string]bool) error {
	if legacy == nil {
		return errors.New("the archive has no manifest.json to check it against")
	}
	var images []struct {
		Config string
		Layers []string
	}
	if err := json.Unmarshal(legacy, &images); err != nil {
		return fmt.Errorf("manifest.json: %w", err)
	}
	if len(images) == 0 {
		return errors.New("the archive holds no image")
	}
	for _, img := range images {
		for _, blob := range append([]string{img.Config}, img.Layers...) {
			if !entries[blob] {
				return fmt.Errorf("the archive is incomplete: it lacks %s, which its manifest lists", blob)
			}
		}
	}
	return nil
}

func nameIndex(b []byte, name string) ([]byte, error) {
	var index map[string]json.RawMessage
	if err := json.Unmarshal(b, &index); err != nil {
		return nil, fmt.Errorf("index.json: %w", err)
	}
	var manifests []map[string]any
	if err := json.Unmarshal(index["manifests"], &manifests); err != nil {
		return nil, fmt.Errorf("index.json manifests: %w", err)
	}
	if len(manifests) != 1 {
		return nil, fmt.Errorf("index.json has %d images, want 1", len(manifests))
	}
	ann, _ := manifests[0]["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
	}
	ann[annotationName] = name
	manifests[0]["annotations"] = ann
	m, err := json.Marshal(manifests)
	if err != nil {
		return nil, err
	}
	index["manifests"] = m
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(index); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// writeAtomic writes a file through a temporary beside it, so an
// interrupted prefetch never leaves a partial artifact that counts as
// cached.
func writeAtomic(path string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
