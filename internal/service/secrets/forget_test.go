package secrets

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// forgetRunner records calls; a "get" finds nothing, so a write creates.
type forgetRunner struct {
	calls []string
	stdin []string
}

func (r *forgetRunner) Run(_ context.Context, stdin string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	r.stdin = append(r.stdin, stdin)
	if len(args) > 2 && args[2] == "get" {
		if strings.Contains(strings.Join(args, " "), " -l ") {
			return `{"items":[]}`, nil
		}
		return "", errNotFoundForTest
	}
	return "", nil
}

type notFound struct{}

func (notFound) Error() string { return `Error from server (NotFound): secrets "x" not found` }

var errNotFoundForTest = notFound{}

// An ephemeral run labels its writes with its epoch and forgets every other
// Secret Manager Secret of the instance; a persistent run forgets only what an
// ephemeral run wrote (#483).
func TestForgetFollowsTheMode(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ epoch, labelled, selector string }{
		{"e1", `"cloudburrow.dev/epoch":"e1"`, "cloudburrow.dev/service=secretmanager,cloudburrow.dev/instance=inst,cloudburrow.dev/epoch!=e1"},
		{"", "", "cloudburrow.dev/service=secretmanager,cloudburrow.dev/instance=inst,cloudburrow.dev/epoch"},
	} {
		r := &forgetRunner{}
		k := NewKubeStore(k8s.NewWith(r, "", "", "default"), "inst")
		k.SetEpoch(c.epoch)
		if err := k.Put("secret/p/s", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		applied := strings.Join(r.stdin, "")
		if c.labelled != "" && !strings.Contains(applied, c.labelled) {
			t.Errorf("epoch %q: the write is not labelled: %s", c.epoch, applied)
		}
		if c.labelled == "" && strings.Contains(applied, "cloudburrow.dev/epoch") {
			t.Errorf("a persistent write carries an epoch: %s", applied)
		}
		if err := k.Forget(ctx); err != nil {
			t.Fatal(err)
		}
		// One DeleteCollection, which the API server carries out itself, not
		// `kubectl delete -l`, which sends a DELETE per Secret (#1232).
		if got, want := r.calls[len(r.calls)-1], "-n default delete --raw /api/v1/namespaces/default/secrets?labelSelector="+url.QueryEscape(c.selector); got != want {
			t.Errorf("epoch %q: Forget ran %q, want %q", c.epoch, got, want)
		}
	}
	if err := NewKubeStore(k8s.NewWith(&forgetRunner{}, "", "", "default"), "").Forget(ctx); err == nil {
		t.Error("Forget without an instance selected every instance's Secrets")
	}
}

// Store.Reset over Kubernetes, which /admin/reset runs, is one
// DeleteCollection of every Secret Manager Secret List reads, not a delete
// per key, each a read and a rewrite of its Secret, under the store's lock
// (#1232).
func TestResetIsOneDeleteCollection(t *testing.T) {
	r := &forgetRunner{}
	st := NewStore(NewKubeStore(k8s.NewWith(r, "", "", "default"), "inst"))
	if err := st.Reset(); err != nil {
		t.Fatal(err)
	}
	want := "-n default delete --raw /api/v1/namespaces/default/secrets?labelSelector=" +
		url.QueryEscape("cloudburrow.dev/owned=true,cloudburrow.dev/service=secretmanager")
	if len(r.calls) != 1 || r.calls[0] != want {
		t.Errorf("Reset ran %q, want only %q", r.calls, want)
	}
}
