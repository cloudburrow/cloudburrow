package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// fakeCluster answers kubectl calls for one instance with two backend pods,
// one owned Cloud Run service with two containers, and a Knative service that
// belongs to someone else.
type fakeCluster struct {
	mu     sync.Mutex
	calls  [][]string
	follow bool
}

const bearerLine = "handling request Authorization: Bearer ya29.super-secret-token-value"

func (f *fakeCluster) kubectl(t *testing.T, kubeconfig string) *kubectl {
	t.Helper()
	record := func(args []string) {
		f.mu.Lock()
		f.calls = append(f.calls, append([]string(nil), args...))
		f.mu.Unlock()
	}
	stamp := func(ago time.Duration, msg string) string {
		return time.Now().Add(-ago).UTC().Format(time.RFC3339Nano) + " " + msg + "\n"
	}
	return &kubectl{
		kubeconfig: kubeconfig,
		run: func(_ context.Context, args []string) ([]byte, error) {
			record(args)
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "get namespace"):
				return []byte("namespace/x"), nil
			case strings.Contains(joined, "get ksvc"):
				if !strings.Contains(joined, "cloudburrow.dev/owned=true,cloudburrow.dev/instance=") {
					return nil, errors.New("an unscoped Knative listing")
				}
				return []byte(`{"items":[{"metadata":{"name":"hello-9f2","annotations":{
					"cloudburrow.dev/cloud-run-name":"projects/p/locations/us-central1/services/hello"}}}]}`), nil
			case strings.Contains(joined, "serving.knative.dev/service=hello-9f2"):
				return []byte(`{"items":[{"metadata":{"name":"hello-9f2-00001-deployment-abc"},
					"spec":{"containers":[{"name":"user-container"},{"name":"queue-proxy"}]}}]}`), nil
			case strings.Contains(joined, "get pods -l cloudburrow.dev/owned=true"):
				return []byte(`{"items":[
					{"metadata":{"name":"pubsub-7d9","labels":{"app":"pubsub"}},"spec":{"containers":[{"name":"pubsub"}]}},
					{"metadata":{"name":"storage-5c1","labels":{"app":"storage"}},"spec":{"containers":[{"name":"storage"}]}}]}`), nil
			}
			return nil, fmt.Errorf("unexpected kubectl %s", joined)
		},
		stream: func(ctx context.Context, args []string) (io.ReadCloser, func() error, error) {
			record(args)
			joined := strings.Join(args, " ")
			var body string
			switch {
			case strings.Contains(joined, "logs pubsub-7d9"):
				body = stamp(3*time.Second, "pubsub emulator started") + stamp(2*time.Second, bearerLine)
			case strings.Contains(joined, "logs storage-5c1"):
				body = stamp(time.Second, "storage ready")
			case strings.Contains(joined, "-c user-container"):
				body = stamp(time.Second, "hello: listening on :8080")
			case strings.Contains(joined, "-c queue-proxy"):
				body = stamp(time.Second, "queue-proxy up")
			}
			if !f.follow {
				return io.NopCloser(strings.NewReader(body)), func() error { return nil }, nil
			}
			pr, pw := io.Pipe()
			go func() {
				_, _ = io.WriteString(pw, body)
				<-ctx.Done()
				_ = pw.Close()
			}()
			return pr, func() error { return nil }, nil
		},
	}
}

func logsConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg, err := config.Load(config.Options{Args: []string{"--name", "logs-test", "--state-dir", t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	kc := cfg.KubeconfigPath()
	if err := os.MkdirAll(filepath.Dir(kc), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, kc
}

// Every kubectl call names the instance's kubeconfig first. A call without
// it would read whatever context the developer's shell points at.
func assertOwnKubeconfig(t *testing.T, calls [][]string, kc string) {
	t.Helper()
	for _, c := range calls {
		if len(c) < 2 || c[0] != "--kubeconfig" || c[1] != kc {
			t.Errorf("kubectl called without the instance's kubeconfig: %v", c)
		}
	}
}

func TestLogsOneServiceTail(t *testing.T) {
	cfg, kc := logsConfig(t)
	f := &fakeCluster{}
	var out strings.Builder
	if err := streamLogs(context.Background(), cfg, logsOptions{service: "pubsub", tail: 20, format: "text"}, f.kubectl(t, kc), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "pubsub emulator started") || strings.Contains(got, "storage ready") {
		t.Errorf("-service pubsub printed:\n%s", got)
	}
	var sawTail bool
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "logs pubsub-7d9 -c pubsub --timestamps --tail 20") {
			sawTail = true
		}
	}
	if !sawTail {
		t.Errorf("the pod was not read by container with --tail 20: %v", f.calls)
	}
	assertOwnKubeconfig(t, f.calls, kc)
}

// A bearer token in a line is redacted, as the console redacts it.
func TestLogsRedactCredentials(t *testing.T) {
	cfg, kc := logsConfig(t)
	f := &fakeCluster{}
	for _, format := range []string{"text", "json"} {
		var out strings.Builder
		if err := streamLogs(context.Background(), cfg, logsOptions{service: "pubsub", tail: 20, format: format}, f.kubectl(t, kc), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "super-secret-token") || !strings.Contains(out.String(), "REDACTED") {
			t.Errorf("%s: a bearer token was printed:\n%s", format, out.String())
		}
	}
}

// Following one Cloud Run service streams only its containers, reached
// through the Knative Service this instance owns, and ends with 130.
func TestLogsFollowOneRunServiceUntilInterrupted(t *testing.T) {
	cfg, kc := logsConfig(t)
	f := &fakeCluster{follow: true}
	ctx, cancel := context.WithCancel(context.Background())
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- streamLogs(ctx, cfg, logsOptions{service: "run", resource: "hello", follow: true, tail: 20, format: "text"}, f.kubectl(t, kc), w)
		_ = w.Close()
	}()
	var got strings.Builder
	buf := make([]byte, 4096)
	deadline := time.After(5 * time.Second)
	for !(strings.Contains(got.String(), "listening on :8080") && strings.Contains(got.String(), "queue-proxy up")) {
		select {
		case <-deadline:
			t.Fatalf("both containers' lines did not arrive:\n%s", got.String())
		default:
		}
		n, _ := r.Read(buf)
		got.Write(buf[:n])
	}
	cancel()
	go func() { _, _ = io.Copy(io.Discard, r) }()
	err := <-done
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != 130 || !exit.quiet {
		t.Fatalf("an interrupted follow returned %v, want a quiet exit 130", err)
	}
	if strings.Contains(got.String(), "pubsub") || strings.Contains(got.String(), "storage") {
		t.Errorf("-service run -resource hello printed other services:\n%s", got.String())
	}
	for _, c := range f.calls {
		if j := strings.Join(c, " "); strings.Contains(j, "logs ") && !strings.Contains(j, "--follow") {
			t.Errorf("a followed container was read without --follow: %v", c)
		}
	}
	assertOwnKubeconfig(t, f.calls, kc)
}

func TestLogsRefuseAnInstanceThatIsNotRunning(t *testing.T) {
	cfg, err := config.Load(config.Options{Args: []string{"--name", "absent", "--state-dir", t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCluster{}
	err = streamLogs(context.Background(), cfg, logsOptions{tail: 10, format: "text"}, f.kubectl(t, cfg.KubeconfigPath()), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("an instance with no cluster returned %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("kubectl was called for an instance with no kubeconfig: %v", f.calls)
	}

	cfg, kc := logsConfig(t)
	k := f.kubectl(t, kc)
	k.run = func(context.Context, []string) ([]byte, error) { return nil, errors.New("connection refused") }
	err = streamLogs(context.Background(), cfg, logsOptions{tail: 10, format: "text"}, k, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Errorf("a stopped cluster returned %v", err)
	}
}

// The in-process services' log is up.log, read with --since and --tail.
func TestLogsInProcessServicesReadUpLog(t *testing.T) {
	cfg, kc := logsConfig(t)
	var b strings.Builder
	for _, l := range []struct {
		ago time.Duration
		msg string
	}{{3 * time.Hour, "old line"}, {2 * time.Minute, "tasks: dispatched"}, {time.Minute, "secretmanager: served"}} {
		b.WriteString(time.Now().Add(-l.ago).UTC().Format(time.RFC3339Nano) + " " + l.msg + "\n")
	}
	if err := os.WriteFile(upLogPath(cfg), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCluster{}
	var out strings.Builder
	if err := streamLogs(context.Background(), cfg, logsOptions{service: "tasks", since: time.Hour, tail: 10, format: "text"}, f.kubectl(t, kc), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "old line") || !strings.Contains(got, "tasks: dispatched") || !strings.Contains(got, "secretmanager: served") {
		t.Errorf("-service tasks -since 1h printed:\n%s", got)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "get pods") {
			t.Errorf("an in-process service listed pods: %v", c)
		}
	}
}

// The real kubectl wrapper removes KUBECONFIG from the child, so the one
// kubeconfig it can read is the one passed on the command line.
func TestTheKubectlWrapperDropsKUBECONFIG(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"KUBECONFIG=[${KUBECONFIG:-}] args=$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBECONFIG", "/home/someone/.kube/production")
	k := newKubectl("/instance/kubeconfig")
	out, err := k.run(context.Background(), k.args("get", "pods"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "KUBECONFIG=[] args=--kubeconfig /instance/kubeconfig get pods" {
		t.Errorf("kubectl ran as: %s", got)
	}
}

func TestLogsFlags(t *testing.T) {
	o, rest, err := logsFlags([]string{"--service", "run", "--resource", "hello", "--follow", "--since", "10m", "--tail", "5", "--format", "json", "--name", "x"})
	if err != nil || o.service != "run" || o.resource != "hello" || !o.follow || o.since != 10*time.Minute || o.tail != 5 || o.format != "json" ||
		strings.Join(rest, " ") != "--name x" {
		t.Errorf("logsFlags = %+v %v %v", o, rest, err)
	}
	for _, bad := range [][]string{
		{"--resource", "hello"},
		{"--service", "dataflow"},
		{"--format", "yaml"},
		{"--tail", "-1"},
		{"--since", "soon"},
	} {
		if _, _, err := logsFlags(bad); err == nil {
			t.Errorf("logsFlags(%v) was accepted", bad)
		}
	}
}
