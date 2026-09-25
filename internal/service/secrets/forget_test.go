package secrets

import (
	"context"
	"strings"
	"testing"
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
		k := NewKubeStore(r, "default", "inst")
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
		if got, want := r.calls[len(r.calls)-1], "-n default delete secrets -l "+c.selector; got != want {
			t.Errorf("epoch %q: Forget ran %q, want %q", c.epoch, got, want)
		}
	}
	if err := NewKubeStore(&forgetRunner{}, "default", "").Forget(ctx); err == nil {
		t.Error("Forget without an instance selected every instance's Secrets")
	}
}
