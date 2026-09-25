package tasks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
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
	// tracer, when tracing is on (#313), spans each attempt of a task whose
	// CreateTask was traced, and carries the trace to the target.
	tracer trace.Tracer
}

// AttemptObserver is notified of each dispatch attempt and its outcome.
//
// The response code is 0 when no response arrived at all, which is a
// different failure from a response that was not 2xx.
type AttemptObserver func(queue, task string, attempt int, statusCode int, err error)

// WithTracing traces dispatch attempts with tp and returns the dispatcher.
func (d *Dispatcher) WithTracing(tp trace.TracerProvider) *Dispatcher {
	d.tracer = tp.Tracer("github.com/cloudburrow/cloudburrow/internal/service/tasks")
	return d
}

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

// RetryBackoff is a queue's retry schedule, as Cloud Tasks computes it.
type RetryBackoff struct{ rc RetryConfig }

// Backoff converts a queue's retry configuration into its retry schedule.
//
// MaxDoublings caps exponential growth: beyond it, Cloud Tasks increases the
// delay linearly rather than continuing to double. Modelling that faithfully
// matters because a queue configured with a small MaxDoublings would otherwise
// back off far more aggressively than the real service. It used to be ignored
// here, and every queue doubled until MaxBackoff whatever it was configured
// with (#276).
func Backoff(rc RetryConfig) RetryBackoff { return RetryBackoff{rc: rc.withDefaults()} }

// Delay returns the wait before the given retry. Attempt 1 is the first retry.
//
// The schedule is the one Cloud Tasks documents: start at MinBackoff, double
// MaxDoublings times, then grow by the last doubled interval each retry, and
// never exceed MaxBackoff. With 10s, 300s and 3 doublings that is 10s, 20s,
// 40s, 80s, 160s, 240s, 300s, 300s...
func (b RetryBackoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	minB, maxB := float64(b.rc.MinBackoff), float64(b.rc.MaxBackoff)
	k, doublings := attempt-1, b.rc.MaxDoublings
	var d float64
	if k <= doublings {
		d = minB * math.Pow(2, float64(k))
	} else {
		step := minB * math.Pow(2, float64(doublings))
		d = step * float64(k-doublings+1)
	}
	// Compared as floats: a large attempt count would overflow a Duration and
	// come out negative, which would fire at once.
	if d > maxB || math.IsInf(d, 0) || math.IsNaN(d) {
		return b.rc.MaxBackoff
	}
	return time.Duration(d)
}

// ShouldRetry reports whether another attempt is permitted after the given
// number of attempts. A negative MaxAttempts is unlimited, as in the API.
func (b RetryBackoff) ShouldRetry(attempts int) bool {
	if b.rc.MaxAttempts < 0 {
		return true
	}
	return attempts < b.rc.MaxAttempts
}

// Dispatch performs one attempt and records the outcome.
//
// It returns nil when the attempt succeeded and the task was removed, and a
// non-nil error when the task should be retried.
func (d *Dispatcher) Dispatch(ctx context.Context, task Task) error {
	var span trace.Span
	if d.tracer != nil && task.Traceparent != "" {
		ctx = telemetry.Propagator.Extract(ctx, propagation.MapCarrier{"traceparent": task.Traceparent})
		ctx, span = d.tracer.Start(ctx, "CloudTasks dispatch", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("cloud_tasks.task", task.Name), attribute.Int("cloud_tasks.retry_count", task.DispatchCount)))
		defer span.End()
	}
	req, err := d.buildRequest(ctx, task)
	if err != nil {
		return err
	}
	if span != nil {
		// The target's own traceparent, if the task set one, is left alone.
		if req.Header.Get("traceparent") == "" {
			telemetry.Propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
		}
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
	if span != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			span.SetStatus(otelcodes.Error, resp.Status)
		}
	}

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
	// distinguish a retry from a first delivery. The names are the short IDs,
	// "my-task" rather than projects/.../tasks/my-task, as the service sends
	// them; the full names were sent here until #276, so a handler written
	// against CloudBurrow would have parsed a value production never sends.
	req.Header.Set("X-CloudTasks-TaskName", task.Name[strings.LastIndex(task.Name, "/")+1:])
	req.Header.Set("X-CloudTasks-QueueName", task.Queue[strings.LastIndex(task.Queue, "/")+1:])
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
