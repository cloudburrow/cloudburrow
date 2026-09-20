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

// ExecRunner is the real command runner.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
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
