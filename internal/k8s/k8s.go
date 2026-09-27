// Package k8s is the one way CloudBurrow's services and adapters reach the
// Kubernetes API: a kubectl Runner whose kubeconfig, context and namespace are
// fixed when it is made (docs/architecture.md §3, rule 2).
//
// It keeps the subprocess approach every caller used before it (#599): each
// method runs kubectl once, with the Runner's global flags first and the verb
// after, and returns what kubectl printed; PortForward alone leaves kubectl
// running and returns the Process. A failure is an *Error that keeps
// kubectl's own message and says, from the API server's status reason,
// whether the object was missing or the request was forbidden.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Ownership labels. Every object CloudBurrow creates carries OwnedLabel set
// to OwnedValue, so cleanup and deletes can select exactly what it made and
// never touch an object a developer created by hand.
const (
	OwnedLabel    = "cloudburrow.dev/owned"
	OwnedValue    = "true"
	OwnedSelector = OwnedLabel + "=" + OwnedValue
	// InstanceLabel scopes an object to one CloudBurrow instance.
	InstanceLabel = "cloudburrow.dev/instance"
	// ServiceLabel names the emulated service an object belongs to.
	ServiceLabel = "cloudburrow.dev/service"
)

// Invoker runs kubectl with args, feeding it stdin when that is not empty,
// and returns its stdout. When kubectl fails, the error carries its stderr.
// It is the seam tests replace, so every caller is testable without a cluster.
type Invoker interface {
	Run(ctx context.Context, stdin string, args ...string) (string, error)
}

// Subprocess is the real Invoker: the kubectl on PATH.
type Subprocess struct{}

// Run execs kubectl. A failure reads "kubectl: <exit status>: <stderr>".
func (Subprocess) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return out.String(), fmt.Errorf("kubectl: %w: %s", err, msg)
		}
		return out.String(), fmt.Errorf("kubectl: %w", err)
	}
	return out.String(), nil
}

// Process is a kubectl left running, such as a port-forward.
type Process interface {
	// Wait blocks until kubectl exits.
	Wait() error
	// Kill stops kubectl.
	Kill() error
}

// Starter is an Invoker that can also start kubectl without waiting for it
// to exit. Subprocess is one; a test Invoker that is not makes PortForward
// fail with ErrCannotStart.
type Starter interface {
	Start(stdout, stderr io.Writer, args ...string) (Process, error)
}

// ErrCannotStart means the Runner's Invoker is not a Starter.
var ErrCannotStart = errors.New("k8s: the invoker cannot start a long-running kubectl")

// Start starts kubectl and returns without waiting for it. It takes no
// context: the process lives until Kill, not until a caller's deadline.
func (Subprocess) Start(stdout, stderr io.Writer, args ...string) (Process, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return process{cmd}, nil
}

type process struct{ cmd *exec.Cmd }

func (p process) Wait() error { return p.cmd.Wait() }
func (p process) Kill() error { return p.cmd.Process.Kill() }

// Runner runs kubectl against one cluster, context and namespace.
type Runner struct {
	inv        Invoker
	kubeconfig string
	context    string
	namespace  string
}

// New returns a Runner over the real kubectl. An empty kubeconfig or context
// means kubectl's default; the namespace is where every call acts.
func New(kubeconfig, kubeContext, namespace string) *Runner {
	return NewWith(Subprocess{}, kubeconfig, kubeContext, namespace)
}

// NewWith returns a Runner over inv, for tests.
func NewWith(inv Invoker, kubeconfig, kubeContext, namespace string) *Runner {
	return &Runner{inv: inv, kubeconfig: kubeconfig, context: kubeContext, namespace: namespace}
}

// Namespace is the namespace every call acts in.
func (r *Runner) Namespace() string { return r.namespace }

// global is the fixed global flags, in the order
// [--kubeconfig K] [--context C] [-n NS], followed by args.
func (r *Runner) global(args ...string) []string {
	full := make([]string, 0, len(args)+6)
	if r.kubeconfig != "" {
		full = append(full, "--kubeconfig", r.kubeconfig)
	}
	if r.context != "" {
		full = append(full, "--context", r.context)
	}
	if r.namespace != "" {
		full = append(full, "-n", r.namespace)
	}
	return append(full, args...)
}

// run runs kubectl after the global flags and classifies a failure.
func (r *Runner) run(ctx context.Context, stdin string, args ...string) (string, error) {
	out, err := r.inv.Run(ctx, stdin, r.global(args...)...)
	if err != nil {
		return out, &Error{Reason: reasonOf(err.Error()), err: err}
	}
	return out, nil
}

// Get reads one object: kubectl get RESOURCE NAME -o OUTPUT.
func (r *Runner) Get(ctx context.Context, resource, name, output string) (string, error) {
	return r.run(ctx, "", "get", resource, name, "-o", output)
}

// List reads the objects matching a label selector:
// kubectl get RESOURCE -l SELECTOR -o OUTPUT.
func (r *Runner) List(ctx context.Context, resource, selector, output string) (string, error) {
	return r.run(ctx, "", "get", resource, "-l", selector, "-o", output)
}

// ApplyOptions selects how Apply applies.
type ApplyOptions struct {
	// ServerSide applies on the server, so kubectl records no
	// last-applied-configuration annotation copying the whole object.
	ServerSide bool
	// ForceConflicts takes ownership of fields another manager holds
	// instead of failing; server-side only.
	ForceConflicts bool
	// FieldManager names the manager a server-side apply records.
	FieldManager string
}

// Apply creates or updates the objects in manifest (JSON or YAML):
// kubectl apply [--server-side] [--force-conflicts] [--field-manager M] -f -.
func (r *Runner) Apply(ctx context.Context, manifest string, opts ApplyOptions) error {
	args := []string{"apply"}
	if opts.ServerSide {
		args = append(args, "--server-side")
	}
	if opts.ForceConflicts {
		args = append(args, "--force-conflicts")
	}
	if opts.FieldManager != "" {
		args = append(args, "--field-manager", opts.FieldManager)
	}
	_, err := r.run(ctx, manifest, append(args, "-f", "-")...)
	return err
}

// Create creates the object in manifest, failing if it exists:
// kubectl create -f -.
func (r *Runner) Create(ctx context.Context, manifest string) error {
	_, err := r.run(ctx, manifest, "create", "-f", "-")
	return err
}

// Replace replaces the object in manifest, failing if its resourceVersion is
// stale: kubectl replace -f -.
func (r *Runner) Replace(ctx context.Context, manifest string) error {
	_, err := r.run(ctx, manifest, "replace", "-f", "-")
	return err
}

// Patch applies a JSON merge patch:
// kubectl patch RESOURCE NAME --type merge -p PATCH.
func (r *Runner) Patch(ctx context.Context, resource, name, mergePatch string) error {
	_, err := r.run(ctx, "", "patch", resource, name, "--type", "merge", "-p", mergePatch)
	return err
}

// Delete removes one object: kubectl delete RESOURCE NAME
// [--ignore-not-found]. Without ignoreNotFound, a missing object is an error
// that IsNotFound reports.
func (r *Runner) Delete(ctx context.Context, resource, name string, ignoreNotFound bool) error {
	args := []string{"delete", resource, name}
	if ignoreNotFound {
		args = append(args, "--ignore-not-found")
	}
	_, err := r.run(ctx, "", args...)
	return err
}

// DeleteSelected removes every object matching a label selector:
// kubectl delete RESOURCE -l SELECTOR [--ignore-not-found].
func (r *Runner) DeleteSelected(ctx context.Context, resource, selector string, ignoreNotFound bool) error {
	args := []string{"delete", resource, "-l", selector}
	if ignoreNotFound {
		args = append(args, "--ignore-not-found")
	}
	_, err := r.run(ctx, "", args...)
	return err
}

// Do runs any other kubectl verb, args after the global flags, feeding it
// stdin when that is not empty: kubectl [globals] ARGS. It is for the verbs
// no method above names (rollout status, wait, get --raw, get events); a
// failure is classified as every other call's is.
func (r *Runner) Do(ctx context.Context, stdin string, args ...string) (string, error) {
	return r.run(ctx, stdin, args...)
}

// PortForward starts a tunnel and returns without waiting for it:
// kubectl [globals] port-forward --address ADDRESS RESOURCE PORTS, with
// kubectl's output written to stdout and stderr as it arrives. The tunnel
// runs until the caller kills it or kubectl exits; a failure to start is
// returned as the Invoker's error, unclassified, since no API server
// answered.
func (r *Runner) PortForward(address, resource, ports string, stdout, stderr io.Writer) (Process, error) {
	s, ok := r.inv.(Starter)
	if !ok {
		return nil, ErrCannotStart
	}
	return s.Start(stdout, stderr, r.global("port-forward", "--address", address, resource, ports)...)
}

// Reason is the Kubernetes API status reason a failure carried, or "" when
// kubectl failed before or without an answer from the API server.
type Reason string

// The reasons callers act on.
const (
	ReasonNotFound  Reason = "NotFound"
	ReasonForbidden Reason = "Forbidden"
)

// Sentinels for errors.Is; an *Error matches the one its Reason names.
var (
	ErrNotFound  = errors.New("kubernetes: not found")
	ErrForbidden = errors.New("kubernetes: forbidden")
)

// Error is a failed kubectl call. Its message is kubectl's, unchanged, so a
// caller can still quote the cluster's reason to its own caller.
type Error struct {
	Reason Reason
	err    error
}

func (e *Error) Error() string { return e.err.Error() }

// Unwrap returns what the Invoker returned.
func (e *Error) Unwrap() error { return e.err }

// Is matches ErrNotFound or ErrForbidden by Reason.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Reason == ReasonNotFound
	case ErrForbidden:
		return e.Reason == ReasonForbidden
	}
	return false
}

// IsNotFound reports whether err is the API server saying the object does not
// exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsForbidden reports whether err is the API server refusing the request
// (RBAC).
func IsForbidden(err error) bool { return errors.Is(err, ErrForbidden) }

// reasonOf reads the API server's status reason from kubectl's message,
// which kubectl prints as `Error from server (NotFound): secrets "x" not
// found`. Only the reason counts: kubectl also prints the words "not found"
// for a context or cluster missing from the kubeconfig, and taking that for
// an absent object would let a caller create over state it cannot see.
func reasonOf(msg string) Reason {
	for _, r := range []Reason{ReasonNotFound, ReasonForbidden} {
		if strings.Contains(msg, string(r)) {
			return r
		}
	}
	return ""
}
