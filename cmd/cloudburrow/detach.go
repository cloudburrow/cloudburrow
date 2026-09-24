package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hooks"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

// Running `up` in the background, waiting for it, and stopping it (#278).
//
// `up` hosts Cloud Tasks, Secret Manager, the metadata server and every
// port-forward in its own process, so it has to keep running. That made it
// awkward in CI: the job backgrounded it by hand, grepped its log for
// "press Ctrl-C", and scraped endpoints out of the log with sed. `stop` could
// not end it at all — it stopped the cluster under a process still serving.
//
// Each running `up` now records itself in a runtime file in its instance
// directory: its pid and its control address, which is how `wait` finds a
// control port that was OS-assigned. `up --detach` starts one in the
// background and returns once it is ready; `wait` polls its readiness; `stop`
// ends it before stopping the cluster.

// runtimeInfo is the runtime file's content. It doubles as the pidfile.
type runtimeInfo struct {
	PID      int       `json:"pid"`
	Control  string    `json:"control"`
	Log      string    `json:"log,omitempty"`
	Detached bool      `json:"detached"`
	Started  time.Time `json:"started"`
	// Endpoints are the host addresses `up` bound, by service, recorded once
	// startup completes. Absent while starting.
	Endpoints map[string]string `json:"endpoints,omitempty"`
	// Hooks are the lifecycle hooks' outcomes, by stage.
	Hooks map[string][]hooks.Result `json:"hooks,omitempty"`
}

func runtimePath(cfg config.Config) string { return filepath.Join(cfg.InstanceDir(), "up.json") }
func upLogPath(cfg config.Config) string   { return filepath.Join(cfg.InstanceDir(), "up.log") }

func readRuntime(cfg config.Config) (runtimeInfo, error) {
	var info runtimeInfo
	b, err := os.ReadFile(runtimePath(cfg))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("read %s: %w", runtimePath(cfg), err)
	}
	return info, nil
}

// running returns the instance's live `up`, if there is one. A runtime file
// whose process is gone — killed with -9, or a crashed host — is stale and
// reported as not running, so it cannot block the next start.
func running(cfg config.Config) (runtimeInfo, bool) {
	info, err := readRuntime(cfg)
	if err != nil || info.PID <= 0 {
		return info, false
	}
	return info, isCloudBurrow(info.PID)
}

// isCloudBurrow reports whether pid is a live CloudBurrow process.
//
// Liveness alone is not enough: a stale pidfile's pid can have been reused by
// an unrelated process, and `stop` must never signal one of those. The
// process's command name is checked too.
func isCloudBurrow(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(filepath.Base(strings.TrimSpace(string(out))), "cloudburrow")
}

// runtimeFile is the lifecycle component that owns the runtime file. It is
// registered right after the control server, so the file names a control
// address that is already listening, and it is removed on the way down
// whether startup succeeded or not.
type runtimeFile struct {
	cfg      config.Config
	control  *lifecycle.ControlServer
	detached bool
	info     runtimeInfo
}

func (r *runtimeFile) Name() string { return "runtime-file" }

func (r *runtimeFile) Start(context.Context) error {
	r.info = runtimeInfo{PID: os.Getpid(), Control: r.control.Addr(), Detached: r.detached, Started: time.Now().UTC()}
	if r.detached {
		r.info.Log = upLogPath(r.cfg)
	}
	return r.write()
}

// RecordHooks records a stage's outcomes, for `status`.
func (r *runtimeFile) RecordHooks(stage string, res []hooks.Result) error {
	if r.info.Hooks == nil {
		r.info.Hooks = map[string][]hooks.Result{}
	}
	r.info.Hooks[stage] = res
	return r.write()
}

// Publish records the endpoints once every one is bound.
func (r *runtimeFile) Publish(endpoints map[string]string) error {
	r.info.Endpoints = endpoints
	return r.write()
}

func (r *runtimeFile) write() error {
	b, _ := json.MarshalIndent(r.info, "", "  ")
	if err := os.MkdirAll(r.cfg.InstanceDir(), 0o700); err != nil {
		return err
	}
	// Written whole and renamed, so a reader never sees half a file.
	tmp := runtimePath(r.cfg) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, runtimePath(r.cfg))
}

func (r *runtimeFile) Stop(context.Context) error {
	// Only our own file: a second instance racing this one must not lose its.
	if info, err := readRuntime(r.cfg); err == nil && info.PID == os.Getpid() {
		return os.Remove(runtimePath(r.cfg))
	}
	return nil
}

// errAlreadyRunning is returned by a foreground `up` for an instance that
// already has one.
var errAlreadyRunning = errors.New("already running")

// exitError carries a specific exit status to main.
type exitError struct {
	code int
	err  error
	// quiet exits without a message, for an ending that is not an error to
	// report, such as Ctrl-C on `logs --follow`.
	quiet bool
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// Exit statuses of `wait`, and of `up --detach`, which waits the same way.
const (
	exitReady   = 0
	exitFailed  = 1
	exitTimeout = 2
)

// splitFlag removes a flag this command owns from args before the shared
// configuration parser sees them, which would refuse it as unknown. Both
// -name and --name, and both "=value" and a separate value, are accepted, as
// the flag package does.
func splitFlag(args []string, name string, boolean bool) (value string, found bool, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return value, found, append(rest, args[i:]...), nil
		}
		trimmed := strings.TrimLeft(a, "-")
		if !strings.HasPrefix(a, "-") || (trimmed != name && !strings.HasPrefix(trimmed, name+"=")) {
			rest = append(rest, a)
			continue
		}
		found = true
		if v, ok := strings.CutPrefix(trimmed, name+"="); ok {
			value = v
			continue
		}
		if boolean {
			value = "true"
			continue
		}
		if i+1 >= len(args) {
			return "", true, nil, fmt.Errorf("flag -%s needs a value", name)
		}
		value = args[i+1]
		i++
	}
	return value, found, rest, nil
}

// readiness is one /readyz answer.
type readiness struct {
	Ready      bool            `json:"ready"`
	State      string          `json:"state"`
	Components map[string]bool `json:"components"`
	Error      string          `json:"error"`
}

func (r readiness) notReady() []string {
	var out []string
	for name, ok := range r.Components {
		if !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// awaitReady polls an instance until it is ready, has failed, or the timeout
// passes, and returns the exit status that outcome maps to. alive, when set,
// reports whether the process being waited for still exists: a process that
// exited during startup is a failure now, not a timeout later.
//
// Readiness is read only from the control address the instance recorded in
// its own runtime file, and, when pid is set, only from that process's file.
// It used to fall back to the configured control port, and a second instance
// whose `up` had failed to bind that port — because another instance held it
// — read the other instance's readiness and reported itself ready (#282).
func awaitReady(cfg config.Config, timeout time.Duration, pid int, alive func() bool) (readiness, int) {
	c := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last readiness
	for {
		info, _ := readRuntime(cfg)
		control := info.Control
		switch {
		case pid > 0 && info.PID != pid:
			// Not yet written by the process being waited for.
			control = ""
		case pid == 0 && (info.PID <= 0 || !isCloudBurrow(info.PID)):
			// Stale, or absent: nothing of this instance's to ask.
			control = ""
		}
		if control != "" {
			if resp, err := c.Get("http://" + control + "/readyz"); err == nil {
				var r readiness
				_ = json.NewDecoder(resp.Body).Decode(&r)
				_ = resp.Body.Close()
				last = r
				switch {
				// Ready means usable: when a runtime file names the process,
				// its endpoints must be recorded too, or an `env` run straight
				// after this returns would miss every OS-assigned port. They
				// are written a moment after /readyz first answers 200.
				case resp.StatusCode == http.StatusOK && r.Ready && (info.PID == 0 || info.Endpoints != nil):
					return r, exitReady
				case r.State == "failed":
					return r, exitFailed
				}
			}
		}
		if alive != nil && !alive() {
			return last, exitFailed
		}
		if time.Now().After(deadline) {
			return last, exitTimeout
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// printReadiness writes the per-component breakdown.
func printReadiness(w io.Writer, r readiness) {
	if len(r.Components) == 0 {
		fmt.Fprintln(w, "  no readiness report yet: the control server is not answering")
		return
	}
	names := make([]string, 0, len(r.Components))
	for n := range r.Components {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		state := "ready"
		if !r.Components[n] {
			state = "NOT READY"
		}
		fmt.Fprintf(w, "  %-28s %s\n", n, state)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", r.Error)
	}
}

// runWait implements `cloudburrow wait`.
func runWait(args []string, stdout, stderr io.Writer) error {
	value, _, rest, err := splitFlag(args, "timeout", false)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	timeout := 5 * time.Minute
	if value != "" {
		if timeout, err = time.ParseDuration(value); err != nil || timeout <= 0 {
			fmt.Fprintf(stderr, "invalid -timeout %q: want a positive duration such as 5m\n", value)
			return errUsage
		}
	}
	cfg, err := config.Load(config.Options{Args: rest, Output: stderr})
	if err != nil {
		return err
	}
	var alive func() bool
	if info, ok := running(cfg); ok {
		alive = func() bool { return isCloudBurrow(info.PID) }
	}
	pid := 0
	if info, ok := running(cfg); ok {
		pid = info.PID
	}
	r, code := awaitReady(cfg, timeout, pid, alive)
	switch code {
	case exitReady:
		fmt.Fprintf(stdout, "instance %q is ready\n", cfg.Name)
		printReadiness(stdout, r)
		return nil
	case exitFailed:
		fmt.Fprintf(stdout, "instance %q failed to start\n", cfg.Name)
		printReadiness(stdout, r)
		return &exitError{code: exitFailed, err: fmt.Errorf("instance %q failed", cfg.Name)}
	default:
		fmt.Fprintf(stdout, "instance %q was not ready after %s; not ready: %s\n",
			cfg.Name, timeout, strings.Join(r.notReady(), ", "))
		printReadiness(stdout, r)
		return &exitError{code: exitTimeout, err: fmt.Errorf("timed out after %s", timeout)}
	}
}

// runDetached starts `up` in the background and returns once it is ready.
//
// The child is the same binary with the same arguments, less -detach, in a
// session of its own, so it survives the shell that started it; its output
// goes to up.log in the instance directory. On failure or timeout the log's
// tail is printed, because the child's own messages are the explanation.
func runDetached(args []string, timeout time.Duration, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	if info, ok := running(cfg); ok {
		// Idempotent: the instance is what was asked for. It is still waited
		// on, so "exit 0" means ready here as it does on a fresh start.
		r, code := awaitReady(cfg, timeout, info.PID, func() bool { return isCloudBurrow(info.PID) })
		if code != exitReady {
			printReadiness(stdout, r)
			return &exitError{code: code, err: fmt.Errorf("instance %q is running (pid %d) but not ready", cfg.Name, info.PID)}
		}
		fmt.Fprintf(stdout, "instance %q is already running (pid %d, control http://%s)\n", cfg.Name, info.PID, info.Control)
		return nil
	}

	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return err
	}
	// A stale runtime file would name a dead process's control address,
	// which the wait below would otherwise poll until the child replaced it.
	_ = os.Remove(runtimePath(cfg))
	logf, err := os.OpenFile(upLogPath(cfg), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(exe, append([]string{"up"}, args...)...)
	child.Stdout, child.Stderr = logf, logf
	child.Env = append(os.Environ(), detachedEnv+"=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start the background process: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	alive := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}

	r, code := awaitReady(cfg, timeout, child.Process.Pid, alive)
	if code == exitReady {
		info, _ := readRuntime(cfg)
		fmt.Fprintf(stdout, "instance %q is ready in the background (pid %d)\n", cfg.Name, child.Process.Pid)
		fmt.Fprintf(stdout, "  control: http://%s\n  log:     %s\n", info.Control, upLogPath(cfg))
		fmt.Fprintln(stdout, "  `cloudburrow env` prints the endpoints; `cloudburrow stop` ends it")
		return nil
	}
	if alive() {
		// A timeout leaves no half-started background process behind.
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(time.Duration(cfg.ShutdownTimeout)):
			_ = child.Process.Kill()
		}
	}
	printReadiness(stdout, r)
	fmt.Fprintf(stderr, "\nlast lines of %s:\n", upLogPath(cfg))
	printTail(stderr, upLogPath(cfg), 40)
	if code == exitTimeout {
		return &exitError{code: code, err: fmt.Errorf("instance %q was not ready after %s; not ready: %s",
			cfg.Name, timeout, strings.Join(r.notReady(), ", "))}
	}
	return &exitError{code: code, err: fmt.Errorf("instance %q failed to start", cfg.Name)}
}

// detachedEnv marks the background child, so it records itself as detached.
const detachedEnv = "CLOUDBURROW_DETACHED"

func printTail(w io.Writer, path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(w, "  (%v)\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		fmt.Fprintf(w, "  %s\n", l)
	}
}

// stopRunning ends the instance's running `up`, if there is one, and waits for
// it to exit. It is what `stop` does first: stopping the cluster under a
// process still serving it left that process holding ports and tunnels to
// nothing.
func stopRunning(cfg config.Config, stdout io.Writer) error {
	info, ok := running(cfg)
	if !ok {
		// A stale file names nothing to stop; removing it is the cleanup.
		_ = os.Remove(runtimePath(cfg))
		return nil
	}
	p, err := os.FindProcess(info.PID)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", info.PID, err)
	}
	// The drain is bounded by the instance's own shutdown timeout; a little
	// more is allowed for the process to exit after it.
	deadline := time.Now().Add(time.Duration(cfg.ShutdownTimeout) + 10*time.Second)
	for isCloudBurrow(info.PID) {
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d did not exit within %s of SIGTERM", info.PID, time.Duration(cfg.ShutdownTimeout)+10*time.Second)
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.Remove(runtimePath(cfg))
	fmt.Fprintf(stdout, "stopped cloudburrow up (pid %d)\n", info.PID)
	return nil
}

// upFlags separates `up`'s own -detach and -detach-timeout from the shared
// configuration flags.
func upFlags(args []string) (detach bool, timeout time.Duration, rest []string, err error) {
	v, found, rest, err := splitFlag(args, "detach", true)
	if err != nil {
		return false, 0, nil, err
	}
	if found {
		switch v {
		case "true", "1", "":
			detach = true
		case "false", "0":
		default:
			return false, 0, nil, fmt.Errorf("invalid -detach %q", v)
		}
	}
	tv, _, rest, err := splitFlag(rest, "detach-timeout", false)
	if err != nil {
		return false, 0, nil, err
	}
	timeout = 10 * time.Minute
	if tv != "" {
		if timeout, err = time.ParseDuration(tv); err != nil || timeout <= 0 {
			return false, 0, nil, fmt.Errorf("invalid -detach-timeout %q: want a positive duration such as 10m", tv)
		}
	}
	return detach, timeout, rest, nil
}
