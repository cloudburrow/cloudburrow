package hostai

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary is its own child server: HOSTAI_CHILD=1 makes it serve
// /health on HOSTAI_PORT, with the behaviours a test asks for.
func TestMain(m *testing.M) {
	if os.Getenv("HOSTAI_CHILD") == "1" {
		child()
		return
	}
	os.Exit(m.Run())
}

func child() {
	port := os.Getenv("HOSTAI_PORT")
	var mu sync.Mutex
	healthy := true
	start := time.Now()
	if d, err := time.ParseDuration(os.Getenv("HOSTAI_UNHEALTHY_AFTER")); err == nil {
		go func() {
			time.Sleep(d)
			mu.Lock()
			healthy = false
			mu.Unlock()
		}()
	}
	if d, err := time.ParseDuration(os.Getenv("HOSTAI_EXIT_AFTER")); err == nil {
		go func() {
			time.Sleep(d)
			fmt.Println("child: exiting on purpose")
			os.Exit(3)
		}()
	}
	if os.Getenv("HOSTAI_SPAWN") != "" {
		c := exec.Command("sleep", "300")
		_ = c.Start()
		_ = os.WriteFile(os.Getenv("HOSTAI_SPAWN"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
	}
	fmt.Println("child: serving on", port)
	fmt.Fprintln(os.Stderr, "child: stderr line")
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "ok %s", time.Since(start))
	})
	_ = http.ListenAndServe("127.0.0.1:"+port, nil)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) add(stream, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, stream+": "+line)
}

func (l *lines) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.all {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func childSpec(t *testing.T, env ...string) (Spec, *lines) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	l := &lines{}
	return Spec{Name: "test-server", Path: exe, Env: append([]string{"HOSTAI_CHILD=1", "HOSTAI_PORT=" + port}, env...),
		Health: "http://127.0.0.1:" + port + "/health", Log: l.add,
		Period: 100 * time.Millisecond, Failures: 2, MinDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond,
		MinUptime: time.Second, StopTimeout: 2 * time.Second}, l
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestProcessServesLogsAndStops(t *testing.T) {
	spec, l := childSpec(t)
	pids := &Pids{File: filepath.Join(t.TempDir(), "pids.json")}
	spec.Pids = pids
	p, err := Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	pid := p.PID()
	waitFor(t, "both streams", func() bool { return l.has("stdout: child: serving") && l.has("stderr: child: stderr line") })
	if !l.has("supervisor: cloudburrow: started test-server") {
		t.Errorf("no start event: %v", l.all)
	}
	if es := pids.read(); len(es) != 1 || es[0].PID != pid {
		t.Errorf("pid file = %+v, want the running pid %d", es, pid)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Running() || alive(pid) {
		t.Errorf("after Stop: running %v, pid %d alive %v", p.Running(), pid, alive(pid))
	}
	if es := pids.read(); len(es) != 0 {
		t.Errorf("pid file after Stop = %+v, want empty", es)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("a second Stop: %v", err)
	}
}

func TestProcessRestartsWhenItExits(t *testing.T) {
	spec, l := childSpec(t, "HOSTAI_EXIT_AFTER=400ms")
	p, err := Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	waitFor(t, "a restart", func() bool { return p.Starts() >= 2 })
	if !l.has("ended (exit status 3); starting it again") {
		t.Errorf("no restart event: %v", l.all)
	}
	waitFor(t, "healthy after the restart", p.Healthy)
}

func TestProcessKilledWhenUnhealthy(t *testing.T) {
	spec, l := childSpec(t, "HOSTAI_UNHEALTHY_AFTER=500ms")
	p, err := Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	if err := p.Wait(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	first := p.PID()
	waitFor(t, "the unhealthy server's kill and restart", func() bool { return p.Starts() >= 2 })
	if !l.has("failed 2 health checks in a row; killing it") {
		t.Errorf("no kill event: %v", l.all)
	}
	waitFor(t, "the first child gone", func() bool { return !alive(first) })
}

func TestProcessStartTimeout(t *testing.T) {
	spec, l := childSpec(t)
	spec.Health = "http://127.0.0.1:1/never" // never answers
	spec.StartTimeout = 300 * time.Millisecond
	p, err := Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	waitFor(t, "a kill after the start timeout", func() bool { return p.Starts() >= 2 })
	if !l.has("did not become healthy within 300ms") {
		t.Errorf("no start-timeout event: %v", l.all)
	}
	if err := p.Wait(context.Background(), 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("Wait = %v, want a timeout naming the server", err)
	}
}

func TestStopEndsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild")
	spec, _ := childSpec(t, "HOSTAI_SPAWN="+marker)
	p, err := Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(context.Background(), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	var gc int
	waitFor(t, "the grandchild", func() bool {
		b, _ := os.ReadFile(marker)
		gc, _ = strconv.Atoi(string(b))
		return gc > 0
	})
	if !alive(gc) {
		t.Fatalf("grandchild %d is not running", gc)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the grandchild ended with its group", func() bool { return !alive(gc) || zombie(gc) })
}

// zombie reports a process that has ended but is not yet reaped (its
// parent, the killed child, is gone, so launchd reaps it shortly).
func zombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestPidsReap(t *testing.T) {
	sleep, _ := exec.LookPath("sleep")
	c := exec.Command(sleep, "300")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { _ = c.Wait(); close(ended) }()
	pids := &Pids{File: filepath.Join(t.TempDir(), "pids.json")}
	pids.Add("left-behind", c.Process.Pid, sleep)
	// A recorded pid now running another program is left alone.
	pids.Add("reused", os.Getpid(), "/no/such/server")
	got := pids.Reap()
	if len(got) != 1 || got[0] != "left-behind" {
		t.Errorf("Reap = %v, want [left-behind]", got)
	}
	waitFor(t, "the reaped server ended", func() bool {
		select {
		case <-ended:
			return true
		default:
			return false
		}
	})
	if es := pids.read(); len(es) != 0 {
		t.Errorf("pid file after Reap = %+v", es)
	}
}
