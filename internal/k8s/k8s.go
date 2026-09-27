// Package k8s is the one way CloudBurrow's services, adapters and CLI reach the
// Kubernetes API: a kubectl Runner whose kubeconfig, context and namespace are
// fixed when it is made (docs/architecture.md §3, rule 2).
//
// It keeps the subprocess approach every caller used before it (#599): each
// method runs kubectl once, with the Runner's global flags first and the verb
// after, and returns what kubectl printed. Three are for what a string
// cannot carry: PortForward leaves kubectl running and returns the Process,
// Stream returns kubectl's output as it is written, and Pipe connects kubectl
// to the caller's reader and writers. A failure is an *Error that keeps
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
type Subprocess struct {
	// Env, when not nil, is kubectl's whole environment instead of this
	// process's; `cloudburrow logs` uses it to leave KUBECONFIG out.
	Env []string
}

// command is kubectl with args and the Subprocess's environment, ended
// with ctx.
func (s Subprocess) command(ctx context.Context, args []string) *exec.Cmd {
	return s.withEnv(exec.CommandContext(ctx, "kubectl", args...))
}

func (s Subprocess) withEnv(cmd *exec.Cmd) *exec.Cmd {
	if s.Env != nil {
		cmd.Env = s.Env
	}
	return cmd
}

// Run execs kubectl. A failure is a *RunError, which reads
// "kubectl: <exit status>: <stderr>".
func (s Subprocess) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := s.command(ctx, args)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), &RunError{Err: err, Stderr: strings.TrimSpace(errOut.String())}
	}
	return out.String(), nil
}

// RunError is a kubectl subprocess that failed: Err is what exec returned
// (the exit status, or why kubectl could not start) and Stderr is what
// kubectl printed, trimmed.
type RunError struct {
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("kubectl: %v: %s", e.Err, e.Stderr)
	}
	return fmt.Sprintf("kubectl: %v", e.Err)
}

// Unwrap returns what exec returned.
func (e *RunError) Unwrap() error { return e.Err }

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
func (s Subprocess) Start(stdout, stderr io.Writer, args ...string) (Process, error) {
	cmd := s.withEnv(exec.Command("kubectl", args...))
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return process{cmd}, nil
}

// Streamer is an Invoker that can also connect kubectl to the caller's
// readers and writers instead of strings, for output too large or too
// long-lived to hold: an exec that pipes a database dump, or a log follower.
// Subprocess is one; a test Invoker that is not makes Pipe and Stream fail
// with ErrCannotStream.
type Streamer interface {
	// Pipe runs kubectl to completion with its standard streams connected
	// to stdin (which may be nil), stdout and stderr.
	Pipe(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error
	// Stream starts kubectl and returns its stdout as kubectl writes it,
	// and wait, which reaps kubectl once the stream is done with. kubectl
	// ends when ctx does.
	Stream(ctx context.Context, args ...string) (stdout io.ReadCloser, wait func() error, err error)
}

// ErrCannotStream means the Runner's Invoker is not a Streamer.
var ErrCannotStream = errors.New("k8s: the invoker cannot stream kubectl's input or output")

// Pipe runs kubectl with the caller's streams. A failure is exec's error,
// unwrapped: what kubectl printed went to stderr.
func (s Subprocess) Pipe(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := s.command(ctx, args)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// Stream starts kubectl with its stdout on a pipe.
func (s Subprocess) Stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	cmd := s.command(ctx, args)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
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

// Pipe runs kubectl [globals] ARGS to completion with its standard streams
// connected to stdin (which may be nil), stdout and stderr, as an exec that
// pipes a database dump in or out needs. The failure is the Invoker's error,
// unclassified: kubectl's message went to stderr, not into the error.
func (r *Runner) Pipe(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	s, ok := r.inv.(Streamer)
	if !ok {
		return ErrCannotStream
	}
	return s.Pipe(ctx, stdin, stdout, stderr, r.global(args...)...)
}

// Stream starts kubectl [globals] ARGS and returns its stdout as kubectl
// writes it, for a follower such as logs --follow, and wait, which the
// caller calls once it has closed or drained stdout. kubectl ends when ctx
// does. A failure to start is the Invoker's error, unclassified.
func (r *Runner) Stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	s, ok := r.inv.(Streamer)
	if !ok {
		return nil, nil, ErrCannotStream
	}
	return s.Stream(ctx, r.global(args...)...)
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
