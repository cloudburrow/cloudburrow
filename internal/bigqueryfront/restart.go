package bigqueryfront

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// The emulator restarting beneath the front (#1016).
//
// The emulator keeps its data in memory, so a restart of its container
// empties it: every dataset, table and job is gone. Kubernetes restarts
// that container alone, when the front fails its liveness probe (engine.go,
// #989) or when the process exits (a crash, an out-of-memory kill), and
// the front's container goes on. What the front keeps about the emulator's
// jobs (the failures it gave, jobFailures; the client's text of jobs it
// changed, jobTexts; configurations, jobConfigs; the jobs it carried out
// itself, frontJobs; times and its own queries, jobRecords; the functions
// CREATE FUNCTION made, knownFunctions; the client's texts of views,
// viewTexts; the projects whose results dataset it made, queryResults)
// would then outlive them, and jobs.get and jobs.list would report jobs
// the emulator no longer has.
//
// So the front watches the emulator's process (emulatorWatch): it holds a
// connection to the emulator's port open, and dials it again when the
// connection is closed. The process ending closes the connection at once,
// and the port then refuses connections until the new process listens;
// the front drops what it keeps when it sees the connection closed or
// refused after the emulator had been up, and again when the emulator is
// back (a request between the two failed: the emulator was not there).
//
// The connection is idle: the front sends nothing on it, and closes and
// dials it again every idle, shorter than the emulator's ReadTimeout (15 s,
// server/server.go), after which the emulator would close it itself.
//
// Resumable upload sessions (resumable.go) are kept: they hold the client's
// data, not the emulator's, and the load their last chunk sends is then
// answered by the emptied emulator.

// emulatorWatch notices the emulator's process ending and coming back.
type emulatorWatch struct {
	dial func() (net.Conn, error)
	// idle is how long a connection is held before it is dialed again,
	// and poll how often a refused port is dialed.
	idle, poll time.Duration
	logf       func(string, ...any)

	mu   sync.Mutex
	subs []func()
}

// newEmulatorWatch returns a watch of the emulator at upstream (host:port).
func newEmulatorWatch(upstream string, logf func(string, ...any)) *emulatorWatch {
	return &emulatorWatch{
		dial: func() (net.Conn, error) { return net.DialTimeout("tcp", upstream, time.Second) },
		idle: 10 * time.Second,
		poll: 100 * time.Millisecond,
		logf: logf,
	}
}

// onRestart has f called when the emulator's process has ended, and again
// when it is back.
func (e *emulatorWatch) onRestart(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.subs = append(e.subs, f)
}

func (e *emulatorWatch) fire() {
	e.mu.Lock()
	subs := append([]func(){}, e.subs...)
	e.mu.Unlock()
	for _, f := range subs {
		f()
	}
}

// run watches until ctx ends.
func (e *emulatorWatch) run(ctx context.Context) {
	// seen: the emulator has been up since the front started; down: it
	// has since ended, and is not back yet.
	seen, down := false, false
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	ended := func() {
		if seen && !down {
			down = true
			if e.logf != nil {
				e.logf("bigquery front: the emulator's process ended; dropping what the front kept of its jobs")
			}
			e.fire()
		}
	}
	for ctx.Err() == nil {
		c, err := e.dial()
		if err != nil {
			ended()
			if !sleep(e.poll) {
				return
			}
			continue
		}
		if down {
			down = false
			if e.logf != nil {
				e.logf("bigquery front: the emulator is back")
			}
			e.fire()
		}
		seen = true
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		_ = c.SetReadDeadline(time.Now().Add(e.idle))
		_, err = c.Read(make([]byte, 1))
		stop()
		_ = c.Close()
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			// Closed by the emulator: its process ended (or it
			// answered bytes nobody asked for; either way, look again).
			ended()
		}
	}
}

// reset drops the failures the front gave (#1016).
func (j *jobFailures) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.errs, j.order, j.loads, j.loadOrder = nil, nil, nil, nil
}

// reset drops the client's texts of the jobs the front changed (#1016).
func (j *jobTexts) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.texts, j.order = nil, nil
}

// reset drops the jobs' configurations (#1016).
func (j *jobConfigs) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.confs, j.order = nil, nil
}

// reset drops the jobs the front carried out itself (#1016).
func (j *frontJobs) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jobs, j.order = nil, nil
}

// reset drops the jobs' times and the front's own queries (#1016).
func (j *jobRecords) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.times, j.timeOrder, j.internal, j.internalOrder = nil, nil, nil, nil
}

// reset drops the functions CREATE FUNCTION statements made (#1016): the
// restarted emulator has none, and no job from before the front started
// to scan.
func (k *knownFunctions) reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.funcs, k.scanned = nil, nil
	k.started = time.Now().UnixMilli()
	k.saveLocked()
}

// reset drops the client's texts of the views the front made (#1016).
func (v *viewTexts) reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.texts, v.order = nil, nil
}
