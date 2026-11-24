package cluster

import (
	"context"
	"strings"
	"testing"
)

// schedulerRunner is a control plane whose kube-scheduler container is
// old until it is stopped, and then, after a poll, a new one.
type schedulerRunner struct {
	calls   []string
	stopped bool
	polls   int
}

func (r *schedulerRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	switch {
	case strings.Contains(cmd, "crictl stop"):
		r.stopped = true
	case strings.Contains(cmd, "crictl ps"):
		if !r.stopped {
			return "old\n", nil
		}
		if r.polls++; r.polls < 2 {
			return "", nil
		}
		return "new\n", nil
	}
	return "", nil
}

// TestCompletedRotationRestartsTheScheduler (#2100, main run 45382531465):
// completing a rotation stops the control plane's running scheduler, whose
// manifest is unchanged, so the kubelet starts it again with its re-issued
// kubeconfig, and waits for a new container to run; it never stops the new
// one.
func TestCompletedRotationRestartsTheScheduler(t *testing.T) {
	r := &schedulerRunner{}
	g := &GKE{Dir: t.TempDir(), Runner: r}
	if err := g.restartControlPlaneContainer(context.Background(), OwnerPrefix+"r", "kube-scheduler"); err != nil {
		t.Fatal(err)
	}
	var stops []string
	for _, c := range r.calls {
		if strings.Contains(c, "crictl stop") {
			stops = append(stops, c)
		}
	}
	if len(stops) != 1 || !strings.HasSuffix(stops[0], " old") || !strings.Contains(stops[0], OwnerPrefix+"r-control-plane") {
		t.Errorf("stops = %q, want the old scheduler container's alone", stops)
	}
	if !strings.Contains(r.calls[0], "--name ^kube-scheduler$") {
		t.Errorf("first call = %q, want the scheduler's container looked up by exact name", r.calls[0])
	}
}
