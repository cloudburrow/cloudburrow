package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder logs the order of lifecycle calls across components.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// fakeComponent is a Component with configurable failure behavior.
type fakeComponent struct {
	name     string
	rec      *recorder
	startErr error
	stopErr  error
	stopFor  time.Duration // block this long in Stop
}

func (f *fakeComponent) Name() string { return f.name }

func (f *fakeComponent) Start(context.Context) error {
	f.rec.record("start:" + f.name)
	return f.startErr
}

func (f *fakeComponent) Stop(ctx context.Context) error {
	if f.stopFor > 0 {
		select {
		case <-time.After(f.stopFor):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.rec.record("stop:" + f.name)
	return f.stopErr
}

func TestStartsInOrderAndStopsInReverse(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(time.Second)
	c.Register(
		&fakeComponent{name: "a", rec: rec},
		&fakeComponent{name: "b", rec: rec},
		&fakeComponent{name: "c", rec: rec},
	)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if !c.Ready() {
		t.Error("Ready() = false after successful start")
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	want := []string{"start:a", "start:b", "start:c", "stop:c", "stop:b", "stop:a"}
	got := rec.snapshot()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", got, want)
	}
}

// A component that fails to start must leave nothing running: already-started
// components are stopped in reverse, and the process is never reported ready.
func TestFailedStartUnwindsAndIsNotReady(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	boom := errors.New("cannot bind")
	c := New(time.Second)
	c.Register(
		&fakeComponent{name: "a", rec: rec},
		&fakeComponent{name: "b", rec: rec},
		&fakeComponent{name: "bad", rec: rec, startErr: boom},
		&fakeComponent{name: "never", rec: rec},
	)

	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil, want error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Start() = %v, want wrapping %v", err, boom)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error should name the failing component, got %v", err)
	}

	if c.Ready() {
		t.Error("Ready() = true after a failed start; a half-started process must never report ready")
	}
	if got := c.State(); got != StateFailed {
		t.Errorf("State() = %v, want failed", got)
	}

	got := rec.snapshot()
	want := []string{"start:a", "start:b", "start:bad", "stop:b", "stop:a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", got, want)
	}

	// The component after the failure must never have been started.
	for _, call := range got {
		if call == "start:never" {
			t.Error("component after the failure was started")
		}
	}
}

func TestReadinessReportsActualComponents(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(time.Second)
	c.Register(
		&fakeComponent{name: "ok", rec: rec},
		&fakeComponent{name: "broken", rec: rec, startErr: errors.New("nope")},
	)

	_ = c.Start(context.Background())

	names, ready := c.SortedReady()
	if len(names) != 2 {
		t.Fatalf("SortedReady() names = %v, want 2 entries", names)
	}
	if ready["broken"] {
		t.Error("broken component reported ready")
	}
	// "ok" started, then was stopped during unwind, so it is no longer ready.
	if ready["ok"] {
		t.Error("component reported ready after being unwound")
	}
	if c.Failure() == nil {
		t.Error("Failure() = nil after a failed start")
	}
}

// Workers must observe cancellation and be waited for before Stop returns.
func TestWorkersAreCancelledAndDrained(t *testing.T) {
	t.Parallel()
	c := New(2 * time.Second)

	started := make(chan struct{})
	finished := make(chan struct{})
	c.RegisterWorker(WorkerFunc{
		WorkerName: "ticker",
		Fn: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		},
	})

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v, want nil (context.Canceled is not a failure)", err)
	}
	select {
	case <-finished:
	default:
		t.Error("Stop() returned before the worker finished draining")
	}
}

// Workers must not start until every component has started, or they would
// observe a half-initialised process.
func TestWorkersStartAfterComponents(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(time.Second)
	c.Register(&fakeComponent{name: "a", rec: rec}, &fakeComponent{name: "b", rec: rec})

	workerRan := make(chan struct{})
	c.RegisterWorker(WorkerFunc{
		WorkerName: "w",
		Fn: func(ctx context.Context) error {
			rec.record("worker")
			close(workerRan)
			<-ctx.Done()
			return nil
		},
	})

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	<-workerRan
	_ = c.Stop(context.Background())

	got := rec.snapshot()
	workerIdx, bIdx := -1, -1
	for i, call := range got {
		switch call {
		case "worker":
			workerIdx = i
		case "start:b":
			bIdx = i
		}
	}
	if workerIdx < 0 || bIdx < 0 || workerIdx < bIdx {
		t.Errorf("worker ran before components finished starting: %v", got)
	}
}

// A worker that fails for a reason other than cancellation is reported.
func TestWorkerErrorIsReported(t *testing.T) {
	t.Parallel()
	c := New(time.Second)
	boom := errors.New("worker exploded")
	c.RegisterWorker(WorkerFunc{
		WorkerName: "bad",
		Fn:         func(context.Context) error { return boom },
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	// Give the worker a moment to fail before stopping.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.workerMu.Lock()
		n := len(c.workerErrs)
		c.workerMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	err := c.Stop(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Stop() = %v, want wrapping %v", err, boom)
	}
}

// Exceeding the drain window is reported rather than hidden: work was lost, and
// exiting zero would misrepresent that.
func TestShutdownTimeoutIsReported(t *testing.T) {
	t.Parallel()
	c := New(50 * time.Millisecond)
	c.RegisterWorker(WorkerFunc{
		WorkerName: "stubborn",
		Fn: func(ctx context.Context) error {
			<-ctx.Done()
			// Ignores cancellation for longer than the drain window.
			time.Sleep(500 * time.Millisecond)
			return nil
		},
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	start := time.Now()
	err := c.Stop(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Stop() = %v, want ErrShutdownTimeout", err)
	}
	// Stop must return near the bound, not wait for the stubborn worker.
	if elapsed > 300*time.Millisecond {
		t.Errorf("Stop() took %s, want to return near the 50ms bound", elapsed)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(time.Second)
	c.Register(&fakeComponent{name: "a", rec: rec})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop() = %v", err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() = %v, want nil", err)
	}
	count := 0
	for _, call := range rec.snapshot() {
		if call == "stop:a" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("component stopped %d times, want exactly 1", count)
	}
}

func TestDoubleStartIsRejected(t *testing.T) {
	t.Parallel()
	c := New(time.Second)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Error("second Start() = nil, want error")
	}
	_ = c.Stop(context.Background())
}

func TestStopErrorsFromComponentsAreJoined(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	e1 := errors.New("close failed")
	c := New(time.Second)
	c.Register(&fakeComponent{name: "a", rec: rec, stopErr: e1})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	err := c.Stop(context.Background())
	if !errors.Is(err, e1) {
		t.Fatalf("Stop() = %v, want wrapping %v", err, e1)
	}
}

func TestStateStrings(t *testing.T) {
	t.Parallel()
	for state, want := range map[State]string{
		StateNew: "new", StateStarting: "starting", StateReady: "ready",
		StateStopping: "stopping", StateStopped: "stopped", StateFailed: "failed",
		State(99): "unknown",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}

// Components registered later may depend on earlier ones, so a slow Stop must
// still respect the bound rather than hanging the process.
func TestSlowComponentStopRespectsDeadline(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(50 * time.Millisecond)
	c.Register(&fakeComponent{name: "slow", rec: rec, stopFor: time.Second})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	start := time.Now()
	err := c.Stop(context.Background())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Stop() took %s, want to respect the 50ms bound", elapsed)
	}
	if err == nil {
		t.Error("Stop() = nil, want the deadline error from the slow component")
	}
	_ = fmt.Sprint(err)
}
