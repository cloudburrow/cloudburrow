package prefetch

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/images"
)

// Loader puts cached images where `up` needs them: the Docker daemon for
// the node and storage images, the cluster's nodes for the rest. An image
// already there is left alone, and one not cached is left to be pulled as
// before, unless the caller refused that up front with --offline.
type Loader struct {
	Cache  Cache
	Runner Runner
	// Nodes imports into the cluster; needed only by LoadNodes.
	Nodes *images.Loader
	Out   io.Writer
}

// LoadHost loads each cached host image the daemon lacks.
func (l *Loader) LoadHost(ctx context.Context, arts []Artifact) error {
	for _, a := range arts {
		if a.Kind != HostImage || !l.Cache.Has(a) {
			continue
		}
		if _, err := l.Runner.Run(ctx, "docker", "image", "inspect", a.Ref); err == nil {
			continue
		}
		l.logf("  offline cache: loading %s into docker\n", a.Ref)
		if _, err := l.Runner.Run(ctx, "docker", "load", "--quiet", "--input", l.Cache.Path(a)); err != nil {
			return fmt.Errorf("load %s from %s: %w", a.What, l.Cache.Path(a), err)
		}
		if _, err := l.Runner.Run(ctx, "docker", "image", "inspect", a.Ref); err != nil {
			return fmt.Errorf("loaded %s, but docker does not find %s: %w. The cache was written for Docker's "+
				"containerd image store; the classic store does not keep a digest reference across save and load",
				l.Cache.Path(a), a.Ref, err)
		}
	}
	return nil
}

// LoadNodes imports each cached node image into the cluster's nodes.
func (l *Loader) LoadNodes(ctx context.Context, arts []Artifact) error {
	for _, a := range arts {
		if a.Kind != NodeImage || !l.Cache.Has(a) {
			continue
		}
		l.logf("  offline cache: %s\n", a.Ref)
		if err := l.Nodes.ImportArchive(ctx, l.Cache.Path(a), a.Ref); err != nil {
			return fmt.Errorf("%s: %w", a.What, err)
		}
	}
	return nil
}

func (l *Loader) logf(format string, a ...any) {
	if l.Out != nil {
		fmt.Fprintf(l.Out, format, a...)
	}
}

// Fetcher returns the Knative YAML fetch `up` uses: the cached copy when
// there is one, else fallback, or, offline, a refusal. The installer checks
// what it returns against the pin before applying it either way.
func (c Cache) Fetcher(plan Plan, fallback Fetcher, offline bool) Fetcher {
	return func(ctx context.Context, url string) ([]byte, error) {
		for _, m := range plan.Knative {
			if m.URL != url {
				continue
			}
			a := Artifact{Kind: Manifest, What: "Knative " + m.Name, Ref: m.URL, SHA256: m.SHA256, Name: m.Name}
			if b, err := c.ReadManifest(a); err == nil {
				return b, nil
			}
			if offline {
				return nil, c.MissingError(a)
			}
		}
		if offline {
			return nil, fmt.Errorf("%w: %s is not a cached manifest", ErrMissing, url)
		}
		return fallback(ctx, url)
	}
}

// ExecRunner runs real commands.
type ExecRunner struct{}

// Run runs a command and returns its standard output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	var out strings.Builder
	err := ExecRunner{}.RunStdout(ctx, &out, name, args...)
	return out.String(), err
}

// RunStdout runs a command with its standard output streamed to w.
func (ExecRunner) RunStdout(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var errOut strings.Builder
	cmd.Stdout = w
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
