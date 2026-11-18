package bigqueryfront

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProcess is an emulator's port: it accepts connections and holds
// them, until it is stopped, which closes them all as a process's end
// does.
type fakeProcess struct {
	l     net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func startProcess(t *testing.T, addr string) *fakeProcess {
	t.Helper()
	var l net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may linger a moment after a stop
		if l, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	p := &fakeProcess{l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, c)
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *fakeProcess) stop() {
	_ = p.l.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// TestEmulatorWatch (#1016): the watch fires when the emulator's process
// ends (the connection it holds is closed) and again when it is back, and
// not while it runs, across many idle connections it closes itself.
func TestEmulatorWatch(t *testing.T) {
	p := startProcess(t, "127.0.0.1:0")
	addr := p.l.Addr().String()
	w := newEmulatorWatch(addr, t.Logf)
	w.idle, w.poll = 20*time.Millisecond, 5*time.Millisecond
	var fired atomic.Int32
	w.onRestart(func() { fired.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	defer func() { cancel(); <-done; p.stop() }()

	time.Sleep(200 * time.Millisecond) // ten idle connections
	if n := fired.Load(); n != 0 {
		t.Fatalf("fired %d times while the emulator ran", n)
	}
	wait := func(want int32) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); fired.Load() < want; {
			if time.Now().After(deadline) {
				t.Fatalf("fired %d times, want %d", fired.Load(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	p.stop()
	wait(1)
	time.Sleep(50 * time.Millisecond)
	if n := fired.Load(); n != 1 {
		t.Fatalf("fired %d times while the emulator was down, want 1", n)
	}
	p = startProcess(t, addr)
	wait(2)
	time.Sleep(100 * time.Millisecond)
	if n := fired.Load(); n != 2 {
		t.Errorf("fired %d times once the emulator was back, want 2", n)
	}
}

// TestEmulatorWatchNotBeforeFirstUp: an emulator that is not up yet when
// the front starts has not restarted.
//
// The watch's dial is refused, as a port nobody listens on is, until the
// process is up, and the test steps on the watch's own dials rather than
// on time: it starts the process after the watch has been refused twice,
// and stops the watch once it has connected twice (the first connection,
// when the emulator came up, and the one after its idle). A port freed by
// closing a listener is no stand-in for "not up yet": on a shared CI host
// another process can bind it, or a dial to it can connect to itself,
// before the test listens there, and the watch then sees an emulator come
// and go (fired 2 times, main CI run 44296179658).
func TestEmulatorWatchNotBeforeFirstUp(t *testing.T) {
	w := newEmulatorWatch("unused:0", t.Logf)
	w.idle, w.poll = 20*time.Millisecond, 5*time.Millisecond
	var proc atomic.Pointer[fakeProcess]
	refused, connected := make(chan struct{}, 1), make(chan struct{}, 1)
	w.dial = func() (net.Conn, error) {
		p := proc.Load()
		if p == nil {
			select {
			case refused <- struct{}{}:
			default:
			}
			return nil, errors.New("connection refused")
		}
		c, err := net.DialTimeout("tcp", p.l.Addr().String(), time.Second)
		if err == nil {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
		return c, err
	}
	var fired atomic.Int32
	w.onRestart(func() { fired.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	<-refused
	<-refused
	p := startProcess(t, "127.0.0.1:0")
	proc.Store(p)
	<-connected
	<-connected
	cancel()
	<-done
	p.stop()
	if n := fired.Load(); n != 0 {
		t.Errorf("fired %d times, want 0", n)
	}
}

// TestFrontDropsJobsAtRestart (#1016): once the emulator has restarted, a
// job of the same ID the new emulator runs is not reported with the
// failure the front gave the old one, nor with its times.
func TestFrontDropsJobsAtRestart(t *testing.T) {
	var mu sync.Mutex
	j1 := `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/jobs/j1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if j1 == "" {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, j1)
	})
	mux.HandleFunc("GET "+base+"/jobs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if j1 == "" {
			_, _ = io.WriteString(w, `{"kind":"bigquery#jobList"}`)
			return
		}
		_, _ = io.WriteString(w, `{"jobs":[`+j1+`]}`)
	})
	mux.Handle("/", &csvEmulator{})
	watch := &emulatorWatch{}
	h := Wrap(mux, func(o *options) { o.restarts = watch })
	if w := upload(t, h, loadJob(`,"nullMarker":"\\N"`), ",x\n"); w.Code != 400 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	failed := func(job map[string]any) bool {
		st, _ := job["status"].(map[string]any)
		return st["errorResult"] != nil
	}
	listed := func() []any {
		_, list := do(t, h, "GET", base+"/jobs", "")
		jobs, _ := list["jobs"].([]any)
		return jobs
	}
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); !failed(job) {
		t.Fatalf("before the restart, jobs.get: %v, want the job failed", job)
	}

	// The emulator restarts, empty.
	mu.Lock()
	j1 = ""
	mu.Unlock()
	watch.fire()
	if code, job := do(t, h, "GET", base+"/jobs/j1", ""); code != 404 {
		t.Errorf("after the restart, jobs.get: %d %v, want 404", code, job)
	}
	if jobs := listed(); len(jobs) != 0 {
		t.Errorf("after the restart, jobs.list: %v, want none", jobs)
	}
	// The new emulator runs a job j1 that succeeds.
	mu.Lock()
	j1 = `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"},` +
		`"statistics":{"creationTime":"1790000000","startTime":"1790000000","endTime":"1790000001"}}`
	mu.Unlock()
	_, job := do(t, h, "GET", base+"/jobs/j1", "")
	if failed(job) {
		t.Errorf("a new job j1: %v, reported with the old one's failure", job)
	}
	if got := statTimes(t, job); got != [3]int64{1790000000000, 1790000000000, 1790000001000} {
		t.Errorf("a new job j1: times %v, want the emulator's, not the old job's", got)
	}
	if jobs := listed(); len(jobs) != 1 || failed(jobs[0].(map[string]any)) {
		t.Errorf("a new job j1, jobs.list: %v", jobs)
	}
}
