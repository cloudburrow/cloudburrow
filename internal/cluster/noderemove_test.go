package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// seqRunner answers each command prefix from a queue of results, the last
// repeated, and records every command.
type seqRunner struct {
	mu    sync.Mutex
	calls []string
	queue map[string][]result
}

func (s *seqRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, cmd)
	for prefix, q := range s.queue {
		if strings.HasPrefix(cmd, prefix) && len(q) > 0 {
			r := q[0]
			if len(q) > 1 {
				s.queue[prefix] = q[1:]
			}
			return r.out, r.err
		}
	}
	return "", nil
}

func (s *seqRunner) indexOf(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

func (s *seqRunner) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

var errNoExit = errors.New("could not kill container: tried to kill container, but did not receive an exit event")

func fastRetries(t *testing.T) {
	t.Helper()
	attempts, delay := removeAttempts, removeDelay
	removeAttempts, removeDelay = 4, time.Millisecond
	t.Cleanup(func() { removeAttempts, removeDelay = attempts, delay })
}

// Every node's NFS mounts are force-unmounted, lazily, and its kubelet
// stopped, before kind deletes the cluster (#2192).
func TestDeleteReleasesNFSMountsFirst(t *testing.T) {
	r := &seqRunner{queue: map[string][]result{
		"kind get clusters": {{out: "cloudburrow-ci-gke-1\n"}},
		"kind get nodes":    {{out: "cloudburrow-ci-gke-1-control-plane\ncloudburrow-ci-gke-1-pool-1\n"}},
	}}
	c := newTestCluster(t, r, "cloudburrow-ci-gke-1")
	if err := c.Delete(context.Background()); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	for _, n := range []string{"cloudburrow-ci-gke-1-control-plane", "cloudburrow-ci-gke-1-pool-1"} {
		i := r.indexOf("docker exec " + n + " sh -c " + releaseScript)
		if i < 0 || i > r.indexOf("kind delete cluster") {
			t.Errorf("node %s was not released before kind delete: %q", n, r.calls)
		}
	}
	if !strings.Contains(releaseScript, "umount -f -l") || !strings.Contains(releaseScript, "systemctl stop kubelet") {
		t.Errorf("releaseScript = %q", releaseScript)
	}
}

// A node whose processes are slow to die, so kind's docker rm fails, is
// removed again until it goes, and kind forgets the cluster.
func TestDeleteRetriesANodeThatDidNotExit(t *testing.T) {
	fastRetries(t)
	node := "cloudburrow-ci-gke-1-pool-1"
	r := &seqRunner{queue: map[string][]result{
		"kind get clusters":   {{out: "cloudburrow-ci-gke-1\n"}},
		"kind get nodes":      {{out: "cloudburrow-ci-gke-1-control-plane\n" + node + "\n"}, {out: node + "\n"}},
		"kind delete cluster": {{err: errNoExit}, {}},
		"docker rm -f -v":     {{err: errNoExit}, {err: errNoExit}, {}},
	}}
	c := newTestCluster(t, r, "cloudburrow-ci-gke-1")
	if err := c.Delete(context.Background()); err != nil {
		t.Fatalf("Delete() = %v; calls %q", err, r.calls)
	}
	if got := r.count("docker rm -f -v " + node); got != 3 {
		t.Errorf("docker rm of %s ran %d times, want 3", node, got)
	}
	if got := r.count("kind delete cluster"); got != 2 {
		t.Errorf("kind delete ran %d times, want 2", got)
	}
}

// A node that never goes fails the delete, naming kind's error.
func TestDeleteFailsWhenANodeNeverGoes(t *testing.T) {
	fastRetries(t)
	r := &seqRunner{queue: map[string][]result{
		"kind get clusters":   {{out: "cloudburrow-ci-gke-1\n"}},
		"kind get nodes":      {{out: "cloudburrow-ci-gke-1-pool-1\n"}},
		"kind delete cluster": {{err: errNoExit}},
		"docker rm -f -v":     {{err: errNoExit}},
	}}
	c := newTestCluster(t, r, "cloudburrow-ci-gke-1")
	err := c.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exit event") {
		t.Fatalf("Delete() = %v, want kind's error", err)
	}
	if got := r.count("docker rm -f -v"); got != removeAttempts {
		t.Errorf("docker rm ran %d times, want %d", got, removeAttempts)
	}
}

// A node container already gone is removed.
func TestRemoveNodeContainerAlreadyGone(t *testing.T) {
	r := &seqRunner{queue: map[string][]result{
		"docker rm -f -v": {{err: errors.New("Error response from daemon: No such container: n")}},
	}}
	if err := removeNodeContainer(context.Background(), r, "n"); err != nil {
		t.Fatalf("removeNodeContainer = %v", err)
	}
}
