// Package lifecycle coordinates startup ordering, readiness, and bounded
// shutdown. It owns every background worker in the process: services register
// work here rather than spawning detached goroutines. It also wires the narrow
// interfaces by which one service reaches another, so that services need not
// import each other.
//
// See docs/architecture.md §8.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Component is a unit of work with a managed lifetime.
//
// Start must not block: it acquires resources (opening a listener, claiming a
// data directory) and returns. Long-running work belongs in a Worker. Stop must
// release everything Start acquired and must respect ctx's deadline.
type Component interface {
	// Name identifies the component in logs, errors, and readiness reports.
	Name() string
	// Start acquires resources. Returning an error aborts startup and unwinds
	// whatever already started.
	Start(ctx context.Context) error
	// Stop releases resources. It must be safe to call after a failed Start.
	Stop(ctx context.Context) error
}

// Worker is long-running background work owned by the coordinator.
//
// Run must return when ctx is cancelled. Returning a non-nil error other than
// context.Canceled marks the worker as failed.
type Worker interface {
	Name() string
	Run(ctx context.Context) error
}

// WorkerFunc adapts a function to Worker.
type WorkerFunc struct {
	WorkerName string
	Fn         func(ctx context.Context) error
}

func (w WorkerFunc) Name() string                  { return w.WorkerName }
func (w WorkerFunc) Run(ctx context.Context) error { return w.Fn(ctx) }

// State is the coordinator's lifecycle state.
type State int

const (
	StateNew State = iota
	StateStarting
	StateReady
	StateStopping
	StateStopped
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ErrShutdownTimeout is returned when components or workers do not finish
// within the configured bound. It is reported rather than hidden: a caller that
// exceeded its drain window has lost work, and silently exiting zero would
// misrepresent that.
var ErrShutdownTimeout = errors.New("shutdown exceeded timeout")

// Coordinator starts components in registration order, supervises workers, and
// stops everything in reverse order within a bounded timeout.
//
// The zero value is not usable; call New.
type Coordinator struct {
	shutdownTimeout time.Duration

	mu         sync.Mutex
	components []Component
	workers    []Worker
	started    []Component // successfully started, in start order
	state      State
	failure    error
	ready      map[string]bool

	workerWG   sync.WaitGroup
	workerMu   sync.Mutex
	workerErrs []error
	cancelWork context.CancelFunc
}

// New returns a Coordinator with the given bounded shutdown timeout.
func New(shutdownTimeout time.Duration) *Coordinator {
	return &Coordinator{
		shutdownTimeout: shutdownTimeout,
		state:           StateNew,
		ready:           map[string]bool{},
	}
}

// Register adds a component. Components start in registration order and stop in
// reverse, so a component may depend on those registered before it.
func (c *Coordinator) Register(components ...Component) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.components = append(c.components, components...)
}

// RegisterWorker adds background work. Workers start only after every component
// has started, so a worker never observes a half-initialised process.
func (c *Coordinator) RegisterWorker(workers ...Worker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workers = append(c.workers, workers...)
}

// State returns the current lifecycle state.
func (c *Coordinator) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Ready reports whether every component started successfully.
//
// It reflects actual initialisation rather than an intention to initialise: a
// process whose mandatory startup work failed is never ready.
func (c *Coordinator) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == StateReady
}

// ReadyComponents returns the per-component readiness map, sorted by name.
func (c *Coordinator) ReadyComponents() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.ready))
	for k, v := range c.ready {
		out[k] = v
	}
	return out
}

// Failure returns the error that caused startup to fail, if any.
func (c *Coordinator) Failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

// Start starts every component in registration order, then launches workers.
//
// If any component fails, the components already started are stopped in reverse
// order and the error is returned. A partially started process is never left
// running, and is never reported ready.
func (c *Coordinator) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.state != StateNew {
		state := c.state
		c.mu.Unlock()
		return fmt.Errorf("coordinator already %s", state)
	}
	c.state = StateStarting
	components := append([]Component(nil), c.components...)
	c.mu.Unlock()

	for _, comp := range components {
		if err := ctx.Err(); err != nil {
			c.unwind(err)
			return err
		}
		if err := comp.Start(ctx); err != nil {
			wrapped := fmt.Errorf("start %s: %w", comp.Name(), err)
			c.mu.Lock()
			c.ready[comp.Name()] = false
			c.mu.Unlock()
			c.unwind(wrapped)
			return wrapped
		}
		c.mu.Lock()
		c.started = append(c.started, comp)
		c.ready[comp.Name()] = true
		c.mu.Unlock()
	}

	// Workers start only once every component is up.
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.cancelWork = cancel
	workers := append([]Worker(nil), c.workers...)
	c.state = StateReady
	c.mu.Unlock()

	for _, w := range workers {
		c.workerWG.Add(1)
		go func(w Worker) {
			defer c.workerWG.Done()
			err := w.Run(workCtx)
			if err != nil && !errors.Is(err, context.Canceled) {
				c.workerMu.Lock()
				c.workerErrs = append(c.workerErrs, fmt.Errorf("worker %s: %w", w.Name(), err))
				c.workerMu.Unlock()
			}
		}(w)
	}

	return nil
}

// unwind stops already-started components after a failed start.
func (c *Coordinator) unwind(cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.shutdownTimeout)
	defer cancel()
	c.stopComponents(ctx)

	c.mu.Lock()
	c.state = StateFailed
	c.failure = cause
	c.mu.Unlock()
}

// stopComponents stops started components in reverse order.
func (c *Coordinator) stopComponents(ctx context.Context) []error {
	c.mu.Lock()
	started := c.started
	c.started = nil
	c.mu.Unlock()

	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		comp := started[i]
		if err := comp.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", comp.Name(), err))
		}
		c.mu.Lock()
		c.ready[comp.Name()] = false
		c.mu.Unlock()
	}
	return errs
}

// Stop shuts down within the configured timeout.
//
// Order matters: workers are cancelled and drained first, then components are
// closed in reverse start order. Stopping listeners before workers would let
// in-flight work fail against closed resources; this way the process stops
// accepting new work, lets current work finish, and only then releases what it
// holds.
//
// Stop returns ErrShutdownTimeout if the drain window elapsed, joined with any
// component or worker errors. It is safe to call more than once.
func (c *Coordinator) Stop(ctx context.Context) error {
	c.mu.Lock()
	switch c.state {
	case StateStopped:
		c.mu.Unlock()
		return nil
	case StateStopping:
		c.mu.Unlock()
		return nil
	}
	c.state = StateStopping
	cancel := c.cancelWork
	c.mu.Unlock()

	deadline, cancelDeadline := context.WithTimeout(ctx, c.shutdownTimeout)
	defer cancelDeadline()

	// Stop accepting work and let workers observe cancellation.
	if cancel != nil {
		cancel()
	}

	var errs []error
	timedOut := false

	done := make(chan struct{})
	go func() {
		c.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-deadline.Done():
		timedOut = true
	}

	errs = append(errs, c.stopComponents(deadline)...)

	c.workerMu.Lock()
	errs = append(errs, c.workerErrs...)
	c.workerErrs = nil
	c.workerMu.Unlock()

	c.mu.Lock()
	c.state = StateStopped
	c.mu.Unlock()

	if timedOut {
		errs = append([]error{ErrShutdownTimeout}, errs...)
	}
	return errors.Join(errs...)
}

// ComponentNames returns the registered component names in start order.
func (c *Coordinator) ComponentNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.components))
	for _, comp := range c.components {
		names = append(names, comp.Name())
	}
	return names
}

// SortedReady returns component names sorted, with their readiness, for
// deterministic reporting.
func (c *Coordinator) SortedReady() ([]string, map[string]bool) {
	ready := c.ReadyComponents()
	names := make([]string, 0, len(ready))
	for k := range ready {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, ready
}
