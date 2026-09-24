package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

// fakeUpEnv turns the test binary into a stand-in for `cloudburrow up`.
//
// runDetached re-executes os.Executable(), which under `go test` is this test
// binary. TestMain intercepts that re-execution and runs fakeUp instead, so
// detach, wait and stop are exercised as real processes — a new session, a
// pidfile, a parent that exits — with nothing but the cluster faked. The
// control server, coordinator and runtime file are the real ones.
const fakeUpEnv = "CLOUDBURROW_TEST_FAKE_UP"

func TestMain(m *testing.M) {
	if delay := os.Getenv(fakeUpEnv); delay != "" && len(os.Args) > 1 && os.Args[1] == "up" {
		os.Exit(fakeUp(os.Args[2:], delay))
	}
	os.Exit(m.Run())
}

// slowComponent stands in for the cluster: it becomes ready after a delay, or
// fails if the delay is "fail".
type slowComponent struct{ delay string }

func (s slowComponent) Name() string { return "cluster" }
func (s slowComponent) Start(ctx context.Context) error {
	if s.delay == "fail" {
		return errors.New("the fake cluster refused to start")
	}
	d, _ := time.ParseDuration(s.delay)
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s slowComponent) Stop(context.Context) error { return nil }

func fakeUp(args []string, delay string) int {
	cfg, err := config.Load(config.Options{Args: args})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	coord := lifecycle.New(5 * time.Second)
	control := lifecycle.NewControlServer(0, coord)
	runtime := &runtimeFile{cfg: cfg, control: control, detached: os.Getenv(detachedEnv) != ""}
	coord.Register(control, runtime, slowComponent{delay})
	if err := coord.Start(ctx); err != nil {
		fmt.Println("startup failed:", err)
		// Held open briefly, as a real `up` reporting its failure would be,
		// so a waiter can read the failed state rather than only a vanished
		// process.
		time.Sleep(500 * time.Millisecond)
		_ = coord.Stop(context.Background())
		return 1
	}
	_ = runtime.Publish(map[string]string{"storage": "127.0.0.1:41999", "control": control.Addr()})
	fmt.Println("press Ctrl-C to stop")
	<-ctx.Done()
	_ = coord.Stop(context.Background())
	return 0
}

func fakeConfigArgs(t *testing.T) ([]string, config.Config) {
	t.Helper()
	args := []string{"--name", "detach-test", "--state-dir", t.TempDir(), "--port-control", "0"}
	cfg, err := config.Load(config.Options{Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return args, cfg
}

// TestDetachWaitStop is the lifecycle end to end: `up --detach` returns only
// once ready, the process outlives it, a second detach is a no-op, `wait`
// agrees, and `stop` ends the process and removes the pidfile.
func TestDetachWaitStop(t *testing.T) {
	t.Setenv(fakeUpEnv, "1500ms")
	args, cfg := fakeConfigArgs(t)

	var out, errOut strings.Builder
	start := time.Now()
	if err := runDetached(args, 20*time.Second, &out, &errOut); err != nil {
		t.Fatalf("up --detach: %v\n%s%s", err, out.String(), errOut.String())
	}
	if time.Since(start) < 1500*time.Millisecond {
		t.Errorf("up --detach returned after %v, before the instance could be ready", time.Since(start))
	}
	info, ok := running(cfg)
	if !ok {
		t.Fatalf("no running instance after up --detach; runtime file: %+v", info)
	}
	t.Cleanup(func() { _ = syscall.Kill(info.PID, syscall.SIGKILL) })
	if info.Endpoints["storage"] != "127.0.0.1:41999" {
		t.Errorf("the running instance's endpoints were not recorded: %+v", info)
	}
	if !info.Detached || info.Log == "" {
		t.Errorf("the runtime file does not record a detached process with its log: %+v", info)
	}
	resp, err := http.Get("http://" + info.Control + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz after up --detach returned: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	// The child is in a session of its own, so the shell that started it
	// exiting does not take it along.
	if sid, _ := syscall.Getsid(info.PID); sid != info.PID {
		t.Errorf("the background process is not a session leader (sid %d, pid %d)", sid, info.PID)
	}

	// Idempotent: the same process, not a second one.
	out.Reset()
	if err := runDetached(args, 5*time.Second, &out, &errOut); err != nil {
		t.Fatalf("a second up --detach failed: %v", err)
	}
	if again, _ := running(cfg); again.PID != info.PID || !strings.Contains(out.String(), "already running") {
		t.Errorf("a second up --detach started pid %d (first %d): %s", again.PID, info.PID, out.String())
	}

	out.Reset()
	if err := runWait(append([]string{"--timeout", "5s"}, args...), &out, &errOut); err != nil {
		t.Errorf("wait on a ready instance: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := stopRunning(cfg, &out); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if isCloudBurrow(info.PID) {
		t.Error("the process survived stop")
	}
	if _, err := os.Stat(runtimePath(cfg)); !os.IsNotExist(err) {
		t.Errorf("stop left the pidfile behind: %v", err)
	}
}

// A detach whose instance fails to start exits 1 and shows why.
func TestDetachReportsAFailedStart(t *testing.T) {
	t.Setenv(fakeUpEnv, "fail")
	args, _ := fakeConfigArgs(t)
	var out, errOut strings.Builder
	err := runDetached(args, 20*time.Second, &out, &errOut)
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != exitFailed {
		t.Fatalf("a failed start returned %v, want exit status 1", err)
	}
	if !strings.Contains(errOut.String(), "the fake cluster refused to start") {
		t.Errorf("the log tail does not explain the failure:\n%s", errOut.String())
	}
}

// fakeControl serves a fixed /readyz answer, recorded in a runtime file as
// if a running `up` had written it.
func fakeControl(t *testing.T, cfg config.Config, status int, r readiness) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(r)
	}))
	t.Cleanup(srv.Close)
	b, _ := json.Marshal(runtimeInfo{PID: os.Getpid(), Control: strings.TrimPrefix(srv.URL, "http://"),
		Endpoints: map[string]string{"control": strings.TrimPrefix(srv.URL, "http://")}})
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWaitExitStatuses(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		r      readiness
		code   int
		says   []string
	}{
		{"ready", 200, readiness{Ready: true, State: "running", Components: map[string]bool{"cluster": true}}, exitReady, []string{"is ready"}},
		{"failed", 503, readiness{State: "failed", Components: map[string]bool{"cluster": false}, Error: "kind: no docker"}, exitFailed, []string{"failed to start", "kind: no docker"}},
		{"starting", 503, readiness{State: "starting", Components: map[string]bool{"control": true, "cluster": false, "forward:storage": false}},
			exitTimeout, []string{"not ready: cluster, forward:storage", "cluster                      NOT READY"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args, cfg := fakeConfigArgs(t)
			fakeControl(t, cfg, c.status, c.r)
			var out, errOut strings.Builder
			err := runWait(append([]string{"--timeout", "1s"}, args...), &out, &errOut)
			code := exitReady
			var exit *exitError
			if errors.As(err, &exit) {
				code = exit.code
			} else if err != nil {
				t.Fatalf("wait returned %v", err)
			}
			if code != c.code {
				t.Errorf("exit status %d, want %d\n%s", code, c.code, out.String())
			}
			for _, s := range c.says {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output lacks %q:\n%s", s, out.String())
				}
			}
		})
	}
}

// A pidfile whose process is gone must not block the next start, and must
// never lead `stop` to signal whatever now holds that pid.
func TestAStalePidfileIsIgnoredAndCleared(t *testing.T) {
	_, cfg := fakeConfigArgs(t)
	// A pid far above any real one on these systems.
	dead := 1 << 22
	b, _ := json.Marshal(runtimeInfo{PID: dead, Control: "127.0.0.1:1"})
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := running(cfg); ok {
		t.Fatal("a dead pid was reported as running")
	}
	if err := stopRunning(cfg, &strings.Builder{}); err != nil {
		t.Fatalf("stop with a stale pidfile: %v", err)
	}
	if _, err := os.Stat(runtimePath(cfg)); !os.IsNotExist(err) {
		t.Error("the stale pidfile was not removed")
	}
	// A live process that is not CloudBurrow's is not one of ours either.
	if isCloudBurrow(1) {
		t.Error("pid 1 was taken for a CloudBurrow process")
	}
}

func TestUpFlagsAreSplitFromTheSharedOnes(t *testing.T) {
	for _, c := range []struct {
		in      []string
		detach  bool
		timeout time.Duration
		rest    string
	}{
		{[]string{"--name", "x"}, false, 10 * time.Minute, "--name x"},
		{[]string{"--detach", "--name", "x"}, true, 10 * time.Minute, "--name x"},
		{[]string{"-detach=true", "--detach-timeout", "90s"}, true, 90 * time.Second, ""},
		{[]string{"--name", "x", "--detach=false", "--detach-timeout=1m"}, false, time.Minute, "--name x"},
	} {
		detach, timeout, rest, err := upFlags(c.in)
		if err != nil || detach != c.detach || timeout != c.timeout || strings.Join(rest, " ") != c.rest {
			t.Errorf("upFlags(%v) = %v %v %v %v", c.in, detach, timeout, rest, err)
		}
	}
	if _, _, _, err := upFlags([]string{"--detach-timeout"}); err == nil {
		t.Error("a missing -detach-timeout value was accepted")
	}
}

// `env` exported configured ports only, so an instance started with
// --port-storage 0 had STORAGE_EMULATOR_HOST=...:0 exported for it. With a
// running instance it now exports the ports actually bound (#278).
func TestEnvExportsTheLivePortsOfARunningInstance(t *testing.T) {
	_, cfg := fakeConfigArgs(t)
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceSpanner}
	cfg.Endpoints.Storage, cfg.Endpoints.PubSub, cfg.Endpoints.Spanner = 0, 0, 0
	live := withLivePorts(cfg, map[string]string{
		"storage": "127.0.0.1:41001", "pubsub": "127.0.0.1:41002", "spanner": "127.0.0.1:41003",
	})
	got := map[string]string{}
	for _, v := range envVars(live, "p", "") {
		got[v.Name] = v.Value
	}
	for name, want := range map[string]string{
		"STORAGE_EMULATOR_HOST": "http://127.0.0.1:41001",
		"PUBSUB_EMULATOR_HOST":  "127.0.0.1:41002",
		"SPANNER_EMULATOR_HOST": "127.0.0.1:41003",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	if u := unexportableEmulators(live); len(u) != 0 {
		t.Errorf("a live port was still reported unexportable: %v", u)
	}
}
