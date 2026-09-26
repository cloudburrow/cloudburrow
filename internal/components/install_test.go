package components

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The Knative CRDs must be Established before serving-core is applied: the
// API refuses objects of a kind it does not yet serve, and applying straight
// on failed intermittently with "no matches for kind" (#357).
func TestKnativeWaitsForItsCRDsBeforeTheCore(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	if err := newTestInstaller(r).InstallKnative(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	idx := func(sub string) int {
		for i, c := range r.calls {
			if strings.Contains(c, sub) {
				return i
			}
		}
		return -1
	}
	crds, wait, core := idx("serving-crds.yaml"), idx("wait --for=condition=Established crd -l knative.dev/crd-install=true"), idx("serving-core.yaml")
	if crds < 0 || wait < 0 || core < 0 {
		t.Fatalf("calls = %v", r.calls)
	}
	if !(crds < wait && wait < core) {
		t.Errorf("order is crds %d, wait %d, core %d; the wait must sit between them:\n%s", crds, wait, core, strings.Join(r.calls, "\n"))
	}
}

// A revision whose container exits at startup was reported failed only after
// Knative's 600s progress deadline (#568). The install sets Cloud Run's 240s.
func TestKnativeInstallSetsTheRevisionProgressDeadline(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	if err := newTestInstaller(r).InstallKnative(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	want := `patch configmap/config-deployment -n knative-serving --type merge -p {"data":{"progress-deadline":"240s"}}`
	if got := r.find("configmap/config-deployment"); !strings.Contains(got, want) {
		t.Errorf("config-deployment patch = %q, want it to contain %q", got, want)
	}
}

// A cluster installed before the deadline existed gets it on the next up:
// the already-installed path applies the setting without reinstalling.
func TestAnAlreadyInstalledKnativeGetsTheProgressDeadline(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	c := &LifecycleComponent{installer: newTestInstaller(r), services: []config.Service{config.ServiceRun},
		timeout: time.Minute, out: io.Discard}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.find("serving-core.yaml") != "" {
		t.Errorf("Knative was reinstalled:\n%s", strings.Join(r.calls, "\n"))
	}
	if got := r.find("configmap/config-deployment"); !strings.Contains(got, `"progress-deadline":"240s"`) {
		t.Errorf("config-deployment patch = %q; want the 240s deadline", got)
	}
}
