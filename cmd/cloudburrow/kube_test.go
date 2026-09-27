package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// fakeKube stands in for kubectl behind kubeInvoker: it records each call's
// arguments and answers every one with out, or err.
type fakeKube struct {
	mu    sync.Mutex
	calls []string
	stdin []io.Reader
	out   string
	err   error
}

func (f *fakeKube) record(args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
}

func (f *fakeKube) Run(_ context.Context, _ string, args ...string) (string, error) {
	f.record(args)
	return f.out, f.err
}

func (f *fakeKube) Pipe(_ context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	f.record(args)
	f.mu.Lock()
	f.stdin = append(f.stdin, stdin)
	f.mu.Unlock()
	_, _ = io.WriteString(stdout, f.out)
	if f.err != nil {
		_, _ = io.WriteString(stderr, "pod says no\n")
	}
	return f.err
}

func (f *fakeKube) Stream(_ context.Context, args ...string) (io.ReadCloser, func() error, error) {
	f.record(args)
	return io.NopCloser(strings.NewReader(f.out)), func() error { return nil }, f.err
}

// useFakeKube points kubeInvoker at a fake for the test.
func useFakeKube(t *testing.T, out string) *fakeKube {
	t.Helper()
	f := &fakeKube{out: out}
	was := kubeInvoker
	kubeInvoker = f
	t.Cleanup(func() { kubeInvoker = was })
	return f
}

func (f *fakeKube) last(t *testing.T) string {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("kubectl was not called")
	}
	return f.calls[len(f.calls)-1]
}

// The console's reads run through internal/k8s with the arguments they
// always had (#599): the kubeconfig first, the verb, then -n or
// --all-namespaces after it.
func TestConsoleReadsKubectlArguments(t *testing.T) {
	ctx := context.Background()
	f := useFakeKube(t, `{"items":[]}`)
	for _, c := range []struct {
		read func() ([]byte, error)
		want string
	}{
		{func() ([]byte, error) { return kubectlJSON(ctx, "/k/config", "cloudburrow", "ksvc") },
			"--kubeconfig /k/config get ksvc -o json -n cloudburrow"},
		{func() ([]byte, error) { return kubectlJSON(ctx, "/k/config", "", "events") },
			"--kubeconfig /k/config get events -o json --all-namespaces"},
		{func() ([]byte, error) { return kubectlSelected(ctx, "/k/config", "default", "pods", "app=x") },
			"--kubeconfig /k/config get pods -o json -l app=x -n default"},
		{func() ([]byte, error) { return kubectlSelected(ctx, "/k/config", "", "pods", "app=x") },
			"--kubeconfig /k/config get pods -o json -l app=x --all-namespaces"},
		{func() ([]byte, error) { return kubectlRaw(ctx, "/k/config", "/api/v1/nodes/n/proxy/stats/summary") },
			"--kubeconfig /k/config get --raw /api/v1/nodes/n/proxy/stats/summary"},
	} {
		out, err := c.read()
		if err != nil || string(out) != `{"items":[]}` {
			t.Errorf("%s: %q, %v", c.want, out, err)
		}
		if got := f.last(t); got != c.want {
			t.Errorf("ran %q, want %q", got, c.want)
		}
	}

	// No kubeconfig is refused before kubectl runs.
	n := len(f.calls)
	if _, err := kubectlJSON(ctx, "", "", "nodes"); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Errorf("kubectlJSON without a kubeconfig = %v", err)
	}
	if _, err := kubectlRaw(ctx, "", "/x"); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Errorf("kubectlRaw without a kubeconfig = %v", err)
	}
	if len(f.calls) != n {
		t.Errorf("kubectl ran without a kubeconfig: %v", f.calls[n:])
	}
}

// A failed read quotes kubectl's stderr, or exec's error when it printed
// nothing, with the same prefix it always had.
func TestConsoleReadsKeepTheirErrorText(t *testing.T) {
	ctx := context.Background()
	f := useFakeKube(t, "")
	exit := errors.New("exit status 1")
	f.err = &k8s.RunError{Err: exit, Stderr: `Error from server (Forbidden): pods is forbidden`}
	if _, err := kubectlJSON(ctx, "/k", "", "pods"); err == nil || err.Error() != `kubectl get pods: Error from server (Forbidden): pods is forbidden` {
		t.Errorf("kubectlJSON error = %v", err)
	}
	if _, err := kubectlSelected(ctx, "/k", "", "pods", "a=b"); err == nil || err.Error() != `Error from server (Forbidden): pods is forbidden` {
		t.Errorf("kubectlSelected error = %v", err)
	}
	if _, err := kubectlRaw(ctx, "/k", "/p"); err == nil || err.Error() != `kubectl get --raw /p: Error from server (Forbidden): pods is forbidden` {
		t.Errorf("kubectlRaw error = %v", err)
	}
	f.err = &k8s.RunError{Err: exit}
	if _, err := kubectlJSON(ctx, "/k", "", "pods"); err == nil || err.Error() != "kubectl get pods: exit status 1" || !errors.Is(err, exit) {
		t.Errorf("kubectlJSON error without stderr = %v", err)
	}
	if _, err := kubectlSelected(ctx, "/k", "", "pods", "a=b"); !errors.Is(err, exit) || err.Error() != "exit status 1" {
		t.Errorf("kubectlSelected error without stderr = %v", err)
	}
}

// The console's log collector lists and follows pods through internal/k8s,
// one namespace named per call, with the arguments it always had.
func TestLogCollectorKubectlArguments(t *testing.T) {
	ctx := context.Background()
	f := useFakeKube(t, `{"items":[{"metadata":{"name":"p","namespace":"default"},"spec":{"containers":[{"name":"app"}]},"status":{"phase":"Running"}}]}`)
	rec := console.NewRecorder(100, time.Now)
	c := newLogCollector("/k/config", []string{"default"}, rec)
	pods, err := c.listPodsIn(ctx, "default")
	if err != nil || len(pods) != 1 || pods[0].name != "p" {
		t.Fatalf("listPodsIn = %+v, %v", pods, err)
	}
	if got, want := f.last(t), "--kubeconfig /k/config -n default get pods -o json"; got != want {
		t.Errorf("listed with %q, want %q", got, want)
	}

	f.out = "2026-09-27T10:00:00Z pod started\n"
	ref := pods[0]
	ref.container = "app"
	c.follow(ctx, ref)
	if got, want := f.last(t), "--kubeconfig /k/config -n default logs p --follow --timestamps -c app --tail 20"; got != want {
		t.Errorf("followed with %q, want %q", got, want)
	}
	entries := rec.Entries(console.Filter{})
	if len(entries) != 1 || entries[0].Message != "pod started" || entries[0].Resource != "p/app" {
		t.Errorf("recorded %+v", entries)
	}
}

// The Cloud SQL snapshotter execs in the server's pod through internal/k8s:
// the instance's kubeconfig and namespace, then exec, -i only with stdin,
// and the command. A failure names the tool and quotes the pod's stderr.
func TestPostgresSnapshotterKubectlArguments(t *testing.T) {
	ctx := context.Background()
	f := useFakeKube(t, "cloudburrow\n")
	var cfg config.Config
	cfg.Name, cfg.StateDir, cfg.Cluster.Namespace = "demo", "/state", "cloudburrow"
	p := newPostgresSnapshotter(cfg)
	g := "--kubeconfig " + cfg.KubeconfigPath() + " -n cloudburrow exec "

	var out bytes.Buffer
	if err := p.exec(ctx, nil, &out, "psql", "-c", "SELECT 1"); err != nil || out.String() != "cloudburrow\n" {
		t.Fatalf("exec = %q, %v", out.String(), err)
	}
	if got, want := f.last(t), g+"deploy/cloudsql -- psql -c SELECT 1"; got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	in := strings.NewReader("archive")
	if err := p.exec(ctx, in, io.Discard, "pg_restore", "-d", "app"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.last(t), g+"-i deploy/cloudsql -- pg_restore -d app"; got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	if f.stdin[1] != in {
		t.Error("the archive was not given to kubectl exec on stdin")
	}

	f.err = errors.New("exit status 1")
	if err := p.exec(ctx, nil, io.Discard, "pg_dump", "app"); err == nil || err.Error() != "pg_dump: exit status 1: pod says no" {
		t.Errorf("a failed exec = %v", err)
	}
}
