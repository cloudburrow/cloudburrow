package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// selfStart is this test process's start time, so a runtime file naming the
// test process reads as a live `up`.
func selfStart(t *testing.T) string {
	t.Helper()
	s, err := procStartTime(os.Getpid())
	if err != nil {
		t.Skipf("the process start time is not readable here: %v", err)
	}
	return s
}

// fakeProcessTable replaces the process table isOurUp reads.
func fakeProcessTable(t *testing.T, alive map[int]bool, start map[int]string, comm map[int]string) {
	t.Helper()
	oldAlive, oldStart, oldComm := pidAlive, pidStartTime, pidCommand
	t.Cleanup(func() { pidAlive, pidStartTime, pidCommand = oldAlive, oldStart, oldComm })
	pidAlive = func(pid int) bool { return alive[pid] }
	pidStartTime = func(pid int) (string, error) {
		if s, ok := start[pid]; ok {
			return s, nil
		}
		return "", errors.New("no such process")
	}
	pidCommand = func(pid int) (string, error) {
		if c, ok := comm[pid]; ok {
			return c, nil
		}
		return "", errors.New("no such process")
	}
}

// An `up` is identified by its pid and the start time recorded beside it,
// not by its binary's name (#820).
func TestIsOurUpIsTheRecordedPidAndStartTime(t *testing.T) {
	fakeProcessTable(t,
		map[int]bool{100: true, 200: true, 300: true, 400: true},
		map[int]string{100: "linux:5000", 200: "linux:7000", 300: "linux:9000", 400: "linux:1"},
		map[int]string{100: "cb798", 200: "/usr/local/bin/cloudburrow", 300: "bash", 400: "cloudburrow"})
	for _, c := range []struct {
		name string
		info runtimeInfo
		want bool
	}{
		{"a binary of another name", runtimeInfo{PID: 100, ProcStart: "linux:5000"}, true},
		{"a binary named cloudburrow", runtimeInfo{PID: 200, ProcStart: "linux:7000"}, true},
		// The pid was reused: by an unrelated process, and even by another
		// cloudburrow, neither of which is the `up` that wrote the file.
		{"a reused pid", runtimeInfo{PID: 300, ProcStart: "linux:5000"}, false},
		{"a pid reused by another cloudburrow", runtimeInfo{PID: 400, ProcStart: "linux:5000"}, false},
		{"a dead pid", runtimeInfo{PID: 500, ProcStart: "linux:5000"}, false},
		{"no pid", runtimeInfo{ProcStart: "linux:5000"}, false},
		// A runtime file from before the start time was recorded is checked
		// by the process's name, as it was then.
		{"an old file naming cloudburrow", runtimeInfo{PID: 200}, true},
		{"an old file naming another process", runtimeInfo{PID: 300}, false},
	} {
		if got := isOurUp(c.info); got != c.want {
			t.Errorf("%s: isOurUp(%+v) = %v, want %v", c.name, c.info, got, c.want)
		}
	}
}

// A runtime file written before the start time was recorded still reads.
func TestAnOldRuntimeFileStillReads(t *testing.T) {
	_, cfg := fakeConfigArgs(t)
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"pid": 4242, "control": "127.0.0.1:9090", "detached": true, "started": "2026-09-01T00:00:00Z"}`
	if err := os.WriteFile(runtimePath(cfg), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeProcessTable(t, map[int]bool{4242: true}, nil, map[int]string{4242: "cloudburrow"})
	info, ok := running(cfg)
	if !ok || info.PID != 4242 || info.Control != "127.0.0.1:9090" || info.ProcStart != "" {
		t.Errorf("an old runtime file read as %+v, running %v", info, ok)
	}
}

// startStandIn starts a real process to stand in for `up`: its name is not
// cloudburrow's, and it is reaped as soon as it exits, so it does not linger
// as a zombie that still reads as alive.
func startStandIn(t *testing.T) (pid int, exited <-chan struct{}) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep(1) to stand in for up")
	}
	cmd := exec.Command(sleep, "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid, done
}

func writeRuntimeFile(t *testing.T, cfg config.Config, info runtimeInfo) {
	t.Helper()
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(info)
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A recorded pid now owned by an unrelated process is never signalled: the
// live process's start time is not the one recorded, so the file is stale.
func TestStopNeverSignalsAReusedPid(t *testing.T) {
	_, cfg := fakeConfigArgs(t)
	pid, exited := startStandIn(t)
	real, err := procStartTime(pid)
	if err != nil {
		t.Skipf("the process start time is not readable here: %v", err)
	}
	// The `up` that wrote this file had the same pid and started earlier.
	writeRuntimeFile(t, cfg, runtimeInfo{PID: pid, ProcStart: real + "-earlier", Control: "127.0.0.1:1"})
	if _, ok := running(cfg); ok {
		t.Fatal("a reused pid was reported as the running instance")
	}
	var out strings.Builder
	if err := stopRunning(cfg, &out); err != nil {
		t.Fatalf("stop with a reused pid: %v", err)
	}
	select {
	case <-exited:
		t.Fatal("stop signalled the process that reused the recorded pid")
	case <-time.After(300 * time.Millisecond):
	}
	if strings.Contains(out.String(), "stopped") {
		t.Errorf("stop reported stopping a process it must not signal: %s", out.String())
	}
	if _, err := os.Stat(runtimePath(cfg)); !os.IsNotExist(err) {
		t.Error("the stale runtime file was not removed")
	}
}

// The CLI built under another name finds an instance whose `up` is not
// named cloudburrow either: `env` prints its endpoints, `status` reports it
// with its control address, and `stop` ends it (#820). The `up` is a stand-in
// process with a runtime file and a control server of its own, which is all
// the three commands read.
func TestTheCLIUnderAnotherNameFindsTheInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	bin := filepath.Join(t.TempDir(), "cb-other")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the CLI as cb-other: %v\n%s", err, out)
	}

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(readiness{Ready: true, State: "ready", Components: map[string]bool{"cluster": true}})
	}))
	t.Cleanup(control.Close)
	controlAddr := strings.TrimPrefix(control.URL, "http://")

	dir := t.TempDir()
	name := fmt.Sprintf("identity-%d", os.Getpid())
	cfg, err := config.Load(config.Options{Args: []string{"--name", name, "--state-dir", dir}})
	if err != nil {
		t.Fatal(err)
	}
	pid, exited := startStandIn(t)
	start, err := procStartTime(pid)
	if err != nil {
		t.Skipf("the process start time is not readable here: %v", err)
	}
	writeRuntimeFile(t, cfg, runtimeInfo{PID: pid, ProcStart: start, Control: controlAddr, Started: time.Now().UTC(),
		Services:  []config.Service{config.ServiceStorage},
		Endpoints: map[string]string{"storage": "127.0.0.1:9722", "control": controlAddr}})

	// No docker, kind or kubectl on PATH: nothing here may reach a cluster.
	emptyPath := t.TempDir()
	cli := func(args ...string) (string, error) {
		cmd := exec.Command(bin, append(args, "--name", name, "--state-dir", dir)...)
		cmd.Env = append(os.Environ(), "PATH="+emptyPath, "HOME="+t.TempDir())
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err != nil {
			err = fmt.Errorf("%w\n%s", err, stderr.String())
		}
		return stdout.String(), err
	}

	out, err := cli("env", "--format", "plain")
	if err != nil || !strings.Contains(out, "STORAGE_EMULATOR_HOST=http://127.0.0.1:9722") {
		t.Errorf("cb-other env = %v, output:\n%s", err, out)
	}

	// status exits non-zero for a cluster it cannot see; the report is what
	// is checked.
	out, _ = cli("status", "--format", "json")
	var report struct {
		ControlURL string `json:"control_url"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.ControlURL != "http://"+controlAddr {
		t.Errorf("cb-other status did not report the running instance (control_url %q, %v):\n%s", report.ControlURL, err, out)
	}

	// stop fails at the cluster, which is not there; the process goes first.
	out, _ = cli("stop")
	if !strings.Contains(out, fmt.Sprintf("stopped cloudburrow up (pid %d)", pid)) {
		t.Errorf("cb-other stop did not stop the instance:\n%s", out)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Error("the instance's process survived cb-other stop")
	}
}
