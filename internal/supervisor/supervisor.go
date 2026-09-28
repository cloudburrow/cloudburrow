// Package supervisor runs a backend's process inside its container and
// restarts it at once when it ends or when the front beside it says it has
// failed (#1091).
//
// Kubernetes restarts a container whose process exits or whose liveness
// probe fails, but it waits longer after each restart (10 s, doubled up to
// five minutes, reset only once the container has run ten minutes: the
// kubelet's CrashLoopBackOff), and the pod's other containers cannot
// restart it themselves. The BigQuery emulator's SQL engine fails for good
// after enough queries, and its process can crash (#989, #1058), so a busy
// instance restarted it several times within minutes, and after the
// seventh restart it was down for minutes, every request meanwhile refused.
//
// So the backend's container runs `cloudburrow-storage supervise`, copied
// into it by an init container from the front's image (`cloudburrow-storage
// install-self`), which starts the backend's own entrypoint as its child:
//
//   - when the child ends, for whatever reason (a crash, an out-of-memory
//     kill of it, the kill below), it is started again after at most
//     MaxDelay: 100 ms, doubled while the child keeps ending within
//     MinUptime of its start, and back to 100 ms once one has run longer;
//   - it gets the front's liveness path (--liveness), and kills the child
//     (SIGKILL) once the front has answered 503 Failures times in a row, the
//     first no sooner than Grace after the child started; any other answer,
//     and no answer (the front restarting), counts as alive;
//   - SIGTERM or SIGINT, which Kubernetes sends to stop the container, is
//     passed to the child, which is given StopTimeout to end, and the
//     supervisor then ends.
//
// The container itself never restarts, so Kubernetes' back-off never
// starts.
package supervisor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// Config is how a Supervisor runs its child.
type Config struct {
	// Command is the child's executable and arguments.
	Command []string
	// Liveness is the URL the front answers 503 at once the child has
	// failed; empty: the child is restarted only when it ends.
	Liveness string
	// Grace is how long after a start the first liveness check waits;
	// Period how often it is checked; Failures how many 503s in a row
	// kill the child.
	Grace, Period time.Duration
	Failures      int
	// MinUptime, MinDelay and MaxDelay set the delay before a restart
	// (package comment).
	MinUptime, MinDelay, MaxDelay time.Duration
	// StopTimeout is how long the child is given to end after SIGTERM.
	StopTimeout time.Duration
	// Stdout and Stderr are the child's; Logf reports each start and end.
	Stdout, Stderr io.Writer
	Logf           func(string, ...any)
}

// Defaults are the settings `cloudburrow-storage supervise` runs with.
var Defaults = Config{
	Grace: time.Second, Period: 500 * time.Millisecond, Failures: 2,
	MinUptime: 2 * time.Second, MinDelay: 100 * time.Millisecond, MaxDelay: time.Second,
	StopTimeout: 10 * time.Second,
}

// Run supervises the command after `--` in args, with the flags of
// `cloudburrow-storage supervise`, until ctx ends.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := Defaults
	fs.StringVar(&c.Liveness, "liveness", "", "URL the front answers 503 at once the child has failed; empty: none")
	fs.DurationVar(&c.Grace, "grace", c.Grace, "how long after a start the first liveness check waits")
	fs.DurationVar(&c.Period, "period", c.Period, "how often the liveness URL is checked")
	fs.IntVar(&c.Failures, "failures", c.Failures, "503 answers in a row that kill the child")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if c.Command = fs.Args(); len(c.Command) == 0 {
		fmt.Fprintln(stderr, "supervise: no command after --")
		return ErrUsage
	}
	c.Stdout, c.Stderr = stdout, stderr
	logger := log.New(stderr, "", log.LstdFlags|log.LUTC)
	c.Logf = logger.Printf
	return Supervise(ctx, c)
}

// Supervise runs c.Command, again each time it ends, until ctx ends
// (package comment). It returns an error only when the command cannot be
// started at all.
func Supervise(ctx context.Context, c Config) error {
	logf := c.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	client := &http.Client{Timeout: 2 * time.Second}
	delay := c.MinDelay
	for restarts := 0; ; restarts++ {
		cmd := exec.Command(c.Command[0], c.Command[1:]...)
		cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
		started := time.Now()
		if err := cmd.Start(); err != nil {
			if restarts == 0 {
				return fmt.Errorf("start %s: %w", c.Command[0], err)
			}
			// It started before: keep trying, as a crash would be.
			logf("supervisor: could not start %s again: %v", c.Command[0], err)
		} else {
			logf("supervisor: started %s (pid %d, restarts %d)", c.Command[0], cmd.Process.Pid, restarts)
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			watchCtx, stopWatch := context.WithCancel(ctx)
			killed := make(chan struct{})
			if c.Liveness != "" {
				go watch(watchCtx, client, c, func() {
					logf("supervisor: the front says %s has failed (%s); killing it", c.Command[0], c.Liveness)
					close(killed)
					_ = cmd.Process.Kill()
				})
			}
			var err error
			select {
			case err = <-exited:
			case <-ctx.Done():
				stopWatch()
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-exited:
				case <-time.After(c.StopTimeout):
					_ = cmd.Process.Kill()
					<-exited
				}
				return nil
			}
			stopWatch()
			up := time.Since(started)
			select {
			case <-killed:
			default:
				logf("supervisor: %s ended after %s: %v", c.Command[0], up.Round(time.Millisecond), exitText(err))
			}
			if up >= c.MinUptime {
				delay = c.MinDelay
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if delay *= 2; delay > c.MaxDelay {
			delay = c.MaxDelay
		}
	}
}

// watch calls kill once the liveness URL has answered 503 c.Failures times
// in a row, checking from c.Grace after the start, every c.Period.
func watch(ctx context.Context, client *http.Client, c Config, kill func()) {
	wait := c.Grace
	failed := 0
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = c.Period
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Liveness, nil)
		if err != nil {
			return
		}
		res, err := client.Do(req)
		if err != nil {
			failed = 0 // the front is not answering: not the child's failure
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		_ = res.Body.Close()
		if res.StatusCode != http.StatusServiceUnavailable {
			failed = 0
			continue
		}
		if failed++; failed >= c.Failures {
			kill()
			return
		}
	}
}

func exitText(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// InstallSelf copies the running executable to dst, executable by anyone,
// for an init container to put the supervisor where the backend's
// container can run it.
func InstallSelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
