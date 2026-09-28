package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary is its own child: with SUPERVISOR_HELPER set it notes
// its start in SUPERVISOR_LOG and then crashes (exit 2) or runs until it
// is killed.
func TestMain(m *testing.M) {
	if mode := os.Getenv("SUPERVISOR_HELPER"); mode != "" {
		f, err := os.OpenFile(os.Getenv("SUPERVISOR_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(3)
		}
		_, _ = f.WriteString(time.Now().Format(time.RFC3339Nano) + "\n")
		_ = f.Close()
		if mode == "crash" {
			time.Sleep(50 * time.Millisecond)
			os.Exit(2)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// starts reads the helper's start times.
func starts(t *testing.T, log string) []time.Time {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []time.Time
	for _, l := range strings.Fields(string(b)) {
		ts, err := time.Parse(time.RFC3339Nano, l)
		if err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, ts)
	}
	return out
}

func helper(t *testing.T, mode string) (Config, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "starts")
	t.Setenv("SUPERVISOR_HELPER", mode)
	t.Setenv("SUPERVISOR_LOG", log)
	c := Defaults
	c.Command = []string{os.Args[0], "-test.run=^$"}
	c.Logf = t.Logf
	return c, log
}

// waitStarts waits until the helper has started n times.
func waitStarts(t *testing.T, log string, n int, within time.Duration) []time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		s := starts(t, log)
		if len(s) >= n {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d starts after %s, want %d", len(s), within, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCrashesRestartPromptly (#1091): a child that crashes again and again
// is started again each time within MaxDelay (1 s) of ending, however many
// times it has crashed: no back-off grows past it, as Kubernetes' does.
func TestCrashesRestartPromptly(t *testing.T) {
	c, log := helper(t, "crash")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, c) }()
	const n = 10 // the first start and 9 restarts
	s := waitStarts(t, log, n, 30*time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		// The child runs 50 ms, then waits at most MaxDelay; allow the
		// process start and a loaded machine.
		if gap := s[i].Sub(s[i-1]); gap > c.MaxDelay+2*time.Second {
			t.Errorf("restart %d came %s after the start before it", i, gap)
		}
	}
}

// fakeFront answers the liveness path 503 from fail() until it sees the
// child started again, as the front's guard does (bigqueryfront/engine.go).
type fakeFront struct {
	mu      sync.Mutex
	failing bool
	checks  atomic.Int32
}

func (f *fakeFront) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.checks.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (f *fakeFront) set(failing bool) {
	f.mu.Lock()
	f.failing = failing
	f.mu.Unlock()
}

// TestLivenessFailuresRestartPromptly (#1091): each time the front fails
// the liveness path, the child is killed and started again within
// seconds, eight times in a row; while the front answers 200, or not at
// all, the child is left running.
func TestLivenessFailuresRestartPromptly(t *testing.T) {
	c, log := helper(t, "serve")
	front := &fakeFront{}
	srv := httptest.NewServer(front)
	defer srv.Close()
	c.Liveness = srv.URL + "/live"
	c.Grace, c.Period, c.Failures = 200*time.Millisecond, 100*time.Millisecond, 2
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, c) }()
	waitStarts(t, log, 1, 10*time.Second)

	// Alive: a second of 200s leaves the child alone.
	time.Sleep(time.Second)
	if n := len(starts(t, log)); n != 1 || front.checks.Load() == 0 {
		t.Fatalf("%d starts, %d checks while the front answered 200", n, front.checks.Load())
	}
	for i := 2; i <= 9; i++ { // 8 restarts
		front.set(true)
		began := time.Now()
		s := waitStarts(t, log, i, 10*time.Second)
		front.set(false)
		if took := s[i-1].Sub(began); took > 5*time.Second {
			t.Errorf("restart %d took %s", i-1, took)
		}
	}
	// No answer at all (the front restarting) is not a failure.
	srv.Close()
	time.Sleep(time.Second)
	if n := len(starts(t, log)); n != 9 {
		t.Errorf("%d starts after the front stopped answering, want 9", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor did not stop the child on SIGTERM")
	}
}

// TestRunUsage: the command comes after the flags.
func TestRunUsage(t *testing.T) {
	var out strings.Builder
	if err := Run(context.Background(), []string{"--liveness", "http://x/"}, &out, &out); err != ErrUsage {
		t.Errorf("no command: %v", err)
	}
	if err := Run(context.Background(), []string{"--", filepath.Join(t.TempDir(), "missing")}, &out, &out); err == nil || err == ErrUsage {
		t.Errorf("a command that cannot start: %v", err)
	}
}

// TestInstallSelf copies the running executable, executable by anyone.
func TestInstallSelf(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "cloudburrow-storage")
	if err := InstallSelf(dst); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	a, _ := os.Stat(self)
	b, err := os.Stat(dst)
	if err != nil || b.Size() != a.Size() || b.Mode().Perm() != 0o755 {
		t.Errorf("installed %v, %v; want %d bytes, 0755", b, err, a.Size())
	}
}
