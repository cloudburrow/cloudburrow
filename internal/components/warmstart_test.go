package components

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// warmCluster answers as a cluster with Knative installed at the pin. Each
// Deployment's pods turn Ready once its template carries a restart stamp
// and ready says so (#1610).
type warmCluster struct {
	mu      sync.Mutex
	deploys map[string]string // namespace: the get deployment answer
	stamps  map[string]string // namespace/name: stamp
	order   []string          // namespace/name, as patched
	ready   func(dep string) bool
}

var stampRe = regexp.MustCompile(`"cloudburrow.dev/restarted-at":"([^"]+)"`)

func newWarmCluster(ready func(string) bool) *warmCluster {
	return &warmCluster{
		deploys: map[string]string{
			"knative-serving": "activator=1\nnet-kourier-controller=1\nwebhook=2\n",
			"kube-system":     "coredns=2\nmetrics-server=1\n",
			"kourier-system":  "3scale-kourier-gateway=1\n",
		},
		stamps: map[string]string{},
		ready:  ready,
	}
}

func (w *warmCluster) respond(call string) (string, error) {
	fields := strings.Fields(call)
	ns := ""
	for i, f := range fields {
		if f == "-n" && i+1 < len(fields) {
			ns = fields[i+1]
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case strings.Contains(call, " get deployment -o jsonpath"):
		return w.deploys[ns], nil
	case strings.Contains(call, " patch deployment "):
		m := stampRe.FindStringSubmatch(call)
		if m == nil {
			return "", errors.New("patch without a restart stamp: " + call)
		}
		name := fields[len(fields)-5]
		w.stamps[ns+"/"+name] = m[1]
		w.order = append(w.order, ns+"/"+name)
		return "", nil
	case strings.Contains(call, " get pods -o jsonpath"):
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimSpace(w.deploys[ns]), "\n") {
			name, reps, _ := strings.Cut(line, "=")
			s, ok := w.stamps[ns+"/"+name]
			// The old pod: Ready by its stale status, unstamped.
			fmt.Fprintf(&b, "%s-old-a||True|\n", name)
			if !ok {
				continue
			}
			// One being deleted, and one of a longer-named Deployment.
			fmt.Fprintf(&b, "%s-new-z|%s|True|2026-10-17T00:00:00Z\n", name, s)
			fmt.Fprintf(&b, "%s-x-new-y|%s|True|\n", name, s)
			if w.ready(name) {
				for i := 0; i < int(reps[0]-'0'); i++ {
					fmt.Fprintf(&b, "%s-new-%d|%s|True|\n", name, i, s)
				}
			}
		}
		return b.String(), nil
	}
	return stampedWith(KnativeVersion)(call)
}

func (w *warmCluster) patched() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.order...)
}

func warmComponent(r *recordingRunner) *LifecycleComponent {
	return &LifecycleComponent{installer: newTestInstaller(r),
		services: []config.Service{config.ServiceRun},
		timeout:  time.Minute, out: &strings.Builder{}}
}

// After a warm start, CoreDNS's and Knative's pods are replaced once the
// Service network answers, the gateway only once CoreDNS and its
// configuration server are Ready; and up waits for the new pods, not the
// Deployments' status, which predates the stop (#1610).
func TestAWarmStartRestartsKnativeOnTheNetwork(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	polls := map[string]int{}
	var w *warmCluster
	w = newWarmCluster(func(dep string) bool {
		mu.Lock()
		defer mu.Unlock()
		polls[dep]++
		return polls[dep] > 2
	})
	r := &recordingRunner{respond: w.respond}
	c := warmComponent(r)
	networkUp := false
	c.SetNodeRestart(func() bool { return true }, func(context.Context) error {
		if len(w.patched()) > 0 {
			t.Error("a Deployment was restarted before the Service network answered")
		}
		networkUp = true
		return nil
	})
	if err := c.ensureKnative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !networkUp {
		t.Fatal("the Service network was not waited for")
	}
	got := strings.Join(w.patched(), " ")
	want := "kube-system/coredns knative-serving/activator knative-serving/net-kourier-controller " +
		"knative-serving/webhook kourier-system/3scale-kourier-gateway"
	if got != want {
		t.Errorf("restarted %s, want %s", got, want)
	}
	mu.Lock()
	for _, dep := range []string{"activator", "webhook", "3scale-kourier-gateway"} {
		if polls[dep] < 3 {
			t.Errorf("returned before %s was Ready (%d polls)", dep, polls[dep])
		}
	}
	mu.Unlock()
	if c := r.find("wait --for=condition=Available"); c != "" {
		t.Errorf("waited on the Deployments' stale status: %s", c)
	}
	if r.find("patch configmap/config-deployment") == "" {
		t.Errorf("the install's settings were not applied: %v", r.calls)
	}
}

// The gateway is restarted only once CoreDNS and net-kourier-controller
// have Ready pods of their new templates.
func TestTheGatewayWaitsForItsConfigurationServer(t *testing.T) {
	t.Parallel()
	for _, slow := range []string{"coredns", "net-kourier-controller"} {
		var mu sync.Mutex
		ready := false
		w := newWarmCluster(func(dep string) bool {
			mu.Lock()
			defer mu.Unlock()
			return dep != slow || ready
		})
		r := &recordingRunner{respond: func(call string) (string, error) {
			if strings.Contains(call, "-n kourier-system patch deployment") {
				mu.Lock()
				if !ready {
					t.Errorf("the gateway was restarted before %s was Ready", slow)
				}
				mu.Unlock()
			}
			return w.respond(call)
		}}
		go func() {
			time.Sleep(600 * time.Millisecond)
			mu.Lock()
			ready = true
			mu.Unlock()
		}()
		if err := newTestInstaller(r).RestartKnative(context.Background(), time.Minute); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(w.patched(), " "), "kourier-system/3scale-kourier-gateway") {
			t.Errorf("the gateway was not restarted: %v", w.patched())
		}
	}
}

// Against a cluster that was already running, nothing is restarted.
func TestARunningClusterKeepsKnativesPods(t *testing.T) {
	t.Parallel()
	w := newWarmCluster(func(string) bool { return true })
	r := &recordingRunner{respond: w.respond}
	c := warmComponent(r)
	c.SetNodeRestart(func() bool { return false }, func(context.Context) error {
		t.Error("the Service network was waited for on a running cluster")
		return nil
	})
	if err := c.ensureKnative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := w.patched(); len(p) > 0 {
		t.Errorf("restarted Knative on a running cluster: %v", p)
	}
	if r.find("wait --for=condition=Available deployment --all") == "" {
		t.Errorf("did not wait for Knative: %v", r.calls)
	}
}

// A Service network that never answers fails the start with its reason.
func TestAWarmStartReportsADeadNetwork(t *testing.T) {
	t.Parallel()
	w := newWarmCluster(func(string) bool { return true })
	r := &recordingRunner{respond: w.respond}
	c := warmComponent(r)
	dead := errors.New("the kubernetes Service is not answering")
	c.SetNodeRestart(func() bool { return true }, func(context.Context) error { return dead })
	err := c.ensureKnative(context.Background())
	if !errors.Is(err, dead) || !errors.Is(err, ErrInstallFailed) {
		t.Fatalf("ensureKnative = %v, want the network's error as an install failure", err)
	}
	if p := w.patched(); len(p) > 0 {
		t.Errorf("restarted Knative with no network: %v", p)
	}
}

// New pods that never turn Ready time out, naming the Deployment and how
// many did; pods being deleted, and another Deployment's, do not count.
func TestRestartKnativeTimesOut(t *testing.T) {
	t.Parallel()
	w := newWarmCluster(func(string) bool { return false })
	r := &recordingRunner{respond: w.respond}
	err := newTestInstaller(r).RestartKnative(context.Background(), 300*time.Millisecond)
	if !errors.Is(err, ErrInstallFailed) || !strings.Contains(err.Error(), "kube-system/coredns did not become ready") ||
		!strings.Contains(err.Error(), "0 of 2 new pods Ready") {
		t.Fatalf("RestartKnative = %v", err)
	}
	if p := w.patched(); strings.Contains(strings.Join(p, " "), "kourier-system") {
		t.Errorf("the gateway was restarted before CoreDNS was Ready: %v", p)
	}
}
