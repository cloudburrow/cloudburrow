package components

import (
	"context"
	"errors"
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
	// A manifest is applied from stdin now (#597), so it is found by its
	// content, a wait by its arguments.
	idx := func(sub string) int {
		for i, c := range r.calls {
			if strings.Contains(c, sub) || strings.Contains(r.stdins[i], sub) {
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

// An instance whose services deploy no backend pod had no managed namespace,
// so Cloud KMS answered Internal and diagnose called the cluster down (#571).
// Start creates it whatever the services are.
func TestStartCreatesTheManagedNamespaceWithoutABackend(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	c := &LifecycleComponent{installer: newTestInstaller(r), services: []config.Service{config.ServiceKMS},
		timeout: time.Minute, out: io.Discard}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var applied string
	for i, call := range r.calls {
		if strings.Contains(call, "apply -f -") {
			applied = r.stdins[i]
		}
	}
	for _, want := range []string{"kind: Namespace", "name: cloudburrow", `cloudburrow.dev/owned: "true"`} {
		if !strings.Contains(applied, want) {
			t.Errorf("no applied manifest contains %q; calls:\n%s", want, strings.Join(r.calls, "\n"))
		}
	}
	if len(r.calls) != 1 {
		t.Errorf("%d kubectl calls for a KMS-only instance, want the one namespace apply:\n%s", len(r.calls), strings.Join(r.calls, "\n"))
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
	r := &recordingRunner{respond: stampedWith(KnativeVersion)}
	c := &LifecycleComponent{installer: newTestInstaller(r), services: []config.Service{config.ServiceRun},
		timeout: time.Minute, out: io.Discard}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.applied("serving-core.yaml") {
		t.Errorf("Knative was reinstalled:\n%s", strings.Join(r.calls, "\n"))
	}
	if got := r.find("configmap/config-deployment"); !strings.Contains(got, `"progress-deadline":"240s"`) {
		t.Errorf("config-deployment patch = %q; want the 240s deadline", got)
	}
	// After stop and up the webhook that validates the ConfigMap is still
	// coming back; the patch made before it answered failed a merge-queue
	// build. The wait comes first.
	idx := func(sub string) int {
		for i, c := range r.calls {
			if strings.Contains(c, sub) {
				return i
			}
		}
		return -1
	}
	wait, patch := idx("-n knative-serving wait --for=condition=Available deployment --all"), idx("configmap/config-deployment")
	if wait < 0 || patch < 0 || wait > patch {
		t.Errorf("wait for knative-serving at %d, patch at %d; the wait must come first:\n%s", wait, patch, strings.Join(r.calls, "\n"))
	}
}

// A manifest whose bytes do not match its pin is never applied: kubectl
// sees nothing, and the error names the file (#597). A matching one is
// applied from stdin, never by URL, and the verification is logged.
func TestKnativeManifestsAreVerifiedBeforeApply(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	var log strings.Builder
	in := newTestInstaller(r)
	in.Out = &log
	if err := in.InstallKnative(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "apply -f http") {
			t.Errorf("a manifest was applied by URL: %s", c)
		}
	}
	if !strings.Contains(log.String(), "verified serving-core.yaml sha256:") {
		t.Errorf("the verification was not logged:\n%s", log.String())
	}

	r = &recordingRunner{}
	tampered := newTestInstaller(r)
	tampered.Fetch = func(context.Context, string) ([]byte, error) { return []byte("kind: ClusterRoleBinding\n"), nil }
	err := tampered.InstallKnative(context.Background(), time.Minute)
	if !errors.Is(err, ErrInstallFailed) || !strings.Contains(err.Error(), "serving-crds.yaml") {
		t.Errorf("a tampered manifest = %v; want ErrInstallFailed naming serving-crds.yaml", err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, " apply ") {
			t.Errorf("kubectl apply ran for a tampered manifest: %s", c)
		}
	}
}

// After stop and up, Knative's webhook can refuse a ConfigMap patch for a
// few seconds after its Deployments are Available (#765's run shard). The
// patch is retried while the error is "failed calling webhook", and any
// other error is returned at once.
func TestConfigMapPatchesRetryWhileTheWebhookIsComingBack(t *testing.T) {
	every := webhookRetryEvery
	webhookRetryEvery = time.Millisecond
	t.Cleanup(func() { webhookRetryEvery = every })

	webhookDown := errors.New(`kubectl: exit status 1: Error from server (InternalError): Internal error occurred: failed calling webhook "config.webhook.serving.knative.dev": connection refused`)
	refusals := 2
	r := &recordingRunner{respond: func(call string) (string, error) {
		if strings.Contains(call, "configmap/config-deployment") && refusals > 0 {
			refusals--
			return "", webhookDown
		}
		return "", nil
	}}
	if err := newTestInstaller(r).ConfigureDeployment(context.Background()); err != nil {
		t.Fatalf("ConfigureDeployment after two webhook refusals: %v", err)
	}
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, "configmap/config-deployment") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("config-deployment was patched %d times, want 3 (two refused, one accepted)", n)
	}

	other := errors.New("kubectl: exit status 1: Error from server (Forbidden): configmaps is forbidden")
	r = &recordingRunner{err: other}
	err := newTestInstaller(r).ConfigureDeployment(context.Background())
	if !errors.Is(err, ErrInstallFailed) || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("ConfigureDeployment on a non-webhook error = %v", err)
	}
	if len(r.calls) != 1 {
		t.Errorf("a non-webhook error was retried: %d calls", len(r.calls))
	}
}
