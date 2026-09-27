// Package images handles getting locally built images into the cluster without
// a cloud registry.
//
// The constraint this package exists for: Knative resolves image tags to
// digests by contacting the registry, so an image loaded straight into the
// cluster fails with "failed to resolve image to digest: 401 Unauthorized".
// Knative skips that resolution for a fixed set of registry prefixes, so a
// local image must carry one. See docs/local-verification.md §5.1.
package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// LocalPrefix is the registry prefix Knative skips tag resolution for.
//
// Knative's default registriesSkippingTagResolving includes dev.local,
// ko.local and kind.local. dev.local is used here because it carries no
// implication about which tool built the image.
const LocalPrefix = "dev.local/"

// Errors callers are expected to distinguish.
var (
	// ErrNotLocal means the reference lacks a prefix Knative will accept
	// without contacting a registry.
	ErrNotLocal = errors.New("image reference is not local")
	// ErrArchMismatch means the image cannot run on the cluster's nodes.
	ErrArchMismatch = errors.New("image architecture does not match the cluster")
	// ErrNotPresent means the image is not available locally to load.
	ErrNotPresent = errors.New("image is not present locally")
)

// Runner executes an external command. Injected so this package is testable
// without Docker or a cluster.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Loader loads locally built images into a kind cluster.
type Loader struct {
	ClusterName string
	Runner      Runner
}

// IsLocal reports whether a reference will bypass Knative tag resolution.
func IsLocal(ref string) bool {
	for _, p := range []string{"dev.local/", "ko.local/", "kind.local/"} {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// Localise returns a reference that Knative will not try to resolve remotely.
//
// A reference that already carries an accepted prefix is returned unchanged; a
// bare local name gains LocalPrefix. A reference that names a real registry is
// left alone, because it is meant to be pulled.
func Localise(ref string) string {
	if IsLocal(ref) {
		return ref
	}
	if isRemoteRef(ref) {
		return ref
	}
	return LocalPrefix + ref
}

// isRemoteRef reports whether a reference names a registry host. The heuristic
// is the standard one: the first path segment is a registry if it contains a
// dot or a colon, or is exactly "localhost".
func isRemoteRef(ref string) bool {
	first, _, ok := strings.Cut(ref, "/")
	if !ok {
		return false // e.g. "myimage:tag" — a bare local name
	}
	return strings.ContainsAny(first, ".:") || first == "localhost"
}

// RequireTagged rejects a reference without an explicit tag or digest, since a
// mutable "latest" is forbidden in anything reproducible (ADR-0005).
func RequireTagged(ref string) error {
	name := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	if strings.Contains(ref, "@sha256:") {
		return nil
	}
	if !strings.Contains(name, ":") {
		return fmt.Errorf("image %q must carry an explicit tag or digest; a bare reference is a mutable target", ref)
	}
	return nil
}

// PullPolicy returns the imagePullPolicy an image reference requires.
//
// A locally loaded image must never be pulled: the node already has it and no
// registry can serve it.
func PullPolicy(ref string) string {
	if IsLocal(ref) {
		return "Never"
	}
	return "IfNotPresent"
}

// Architecture returns the architecture of a local image.
func (l *Loader) Architecture(ctx context.Context, ref string) (string, error) {
	out, err := l.Runner.Run(ctx, "docker", "image", "inspect", ref, "--format", "{{.Architecture}}")
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNotPresent, ref, err)
	}
	return strings.TrimSpace(out), nil
}

// NodeArchitecture returns the architecture of the cluster's nodes.
func (l *Loader) NodeArchitecture(ctx context.Context, kubeconfig string) (string, error) {
	out, err := l.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Load makes a locally built image available to the cluster.
//
// It verifies the reference is tagged, is local, exists, and matches the node
// architecture before loading — so a mismatch is reported as itself rather than
// as an opaque ImagePullBackOff later.
func (l *Loader) Load(ctx context.Context, ref, kubeconfig string) error {
	if err := RequireTagged(ref); err != nil {
		return err
	}
	if !IsLocal(ref) {
		return fmt.Errorf("%w: %s lacks a prefix Knative accepts without a registry (use %s)",
			ErrNotLocal, ref, LocalPrefix)
	}

	imageArch, err := l.Architecture(ctx, ref)
	if err != nil {
		return err
	}
	if kubeconfig != "" {
		nodeArch, err := l.NodeArchitecture(ctx, kubeconfig)
		if err == nil && nodeArch != "" && imageArch != "" && imageArch != nodeArch {
			return fmt.Errorf("%w: image %s is %s but the cluster nodes are %s; rebuild with --platform linux/%s",
				ErrArchMismatch, ref, imageArch, nodeArch, nodeArch)
		}
	}

	if _, err := l.Runner.Run(ctx, "kind", "load", "docker-image", ref, "--name", l.ClusterName); err != nil {
		return fmt.Errorf("load image %s into cluster %s: %w", ref, l.ClusterName, err)
	}
	return nil
}

// StdinRunner is a Runner that can also feed a command's standard input.
// Importing an archive into a node streams it through `docker exec -i`.
type StdinRunner interface {
	RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error)
}

// Nodes returns the cluster's node containers.
func (l *Loader) Nodes(ctx context.Context) ([]string, error) {
	out, err := l.Runner.Run(ctx, "kind", "get", "nodes", "--name", l.ClusterName)
	if err != nil {
		return nil, fmt.Errorf("list the nodes of %s: %w", l.ClusterName, err)
	}
	var nodes []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("cluster %s has no nodes", l.ClusterName)
	}
	return nodes, nil
}

// NodeHas reports whether a node's container runtime has ref, as the
// kubelet would find it: through the CRI, by the same reference a pod names.
func (l *Loader) NodeHas(ctx context.Context, node, ref string) bool {
	_, err := l.Runner.Run(ctx, "docker", "exec", node, "crictl", "inspecti", "-q", ref)
	return err == nil
}

// ImportArchive imports an image archive into every node that lacks ref,
// and confirms each node then has it (#604).
//
// It is the import `kind load image-archive` performs, less two flags
// measured to break a digest-pinned image. --all-platforms fails on an
// archive of a multi-platform index that holds one platform's layers,
// which is what `docker save` of a pulled index writes; --digests adds an
// "import-<date>" name the CRI then resolved the pod's image to, and the
// container failed to start with "image ... not found". Without them the
// image is named by the archive's io.containerd.image.name annotation,
// which the cache writes.
func (l *Loader) ImportArchive(ctx context.Context, archive, ref string) error {
	sr, ok := l.Runner.(StdinRunner)
	if !ok {
		return errors.New("this runner cannot stream an archive into a node")
	}
	nodes, err := l.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if l.NodeHas(ctx, node, ref) {
			continue
		}
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		_, err = sr.RunStdin(ctx, f, "docker", "exec", "--privileged", "-i", node,
			"ctr", "--namespace=k8s.io", "images", "import", "--snapshotter=overlayfs", "-")
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("import %s into %s: %w", archive, node, err)
		}
		if !l.NodeHas(ctx, node, ref) {
			return fmt.Errorf("imported %s into %s, but the node's runtime does not have %s", archive, node, ref)
		}
	}
	return nil
}

// ExecRunner is the real command runner.
type ExecRunner struct{}

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return r.RunStdin(ctx, nil, name, args...)
}

// RunStdin runs a command with stdin as its standard input.
func (ExecRunner) RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return out.String(), fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out.String(), fmt.Errorf("%s: %w", name, err)
	}
	return out.String(), nil
}
