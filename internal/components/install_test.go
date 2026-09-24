package components

import (
	"context"
	"strings"
	"testing"
	"time"
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
