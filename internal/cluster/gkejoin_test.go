package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// registeringRunner is a kind node that joins at once but whose kubelet
// registers its Node only after the first reads of it, as on a loaded
// runner (main CI 44926873355).
type registeringRunner struct {
	mu       sync.Mutex
	reads    int
	register int // the read from which the Node exists, Ready
	removed  bool
}

func (f *registeringRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "docker ps"):
		return "", nil
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
