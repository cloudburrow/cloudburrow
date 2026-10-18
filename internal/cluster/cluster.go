// Package cluster owns the local Kubernetes cluster: create, discover, stop,
// start and delete, with an explicit kubeconfig.
//
// It is the only package besides internal/k8s that knows Kubernetes exists
// (docs/architecture.md §3). It reuses kind rather than implementing cluster
// management (ADR-0005).
//
// Every operation is scoped to a cluster CloudBurrow created. The developer's
// current kubecontext is never changed, and a cluster this package did not
// create is never modified or deleted.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Status describes what exists for an instance.
type Status int

const (
	// StatusAbsent means no cluster exists for this instance.
	StatusAbsent Status = iota
	// StatusStopped means the cluster exists but its nodes are not running.
	StatusStopped
	// StatusRunning means the cluster exists and its nodes are running.
	StatusRunning
)

func (s Status) String() string {
	switch s {
	case StatusAbsent:
		return "absent"
	case StatusStopped:
		return "stopped"
	case StatusRunning:
		return "running"
	default:
		return "unknown"
	}
}

// Errors callers are expected to distinguish.
var (
	// ErrDockerUnavailable means the container runtime is missing or not
	// responding. Reported only when an operation actually needs it.
	ErrDockerUnavailable = errors.New("docker is not available")
	// ErrNotOwned means the named cluster exists but was not created by
	// CloudBurrow, so it will not be touched.
	ErrNotOwned = errors.New("cluster is not owned by cloudburrow")
	// ErrAbsent means no cluster exists for this instance.
	ErrAbsent = errors.New("cluster does not exist")
)

// OwnerPrefix is required on every cluster name CloudBurrow manages. Anything
// without it is someone else's and is never modified.
const OwnerPrefix = "cloudburrow"

// Options configure a Cluster.
type Options struct {
	// Name is the kind cluster name. Must carry OwnerPrefix.
	Name string
	// NodeImage pins the Kubernetes version. Must be tagged or digested.
	NodeImage string
	// Kubeconfig is the explicit path written and used. Never the developer's
	// default file.
	Kubeconfig string
	// CLIVersion is recorded in the cluster's stamp by Reconcile (#601).
	CLIVersion string
	// Runner executes external commands. Nil uses the real one; tests inject.
	Runner Runner
	// LookPath resolves a binary on PATH. Nil uses exec.LookPath.
	//
	// Injectable because otherwise a unit test's behaviour depends on whether
	// the machine happens to have Docker installed — which is exactly how this
	// passed locally and on Linux CI while failing on a macOS runner.
	LookPath func(string) (string, error)
}

// Runner executes an external command. Injecting it keeps the ownership and
// error-mapping logic unit-testable without Docker or a cluster.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out strings.Builder
	var errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg != "" {
			return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// Cluster manages one instance-owned local Kubernetes cluster.
type Cluster struct {
	opts     Options
	runner   Runner
	lookPath func(string) (string, error)
	// restarted says Ensure started stopped nodes (#1610).
	restarted bool
}

// New validates options and returns a Cluster.
//
// The ownership prefix is enforced here rather than at the call site, so no
// code path can construct a Cluster pointed at something we do not own.
func New(opts Options) (*Cluster, error) {
	if opts.Name == "" {
		return nil, errors.New("cluster name must not be empty")
	}
	if !strings.HasPrefix(opts.Name, OwnerPrefix) {
		return nil, fmt.Errorf("%w: name %q lacks the %q prefix", ErrNotOwned, opts.Name, OwnerPrefix)
	}
	if opts.NodeImage == "" {
		return nil, errors.New("node image must be set")
	}
	if !strings.Contains(opts.NodeImage, ":") && !strings.Contains(opts.NodeImage, "@") {
		// A bare reference is a mutable target, which ADR-0005 forbids.
		return nil, fmt.Errorf("node image %q must carry an explicit tag or digest", opts.NodeImage)
	}
	if opts.Kubeconfig == "" {
		return nil, errors.New("kubeconfig path must be set")
	}
	r := opts.Runner
	if r == nil {
		r = execRunner{}
	}
	lp := opts.LookPath
	if lp == nil {
		lp = exec.LookPath
	}
	return &Cluster{opts: opts, runner: r, lookPath: lp}, nil
}

// Name returns the cluster name.
func (c *Cluster) Name() string { return c.opts.Name }

// KubeconfigPath returns the explicit kubeconfig path.
func (c *Cluster) KubeconfigPath() string { return c.opts.Kubeconfig }

// CheckRuntime reports whether the container runtime is usable.
//
// It is called only by operations that need Docker, so a user running a command
// that does not touch the cluster never sees a Docker error.
func (c *Cluster) CheckRuntime(ctx context.Context) error {
	if _, err := c.lookPath("docker"); err != nil {
		return fmt.Errorf("%w: docker was not found on PATH; install Docker Desktop (macOS) or Docker Engine (Linux)", ErrDockerUnavailable)
	}
	if _, err := c.runner.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("%w: the docker daemon is not responding; start Docker and retry: %w", ErrDockerUnavailable, err)
	}
	return nil
}

// Exists reports whether a cluster with this name exists.
func (c *Cluster) Exists(ctx context.Context) (bool, error) {
	out, err := c.runner.Run(ctx, "kind", "get", "clusters")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == c.opts.Name {
			return true, nil
		}
	}
	return false, nil
}

// nodes returns the node container names for this cluster.
func (c *Cluster) nodes(ctx context.Context) ([]string, error) {
	out, err := c.runner.Run(ctx, "kind", "get", "nodes", "--name", c.opts.Name)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// Status reports whether the cluster is absent, stopped or running.
func (c *Cluster) Status(ctx context.Context) (Status, error) {
	exists, err := c.Exists(ctx)
	if err != nil {
		return StatusAbsent, err
	}
	if !exists {
		return StatusAbsent, nil
	}
	nodes, err := c.nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return StatusStopped, err
	}
	out, err := c.runner.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", nodes[0])
	if err != nil {
		return StatusStopped, nil
	}
	if strings.TrimSpace(out) == "true" {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

// Create creates the cluster if absent, or starts it if it exists but is
// stopped. It is idempotent: a second call against a running cluster does
// nothing and returns nil.
func (c *Cluster) Create(ctx context.Context, configPath string) error {
	_, err := c.Ensure(ctx, configPath)
	return err
}

// Ensure is Create, and reports whether it created the cluster rather than
// finding one: only a cluster it created is known to run the pinned node
// image (#601).
func (c *Cluster) Ensure(ctx context.Context, configPath string) (created bool, err error) {
	if err := c.CheckRuntime(ctx); err != nil {
		return false, err
	}

	status, err := c.Status(ctx)
	if err != nil {
		return false, err
	}
	switch status {
	case StatusRunning:
		// Idempotent: refresh the kubeconfig in case it was removed, then stop.
		return false, c.ExportKubeconfig(ctx)
	case StatusStopped:
		if err := c.Start(ctx); err != nil {
			return false, err
		}
		c.restarted = true
		return false, c.ExportKubeconfig(ctx)
	}

	if err := os.MkdirAll(filepath.Dir(c.opts.Kubeconfig), 0o755); err != nil {
		return false, fmt.Errorf("create kubeconfig directory: %w", err)
	}

	args := []string{"create", "cluster",
		"--name", c.opts.Name,
		"--image", c.opts.NodeImage,
		"--kubeconfig", c.opts.Kubeconfig,
	}
	// No --wait: kind returns once the API server is up, and the node
	// becomes Ready (its CNI up) while the images load and Knative's
	// manifests apply, which need only the API and the node's container.
	// WaitNodesReady, which the cluster component runs in the background
	// and its NodesReady joins, keeps what --wait 180s guaranteed (#1588).
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	if _, err := c.runner.Run(ctx, "kind", args...); err != nil {
		// A failed create can leave a partial cluster behind. Clean up only
		// what we were creating, then report the original cause.
		if exists, checkErr := c.Exists(ctx); checkErr == nil && exists {
			_, _ = c.runner.Run(ctx, "kind", "delete", "cluster", "--name", c.opts.Name)
		}
		return false, fmt.Errorf("create cluster %s: %w", c.opts.Name, err)
	}

	return true, nil
}

// ExportKubeconfig writes the kubeconfig for this cluster to the explicit path.
//
// It passes --kubeconfig so kind writes only that file. The developer's default
// kubeconfig and current context are never touched.
func (c *Cluster) ExportKubeconfig(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(c.opts.Kubeconfig), 0o755); err != nil {
		return fmt.Errorf("create kubeconfig directory: %w", err)
	}
	_, err := c.runner.Run(ctx, "kind", "export", "kubeconfig",
		"--name", c.opts.Name, "--kubeconfig", c.opts.Kubeconfig)
	if err != nil {
		return fmt.Errorf("export kubeconfig for %s: %w", c.opts.Name, err)
	}
	return nil
}

// Stop stops the cluster's node containers without destroying the cluster.
//
// Data in volumes survives, which is what distinguishes stop from delete.
func (c *Cluster) Stop(ctx context.Context) error {
	if err := c.CheckRuntime(ctx); err != nil {
		return err
	}
	exists, err := c.Exists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrAbsent, c.opts.Name)
	}
	nodes, err := c.nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if _, err := c.runner.Run(ctx, "docker", "stop", n); err != nil {
			return fmt.Errorf("stop node %s: %w", n, err)
		}
	}
	return nil
}

// Start starts a stopped cluster's node containers.
func (c *Cluster) Start(ctx context.Context) error {
	if err := c.CheckRuntime(ctx); err != nil {
		return err
	}
	nodes, err := c.nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("%w: %s", ErrAbsent, c.opts.Name)
	}
	for _, n := range nodes {
		if _, err := c.runner.Run(ctx, "docker", "start", n); err != nil {
			return fmt.Errorf("start node %s: %w", n, err)
		}
	}
	return nil
}

// Delete destroys the cluster and removes its kubeconfig.
//
// It refuses any name lacking the ownership prefix — enforced again here rather
// than trusting the constructor, because this is the destructive path.
func (c *Cluster) Delete(ctx context.Context) error {
	if !strings.HasPrefix(c.opts.Name, OwnerPrefix) {
		return fmt.Errorf("%w: refusing to delete %q", ErrNotOwned, c.opts.Name)
	}
	if err := c.CheckRuntime(ctx); err != nil {
		return err
	}
	exists, err := c.Exists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrAbsent, c.opts.Name)
	}
	if _, err := c.runner.Run(ctx, "kind", "delete", "cluster", "--name", c.opts.Name); err != nil {
		return fmt.Errorf("delete cluster %s: %w", c.opts.Name, err)
	}
	// Remove only our own kubeconfig file.
	if err := os.Remove(c.opts.Kubeconfig); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove kubeconfig %s: %w", c.opts.Kubeconfig, err)
	}
	return nil
}

// WaitReady polls until the Kubernetes API answers, or the deadline elapses.
//
// This is bounded polling rather than an injected clock: the API server is an
// external process whose clock we cannot advance (ADR-0005, architecture §9).
func (c *Cluster) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
			"get", "--raw", "/readyz")
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("cluster %s did not become ready within %s: %w", c.opts.Name, timeout, last)
}

// NodesReadyTimeout bounds WaitNodesReady: what `kind create --wait` was
// given before the wait moved out of the create (#1588).
const NodesReadyTimeout = 180 * time.Second

// WaitNodesReady polls until the cluster lists at least one node and every
// node's Ready condition is True, or the deadline elapses: what `kind
// create cluster --wait` waited for, done after the create so the work
// that needs only the API overlaps it (#1588).
func (c *Cluster) WaitNodesReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := errors.New("no node reported")
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
			"get", "nodes", "-o", `jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
		if err == nil {
			last = nodesReady(out)
			if last == nil {
				return nil
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("cluster %s: nodes not Ready within %s: %w", c.opts.Name, timeout, last)
}

// nodesReady reads WaitNodesReady's name=status lines: nil when there is at
// least one and every status is True.
func nodesReady(out string) error {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n++
		name, status, _ := strings.Cut(line, "=")
		if status != "True" {
			if status == "" {
				status = "unknown"
			}
			return fmt.Errorf("node %s Ready is %s", name, status)
		}
	}
	if n == 0 {
		return errors.New("no node reported")
	}
	return nil
}

// Restarted reports whether Ensure started the nodes of a stopped cluster:
// a warm start, whose pods the kubelet restarts before the Service network
// works (#1610).
func (c *Cluster) Restarted() bool { return c.restarted }

// ServiceNetworkTimeout bounds WaitServiceNetwork.
const ServiceNetworkTimeout = 2 * time.Minute

// WaitServiceNetwork polls until, from inside every node, the kubernetes
// Service's ClusterIP answers /readyz with ok: kube-proxy has written the
// Service rules again and the API server serves behind them.
//
// After a stopped node starts, the kubelet starts every pod at once, while
// kube-proxy is still writing those rules. Pods that read the API through
// the kubernetes Service at startup then fail: Knative's exit and wait out
// the kubelet's restart backoff, up to 45 s, and CoreDNS's informers wait
// out their own retry backoff, some 40 s (#1610). This is what has to be
// true before they are started again.
func (c *Cluster) WaitServiceNetwork(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := errors.New("not probed")
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if last = c.serviceNetwork(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("cluster %s: the Service network did not answer within %s: %w", c.opts.Name, timeout, last)
}

// serviceNetwork is one probe of WaitServiceNetwork.
func (c *Cluster) serviceNetwork(ctx context.Context) error {
	out, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
		"-n", "default", "get", "service", "kubernetes", "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return err
	}
	api := strings.TrimSpace(out)
	if api == "" {
		return errors.New("the kubernetes Service has no ClusterIP")
	}
	nodes, err := c.nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("no node")
	}
	for _, n := range nodes {
		out, err := c.runner.Run(ctx, "docker", "exec", n, "curl", "-sk", "--max-time", "1",
			"https://"+net.JoinHostPort(api, "443")+"/readyz")
		if err != nil || strings.TrimSpace(out) != "ok" {
			return fmt.Errorf("node %s: the kubernetes Service %s is not answering: %q %v", n, api, truncateOut(out), err)
		}
	}
	return nil
}

// SingleLeader restarts the controller manager and the scheduler of a
// cluster created before #1610 without leader election, as new clusters
// are created (kubeadmPatches): their static pod manifests are rewritten in
// place on each node, and the kubelet restarts them. It reports whether it
// changed anything, and is a no-op on a cluster that has it already.
func (c *Cluster) SingleLeader(ctx context.Context) (bool, error) {
	nodes, err := c.nodes(ctx)
	if err != nil {
		return false, err
	}
	changed := false
	for _, n := range nodes {
		// Rewritten beside, then renamed over, so the kubelet never reads
		// half a manifest; and only a file that names the flag.
		out, err := c.runner.Run(ctx, "docker", "exec", n, "sh", "-c", singleLeaderScript)
		if err != nil {
			return changed, fmt.Errorf("node %s: turn leader election off: %w", n, err)
		}
		if strings.Contains(out, "changed") {
			changed = true
		}
	}
	return changed, nil
}

// singleLeaderScript is what SingleLeader runs on a node.
const singleLeaderScript = `set -e
for f in kube-controller-manager kube-scheduler; do
  m=/etc/kubernetes/manifests/$f.yaml
  [ -f "$m" ] || continue
  grep -q -- '--leader-elect=true' "$m" || continue
  sed 's/--leader-elect=true/--leader-elect=false/' "$m" > /etc/kubernetes/$f.yaml.cloudburrow
  mv /etc/kubernetes/$f.yaml.cloudburrow "$m"
  echo changed $f
done`

// truncateOut shortens command output quoted in an error.
func truncateOut(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// ServerVersion returns the Kubernetes server version reported by the cluster.
func (c *Cluster) ServerVersion(ctx context.Context) (string, error) {
	out, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
		"version", "-o", "json")
	if err != nil {
		return "", err
	}
	var v struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parse kubectl version output: %w", err)
	}
	if v.ServerVersion.GitVersion == "" {
		return "", errors.New("kubectl reported no server version; the cluster may not be reachable")
	}
	return v.ServerVersion.GitVersion, nil
}

// DeleteNamespace deletes a namespace CloudBurrow manages.
//
// It refuses namespaces that are obviously not ours. Deleting a namespace is
// destructive and irreversible, so the guard is explicit rather than assumed
// from the caller.
func (c *Cluster) DeleteNamespace(ctx context.Context, namespace string) error {
	switch namespace {
	case "", "default", "kube-system", "kube-public", "kube-node-lease":
		return fmt.Errorf("%w: refusing to delete namespace %q", ErrNotOwned, namespace)
	}
	// Verify the namespace carries our ownership label before removing it.
	out, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
		"get", "namespace", namespace, "-o", "jsonpath={.metadata.labels.cloudburrow\\.dev/owned}")
	if err != nil {
		return fmt.Errorf("inspect namespace %s: %w", namespace, err)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%w: namespace %q does not carry cloudburrow.dev/owned=true", ErrNotOwned, namespace)
	}
	if _, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
		"delete", "namespace", namespace, "--wait=true"); err != nil {
		return fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	return nil
}

// DeleteOwnedSecrets removes Secret Manager's Kubernetes Secrets from a
// namespace and reports how many were deleted.
//
// Those objects live in the workload namespace rather than the managed one,
// because a secretKeyRef cannot cross namespaces — so deleting the managed
// namespace leaves them behind. They are selected purely by ownership label,
// never by name and never by namespace, so this can only ever remove
// something CloudBurrow created.
func (c *Cluster) DeleteOwnedSecrets(ctx context.Context, namespace string) (int, error) {
	if namespace == "" {
		return 0, fmt.Errorf("%w: namespace must not be empty", ErrNotOwned)
	}
	const selector = "cloudburrow.dev/owned=true,cloudburrow.dev/service=secretmanager"

	out, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
		"-n", namespace, "get", "secret", "-l", selector,
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return 0, fmt.Errorf("list owned secrets in %s: %w", namespace, err)
	}
	names := strings.Fields(strings.TrimSpace(out))
	if len(names) == 0 {
		return 0, nil
	}

	if _, err := c.runner.Run(ctx, "kubectl", "--kubeconfig", c.opts.Kubeconfig,
		"-n", namespace, "delete", "secret", "-l", selector, "--ignore-not-found"); err != nil {
		return 0, fmt.Errorf("delete owned secrets in %s: %w", namespace, err)
	}
	return len(names), nil
}
