package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// registeringRunner is a kind node that joins at once but whose kubelet
// registers its Node only after the first reads of it, as on a loaded
// runner (main CI 44926873355).
type registeringRunner struct {
	mu       sync.Mutex
	reads    int
	register int // the read from which the Node exists, Ready
	removed  bool
	node     string // the node container docker run made
	routeAt  int    // the route read from which kindnet has routed its pod range
	routes   int
}

func (f *registeringRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "docker ps"):
		return "", nil
	case strings.HasPrefix(cmd, "docker run"):
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				f.node = args[i+1]
			}
		}
		return "", nil
	case strings.HasPrefix(cmd, "kubectl") && strings.Contains(cmd, " get nodes "):
		return f.node + "|172.18.0.3|10.244.1.0/24\ncp|172.18.0.2|10.244.0.0/24\n", nil
	case strings.HasPrefix(cmd, "docker exec") && strings.Contains(cmd, "ip -o route"):
		f.routes++
		if f.routes < f.routeAt {
			return "default via 172.18.0.1 dev eth0\n", nil
		}
		if args[1] == "cp" {
			return "default via 172.18.0.1 dev eth0\n10.244.1.0/24 via 172.18.0.3 dev eth0\n", nil
		}
		return "default via 172.18.0.1 dev eth0\n10.244.0.0/24 via 172.18.0.2 dev eth0\n", nil
	case strings.Contains(cmd, "systemctl is-system-running"):
		return "running\n", nil
	case strings.Contains(cmd, "kubeadm token create"):
		return "abcdef.0123456789abcdef\n", nil
	case strings.HasPrefix(cmd, "docker inspect"):
		return "172.18.0.3\n", nil
	case strings.HasPrefix(cmd, "docker rm"):
		f.removed = true
		return "", nil
	case strings.HasPrefix(cmd, "kubectl") && strings.Contains(cmd, " wait "):
		return "", errors.New(`Error from server (NotFound): nodes "x" not found`)
	case strings.HasPrefix(cmd, "kubectl") && strings.Contains(cmd, " get node "):
		f.reads++
		if f.reads < f.register {
			return "", errors.New(`Error from server (NotFound): nodes "x" not found`)
		}
		return "True", nil
	}
	return "", nil
}

// TestJoinWaitsForTheNodeToRegister: a node pool's new node is Ready once
// its kubelet registers it, however long after kubeadm join that is; the
// first read of a Node not registered yet is not a failure (kubectl wait
// on it failed at once and left the pool in ERROR).
func TestJoinWaitsForTheNodeToRegister(t *testing.T) {
	r := &registeringRunner{register: 2}
	g := &GKE{Dir: t.TempDir(), Runner: r}
	if err := g.Resize(context.Background(), OwnerPrefix+"-gke-x", "kindest/node:v0", "tfpool", 1); err != nil {
		t.Fatalf("Resize to one node, registered on the second read: %v", err)
	}
	if r.reads != 2 || r.removed {
		t.Errorf("read the Node %d times and removed it %v, want 2 reads and kept", r.reads, r.removed)
	}
}

// TestJoinWaitsForThePodNetworkRoutes: a joined node is Ready before
// kindnet, on its 10-second tick, routes its pod range from the other
// nodes; a pod made on it then cannot be reached from the control plane
// (main run 45555303799), so join returns only once every node routes it.
func TestJoinWaitsForThePodNetworkRoutes(t *testing.T) {
	r := &registeringRunner{register: 1, routeAt: 5}
	g := &GKE{Dir: t.TempDir(), Runner: r}
	if err := g.Resize(context.Background(), OwnerPrefix+"-gke-x", "kindest/node:v0", "tfpool", 1); err != nil {
		t.Fatalf("Resize to one node, routed on the fifth route read: %v", err)
	}
	if r.routes < 6 {
		t.Errorf("read the routes %d times, want until both nodes route the other's pod range", r.routes)
	}
}

// TestJoinFailsWhenThePodNetworkIsNeverRouted: a node whose pod range is
// never routed is not a node pods can serve from.
func TestJoinFailsWhenThePodNetworkIsNeverRouted(t *testing.T) {
	old := podRoutesWait
	podRoutesWait = 2 * time.Second
	t.Cleanup(func() { podRoutesWait = old })
	r := &registeringRunner{register: 1, routeAt: 1 << 30}
	g := &GKE{Dir: t.TempDir(), Runner: r}
	err := g.Resize(context.Background(), OwnerPrefix+"-gke-x", "kindest/node:v0", "tfpool", 1)
	if err == nil || !strings.Contains(err.Error(), "no route to 10.244.1.0/24 via 172.18.0.3") {
		t.Fatalf("Resize with a pod range never routed: %v", err)
	}
}

func TestRoutedVia(t *testing.T) {
	table := "default via 172.18.0.1 dev eth0 \n10.244.1.0/24 via 172.18.0.3 dev eth0 \n10.244.0.2 dev veth1 scope host \n"
	for _, c := range []struct {
		cidr, gw string
		want     bool
	}{{"10.244.1.0/24", "172.18.0.3", true}, {"10.244.1.0/24", "172.18.0.4", false}, {"10.244.2.0/24", "172.18.0.3", false}} {
		if got := routedVia(table, c.cidr, c.gw); got != c.want {
			t.Errorf("routedVia(%s via %s) = %v", c.cidr, c.gw, got)
		}
	}
	if sameFamily([]string{"172.18.0.3", "fc00:f853::3"}, "fd00:10:244:1::/64") != "fc00:f853::3" || sameFamily([]string{"172.18.0.3"}, "fd00::/64") != "" {
		t.Error("sameFamily")
	}
}
