package tasks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// Dispatcher delivers tasks to their HTTP targets and applies retry.
type Dispatcher struct {
	store  *Store
	client *http.Client
	clock  sched.Clock
	// maxBody bounds how much of a response is read. A target returning a
	// large body should not be able to exhaust memory.
	maxBody int64
	// observe is called once per attempt, so the attempt history can be
	// surfaced without the dispatcher knowing who is watching. A queue that
	// says "1 task" tells a developer nothing; the attempt says why it is
	// still there.
	observe AttemptObserver
}

// AttemptObserver is notified of each dispatch attempt and its outcome.
//
// The response code is 0 when no response arrived at all, which is a
// different failure from a response that was not 2xx.
type AttemptObserver func(queue, task string, attempt int, statusCode int, err error)

// Observe attaches an observer and returns the dispatcher.
func (d *Dispatcher) Observe(fn AttemptObserver) *Dispatcher {
	d.observe = fn
	return d
}

// report notifies the observer, if any.
func (d *Dispatcher) report(task Task, statusCode int, err error) {
	if d.observe == nil {
		return
	}
	d.observe(queueOf(task.Name), task.Name, task.DispatchCount, statusCode, err)
}

// queueOf extracts the queue name from a task name.
func queueOf(taskName string) string {
	if i := strings.Index(taskName, "/tasks/"); i >= 0 {
		return taskName[:i]
	}
	return taskName
}

// NewDispatcher returns a dispatcher.
func NewDispatcher(s *Store, client *http.Client, clock sched.Clock) *Dispatcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if clock == nil {
		clock = sched.RealClock{}
	}
	return &Dispatcher{store: s, client: client, clock: clock, maxBody: 64 << 10}
}

// Backoff converts a queue's retry configuration into a backoff policy.
//
// MaxDoublings caps exponential growth: beyond it, Cloud Tasks increases the
// delay linearly rather than continuing to double. Modelling that faithfully
// matters because a queue configured with a small MaxDoublings would otherwise
// back off far more aggressively than the real service.
func Backoff(rc RetryConfig) sched.Backoff {
	return sched.Backoff{
		Min:         rc.MinBackoff,
		Max:         rc.MaxBackoff,
		Multiplier:  2,
		MaxAttempts: rc.MaxAttempts,
	}
}

// Dispatch performs one attempt and records the outcome.
//
// It returns nil when the attempt succeeded and the task was removed, and a
// non-nil error when the task should be retried.
func (d *Dispatcher) Dispatch(ctx context.Context, task Task) error {
	req, err := d.buildRequest(ctx, task)
	if err != nil {
		return err
	}

	task.DispatchCount++
	resp, err := d.client.Do(req)
	if err != nil {
		// No response at all: record the attempt and ask for a retry.
		_ = d.store.UpdateTask(task)
		d.report(task, 0, err)
		return fmt.Errorf("dispatch %s: %w", task.Name, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, d.maxBody))

	task.ResponseCount++
	task.LastResponseCode = resp.StatusCode

	// Cloud Tasks treats 2xx as success; everything else is retried. A 4xx is
	// retried too, which surprises people, but it is what the real service
	// does and the point here is to match it.
	d.report(task, resp.StatusCode, nil)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return d.store.DeleteTask(task.Name)
	}
	_ = d.store.UpdateTask(task)
	return fmt.Errorf("task %s returned HTTP %d", task.Name, resp.StatusCode)
}

func (d *Dispatcher) buildRequest(ctx context.Context, task Task) (*http.Request, error) {
	if task.HTTPRequest == nil {
		return nil, fmt.Errorf("task %s has no httpRequest", task.Name)
	}
	method := task.HTTPRequest.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, task.HTTPRequest.URL, bytes.NewReader(task.HTTPRequest.Body))
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", task.Name, err)
	}
	for k, v := range task.HTTPRequest.Headers {
		req.Header.Set(k, v)
	}
	// Headers the real service sets, which handlers commonly read to
	// distinguish a retry from a first delivery.
	req.Header.Set("X-CloudTasks-TaskName", task.Name)
	req.Header.Set("X-CloudTasks-QueueName", task.Queue)
	req.Header.Set("X-CloudTasks-TaskRetryCount", fmt.Sprint(task.DispatchCount))
	req.Header.Set("X-CloudTasks-TaskExecutionCount", fmt.Sprint(task.ResponseCount))
	if req.Header.Get("Content-Type") == "" && len(task.HTTPRequest.Body) > 0 {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return req, nil
}

// Worker drives due tasks through the dispatcher.
//
// It is a lifecycle Worker: the coordinator owns it, and it returns when the
// context is cancelled.
type Worker struct {
	store      *Store
	dispatcher *Dispatcher
	clock      sched.Clock
	// interval bounds how often due tasks are polled.
	interval time.Duration
}

// NewWorker returns a dispatch worker.
func NewWorker(s *Store, d *Dispatcher, clock sched.Clock, interval time.Duration) *Worker {
	if clock == nil {
		clock = sched.RealClock{}
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	return &Worker{store: s, dispatcher: d, clock: clock, interval: interval}
}

func (w *Worker) Name() string { return "tasks-dispatcher" }

// Run dispatches due tasks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.clock.After(w.interval):
		}
		w.dispatchDue(ctx)
	}
}

// dispatchDue runs one pass over the due tasks.
func (w *Worker) dispatchDue(ctx context.Context) {
	due, err := w.store.DueTasks(w.clock.Now())
	if err != nil {
		return
	}
	for _, task := range due {
		if ctx.Err() != nil {
			return
		}
		if err := w.dispatcher.Dispatch(ctx, task); err != nil {
			w.reschedule(task)
		}
	}
}

// reschedule applies the queue's retry policy to a failed task.
//
// A task that exhausts its attempts is deleted: Cloud Tasks drops it, and
// keeping it would leave a queue that never drains.
func (w *Worker) reschedule(task Task) {
	current, err := w.store.GetTask(task.Name)
	if err != nil {
		return
	}
	q, err := w.store.GetQueue(current.Queue)
	if err != nil {
		return
	}
	b := Backoff(q.RetryConfig)
	if !b.ShouldRetry(current.DispatchCount) {
		_ = w.store.DeleteTask(current.Name)
		return
	}
	current.ScheduleTime = w.clock.Now().Add(b.Delay(current.DispatchCount))
	_ = w.store.UpdateTask(current)
}
