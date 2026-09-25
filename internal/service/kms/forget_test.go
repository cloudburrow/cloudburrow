package kms

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// argsRunner records each kubectl call and its stdin.
type argsRunner struct {
	calls []string
	stdin []string
	err   error
}

func (r *argsRunner) Run(_ context.Context, stdin string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	r.stdin = append(r.stdin, stdin)
	return "", r.err
}

// An ephemeral run labels its writes with its epoch and forgets every other
// KMS Secret of the instance, labelled or not; a persistent run forgets only
// what an ephemeral run wrote (#481).
func TestForgetFollowsTheMode(t *testing.T) {
	r := &argsRunner{}
	k := NewKubeStore(r, "cloudburrow", "inst")
	k.SetEpoch("e1")
	if err := k.Put("kms/ring/x", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.stdin[0], `"cloudburrow.dev/kms-epoch":"e1"`) {
		t.Errorf("an ephemeral write is not labelled with its epoch: %s", r.stdin[0])
	}
	if err := k.Forget(); err != nil {
		t.Fatal(err)
	}
	want := "-n cloudburrow delete secrets -l cloudburrow.dev/service=kms,cloudburrow.dev/instance=inst,cloudburrow.dev/kms-epoch!=e1"
	if got := r.calls[len(r.calls)-1]; got != want {
		t.Errorf("ephemeral Forget ran %q, want %q", got, want)
	}

	r = &argsRunner{}
	k = NewKubeStore(r, "cloudburrow", "inst")
	if err := k.Put("kms/ring/x", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.stdin[0], "kms-epoch") {
		t.Errorf("a persistent write carries an epoch: %s", r.stdin[0])
	}
	if err := k.Forget(); err != nil {
		t.Fatal(err)
	}
	want = "-n cloudburrow delete secrets -l cloudburrow.dev/service=kms,cloudburrow.dev/instance=inst,cloudburrow.dev/kms-epoch"
	if got := r.calls[len(r.calls)-1]; got != want {
		t.Errorf("persistent Forget ran %q, want %q", got, want)
	}
}

// A failed delete is an error: serving an earlier run's keys in an ephemeral
// instance would be the silent failure #481 is about.
func TestForgetReportsAFailure(t *testing.T) {
	k := NewKubeStore(&argsRunner{err: errors.New("connection refused")}, "cloudburrow", "inst")
	k.SetEpoch("e1")
	if err := k.Forget(); err == nil {
		t.Fatal("Forget over an unreachable API server reported success")
	}
}
