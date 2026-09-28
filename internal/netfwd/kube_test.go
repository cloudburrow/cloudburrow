package netfwd

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// fakeKube is a k8s Invoker and Starter: it records every kubectl call and
// answers `get` from a table, so the forwarder's kubectl arguments are
// pinned without a cluster or a fake binary.
type fakeKube struct {
	mu       sync.Mutex
	calls    []string
	answers  map[string]string // call suffix -> stdout
	fail     map[string]error  // call suffix -> error
	started  []string
	startErr error
}

func (k *fakeKube) Run(_ context.Context, _ string, args ...string) (string, error) {
	call := strings.Join(args, " ")
	k.mu.Lock()
	k.calls = append(k.calls, call)
	k.mu.Unlock()
	for suffix, err := range k.fail {
		if strings.HasSuffix(call, suffix) {
			return "", err
		}
	}
	for suffix, out := range k.answers {
		if strings.HasSuffix(call, suffix) {
			return out, nil
		}
	}
	return "", errors.New("kubectl: exit status 1: unexpected call")
}

func (k *fakeKube) Start(_, _ io.Writer, args ...string) (k8s.Process, error) {
	k.mu.Lock()
	k.started = append(k.started, strings.Join(args, " "))
	k.mu.Unlock()
	return nil, k.startErr
}

// idleProcess is a kubectl that is running and never exits by itself.
type idleProcess struct{}

func (idleProcess) Wait() error { select {} }
func (idleProcess) Kill() error { return nil }

func fakeForwarder(k *fakeKube) *Forwarder {
	f := New(Target{Name: "spanner", Namespace: "cb", ServicePort: 9010}, "/k/config", "127.0.0.1")
	f.kube = k8s.NewWith(k, "/k/config", "", "cb")
	return f
}

const readyPod = `{"items":[{"metadata":{"name":"spanner-abc","creationTimestamp":"2026-01-01T00:00:00Z"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`

// The forwarder reads the Service, then its pods by selector, with the
// kubeconfig and namespace it was made with and no context, as it did
// before it used internal/k8s.
func TestForwardTargetReadsTheServiceThenItsPods(t *testing.T) {
	k := &fakeKube{answers: map[string]string{
		"get svc spanner -o json":                 `{"spec":{"selector":{"app":"spanner","tier":"db"},"ports":[{"port":9010,"targetPort":9010}]}}`,
		"get pods -l app=spanner,tier=db -o json": readyPod,
	}}
	resource, port, pod := fakeForwarder(k).forwardTarget(context.Background())
	if resource != "pod/spanner-abc" || port != 9010 || pod != "spanner-abc" {
		t.Errorf("forwardTarget = %q %d %q", resource, port, pod)
	}
	want := []string{
		"--kubeconfig /k/config -n cb get svc spanner -o json",
		"--kubeconfig /k/config -n cb get pods -l app=spanner,tier=db -o json",
	}
	if strings.Join(k.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(k.calls, "\n"), strings.Join(want, "\n"))
	}
}

// Any failed read falls back to the Service.
func TestForwardTargetFallsBackToTheService(t *testing.T) {
	k := &fakeKube{fail: map[string]error{"get svc spanner -o json": errors.New("kubectl: exit status 1: Unable to connect to the server")}}
	resource, port, pod := fakeForwarder(k).forwardTarget(context.Background())
	if resource != "svc/spanner" || port != 9010 || pod != "" {
		t.Errorf("forwardTarget = %q %d %q", resource, port, pod)
	}
}

// Only the API server saying NotFound makes the bound pod gone; any other
// failure (an API server under load) keeps the tunnel.
func TestPodAliveTrustsOnlyNotFound(t *testing.T) {
	for msg, alive := range map[string]bool{
		`kubectl: exit status 1: Error from server (NotFound): pods "spanner-abc" not found`: false,
		"kubectl: exit status 1: Unable to connect to the server: EOF":                       true,
		"kubectl: signal: killed": true,
	} {
		k := &fakeKube{fail: map[string]error{"get pod spanner-abc -o json": errors.New(msg)}}
		if got, _ := fakeForwarder(k).podAlive(context.Background(), "spanner-abc"); got != alive {
			t.Errorf("%q: alive = %v, want %v", msg, got, alive)
		}
		if want := "--kubeconfig /k/config -n cb get pod spanner-abc -o json"; len(k.calls) != 1 || k.calls[0] != want {
			t.Errorf("ran %q, want %q", k.calls, want)
		}
	}
	k := &fakeKube{answers: map[string]string{
		"get pod spanner-abc -o json": `{"metadata":{"deletionTimestamp":"2026-01-01T00:00:00Z"}}`,
	}}
	if alive, _ := fakeForwarder(k).podAlive(context.Background(), "spanner-abc"); alive {
		t.Error("a terminating pod reports alive")
	}
}

// The port-forward is started with the Runner's global flags, then the verb,
// its listen address, the chosen pod and the port pair; a failure to start
// is ErrForwardFailed with the words it has always had.
func TestLaunchStartsThePortForward(t *testing.T) {
	k := &fakeKube{
		answers: map[string]string{
			"get svc spanner -o json":         `{"spec":{"selector":{"app":"spanner"},"ports":[{"port":9010,"targetPort":9010}]}}`,
			"get pods -l app=spanner -o json": readyPod,
		},
		startErr: errors.New(`exec: "kubectl": executable file not found in $PATH`),
	}
	f := fakeForwarder(k)
	err := f.launch(context.Background(), true)
	if !errors.Is(err, ErrForwardFailed) {
		t.Fatalf("launch = %v, want ErrForwardFailed", err)
	}
	if want := `port-forward failed: start kubectl port-forward: exec: "kubectl": executable file not found in $PATH`; err.Error() != want {
		t.Errorf("launch = %q, want %q", err, want)
	}
	host := f.HostAddr()
	_, port, _ := strings.Cut(host, "127.0.0.1:")
	if want := "--kubeconfig /k/config -n cb port-forward --address 127.0.0.1 pod/spanner-abc " + port + ":9010"; len(k.started) != 1 || k.started[0] != want {
		t.Errorf("started %q, want %q", k.started, want)
	}
}

// A target with a Service of its own forwards to that Service's pods, and
// pods are still given Name's address (#881).
func TestForwardTargetUsesTheTunnelService(t *testing.T) {
	k := &fakeKube{answers: map[string]string{
		"get svc bigquery-emulator -o json": `{"spec":{"selector":{"app":"bigquery"},"ports":[{"port":9050,"targetPort":9050}]}}`,
		"get pods -l app=bigquery -o json":  strings.ReplaceAll(readyPod, "spanner-abc", "bigquery-abc"),
	}}
	target := Target{Name: "bigquery", Service: "bigquery-emulator", Namespace: "cb", ServicePort: 9050}
	f := New(target, "/k/config", "127.0.0.1")
	f.kube = k8s.NewWith(k, "/k/config", "", "cb")
	if resource, _, _ := f.forwardTarget(context.Background()); resource != "pod/bigquery-abc" {
		t.Errorf("forwardTarget = %q, calls %v", resource, k.calls)
	}
	if got := target.InClusterAddr(); got != "bigquery.cb.svc.cluster.local:9050" {
		t.Errorf("InClusterAddr = %q", got)
	}
	k.fail = map[string]error{"get svc bigquery-emulator -o json": errors.New("kubectl: exit status 1")}
	if resource, _, _ := f.forwardTarget(context.Background()); resource != "svc/bigquery-emulator" {
		t.Errorf("fallback = %q, want the tunnel Service", resource)
	}
}
