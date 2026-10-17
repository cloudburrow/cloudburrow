package cluster

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// kind create no longer waits for the node: the wait is WaitNodesReady,
// after the create, so the work that needs only the API overlaps it (#1588).
func TestCreateDoesNotWaitForTheNode(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get clusters", "", nil).
		script("docker info", "29.0.0\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")
	if err := c.Create(context.Background(), ""); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if !r.ran("kind create cluster") {
		t.Fatal("Create() did not create the cluster")
	}
	if r.ran("--wait") {
		t.Errorf("kind create was given --wait: %v", r.calls)
	}
}

func TestWaitNodesReady(t *testing.T) {
	t.Parallel()

	t.Run("polls until every node is Ready", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		attempts := 0
		r := &funcRunner{fn: func(cmd string) (string, error) {
			if !strings.HasPrefix(cmd, "kubectl ") || !strings.Contains(cmd, "get nodes") {
				return "", nil
			}
			if !strings.Contains(cmd, "--kubeconfig ") {
				t.Errorf("kubectl without an explicit --kubeconfig: %s", cmd)
			}
			mu.Lock()
			defer mu.Unlock()
			attempts++
			switch attempts {
			case 1:
				return "", errors.New("connection refused")
			case 2:
				return "", nil // no node registered yet
			case 3:
				return "cloudburrow-dev-control-plane=False\n", nil
			}
			return "cloudburrow-dev-control-plane=True\n", nil
		}}
		c := newTestCluster(t, r, "cloudburrow-dev")
		if err := c.WaitNodesReady(context.Background(), 10*time.Second); err != nil {
			t.Fatalf("WaitNodesReady() = %v", err)
		}
		if attempts != 4 {
			t.Errorf("attempts = %d, want 4", attempts)
		}
	})

	t.Run("names the node that is not Ready on timeout", func(t *testing.T) {
		t.Parallel()
		r := &funcRunner{fn: func(string) (string, error) {
			return "a=True\nb=False\n", nil
		}}
		c := newTestCluster(t, r, "cloudburrow-dev")
		err := c.WaitNodesReady(context.Background(), 300*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "node b Ready is False") {
			t.Errorf("WaitNodesReady() = %v, want node b named", err)
		}
	})

	t.Run("honours cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := &funcRunner{fn: func(string) (string, error) { return "", nil }}
		c := newTestCluster(t, r, "cloudburrow-dev")
		if err := c.WaitNodesReady(ctx, time.Minute); !errors.Is(err, context.Canceled) {
			t.Errorf("WaitNodesReady() = %v, want context.Canceled", err)
		}
	})
}

// gatedRunner is a cluster that kind creates, whose API answers at once
// and whose node turns Ready only once release is closed.
type gatedRunner struct {
	mu      sync.Mutex
	exists  bool
	release chan struct{}
}

func (g *gatedRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "docker info"):
		return "29.0.0\n", nil
	case strings.HasPrefix(cmd, "kind get clusters"):
		if g.exists {
			return "cloudburrow-dev\n", nil
		}
		return "", nil
	case strings.HasPrefix(cmd, "kind create cluster"):
		g.exists = true
		return "", nil
	case strings.Contains(cmd, "get nodes"):
		select {
		case <-g.release:
			return "cloudburrow-dev-control-plane=True\n", nil
		default:
			return "cloudburrow-dev-control-plane=False\n", nil
		}
	case strings.Contains(cmd, "version -o json"):
		return `{"serverVersion":{"gitVersion":"v1.36.4"}}`, nil
	}
	return "", nil
}

// The cluster component returns once the API answers, before the node is
// Ready, and NodesReady is what waits for the node (#1588).
func TestComponentReturnsBeforeTheNodeAndNodesReadyWaits(t *testing.T) {
	t.Parallel()
	g := &gatedRunner{release: make(chan struct{})}
	comp := NewComponent(newTestCluster(t, g, "cloudburrow-dev"), "", time.Minute, io.Discard)
	if err := comp.Start(context.Background()); err != nil {
		t.Fatalf("cluster Start() = %v", err)
	}
	phase := comp.NodesReady()
	if phase.Name() != "cluster-nodes" {
		t.Errorf("Name() = %q", phase.Name())
	}
	done := make(chan error, 1)
	go func() { done <- phase.Start(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("NodesReady returned %v before the node was Ready", err)
	case <-time.After(400 * time.Millisecond):
	}
	close(g.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("NodesReady() = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("NodesReady did not return once the node was Ready")
	}
	if err := comp.Stop(context.Background()); err != nil {
		t.Errorf("Stop() = %v", err)
	}
}

// A cluster the component found rather than created is not waited on, as
// `up` never has: NodesReady returns at once.
func TestNodesReadyAgainstAFoundCluster(t *testing.T) {
	t.Parallel()
	g := &gatedRunner{exists: true, release: make(chan struct{})}
	comp := NewComponent(newTestCluster(t, g, "cloudburrow-dev"), "", time.Minute, io.Discard)
	_ = comp.Start(context.Background()) // reconciling a found cluster may refuse; not this test's concern
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := comp.NodesReady().Start(ctx); err != nil {
		t.Errorf("NodesReady() against a found cluster = %v, want nil at once", err)
	}
}

// A startup that unwinds before NodesReady ends the background wait.
func TestStopEndsTheNodeWait(t *testing.T) {
	t.Parallel()
	g := &gatedRunner{release: make(chan struct{})}
	comp := NewComponent(newTestCluster(t, g, "cloudburrow-dev"), "", time.Minute, io.Discard)
	if err := comp.Start(context.Background()); err != nil {
		t.Fatalf("cluster Start() = %v", err)
	}
	stopped := make(chan struct{})
	go func() { _ = comp.Stop(context.Background()); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not end the node wait")
	}
	if err := comp.NodesReady().Start(context.Background()); !errors.Is(err, context.Canceled) {
		t.Errorf("NodesReady() after Stop = %v, want context.Canceled", err)
	}
}
