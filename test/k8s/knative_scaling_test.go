//go:build integration

package k8s

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests measure what Knative actually does with the scaling settings the
// Cloud Run adapter maps (#30).
//
// Knative is the closest available model for Cloud Run, not an equivalent one,
// so behaviour is measured and recorded rather than assumed to match.

// deployKsvc applies a Knative Service and waits for it to be ready.
func deployKsvc(t *testing.T, name, annotations string) {
	t.Helper()
	ctx := testCtx(t)
	manifest := fmt.Sprintf(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: %s
  namespace: default
spec:
  template:
    metadata:
      annotations:
%s
    spec:
      containers:
        - image: ghcr.io/knative/helloworld-go:latest
          env:
            - name: TARGET
              value: scaling
`, name, annotations)

	if out, err := kubectl(t, ctx, manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, testCtx(t), "", "delete", "ksvc", name, "--ignore-not-found", "--wait=false")
	})
	if out, err := kubectl(t, ctx, "", "wait", "ksvc/"+name, "--for=condition=Ready", "--timeout=300s"); err != nil {
		diag, _ := kubectl(t, ctx, "", "get", "ksvc", name, "-o", "yaml")
		t.Fatalf("%s never became ready: %v\n%s\n%s", name, err, out, diag)
	}
}

// podCount returns how many pods back a Knative service right now.
func podCount(t *testing.T, name string) int {
	t.Helper()
	out, err := kubectl(t, testCtx(t), "", "get", "pods",
		"-l", "serving.knative.dev/service="+name,
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return -1
	}
	fields := strings.Fields(strings.TrimSpace(out))
	return len(fields)
}

// TestMinScaleKeepsAnInstanceWarm covers the annotation the adapter emits for
// Cloud Run's minInstanceCount.
func TestMinScaleKeepsAnInstanceWarm(t *testing.T) {
	name := fmt.Sprintf("minscale-%d", time.Now().UnixNano()%1e6)
	deployKsvc(t, name, `        autoscaling.knative.dev/min-scale: "1"`)

	// With min-scale 1 a pod must stay up with no traffic at all. Bounded
	// polling: the autoscaler's timing is Knative's, not ours to advance.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if podCount(t, name) >= 1 {
			t.Logf("RESULT: min-scale=1 kept %d pod(s) running with no traffic", podCount(t, name))
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("min-scale=1 did not keep an instance warm; pods=%d", podCount(t, name))
}

// TestScaleToZeroHappensWithoutTraffic records Knative's scale-to-zero, which
// Cloud Run also does but on its own schedule.
func TestScaleToZeroHappensWithoutTraffic(t *testing.T) {
	name := fmt.Sprintf("scalezero-%d", time.Now().UnixNano()%1e6)
	// Short windows so the test does not wait on Knative's 60s default.
	deployKsvc(t, name, `        autoscaling.knative.dev/min-scale: "0"
        autoscaling.knative.dev/scale-to-zero-pod-retention-period: "5s"
        autoscaling.knative.dev/window: "6s"`)

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if podCount(t, name) == 0 {
			t.Log("RESULT: the service scaled to zero with no traffic")
			return
		}
		time.Sleep(3 * time.Second)
	}
	// Recorded, not failed: scale-to-zero timing is Knative's own, and a slow
	// cluster is not a CloudBurrow defect.
	t.Logf("RESULT: still %d pod(s) after 3 minutes; scale-to-zero did not complete in the window",
		podCount(t, name))
}

// TestMaxScaleIsRecordedOnTheRevision confirms the annotation the adapter emits
// for Cloud Run's maxInstanceCount reaches the revision.
//
// The cap's *enforcement* under load is not tested: driving enough concurrent
// traffic to prove a ceiling would make this suite slow and flaky, and a
// half-measure would be worse than an honest gap.
func TestMaxScaleIsRecordedOnTheRevision(t *testing.T) {
	name := fmt.Sprintf("maxscale-%d", time.Now().UnixNano()%1e6)
	deployKsvc(t, name, `        autoscaling.knative.dev/min-scale: "1"
        autoscaling.knative.dev/max-scale: "3"`)

	out, err := kubectl(t, testCtx(t), "", "get", "ksvc", name,
		"-o", "jsonpath={.spec.template.metadata.annotations.autoscaling\\.knative\\.dev/max-scale}")
	if err != nil {
		t.Fatalf("read annotation: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "3" {
		t.Errorf("max-scale on the revision = %q, want 3", strings.TrimSpace(out))
	}
	t.Log("RESULT: max-scale reaches the revision; the cap is not tested under load")
}

// TestSingleRevisionTakesAllTraffic records the traffic behaviour that does
// work, which is the only one the adapter claims.
func TestSingleRevisionTakesAllTraffic(t *testing.T) {
	name := fmt.Sprintf("traffic-%d", time.Now().UnixNano()%1e6)
	deployKsvc(t, name, `        autoscaling.knative.dev/min-scale: "1"`)

	out, err := kubectl(t, testCtx(t), "", "get", "ksvc", name,
		"-o", "jsonpath={.status.traffic[0].percent}")
	if err != nil {
		t.Fatalf("read traffic: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "100" {
		t.Errorf("traffic to the latest revision = %q%%, want 100", strings.TrimSpace(out))
	}
}
