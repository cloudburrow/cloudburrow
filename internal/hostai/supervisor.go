package hostai

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Spec is one supervised model server.
type Spec struct {
	// Name identifies the server in its log lines and the pid file.
	Name string
	// Path and Args are the executable and its arguments; Env is added to
	// a minimal environment (PATH, HOME, TMPDIR), Dir the working
	// directory.
	Path string
	Args []string
	Env  []string
	Dir  string
	// Health is the URL that answers 200 once the server is serving.
	Health string
	// Log receives every line the server writes, with its stream (stdout
	// or stderr); supervisor events come as stream "supervisor".
	Log func(stream, line string)
	// Pids, when set, records the running child so an abruptly ended CLI's
	// servers are ended by the next one (Pids.Reap).
	Pids *Pids

	// StartTimeout bounds the wait for a start's first healthy answer;
	// the server is killed and started again after it. 15 minutes when
	// zero (a large model's first load).
	StartTimeout time.Duration
	// Period is how often health is checked; Failures how many failed
	// checks in a row, once the server was healthy, kill it to be started
	// again. 5 s and 3 when zero.
	Period   time.Duration
	Failures int
	// MinDelay and MaxDelay bound the delay before a restart: MinDelay,
	// doubled while the server keeps ending within MinUptime of its
	// start, up to MaxDelay. 1 s, 1 min and 30 s when zero.
	MinDelay, MaxDelay, MinUptime time.Duration
	// StopTimeout is how long the server is given to end after SIGTERM
	// before its process group is killed. 10 s when zero.
	StopTimeout time.Duration
}

func (s *Spec) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&s.StartTimeout, 15*time.Minute)
	def(&s.Period, 5*time.Second)
	def(&s.MinDelay, time.Second)
	def(&s.MaxDelay, time.Minute)
	def(&s.MinUptime, 30*time.Second)
	def(&s.StopTimeout, 10*time.Second)
	if s.Failures <= 0 {
		s.Failures = 3
	}
}

// Process supervises one server: it starts it, checks its health, starts
// it again when it ends or stops answering, and ends it on Stop.
type Process struct {
	spec   Spec
	client *http.Client

	mu       sync.Mutex
	cmd      *exec.Cmd
	pid      int
	healthy  bool
	starts   int
	lastExit string
	tail     []string
	stopping bool

	stop chan struct{}
	done chan struct{}
}

const tailLines = 200

// Start starts the server and its supervision. It returns once the first
// child is running (not yet healthy: see Wait).
func Start(spec Spec) (*Process, error) {
	spec.defaults()
	if spec.Path == "" {
		return nil, errors.New("no server to start")
	}
	p := &Process{spec: spec, client: &http.Client{Timeout: 3 * time.Second},
		stop: make(chan struct{}), done: make(chan struct{})}
	exited, err := p.launch()
	if err != nil {
		return nil, err
	}
	go p.supervise(exited)
	return p, nil
}

// env is the child's environment: no more of the CLI's than a server
// needs, so no credential or token of the developer's shell is passed on.
func (p *Process) env() []string {
	out := []string{}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "USER"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return append(out, p.spec.Env...)
}

// launch starts one child in a process group of its own and returns the
// channel its exit is reported on.
func (p *Process) launch() (<-chan error, error) {
	cmd := exec.Command(p.spec.Path, p.spec.Args...)
	cmd.Dir = p.spec.Dir
	cmd.Env = p.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.spec.Name, err)
	}
	p.mu.Lock()
	p.cmd, p.pid, p.healthy = cmd, cmd.Process.Pid, false
	p.starts++
	n := p.starts
	p.mu.Unlock()
	if p.spec.Pids != nil {
		p.spec.Pids.Add(p.spec.Name, cmd.Process.Pid, p.spec.Path)
	}
	p.event(fmt.Sprintf("started %s (pid %d, start %d)", p.spec.Name, cmd.Process.Pid, n))
	var wg sync.WaitGroup
	wg.Add(2)
	go p.lines("stdout", stdout, &wg)
	go p.lines("stderr", stderr, &wg)
	exited := make(chan error, 1)
	go func() {
		wg.Wait()
		exited <- cmd.Wait()
	}()
	return exited, nil
}

func (p *Process) lines(stream string, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		p.record(stream, sc.Text())
	}
	// A line longer than the buffer ends the scan; the rest is drained so
	// the child never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
}

func (p *Process) record(stream, line string) {
	p.mu.Lock()
	p.tail = append(p.tail, line)
	if len(p.tail) > tailLines {
		p.tail = p.tail[len(p.tail)-tailLines:]
	}
	p.mu.Unlock()
	if p.spec.Log != nil {
		p.spec.Log(stream, line)
	}
}

func (p *Process) event(s string) { p.record("supervisor", "cloudburrow: "+s) }

// signal sends sig to the child's process group.
func (p *Process) signal(sig syscall.Signal) {
	p.mu.Lock()
	pid := p.pid
	p.mu.Unlock()
	if pid > 0 {
		_ = syscall.Kill(-pid, sig)
	}
}

func (p *Process) supervise(exited <-chan error) {
	defer close(p.done)
	delay := p.spec.MinDelay
	for {
		started := time.Now()
		err, stopped := p.watch(exited, started)
		p.mu.Lock()
		pid := p.pid
		p.pid, p.healthy = 0, false
		if err != nil {
			p.lastExit = err.Error()
		} else {
			p.lastExit = "exit status 0"
		}
		last := p.lastExit
		p.mu.Unlock()
		if p.spec.Pids != nil {
			p.spec.Pids.Remove(p.spec.Name, pid)
		}
		if stopped {
			p.event(fmt.Sprintf("stopped %s (%s)", p.spec.Name, last))
			return
		}
		if time.Since(started) >= p.spec.MinUptime {
			delay = p.spec.MinDelay
		}
		p.event(fmt.Sprintf("%s ended (%s); starting it again in %s", p.spec.Name, last, delay))
		select {
		case <-p.stop:
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > p.spec.MaxDelay {
			delay = p.spec.MaxDelay
		}
		for {
			var lerr error
			if exited, lerr = p.launch(); lerr == nil {
				break
			}
			p.event(lerr.Error())
			select {
			case <-p.stop:
				return
			case <-time.After(delay):
			}
		}
	}
}

// watch follows one child until it exits, killing it when it does not
// become healthy within StartTimeout or stops answering once it was. It
// reports whether the exit was Stop's.
func (p *Process) watch(exited <-chan error, started time.Time) (error, bool) {
	t := time.NewTicker(p.spec.Period)
	defer t.Stop()
	fails := 0
	first := time.NewTimer(200 * time.Millisecond)
	defer first.Stop()
	check := func() {
		ok := p.spec.Health == "" || p.probe()
		p.mu.Lock()
		was := p.healthy
		if ok {
			p.healthy, fails = true, 0
		}
		p.mu.Unlock()
		switch {
		case ok:
		case was:
			if fails++; fails >= p.spec.Failures {
				p.event(fmt.Sprintf("%s failed %d health checks in a row; killing it", p.spec.Name, fails))
				p.mu.Lock()
				p.healthy = false
				p.mu.Unlock()
				p.signal(syscall.SIGKILL)
			}
		case time.Since(started) > p.spec.StartTimeout:
			p.event(fmt.Sprintf("%s did not become healthy within %s; killing it", p.spec.Name, p.spec.StartTimeout))
			p.signal(syscall.SIGKILL)
		}
	}
	for {
		select {
		case err := <-exited:
			p.mu.Lock()
			stopping := p.stopping
			p.mu.Unlock()
			return err, stopping
		case <-first.C:
			check()
		case <-t.C:
			check()
		}
	}
}

func (p *Process) probe() bool {
	req, err := http.NewRequest(http.MethodGet, p.spec.Health, nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Healthy reports whether the running child has answered its health check.
func (p *Process) Healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.healthy && p.pid > 0
}

// Running reports whether a child is running (or about to be started
// again): false only once Stop has ended it.
func (p *Process) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// PID is the running child's process ID, 0 between starts.
func (p *Process) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid
}

// Starts is how many times the server has been started.
func (p *Process) Starts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// Tail is the server's last n lines of output.
func (p *Process) Tail(n int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tail
	if n > 0 && len(t) > n {
		t = t[len(t)-n:]
	}
	return strings.Join(t, "\n")
}

// Wait waits until the server is healthy, ctx ends, or timeout passes.
// While it waits the server may be started again; the error then carries
// its last lines.
func (p *Process) Wait(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if p.Healthy() {
			return nil
		}
		if !p.Running() {
			return fmt.Errorf("%s stopped: %s", p.spec.Name, p.Tail(12))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become healthy within %s: %s", p.spec.Name, timeout, p.Tail(12))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Stop ends the server: SIGTERM to its process group, then SIGKILL after
// StopTimeout, and waits until supervision has ended. It is safe to call
// more than once.
func (p *Process) Stop(ctx context.Context) error {
	p.mu.Lock()
	first := !p.stopping
	p.stopping = true
	p.mu.Unlock()
	if first {
		close(p.stop)
	}
	p.signal(syscall.SIGTERM)
	t := time.NewTimer(p.spec.StopTimeout)
	defer t.Stop()
	select {
	case <-p.done:
		return nil
	case <-t.C:
	case <-ctx.Done():
	}
	p.signal(syscall.SIGKILL)
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("%s did not end after SIGKILL", p.spec.Name)
	}
}
