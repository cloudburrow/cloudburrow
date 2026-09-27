package components

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// The installer's kubectl calls, now made through internal/k8s, run with
// exactly the arguments they had before: --kubeconfig first, no context,
// and a namespace only where the call names one.
func TestTheInstallersKubectlArgumentsAreUnchanged(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	i := newTestInstaller(r)
	ctx := context.Background()
	if !i.KnativeInstalled(ctx) {
		t.Fatal("KnativeInstalled = false with kubectl succeeding")
	}
	if err := i.WaitKnative(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := i.Apply(ctx, "manifest"); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Kubectl(ctx, "get", "ksvc", "-A"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kubectl --kubeconfig /tmp/kubeconfig get namespace knative-serving",
		"kubectl --kubeconfig /tmp/kubeconfig -n knative-serving wait --for=condition=Available deployment --all --timeout=60s",
		"kubectl --kubeconfig /tmp/kubeconfig -n kourier-system wait --for=condition=Available deployment --all --timeout=60s",
		"kubectl --kubeconfig /tmp/kubeconfig apply -f -",
		"kubectl --kubeconfig /tmp/kubeconfig get ksvc -A",
	}
	if got := strings.Join(r.calls, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("ran\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}
	if r.stdins[3] != "manifest" {
		t.Errorf("apply stdin = %q", r.stdins[3])
	}
}

// A deployment that never rolls out is reported with kubectl's own words
// and the events that say why, as before; the failure is still classified.
func TestAFailedRolloutKeepsKubectlsWords(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{respond: func(call string) (string, error) {
		if strings.Contains(call, "rollout status") {
			return "", errors.New(`kubectl: exit status 1: Error from server (NotFound): deployments.apps "pubsub" not found`)
		}
		return "Warning FailedScheduling pod/pubsub-x", nil
	}}
	err := newTestInstaller(r).waitDeployment(context.Background(), "cloudburrow", "pubsub", 90*time.Second)
	want := "component install failed: pubsub did not become ready within 1m30s: " +
		`kubectl: exit status 1: Error from server (NotFound): deployments.apps "pubsub" not found` +
		"\nWarning FailedScheduling pod/pubsub-x"
	if err == nil || err.Error() != want {
		t.Errorf("waitDeployment = %q, want %q", err, want)
	}
	if !errors.Is(err, ErrInstallFailed) || !k8s.IsNotFound(err) {
		t.Errorf("waitDeployment = %v: ErrInstallFailed %v, NotFound %v", err, errors.Is(err, ErrInstallFailed), k8s.IsNotFound(err))
	}
	if want := "kubectl --kubeconfig /tmp/kubeconfig -n cloudburrow rollout status deployment/pubsub --watch=true --timeout=90s"; r.calls[0] != want {
		t.Errorf("ran %q, want %q", r.calls[0], want)
	}
	if want := "kubectl --kubeconfig /tmp/kubeconfig -n cloudburrow get events --field-selector involvedObject.name=pubsub --sort-by=.lastTimestamp"; r.calls[1] != want {
		t.Errorf("ran %q, want %q", r.calls[1], want)
	}
}
