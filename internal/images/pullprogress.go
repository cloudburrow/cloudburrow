package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// PullMeter reads, from a kind node's containerd, how much of an image the
// node has fetched while its kubelet pulls it (#826). The kubelet reports a
// pull's start and end and nothing in between; the node's content store has
// the rest:
//
//   - the image's index and platform manifest, fetched first, which give
//     the compressed size of every layer;
//   - `ctr content active`, the ingests in flight, one `layer-sha256:<digest>`
//     ref per layer being downloaded, with the bytes written so far;
//   - `ctr content ls -q`, the blobs committed, which include the layers
//     already downloaded.
//
// Measured on a kind v1.36.4 node (containerd 2.3.4) pulling the terminal
// image: committed layers stay in the content store until the whole pull
// has been unpacked, and only then are discarded (kind sets
// discard_unpacked_layers), by which time the kubelet reports the pull
// finished. So fetched is the committed layers' sizes plus the active
// layers' offsets, and total is the manifest's layer sizes; both come from
// the node, and nothing is estimated.
//
// Everything it runs is read-only: it never imports, pulls or removes.
type PullMeter struct {
	// Loader names the cluster and runs docker.
	Loader *Loader
	// Ref is the image, pinned by digest: repository@sha256:<hex>.
	Ref string

	mu     sync.Mutex
	layers map[string][]layer // by node, once read
}

// PullProgress is what a PullMeter read.
type PullProgress struct {
	// Fetched is the image's compressed bytes on the node: committed layers
	// and the bytes written so far of the layers being downloaded. ctr
	// prints an ingest's offset to four significant digits, so Fetched is as
	// precise as that.
	Fetched int64
	// Total is the sum of the compressed sizes of the image's layers for the
	// node's platform, from the manifest on the node.
	Total int64
}

type layer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// ociManifest is the part of an image index or manifest the meter reads.
type ociManifest struct {
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
	Layers []layer `json:"layers"`
}

// ctr runs ctr in a node, in the namespace the kubelet's images live in.
func (m *PullMeter) ctr(ctx context.Context, node string, args ...string) (string, error) {
	return m.Loader.Runner.Run(ctx, "docker", append([]string{"exec", node, "ctr", "--namespace=k8s.io"}, args...)...)
}

// Read reports the pull's progress on node, which must be one of the
// cluster's nodes. It fails, and the caller shows something else, when the
// node cannot be read or the image's manifest is not yet on it.
func (m *PullMeter) Read(ctx context.Context, node string) (PullProgress, error) {
	layers, err := m.manifestLayers(ctx, node)
	if err != nil {
		return PullProgress{}, err
	}
	active, err := m.ctr(ctx, node, "content", "active")
	if err != nil {
		return PullProgress{}, fmt.Errorf("read the ingests on %s: %w", node, err)
	}
	ingests, err := parseActive(active)
	if err != nil {
		return PullProgress{}, err
	}
	ls, err := m.ctr(ctx, node, "content", "ls", "-q")
	if err != nil {
		return PullProgress{}, fmt.Errorf("read the content store on %s: %w", node, err)
	}
	return measure(layers, ingests, parseCommitted(ls)), nil
}

// measure adds up what is on the node of each layer.
func measure(layers []layer, ingests map[string]int64, committed map[string]bool) PullProgress {
	var p PullProgress
	for _, l := range layers {
		p.Total += l.Size
		if committed[l.Digest] {
			p.Fetched += l.Size
		} else {
			// An offset rounded up to ctr's four digits can pass the
			// layer's size; it is never more than the layer.
			p.Fetched += min(ingests["layer-"+l.Digest], l.Size)
		}
	}
	return p
}

// manifestLayers reads the layers of the image for node's platform, once.
func (m *PullMeter) manifestLayers(ctx context.Context, node string) ([]layer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.layers[node]; ok {
		return l, nil
	}
	_, digest, ok := strings.Cut(m.Ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return nil, fmt.Errorf("%s is not pinned by digest", m.Ref)
	}
	// Only a node of this cluster is read: a container of the same name
	// that is not one would be another's.
	nodes, err := m.Loader.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, n := range nodes {
		found = found || n == node
	}
	if !found {
		return nil, fmt.Errorf("%s is not a node of cluster %s", node, m.Loader.ClusterName)
	}
	machine, err := m.Loader.Runner.Run(ctx, "docker", "exec", node, "uname", "-m")
	if err != nil {
		return nil, fmt.Errorf("read the architecture of %s: %w", node, err)
	}
	arch, err := goArch(strings.TrimSpace(machine))
	if err != nil {
		return nil, err
	}
	layers, err := m.readLayers(ctx, node, digest, arch)
	if err != nil {
		return nil, err
	}
	if m.layers == nil {
		m.layers = map[string][]layer{}
	}
	m.layers[node] = layers
	return layers, nil
}

func (m *PullMeter) readLayers(ctx context.Context, node, digest, arch string) ([]layer, error) {
	for depth := 0; depth < 2; depth++ {
		out, err := m.ctr(ctx, node, "content", "get", digest)
		if err != nil {
			return nil, fmt.Errorf("read %s on %s (its pull may not have started): %w", digest, node, err)
		}
		next, layers, err := platformManifest([]byte(out), arch)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return layers, nil
		}
		digest = next
	}
	return nil, errors.New("image index nests another index")
}

// platformManifest returns, for an index, the digest of arch's linux
// manifest, and for a manifest, its layers.
func platformManifest(b []byte, arch string) (string, []layer, error) {
	var m ociManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return "", nil, fmt.Errorf("read the image's manifest: %w", err)
	}
	if len(m.Manifests) > 0 {
		for _, d := range m.Manifests {
			if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == arch {
				return d.Digest, nil, nil
			}
		}
		return "", nil, fmt.Errorf("the image has no linux/%s manifest", arch)
	}
	if len(m.Layers) == 0 {
		return "", nil, errors.New("the image's manifest lists no layers")
	}
	return "", m.Layers, nil
}

// goArch is an image platform's architecture from `uname -m`.
func goArch(machine string) (string, error) {
	switch machine {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("unrecognised node architecture %q", machine)
}

// parseActive reads `ctr content active`: a header, then one line per
// ingest of its ref, the bytes written so far and its age, separated by
// tabs, the size as go-units' HumanSize prints it ("243.3MB").
func parseActive(out string) (map[string]int64, error) {
	ingests := map[string]int64{}
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || (i == 0 && f[0] == "REF") {
			continue
		}
		if len(f) < 2 {
			return nil, fmt.Errorf("unrecognised ctr content active line %q", line)
		}
		n, err := parseHumanSize(f[1])
		if err != nil {
			return nil, err
		}
		ingests[f[0]] = n
	}
	return ingests, nil
}

// parseCommitted reads `ctr content ls -q`: one digest a line.
func parseCommitted(out string) map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if d := strings.TrimSpace(line); d != "" {
			set[d] = true
		}
	}
	return set
}

var sizeUnits = map[string]float64{
	"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
}

// parseHumanSize reads a size as ctr prints it: a number and a unit, with
// no space ("1.049MB", "0B").
func parseHumanSize(s string) (int64, error) {
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0, fmt.Errorf("unrecognised size %q", s)
	}
	unit, ok := sizeUnits[s[i:]]
	if !ok {
		return 0, fmt.Errorf("unrecognised size unit in %q", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognised size %q", s)
	}
	return int64(math.Round(n * unit)), nil
}
