package netfwd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake kubectl (#526): the test binary re-executes itself as kubectl,
// reading its world from a directory the test edits:
//
//   - pods.json is what `get pods` answers;
//   - pod-<name> exists while `get pod <name>` finds it, with the content
//     "deleting" for a pod with a deletionTimestamp;
//   - refuse-<name> makes a port-forward to that pod report a failed stream
//     on its first connection, as kubectl does for a pod that is not
//     listening or is gone;
//   - launches.log records each port-forward's resource, one per line.
//
// `port-forward` listens on the requested host port and holds every
// connection open, which is what a healthy tunnel looks like from outside.
const fakeEnv = "NETFWD_FAKE_KUBECTL"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeEnv); dir != "" {
		os.Exit(fakeKubectl(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeKubectl(dir string, args []string) int {
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--kubeconfig", "-n", "--address":
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		return 2
	}
	switch rest[0] {
	case "get":
		switch {
		case rest[1] == "svc":
			fmt.Printf(`{"spec":{"selector":{"app":%q},"ports":[{"port":1,"targetPort":1}]}}`, rest[2])
			return 0
		case rest[1] == "pods":
			b, err := os.ReadFile(filepath.Join(dir, "pods.json"))
			if err != nil {
				return 1
			}
			os.Stdout.Write(b)
			return 0
		case rest[1] == "pod":
			b, err := os.ReadFile(filepath.Join(dir, "pod-"+rest[2]))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error from server (NotFound): pods %q not found\n", rest[2])
				return 1
			}
			if strings.TrimSpace(string(b)) == "deleting" {
				fmt.Printf(`{"metadata":{"name":%q,"deletionTimestamp":"2026-01-01T00:00:00Z"}}`, rest[2])
			} else {
				fmt.Printf(`{"metadata":{"name":%q}}`, rest[2])
			}
			return 0
		}
	case "port-forward":
		resource, spec := rest[1], rest[2]
		f, _ := os.OpenFile(filepath.Join(dir, "launches.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, resource)
		f.Close()
		port, _, _ := strings.Cut(spec, ":")
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			fmt.Fprintln(os.Stderr, "unable to listen:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Forwarding from 127.0.0.1:%s -> 1\n", port)
		_, refuse := os.Stat(filepath.Join(dir, "refuse-"+strings.TrimPrefix(resource, "pod/")))
		for {
			c, err := ln.Accept()
			if err != nil {
				return 0
			}
			fmt.Fprintf(os.Stderr, "Handling connection for %s\n", port)
			if refuse == nil {
				fmt.Fprintf(os.Stderr, "E0000 portforward.go:522] \"Unhandled Error\" err=\"an error occurred forwarding %s -> 1: error forwarding port 1 to pod abc, uid : connect: connection refused\"\n", port)
				c.Close()
				continue
			}
			go func() { <-time.After(time.Hour); c.Close() }()
		}
	}
	return 2
}

// fakeWorld puts the fake kubectl first on PATH and returns its directory.
func fakeWorld(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "kubectl")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeEnv, dir)
	return dir
}

type fakePod struct {
	name  string
	ready bool
}

func podsJSON(t *testing.T, dir string, pods ...fakePod) {
	t.Helper()
	items := make([]map[string]any, 0, len(pods))
	for i, p := range pods {
		status := "False"
		if p.ready {
			status = "True"
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": p.name, "creationTimestamp": time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)},
			"spec":     map[string]any{"containers": []any{}},
			"status":   map[string]any{"conditions": []map[string]any{{"type": "Ready", "status": status}}},
		})
		if err := os.WriteFile(filepath.Join(dir, "pod-"+p.name), []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	if err := os.WriteFile(filepath.Join(dir, "pods.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func launches(t *testing.T, dir string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "launches.log"))
	return strings.Fields(string(b))
}

// newFakeForwarder returns the forwarder and a reader of what it logged.
func newFakeForwarder(t *testing.T, dir string) (*Forwarder, func() string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	f := New(Target{Name: "spanner", Namespace: "cb", ServicePort: 1}, filepath.Join(dir, "kubeconfig"), "127.0.0.1")
	f.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { _ = f.Stop(context.Background()) })
	return f, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not within %s: %s", within, what)
}

// A tunnel whose pod is deleted while idle is replaced by the supervisor,
// bound to the new Ready pod, without any client tripping over it: kubectl
// itself keeps running after its pod is gone (#526, measured).
func TestATunnelWhosePodIsGoneIsReplacedWithoutAClient(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	f, logged := newFakeForwarder(t, dir)
	old := podWatch
	podWatch = 200 * time.Millisecond
	t.Cleanup(func() { podWatch = old })

	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || got[0] != "pod/spanner-a" {
		t.Fatalf("first launch bound %v, want pod/spanner-a", got)
	}

	// The pod is deleted and a new one comes up Ready; nobody connects.
	if err := os.Remove(filepath.Join(dir, "pod-spanner-a")); err != nil {
		t.Fatal(err)
	}
	podsJSON(t, dir, fakePod{"spanner-b", true})
	waitFor(t, 5*time.Second, "the tunnel re-established to pod/spanner-b", func() bool {
		got := launches(t, dir)
		return f.Restarts() == 1 && len(got) == 2 && got[1] == "pod/spanner-b"
	})
	if !f.Running() {
		t.Error("the re-established tunnel reports not running")
	}
	joined := logged()
	if !strings.Contains(joined, "pod/spanner-a is gone") || !strings.Contains(joined, "re-established to pod/spanner-b") {
		t.Errorf("the log does not say what happened:\n%s", joined)
	}
}

// After a restart the tunnel binds only a Ready pod, never the Service:
// while the new pod is not Ready it waits, and a terminating pod is skipped.
func TestRelaunchWaitsForAReadyPodInsteadOfTheService(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	f, _ := newFakeForwarder(t, dir)
	old := podWatch
	podWatch = 200 * time.Millisecond
	t.Cleanup(func() { podWatch = old })
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The old pod is terminating and the new one is not Ready yet.
	podsJSON(t, dir, fakePod{"spanner-a", true}, fakePod{"spanner-b", false})
	if err := os.WriteFile(filepath.Join(dir, "pod-spanner-a"), []byte("deleting"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pods.json"))
	s := strings.Replace(string(b), `"name":"spanner-a"`, `"name":"spanner-a","deletionTimestamp":"2026-01-01T00:00:00Z"`, 1)
	_ = os.WriteFile(filepath.Join(dir, "pods.json"), []byte(s), 0o644)

	waitFor(t, 5*time.Second, "the supervisor noticed the terminating pod", func() bool { return !f.Running() })
	time.Sleep(1200 * time.Millisecond) // several retry rounds with nothing Ready
	for _, l := range launches(t, dir)[1:] {
		t.Errorf("launched %s while no pod was Ready; a relaunch must wait, never fall back to the Service", l)
	}

	podsJSON(t, dir, fakePod{"spanner-b", true})
	waitFor(t, 5*time.Second, "the tunnel re-established to pod/spanner-b", func() bool {
		got := launches(t, dir)
		return len(got) == 2 && got[1] == "pod/spanner-b" && f.Running()
	})
}

// A launch whose stream fails is not a tunnel: kubectl accepting a
// connection it cannot carry is retried until the pod answers.
func TestALaunchWhoseStreamFailsIsRetried(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	f, logged := newFakeForwarder(t, dir)
	old := podWatch
	podWatch = 200 * time.Millisecond
	t.Cleanup(func() { podWatch = old })
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The pod restarts into one that is Ready but refuses its port.
	if err := os.WriteFile(filepath.Join(dir, "refuse-spanner-b"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "pod-spanner-a"))
	podsJSON(t, dir, fakePod{"spanner-b", true})
	waitFor(t, 5*time.Second, "a launch to pod/spanner-b was refused and logged", func() bool {
		return strings.Contains(logged(), "accepts but does not carry")
	})
	if f.Running() {
		t.Error("a tunnel whose stream fails reports running")
	}

	// The pod starts listening; the next retry carries.
	_ = os.Remove(filepath.Join(dir, "refuse-spanner-b"))
	waitFor(t, 8*time.Second, "the tunnel carried after the pod started listening", func() bool {
		return f.Running() && f.Restarts() == 1
	})
	if n := len(launches(t, dir)); n < 3 {
		t.Errorf("%d launches, want the first, a refused one and a carrying one", n)
	}
}

// Start waits for a Ready pod rather than binding the Service at once, so
// the tunnel can be watched from the start.
func TestStartWaitsBrieflyForAReadyPod(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", false})
	f, logged := newFakeForwarder(t, dir)
	old := startPodWait
	startPodWait = 3 * time.Second
	t.Cleanup(func() { startPodWait = old })
	go func() {
		time.Sleep(700 * time.Millisecond)
		podsJSON(t, dir, fakePod{"spanner-a", true})
	}()
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || got[0] != "pod/spanner-a" {
		t.Errorf("Start launched %v; want one launch, to pod/spanner-a once it was Ready", got)
	}
	if !strings.Contains(logged(), "bound to pod/spanner-a") {
		t.Errorf("the start was not logged:\n%s", logged())
	}
}

// With no pod Ready in time, Start binds the Service and says so.
func TestStartFallsBackToTheServiceAndSaysSo(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", false})
	f, logged := newFakeForwarder(t, dir)
	old := startPodWait
	startPodWait = 600 * time.Millisecond
	t.Cleanup(func() { startPodWait = old })
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := launches(t, dir); len(got) != 1 || got[0] != "svc/spanner" {
		t.Errorf("Start launched %v; want svc/spanner after the wait", got)
	}
	if !strings.Contains(logged(), "bound to the Service") {
		t.Errorf("the fallback was not logged:\n%s", logged())
	}
}

// A first launch that binds a Ready pod and does not carry used to fail
// Start, and so `up`, at once: after stop and up a pod keeps its name and
// its Ready condition for a moment while its sandbox is recreated (#572).
// Start now retries within its pod wait, as it does when no pod is Ready.
func TestStartRetriesALaunchThatDoesNotCarry(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	if err := os.WriteFile(filepath.Join(dir, "refuse-spanner-a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, logged := newFakeForwarder(t, dir)
	old := startPodWait
	startPodWait = 20 * time.Second // one launch is seconds under -race
	t.Cleanup(func() { startPodWait = old })
	// The pod starts listening once the first launch has been refused: the
	// fake reads the refusal when it starts, so lifting it earlier would
	// let the first launch carry.
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && !strings.Contains(logged(), "retrying") {
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.Remove(filepath.Join(dir, "refuse-spanner-a"))
	}()
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("Start failed on a launch that did not carry at first: %v", err)
	}
	if n := len(launches(t, dir)); n < 2 {
		t.Errorf("%d launches, want a refused one and a carrying one", n)
	}
	if !strings.Contains(logged(), "does not carry") || !strings.Contains(logged(), "retrying") {
		t.Errorf("the retry was not logged:\n%s", logged())
	}
	if !strings.Contains(logged(), "bound to pod/spanner-a") {
		t.Errorf("the start was not logged:\n%s", logged())
	}
}

// A launch that never carries within the pod wait still fails Start, with
// the reason: Start does not wait forever, and it does not fall back to the
// Service for a pod that answers nothing.
func TestStartGivesUpOnALaunchThatNeverCarries(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	if err := os.WriteFile(filepath.Join(dir, "refuse-spanner-a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := newFakeForwarder(t, dir)
	old := startPodWait
	startPodWait = 5 * time.Second // room for more than one launch under -race
	t.Cleanup(func() { startPodWait = old })
	err := f.Start(context.Background())
	if !errors.Is(err, ErrForwardFailed) {
		t.Fatalf("Start = %v, want ErrForwardFailed after the pod wait", err)
	}
	if n := len(launches(t, dir)); n < 2 {
		t.Errorf("%d launches, want more than one attempt within the wait", n)
	}
}
