package components

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ErrInstallFailed means a component could not be installed or did not become
// ready within its bound.
var ErrInstallFailed = errors.New("component install failed")

// Runner executes an external command, optionally with stdin.
type Runner interface {
	Run(ctx context.Context, stdin string, name string, args ...string) (string, error)
}

// ExecRunner is the real runner.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
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

// Installer applies CloudBurrow's components to a cluster.
type Installer struct {
	Kubeconfig string
	Namespace  string
	Instance   string
	Runner     Runner
	Out        io.Writer
}

func (i *Installer) kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", i.Kubeconfig}, args...)
	return i.Runner.Run(ctx, stdin, "kubectl", full...)
}

func (i *Installer) logf(format string, a ...any) {
	if i.Out != nil {
		fmt.Fprintf(i.Out, format, a...)
	}
}

// Apply applies a manifest from stdin.
func (i *Installer) Apply(ctx context.Context, manifest string) error {
	if _, err := i.kubectl(ctx, manifest, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	return nil
}

// InstallBackends creates the namespace and deploys the selected backends.
func (i *Installer) InstallBackends(ctx context.Context, backends []Backend, timeout time.Duration) error {
	if err := i.Apply(ctx, NamespaceManifest(i.Namespace, i.Instance)); err != nil {
		return err
	}
	for _, b := range backends {
		i.logf("  installing %s...\n", b.Name)
		if err := i.Apply(ctx, b.Manifest(i.Namespace, i.Instance)); err != nil {
			return fmt.Errorf("install %s: %w", b.Name, err)
		}
	}
	for _, b := range backends {
		if err := i.waitDeployment(ctx, i.Namespace, b.Name, timeout); err != nil {
			return err
		}
	}
	return nil
}

// waitDeployment waits for a Deployment to report Available.
//
// Readiness is asked of Kubernetes rather than assumed from a successful apply:
// an apply only means the object was accepted.
func (i *Installer) waitDeployment(ctx context.Context, namespace, name string, timeout time.Duration) error {
	_, err := i.kubectl(ctx, "", "-n", namespace, "wait",
		"--for=condition=Available", "deployment/"+name,
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err != nil {
		// Surface why it never became ready, not just that it did not.
		events, _ := i.kubectl(ctx, "", "-n", namespace, "get", "events",
			"--field-selector", "involvedObject.name="+name, "--sort-by=.lastTimestamp")
		return fmt.Errorf("%w: %s did not become ready within %s: %w\n%s",
			ErrInstallFailed, name, timeout, err, truncate(events, 600))
	}
	return nil
}

// InstallKnative applies Knative Serving and its networking layer.
//
// Cloud Run workloads execute on Knative (ADR-0005); this is what makes the
// Cloud Run adapter possible at all.
func (i *Installer) InstallKnative(ctx context.Context, timeout time.Duration) error {
	for _, url := range KnativeManifests() {
		i.logf("  applying %s\n", shortURL(url))
		if _, err := i.kubectl(ctx, "", "apply", "-f", url); err != nil {
			return fmt.Errorf("%w: apply %s: %w", ErrInstallFailed, url, err)
		}
	}

	// Kourier must be selected explicitly; Knative ships no default ingress.
	if _, err := i.kubectl(ctx, "", "patch", "configmap/config-network",
		"-n", "knative-serving", "--type", "merge",
		"-p", `{"data":{"ingress-class":"kourier.ingress.networking.knative.dev"}}`); err != nil {
		return fmt.Errorf("%w: select kourier ingress: %w", ErrInstallFailed, err)
	}

	// Publish the gateway and name services under a domain that resolves to
	// loopback without DNS egress (#86).
	if err := i.ConfigureIngress(ctx, DefaultDomain); err != nil {
		return err
	}

	for _, ns := range []string{"knative-serving", "kourier-system"} {
		i.logf("  waiting for %s...\n", ns)
		if _, err := i.kubectl(ctx, "", "-n", ns, "wait",
			"--for=condition=Available", "deployment", "--all",
			fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
			return fmt.Errorf("%w: %s did not become ready: %w", ErrInstallFailed, ns, err)
		}
	}
	return nil
}

// KnativeInstalled reports whether Knative Serving is already present, so
// installation is idempotent.
func (i *Installer) KnativeInstalled(ctx context.Context) bool {
	_, err := i.kubectl(ctx, "", "get", "namespace", "knative-serving")
	return err == nil
}

func shortURL(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Installer exposes its kubectl for callers that need a raw operation.
func (i *Installer) Kubectl(ctx context.Context, args ...string) (string, error) {
	return i.kubectl(ctx, "", args...)
}
