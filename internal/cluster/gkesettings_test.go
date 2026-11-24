package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nodesRunner records every command, and lists two node containers.
type nodesRunner struct{ calls []string }

func (r *nodesRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	if strings.HasPrefix(cmd, "docker ps ") {
		return OwnerPrefix + "n-control-plane\n" + OwnerPrefix + "n-default-pool-1\n", nil
	}
	return "", nil
}

// TestSetNetworkPolicyFlushesConnectionTracking (main run 45422957668):
// once kindnet runs with the change, every node's connection tracking is
// flushed, so a connection kept alive across it (the API server's service
// proxy's) is checked against the policies like a new one, as on GKE's
// re-created nodes.
func TestSetNetworkPolicyFlushesConnectionTracking(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := &nodesRunner{}
		g := &GKE{Dir: t.TempDir(), Runner: r}
		kind := OwnerPrefix + "n"
		if err := os.MkdirAll(filepath.Join(g.Dir, kind), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNetworkPolicy(context.Background(), kind, on); err != nil {
			t.Fatal(err)
		}
		rollout, flushes := -1, map[string]int{}
		for i, c := range r.calls {
			if strings.Contains(c, "rollout status daemonset kindnet") {
				rollout = i
			}
			if strings.HasSuffix(c, " conntrack -F") {
				flushes[strings.Fields(c)[2]] = i
			}
		}
		for _, n := range []string{kind + "-control-plane", kind + "-default-pool-1"} {
			i, ok := flushes[n]
			if !ok || i < rollout || rollout < 0 {
				t.Errorf("enabled=%v: node %s's connection tracking not flushed after kindnet rolled out: %q", on, n, r.calls)
			}
		}
	}
}
