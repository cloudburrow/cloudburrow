package components

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingRunner captures the commands an installer would run, so the
// patches can be asserted without a cluster.
type recordingRunner struct {
	calls []string
	// stdins is what each call was given on stdin: the manifests.
	stdins []string
	err    error
}

func (r *recordingRunner) Run(_ context.Context, stdin string, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	r.stdins = append(r.stdins, stdin)
	return "", r.err
}

func (r *recordingRunner) find(substr string) string {
	for _, c := range r.calls {
		if strings.Contains(c, substr) {
			return c
		}
	}
	return ""
}

func newTestInstaller(r Runner) *Installer {
	return &Installer{Kubeconfig: "/tmp/kubeconfig", Namespace: "cloudburrow", Runner: r}
}

// Kourier ships as a LoadBalancer, which on a local cluster shows
// EXTERNAL-IP <pending> forever. A NodePort is what a kind port mapping can
// actually reach.
func TestConfigureIngressPublishesKourierOnAFixedNodePort(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	if err := newTestInstaller(r).ConfigureIngress(context.Background(), ""); err != nil {
		t.Fatalf("ConfigureIngress: %v", err)
	}

	got := r.find("service/" + IngressService)
	if got == "" {
		t.Fatalf("the kourier Service was never patched: %v", r.calls)
	}
	for _, want := range []string{
		`"type":"NodePort"`,
		`"nodePort":31080`,
		`"nodePort":31443`,
		"-n " + IngressNamespace,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patch is missing %q:\n%s", want, got)
		}
	}
}

// Two default suffixes in config-domain leave Knative choosing between them,
// so an upgraded cluster would name services unpredictably.
func TestConfigureIngressRemovesThePreviousDomain(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	if err := newTestInstaller(r).ConfigureIngress(context.Background(), ""); err != nil {
		t.Fatalf("ConfigureIngress: %v", err)
	}

	got := r.find("configmap/config-domain")
	if got == "" {
		t.Fatalf("config-domain was never patched: %v", r.calls)
	}
	if !strings.Contains(got, `"`+PreviousDomain+`":null`) {
		t.Errorf("the previous domain is not deleted:\n%s", got)
	}
	if !strings.Contains(got, `"`+DefaultDomain+`":""`) {
		t.Errorf("the new domain is not set:\n%s", got)
	}
}

func TestConfigureIngressHonoursAnExplicitDomain(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	if err := newTestInstaller(r).ConfigureIngress(context.Background(), "example.test"); err != nil {
		t.Fatalf("ConfigureIngress: %v", err)
	}
	if got := r.find("configmap/config-domain"); !strings.Contains(got, `"example.test":""`) {
		t.Errorf("an explicit domain was ignored:\n%s", got)
	}
}

// A failed patch must not be reported as a configured ingress: the gateway
// would then be unreachable with nothing saying why.
func TestConfigureIngressReportsAFailedPatch(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{err: errors.New("forbidden")}
	err := newTestInstaller(r).ConfigureIngress(context.Background(), "")
	if err == nil {
		t.Fatal("a failed patch was reported as success")
	}
	if !errors.Is(err, ErrInstallFailed) {
		t.Errorf("error = %v, want it to wrap ErrInstallFailed", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("the underlying cause was lost: %v", err)
	}
}

// The node port in the Service patch and the one a kind mapping points at are
// set at different times and must agree, so the constant is the single source.
func TestNodePortsAreDistinct(t *testing.T) {
	t.Parallel()
	if IngressHTTPNodePort == IngressHTTPSNodePort {
		t.Fatal("HTTP and HTTPS share a node port")
	}
	for _, p := range []int{IngressHTTPNodePort, IngressHTTPSNodePort} {
		// Kubernetes' default NodePort range.
		if p < 30000 || p > 32767 {
			t.Errorf("node port %d is outside the default NodePort range", p)
		}
	}
}

// `.localhost` is the point of #86: no DNS egress, no /etc/hosts entry, no
// third-party wildcard service.
func TestDefaultDomainIsReservedAndLocal(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(DefaultDomain, ".localhost") {
		t.Errorf("DefaultDomain = %q, want a .localhost name", DefaultDomain)
	}
	if strings.Contains(DefaultDomain, "sslip") || strings.Contains(DefaultDomain, "nip.io") {
		t.Errorf("DefaultDomain = %q depends on a third-party DNS service", DefaultDomain)
	}
}
