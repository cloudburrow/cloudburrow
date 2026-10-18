package components

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// rolloutWaitsForKnative answers every call, holding a backend's rollout
// until Knative's first manifest has been applied: a Start that installed
// Knative only after the backends would never get there (#1588).
func rolloutWaitsForKnative(r *recordingRunner, rollout error) func(string) (string, error) {
	return func(call string) (string, error) {
		if strings.Contains(call, "rollout status") {
			deadline := time.Now().Add(5 * time.Second)
			for !r.appliedLocked("serving-crds") {
				if time.Now().After(deadline) {
					return "", errors.New("knative was not installed while the backends rolled out")
				}
				time.Sleep(5 * time.Millisecond)
			}
			return "", rollout
		}
		if strings.Contains(call, "get namespace knative-serving") {
			return "", errors.New("not found")
		}
		return "", nil
	}
}

func (r *recordingRunner) appliedLocked(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied(name)
}

func knativeAndPubSub(r *recordingRunner) *LifecycleComponent {
	return &LifecycleComponent{installer: newTestInstaller(r),
		services: []config.Service{config.ServicePubSub, config.ServiceRun},
		timeout:  time.Minute, out: &strings.Builder{}}
}

// Knative installs while the backends roll out, and Start returns only
// once both have.
func TestKnativeInstallsBesideTheBackends(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	r.respond = rolloutWaitsForKnative(r, nil)
	c := knativeAndPubSub(r)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.find("wait --for=condition=Available deployment --all") == "" {
		t.Fatalf("Start returned before waiting for Knative: %v", r.calls)
	}
}

// A backend that fails is the error reported, and the Knative install
// beside it is stopped rather than waited out.
func TestABackendFailureStopsTheKnativeInstall(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	boom := errors.New("crashloop")
	r.respond = rolloutWaitsForKnative(r, boom)
	c := knativeAndPubSub(r)
	err := c.Start(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want the backend's failure", err)
	}
}

// Split across the coordinator, KnativeStart begins the install before
// the backends, Start leaves it running, and KnativeReady waits for it.
func TestKnativeStartAndReadyBracketTheBackends(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	release := make(chan struct{})
	r.respond = func(call string) (string, error) {
		if strings.Contains(call, "get namespace knative-serving") {
			return "", errors.New("not found")
		}
		if strings.Contains(call, "-n kourier-system wait") {
			<-release
		}
		return "", nil
	}
	c := knativeAndPubSub(r)
	start, ready := c.KnativeStart(), c.KnativeReady()
	ctx := context.Background()
	if err := start.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Start returns with Knative still waiting for kourier.
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	joined := make(chan error, 1)
	go func() { joined <- ready.Start(ctx) }()
	select {
	case err := <-joined:
		t.Fatalf("KnativeReady returned (%v) before Knative was ready", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if !r.appliedLocked("serving-core") {
		t.Fatal("Knative was never installed")
	}
}

// Without Cloud Run both phases do nothing.
func TestKnativePhasesWithoutRun(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	c := &LifecycleComponent{installer: newTestInstaller(r), services: []config.Service{config.ServicePubSub},
		timeout: time.Minute, out: &strings.Builder{}}
	for _, p := range []*KnativePhase{c.KnativeStart(), c.KnativeReady()} {
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls without Cloud Run: %v", r.calls)
	}
}
