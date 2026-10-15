package kms

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
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
	k := NewKubeStore(k8s.NewWith(r, "", "", "cloudburrow"), "inst")
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
	want := "-n cloudburrow delete --raw /api/v1/namespaces/cloudburrow/secrets?labelSelector=" +
		url.QueryEscape("cloudburrow.dev/service=kms,cloudburrow.dev/instance=inst,cloudburrow.dev/kms-epoch!=e1")
	if got := r.calls[len(r.calls)-1]; got != want {
		t.Errorf("ephemeral Forget ran %q, want %q", got, want)
	}

	r = &argsRunner{}
	k = NewKubeStore(k8s.NewWith(r, "", "", "cloudburrow"), "inst")
	if err := k.Put("kms/ring/x", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.stdin[0], "kms-epoch") {
		t.Errorf("a persistent write carries an epoch: %s", r.stdin[0])
	}
	if err := k.Forget(); err != nil {
		t.Fatal(err)
	}
	want = "-n cloudburrow delete --raw /api/v1/namespaces/cloudburrow/secrets?labelSelector=" +
		url.QueryEscape("cloudburrow.dev/service=kms,cloudburrow.dev/instance=inst,cloudburrow.dev/kms-epoch")
	if got := r.calls[len(r.calls)-1]; got != want {
		t.Errorf("persistent Forget ran %q, want %q", got, want)
	}
}

// A failed delete is an error: serving an earlier run's keys in an ephemeral
// instance would be the silent failure #481 is about.
func TestForgetReportsAFailure(t *testing.T) {
	k := NewKubeStore(k8s.NewWith(&argsRunner{err: errors.New("connection refused")}, "", "", "cloudburrow"), "inst")
	k.SetEpoch("e1")
	if err := k.Forget(); err == nil {
		t.Fatal("Forget over an unreachable API server reported success")
	}
}

// DeleteAll, which /admin/reset runs, is one DeleteCollection of the
// instance's KMS Secrets, so its time does not grow with how many earlier
// runs left: one DELETE per Secret outlasted its bound with a few hundred
// (TestKMSResources).
func TestDeleteAllIsOneDeleteCollection(t *testing.T) {
	r := &argsRunner{}
	k := NewKubeStore(k8s.NewWith(r, "", "", "cloudburrow"), "inst")
	if err := k.DeleteAll(); err != nil {
		t.Fatal(err)
	}
	want := "-n cloudburrow delete --raw /api/v1/namespaces/cloudburrow/secrets?labelSelector=" +
		url.QueryEscape("cloudburrow.dev/service=kms,cloudburrow.dev/instance=inst")
	if len(r.calls) != 1 || r.calls[0] != want {
		t.Errorf("DeleteAll ran %q, want only %q", r.calls, want)
	}
	k = NewKubeStore(k8s.NewWith(&argsRunner{err: errors.New("connection refused")}, "", "", "cloudburrow"), "inst")
	if err := k.DeleteAll(); err == nil {
		t.Error("DeleteAll over an unreachable API server reported success")
	}
}
