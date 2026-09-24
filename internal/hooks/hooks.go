// Package hooks runs a directory of lifecycle scripts (#285).
//
// A stage is a directory, ready.d or shutdown.d, of executable files run in
// lexical order. Each runs on the host, as the user, with the environment it
// is given; none of it reaches the cluster. A script that fails, or overruns
// its timeout, is reported and the rest still run: one broken script must not
// silently skip the ones after it.
package hooks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Stages.
const (
	Ready    = "ready.d"
	Shutdown = "shutdown.d"
)

// Result is one script's outcome.
type Result struct {
	Stage    string        `json:"stage"`
	Name     string        `json:"name"`
	ExitCode int           `json:"exit_code"`
	TimedOut bool          `json:"timed_out,omitempty"`
	Skipped  string        `json:"skipped,omitempty"` // why it did not run
	Error    string        `json:"error,omitempty"`   // could not be started
	Duration time.Duration `json:"duration_ns"`
}

// OK reports whether the script ran and succeeded.
func (r Result) OK() bool { return r.Skipped == "" && r.Error == "" && !r.TimedOut && r.ExitCode == 0 }

// Scripts lists a stage's entries in the order they run. An absent directory
// is no scripts, not an error: hooks are optional.
func Scripts(dir, stage string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, stage))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && e.Name()[0] != '.' {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Run runs every script of a stage, in order, and returns each outcome.
// Output is streamed to out, each line prefixed with the stage and script.
func Run(ctx context.Context, dir, stage string, env []string, timeout time.Duration, out io.Writer) ([]Result, error) {
	names, err := Scripts(dir, stage)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex // out is shared by each script's two streams
	var results []Result
	for _, name := range names {
		path := filepath.Join(dir, stage, name)
		r := Result{Stage: stage, Name: name}
		info, err := os.Stat(path)
		switch {
		case err != nil:
			r.Error = err.Error()
		case !info.Mode().IsRegular():
			r.Skipped = "not a regular file"
		case info.Mode().Perm()&0o111 == 0:
			// Named, not run: a file that is not executable is most likely
			// a note or a helper, and guessing an interpreter for it would
			// run something its author never marked as runnable.
			r.Skipped = "not executable"
		default:
			r = runOne(ctx, path, stage, name, env, timeout, out, &mu)
		}
		mu.Lock()
		fmt.Fprintf(out, "[%s/%s] %s\n", stage, name, r.summary())
		mu.Unlock()
		results = append(results, r)
	}
	return results, nil
}

func (r Result) summary() string {
	switch {
	case r.Skipped != "":
		return "skipped: " + r.Skipped
	case r.Error != "":
		return "could not run: " + r.Error
	case r.TimedOut:
		return fmt.Sprintf("killed after its %s timeout", r.Duration.Round(time.Millisecond))
	case r.ExitCode != 0:
		return fmt.Sprintf("failed with exit status %d after %s", r.ExitCode, r.Duration.Round(time.Millisecond))
	default:
		return fmt.Sprintf("ok in %s", r.Duration.Round(time.Millisecond))
	}
}

func runOne(ctx context.Context, path, stage, name string, env []string, timeout time.Duration, out io.Writer, mu *sync.Mutex) Result {
	r := Result{Stage: stage, Name: name}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cmd := exec.Command(path)
	cmd.Dir = filepath.Dir(path)
	cmd.Env = env
	// Its own process group, so a timeout reaches whatever it started too:
	// a script that backgrounds a server would otherwise outlive its kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		r.Error = err.Error()
		return r
	}
	var wg sync.WaitGroup
	for _, s := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(s io.Reader) {
			defer wg.Done()
			sc := bufio.NewScanner(s)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				mu.Lock()
				fmt.Fprintf(out, "[%s/%s] %s\n", stage, name, sc.Text())
				mu.Unlock()
			}
		}(s)
	}
	done := make(chan error, 1)
	go func() { wg.Wait(); done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		r.TimedOut = true
		err = kill(cmd, done)
	case <-ctx.Done():
		err = kill(cmd, done)
		r.Error = "interrupted: " + ctx.Err().Error()
	}
	r.Duration = time.Since(start)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.ExitCode = exit.ExitCode()
	}
	return r
}

// kill ends a script's process group and waits, boundedly, for it. A
// grandchild that left the group (setsid) can hold the output pipes open
// after the kill; that must not hang the stage, so the wait gives up.
func kill(cmd *exec.Cmd, done <-chan error) error {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("killed, but its output did not close")
	}
}
