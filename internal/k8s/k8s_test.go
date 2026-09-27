package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recorder records each call and answers with err.
type recorder struct {
	calls []string
	stdin []string
	err   error
}

func (r *recorder) Run(_ context.Context, stdin string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	r.stdin = append(r.stdin, stdin)
	return "out", r.err
}

// Every verb gets the fixed global flags first, then its own arguments in
// the order kubectl documents them.
func TestTheRunnerBuildsEachVerb(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	r := NewWith(rec, "/k/config", "kind-x", "ns")
	global := "--kubeconfig /k/config --context kind-x -n ns "
	for _, c := range []struct {
		call func() error
		want string
	}{
		{func() error { _, err := r.Get(ctx, "secret", "s", "json"); return err }, "get secret s -o json"},
		{func() error { _, err := r.List(ctx, "ksvc", OwnedSelector, "json"); return err }, "get ksvc -l cloudburrow.dev/owned=true -o json"},
		{func() error { return r.Apply(ctx, "m", ApplyOptions{}) }, "apply -f -"},
		{func() error {
			return r.Apply(ctx, "m", ApplyOptions{ServerSide: true, ForceConflicts: true, FieldManager: "fm"})
		}, "apply --server-side --force-conflicts --field-manager fm -f -"},
		{func() error { return r.Create(ctx, "m") }, "create -f -"},
		{func() error { return r.Replace(ctx, "m") }, "replace -f -"},
		{func() error { return r.Patch(ctx, "jobs.batch", "j", `{"a":1}`) }, `patch jobs.batch j --type merge -p {"a":1}`},
		{func() error { return r.Delete(ctx, "secret", "s", true) }, "delete secret s --ignore-not-found"},
		{func() error { return r.Delete(ctx, "ksvc", "s", false) }, "delete ksvc s"},
		{func() error { return r.DeleteSelected(ctx, "pods", "a=b", true) }, "delete pods -l a=b --ignore-not-found"},
		{func() error { return r.DeleteSelected(ctx, "secrets", "a=b", false) }, "delete secrets -l a=b"},
	} {
		if err := c.call(); err != nil {
			t.Fatal(err)
		}
		if got, want := rec.calls[len(rec.calls)-1], global+c.want; got != want {
			t.Errorf("ran %q, want %q", got, want)
		}
	}
	if rec.stdin[2] != "m" {
		t.Errorf("apply was not given the manifest on stdin: %q", rec.stdin[2])
	}
	if r.Namespace() != "ns" {
		t.Errorf("Namespace() = %q", r.Namespace())
	}
}

// An empty kubeconfig or context is kubectl's default, not a flag with an
// empty value.
func TestEmptyGlobalsAreLeftToKubectl(t *testing.T) {
	rec := &recorder{}
	if _, err := NewWith(rec, "", "", "ns").Get(context.Background(), "secret", "s", "json"); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.calls[0], "-n ns get secret s -o json"; got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
}

// Only the API server's status reason makes a failure NotFound or Forbidden.
// A missing context or kubeconfig is a broken instance, and taking it for an
// absent object would let a caller create over state it cannot see.
func TestFailuresAreClassifiedByTheServersReason(t *testing.T) {
	for msg, want := range map[string]Reason{
		`kubectl: exit status 1: Error from server (NotFound): secrets "x" not found`:                                   ReasonNotFound,
		`kubectl: exit status 1: Error from server (Forbidden): secrets "x" is forbidden: User "u" cannot get`:          ReasonForbidden,
		"kubectl: exit status 1: Unable to connect to the server: dial tcp 127.0.0.1:6443: connect: connection refused": "",
		`kubectl: exit status 1: error: stat /nope/kubeconfig: no such file or directory`:                               "",
		`kubectl: exit status 1: error: context "missing" not found`:                                                    "",
		"the server could not find the requested resource":                                                              "",
	} {
		inner := errors.New(msg)
		_, err := NewWith(&recorder{err: inner}, "", "", "ns").Get(context.Background(), "secret", "x", "json")
		var kerr *Error
		if !errors.As(err, &kerr) {
			t.Fatalf("%q: error is %T, want *Error", msg, err)
		}
		if kerr.Reason != want {
			t.Errorf("%q: reason %q, want %q", msg, kerr.Reason, want)
		}
		if IsNotFound(err) != (want == ReasonNotFound) || IsForbidden(err) != (want == ReasonForbidden) {
			t.Errorf("%q: IsNotFound %v, IsForbidden %v", msg, IsNotFound(err), IsForbidden(err))
		}
		// kubectl's message reaches the caller unchanged, and so does the
		// Invoker's error.
		if err.Error() != msg || !errors.Is(err, inner) {
			t.Errorf("%q: error became %q", msg, err)
		}
	}
}

func TestSuccessIsNoError(t *testing.T) {
	out, err := NewWith(&recorder{}, "", "", "ns").Get(context.Background(), "secret", "x", "json")
	if err != nil || out != "out" {
		t.Errorf("Get = %q, %v", out, err)
	}
}
