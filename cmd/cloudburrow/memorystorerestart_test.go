package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// restartKube is a cluster with one Memorystore server's Deployment: its
// pod stays listed for a few polls after the Deployment is scaled to none,
// as a pod still stopping does.
type restartKube struct {
	mu       sync.Mutex
	calls    []string
	scaled   bool
	lingers  int
	onGone   func()
	goneSeen bool
}

func (k *restartKube) Run(_ context.Context, _ string, args ...string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	call := strings.Join(args, " ")
	k.calls = append(k.calls, call)
	switch {
	case strings.Contains(call, " scale deployment/"):
		k.scaled = true
	case strings.Contains(call, " get pods -l app="):
		if !k.scaled || k.lingers > 0 {
			if k.scaled {
				k.lingers--
			}
			return "pod/srv-abc\n", nil
		}
		if !k.goneSeen {
			k.goneSeen = true
			k.onGone()
		}
		return "", nil
	}
	return "", nil
}

// TestMemorystoreRestartStopsBeforeStarting is a Basic tier maintenance's
// restart (#1911, main run 45823073864): the Deployment is scaled to none
// and its pod waited for until gone, before anything starts the next one,
// and the pod is never deleted beside a ReplicaSet that would replace it
// at once, so an RDB instance's last snapshot is on the volume before the
// next server loads it.
func TestMemorystoreRestartStopsBeforeStarting(t *testing.T) {
	old := memorystoreStopPoll
	memorystoreStopPoll = time.Millisecond
	t.Cleanup(func() { memorystoreStopPoll = old })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	kube := &restartKube{lingers: 3, onGone: func() { close(stopped); cancel() }}
	var cfg config.Config
	cfg.Cluster.Namespace = "cloudburrow"
	p := newMemorystoreProvisioner(cfg, context.Background())
	p.installer = &components.Installer{Namespace: "cloudburrow", Kube: kube}

	// The provisioner is never opened (p.ready), so Provision, after the
	// stop, waits for it until ctx, which the pod's end cancels, is done.
	_, err := p.Restart(ctx, redisadmin.Spec{Server: "srv"})
	select {
	case <-stopped:
	default:
		t.Fatalf("Restart returned %v before its pod was gone; calls:\n%s", err, strings.Join(kube.calls, "\n"))
	}

	kube.mu.Lock()
	defer kube.mu.Unlock()
	scale, polls := -1, 0
	for i, c := range kube.calls {
		switch {
		case strings.Contains(c, "-n cloudburrow scale deployment/srv --replicas=0"):
			if scale < 0 {
				scale = i
			}
		case strings.Contains(c, "-n cloudburrow get pods -l app=srv -o name"):
			if scale < 0 || i < scale {
				t.Errorf("call %d polled the pods before the scale to none: %q", i, c)
			}
			polls++
		case strings.Contains(c, " delete "):
			t.Errorf("call %d deletes beside a ReplicaSet that replaces at once: %q", i, c)
		case strings.Contains(c, " apply "):
			t.Errorf("call %d applies the Deployment before its pod is gone: %q", i, c)
		}
	}
	if scale != 0 {
		t.Errorf("the first call is not the scale to none; calls:\n%s", strings.Join(kube.calls, "\n"))
	}
	if polls != 4 {
		t.Errorf("polled the pods %d times, want 4 (3 while it stops, 1 gone); calls:\n%s", polls, strings.Join(kube.calls, "\n"))
	}
}
