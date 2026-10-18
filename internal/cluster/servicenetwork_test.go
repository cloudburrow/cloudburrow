package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A new cluster's controller manager and scheduler run without leader
// election, and the controller manager keeps the flag kind gives it (#1610).
func TestRenderConfigTurnsLeaderElectionOff(t *testing.T) {
	t.Parallel()
	got := RenderConfig(nil)
	for _, want := range []string{
		"kubeadmConfigPatches:",
		"apiVersion: kubeadm.k8s.io/v1beta4",
		"kind: ClusterConfiguration",
		"controllerManager:\n      extraArgs:\n        - name: enable-hostpath-provisioner\n          value: \"true\"\n        - name: leader-elect\n          value: \"false\"",
		"scheduler:\n      extraArgs:\n        - name: leader-elect\n          value: \"false\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config is missing %q:\n%s", want, got)
		}
	}
}

// WaitServiceNetwork polls until the kubernetes Service answers from the
// node, through its ClusterIP.
func TestWaitServiceNetwork(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	probes := 0
	r := &funcRunner{fn: func(cmd string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(cmd, "kind get nodes"):
			return "cloudburrow-dev-control-plane\n", nil
		case strings.Contains(cmd, "get service kubernetes"):
			if !strings.Contains(cmd, "--kubeconfig ") {
				t.Errorf("kubectl without an explicit --kubeconfig: %s", cmd)
			}
			return "10.96.0.1", nil
		case strings.HasPrefix(cmd, "docker exec cloudburrow-dev-control-plane curl -sk --max-time 1 https://10.96.0.1:443/readyz"):
			probes++
			if probes < 3 {
				return "", errors.New("connection refused")
			}
			return "ok", nil
		}
		t.Errorf("unexpected call %s", cmd)
		return "", nil
	}}
	c := newTestCluster(t, r, "cloudburrow-dev")
	if err := c.WaitServiceNetwork(context.Background(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if probes != 3 {
		t.Errorf("returned after %d probes, want 3", probes)
	}
}

// A Service network that never answers is reported with the last reason.
func TestWaitServiceNetworkTimesOut(t *testing.T) {
	t.Parallel()
	r := &funcRunner{fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "kind get nodes"):
			return "cloudburrow-dev-control-plane\n", nil
		case strings.Contains(cmd, "get service kubernetes"):
			return "10.96.0.1", nil
		}
		return "", errors.New("connection refused")
	}}
	c := newTestCluster(t, r, "cloudburrow-dev")
	err := c.WaitServiceNetwork(context.Background(), 600*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "the kubernetes Service 10.96.0.1 is not answering") {
		t.Fatalf("WaitServiceNetwork = %v", err)
	}
}

// An existing cluster's static manifests are rewritten on the node, and
// the change is reported once.
func TestSingleLeader(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get nodes", "cloudburrow-dev-control-plane\n", nil).
		script("docker exec cloudburrow-dev-control-plane sh -c", "changed kube-controller-manager\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")
	changed, err := c.SingleLeader(context.Background())
	if err != nil || !changed {
		t.Fatalf("SingleLeader = %v, %v", changed, err)
	}
	if !r.ran("--leader-elect=false") || !r.ran("/etc/kubernetes/manifests/") {
		t.Errorf("did not rewrite the manifests: %v", r.calls)
	}
	r.script("docker exec cloudburrow-dev-control-plane sh -c", "", nil)
	if changed, err := c.SingleLeader(context.Background()); err != nil || changed {
		t.Errorf("SingleLeader on a migrated cluster = %v, %v", changed, err)
	}
}

// Only stopped nodes that Ensure started make a warm start.
func TestEnsureRecordsARestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		running string
		want    bool
	}{{"false", true}, {"true", false}} {
		r := newRunner().
			script("docker info", "29.0.0\n", nil).
			script("kind get clusters", "cloudburrow-dev\n", nil).
			script("kind get nodes", "cloudburrow-dev-control-plane\n", nil).
			script("docker inspect", tc.running+"\n", nil)
		c := newTestCluster(t, r, "cloudburrow-dev")
		if _, err := c.Ensure(context.Background(), ""); err != nil {
			t.Fatalf("Ensure = %v", err)
		}
		if c.Restarted() != tc.want {
			t.Errorf("running=%s: Restarted = %v, want %v", tc.running, c.Restarted(), tc.want)
		}
	}
}
