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
	// groups[i] is the concurrent group components[i] belongs to, or 0
	// when it starts on its own (RegisterConcurrent).
	groups    []int
	nextGroup int
	workers   []Worker
	started   []Component // successfully started, in start order
	state     State
	failure   error
	ready     map[string]bool
	// timing is when each component started and how long it took (#312).
	timing map[string]Timing
	now    func() time.Time

	workerWG   sync.WaitGroup
	workerMu   sync.Mutex
	workerErrs []error
	cancelWork context.CancelFunc
	// workCtx is the context workers run under, kept so a worker registered
	// after startup can join the same lifetime rather than outliving it.
	workCtx context.Context
}

// New returns a Coordinator with the given bounded shutdown timeout.
func New(shutdownTimeout time.Duration) *Coordinator {
	return &Coordinator{
		shutdownTimeout: shutdownTimeout,
		state:           StateNew,
		ready:           map[string]bool{},
		timing:          map[string]Timing{},
		now:             time.Now,
	}
}

// Timing is one component's start: when it began and how long it took to
// be ready. ReadyAfter is zero until it is ready.
type Timing struct {
	StartedAt  time.Time
	ReadyAfter time.Duration
}

// Timings returns every component's timing so far, by name.
func (c *Coordinator) Timings() map[string]Timing {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]Timing, len(c.timing))
	for k, v := range c.timing {
		out[k] = v
	}
	return out
}

// TimingOrder is the component names in start order, for a summary.
func (c *Coordinator) TimingOrder() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.components))
	for _, comp := range c.components {
		if _, ok := c.timing[comp.Name()]; ok {
			names = append(names, comp.Name())
		}
	}
	return names
}

// Register adds a component. Components start in registration order and stop in
// reverse, so a component may depend on those registered before it.
func (c *Coordinator) Register(components ...Component) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.components = append(c.components, components...)
	c.groups = append(c.groups, make([]int, len(components))...)
}

// RegisterConcurrent adds components that depend on what was registered
// before them but not on each other, so they start at the same time: the
// next registration starts once every one of them has. Each still has its
// own name, readiness and timing, and they stop in reverse registration
// order like any others. A failure of one cancels the others' starts and
// unwinds everything that started, as a sequential failure does.
func (c *Coordinator) RegisterConcurrent(components ...Component) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextGroup++
	for range components {
		c.groups = append(c.groups, c.nextGroup)
	}
	c.components = append(c.components, components...)
}

// RegisterWorker adds background work. Workers start only after every component
// has started, so a worker never observes a half-initialised process.
// RegisterWorker adds background workers.
//
// A worker registered after the coordinator is already running is started
// immediately. Without that, it would sit in the list and never run: Start
// snapshots the workers once, so anything registered afterwards was silently
// inert — which is how Cloud Tasks dispatch came to be wired up in `up` and
// never actually dispatch anything.
func (c *Coordinator) RegisterWorker(workers ...Worker) {
	c.mu.Lock()
	c.workers = append(c.workers, workers...)
	running := c.state == StateReady && c.cancelWork != nil
	workCtx := c.workCtx
	c.mu.Unlock()

	if !running || workCtx == nil {
		return
	}
	for _, w := range workers {
		c.startWorker(workCtx, w)
	}
}

// startWorker runs one worker and records a failure that is not a shutdown.
func (c *Coordinator) startWorker(ctx context.Context, w Worker) {
	c.workerWG.Add(1)
	go func() {
		defer c.workerWG.Done()
		err := w.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			c.workerMu.Lock()
			c.workerErrs = append(c.workerErrs, fmt.Errorf("worker %s: %w", w.Name(), err))
			c.workerMu.Unlock()
		}
	}()
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
	groups := append([]int(nil), c.groups...)
	c.mu.Unlock()

	for i := 0; i < len(components); {
		if err := ctx.Err(); err != nil {
			c.unwind(err)
			return err
		}
		j := i + 1
		for groups[i] != 0 && j < len(components) && groups[j] == groups[i] {
			j++
		}
		if err := c.startStep(ctx, components[i:j]); err != nil {
			c.unwind(err)
			return err
		}
		i = j
	}

	// Workers start only once every component is up.
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.cancelWork = cancel
	c.workCtx = workCtx
	workers := append([]Worker(nil), c.workers...)
	c.state = StateReady
	c.mu.Unlock()

	for _, w := range workers {
		c.startWorker(workCtx, w)
	}

	return nil
}

// startStep starts one component, or a concurrent group of them, and
// records each one's readiness and timing. A group's members are recorded
// as started in registration order, whatever order they finished in, so
// they stop in reverse registration order.
func (c *Coordinator) startStep(ctx context.Context, step []Component) error {
	if len(step) == 1 {
		err := c.startOne(ctx, step[0])
		if err == nil {
			c.mu.Lock()
			c.started = append(c.started, step[0])
			c.mu.Unlock()
		}
		return err
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(step))
	var wg sync.WaitGroup
	for k, comp := range step {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if errs[k] = c.startOne(gctx, comp); errs[k] != nil {
				cancel()
			}
		}()
	}
	wg.Wait()
	// The first failure in registration order that is not merely the
	// cancellation another member's failure caused.
	var first error
	for _, err := range errs {
		if err != nil && (first == nil || errors.Is(first, context.Canceled) && !errors.Is(err, context.Canceled)) {
			first = err
		}
	}
	// Whatever started is recorded, so the unwind stops it.
	c.mu.Lock()
	for k, comp := range step {
		if errs[k] == nil && c.ready[comp.Name()] {
			c.started = append(c.started, comp)
		}
	}
	c.mu.Unlock()
	return first
}

// startOne starts a component and records its readiness and timing;
// startStep records it as started.
func (c *Coordinator) startOne(ctx context.Context, comp Component) error {
	began := c.now()
	c.mu.Lock()
	c.timing[comp.Name()] = Timing{StartedAt: began}
	c.mu.Unlock()
	if err := comp.Start(ctx); err != nil {
		c.mu.Lock()
		c.ready[comp.Name()] = false
		c.mu.Unlock()
		return fmt.Errorf("start %s: %w", comp.Name(), err)
	}
	c.mu.Lock()
	c.ready[comp.Name()] = true
	c.timing[comp.Name()] = Timing{StartedAt: began, ReadyAfter: c.now().Sub(began)}
	c.mu.Unlock()
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
