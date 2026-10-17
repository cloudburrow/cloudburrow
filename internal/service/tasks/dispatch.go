package tasks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
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
	// tokens mints the Authorization header of a task with oidcToken or
	// oauthToken (#1243).
	tokens TokenMinter
	// logs writes the attempts a queue's stackdriver_logging_config samples
	// to Cloud Logging (#1256); nil when logging is not served.
	logs LogWriter
	// appEngine is the base URL of the local server standing in for the
	// App Engine app (--appengine-url, #1506); "" fails App Engine
	// attempts 404.
	appEngine string
}

// WithTokens mints the tokens of tasks with oidcToken or oauthToken with m
// and returns the dispatcher.
func (d *Dispatcher) WithTokens(m TokenMinter) *Dispatcher {
	d.tokens = m
	return d
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
		// No client timeout: each attempt runs under its task's own
		// dispatch_deadline (#579). A fixed 30 s used to fail a handler
		// that takes 40 s, which succeeds on Google.
		client = &http.Client{}
	}
	if clock == nil {
		clock = sched.RealClock{}
	}
	return &Dispatcher{store: s, client: client, clock: clock, maxBody: 64 << 10}
}

// RetryBackoff is a queue's retry schedule, as Cloud Tasks computes it. The
// schedule lives in internal/sched so Cloud Scheduler can share it without
// importing this package (#599).
type RetryBackoff = sched.CloudTasksBackoff

// Backoff converts a queue's retry configuration into its retry schedule,
// defaulting each unset field as the service does.
//
// MaxDoublings caps exponential growth: beyond it, Cloud Tasks increases the
// delay linearly rather than continuing to double. Modelling that faithfully
// matters because a queue configured with a small MaxDoublings would otherwise
// back off far more aggressively than the real service. It used to be ignored
// here, and every queue doubled until MaxBackoff whatever it was configured
// with (#276).
func Backoff(rc RetryConfig) RetryBackoff { return sched.CloudTasksBackoff(rc).WithDefaults() }

// DefaultDispatchDeadline is Google's per-attempt deadline for an HTTP task
// that sets none (Task.dispatch_deadline).
const DefaultDispatchDeadline = 10 * time.Minute

// Dispatch performs one attempt and records the outcome.
//
// It returns nil when the attempt succeeded and the task was removed, and a
// non-nil error when the task should be retried.
func (d *Dispatcher) Dispatch(ctx context.Context, task Task) error {
	deadline := task.DispatchDeadline
	if deadline <= 0 {
		deadline = DefaultDispatchDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var span trace.Span
	if d.tracer != nil && task.Traceparent != "" {
		ctx = telemetry.Propagator.Extract(ctx, propagation.MapCarrier{"traceparent": task.Traceparent})
		ctx, span = d.tracer.Start(ctx, "CloudTasks dispatch", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("cloud_tasks.task", task.Name), attribute.Int("cloud_tasks.retry_count", task.DispatchCount)))
		defer span.End()
	}
	// The queue's http_target, as the queue is now (#1282), on a copy: the
	// task keeps its own httpRequest.
	sent := task
	if task.HTTPRequest != nil {
		if q, err := d.store.GetQueue(task.Queue); err == nil && len(q.HTTPTarget) > 0 {
			hr := withQueueTarget(*task.HTTPRequest, q)
			sent.HTTPRequest = &hr
		}
	}
	if task.AppEngineHTTPRequest != nil && d.appEngine == "" {
		// No App Engine here: the attempt fails (#1447).
		if task.FirstAttempt.IsZero() {
			task.FirstAttempt = d.clock.Now()
		}
		task.DispatchCount++
		started := d.clock.Now()
		err := d.attemptAppEngine(&task, started)
		d.logAttempt(ctx, task, AppEngineAttemptStatus, nil, started)
		if span != nil {
			span.SetAttributes(attribute.Int("http.response.status_code", AppEngineAttemptStatus))
			span.SetStatus(otelcodes.Error, err.Error())
		}
		return err
	}
	req, err := d.buildRequest(ctx, sent)
	if err != nil {
		return err
	}
	if span != nil {
		// The target's own traceparent, if the task set one, is left alone.
		if req.Header.Get("traceparent") == "" {
			telemetry.Propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
		}
	}

	if task.FirstAttempt.IsZero() {
		task.FirstAttempt = d.clock.Now()
	}
	task.DispatchCount++
	started := d.clock.Now()
	resp, err := d.client.Do(req)
	if err != nil {
		// No response at all: record the attempt and ask for a retry.
		task.LastAttempt = &Attempt{Scheduled: task.ScheduleTime, Dispatched: started}
		_ = d.store.UpdateTask(task)
		d.report(task, 0, err)
		d.logAttempt(ctx, task, 0, err, started)
		return fmt.Errorf("dispatch %s: %w", task.Name, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, d.maxBody))

	task.ResponseCount++
	task.LastResponseCode = resp.StatusCode
	task.LastAttempt = &Attempt{Scheduled: task.ScheduleTime, Dispatched: started, Responded: d.clock.Now(),
		Code: int32(attemptStatus(resp.StatusCode, nil))}
	if code := attemptStatus(resp.StatusCode, nil); code != 0 {
		task.LastAttempt.Message = resp.Status
	}
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
	d.logAttempt(ctx, task, resp.StatusCode, nil, started)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return d.store.DeleteTask(task.Name)
	}
	_ = d.store.UpdateTask(task)
	return fmt.Errorf("task %s returned HTTP %d", task.Name, resp.StatusCode)
}

func (d *Dispatcher) buildRequest(ctx context.Context, task Task) (*http.Request, error) {
	if task.AppEngineHTTPRequest != nil {
		return d.appEngineRequest(ctx, task)
	}
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
	if err := d.authorize(req, task); err != nil {
		return nil, err
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

// DefaultOAuthScope is the scope of an oauthToken that names none.
const DefaultOAuthScope = "https://www.googleapis.com/auth/cloud-platform"

// authorize sets the Authorization header a task's oidcToken or oauthToken
// asks for, minted afresh for each attempt as Cloud Tasks does. It replaces
// any Authorization the task's own headers set.
func (d *Dispatcher) authorize(req *http.Request, task Task) error {
	hr := task.HTTPRequest
	if hr.OIDCToken == nil && hr.OAuthToken == nil {
		return nil
	}
	if d.tokens == nil {
		return fmt.Errorf("task %s asks for a token, and this instance has no local credentials to mint one", task.Name)
	}
	var tok string
	var err error
	if o := hr.OIDCToken; o != nil {
		aud := o.Audience
		if aud == "" {
			aud = hr.URL
		}
		tok, err = d.tokens.ServiceAccountIDToken(o.ServiceAccountEmail, aud)
	} else {
		scope := hr.OAuthToken.Scope
		if scope == "" {
			scope = DefaultOAuthScope
		}
		tok, err = d.tokens.ServiceAccountAccessToken(hr.OAuthToken.ServiceAccountEmail, scope)
	}
	if err != nil {
		return fmt.Errorf("mint the token for %s: %w", task.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
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

	// Attempts run concurrently (#579): one slow handler used to hold up
	// every other queue. inflight keeps a task from being dispatched again
	// while an attempt is out, perQueue bounds each queue by its
	// max_concurrent_dispatches, and running lets a pass or a shutdown wait
	// for the attempts it started.
	mu       sync.Mutex
	inflight map[string]bool
	perQueue map[string]int
	running  sync.WaitGroup
	// buckets holds each queue's token bucket, which paces its dispatches
	// to max_dispatches_per_second with bursts of up to BurstSize (#716).
	buckets map[string]*rateBucket
}

// BurstSize is the max_burst_size CloudBurrow uses for a rate: one second's
// worth of dispatches, rounded up, and at least 1. Cloud Tasks documents
// max_burst_size as output only and picked "based on the value of
// max_dispatches_per_second", without the rule; this is CloudBurrow's own
// choice. One second is the smallest burst that lets a queue reach its rate
// when the worker polls less often than once a dispatch, and at a rate of
// 1/s or less it allows no burst at all.
func BurstSize(rate float64) int {
	if rate <= 0 || math.IsNaN(rate) {
		rate = DefaultRateLimits().MaxDispatchesPerSecond
	}
	if rate > math.MaxInt32 {
		return math.MaxInt32
	}
	return max(1, int(math.Ceil(rate)))
}

// rateBucket is a token bucket kept as the time its next token is due, the
// generic cell rate algorithm, so it is exact in integer time on any clock:
// a token is available while next is at most (burst-1) intervals ahead of
// now, and a full bucket is one whose next is not ahead of now at all.
type rateBucket struct {
	next time.Time
}

// take removes a token at now if the bucket has one, refilling it at rate
// per second up to burst.
func (b *rateBucket) take(now time.Time, rate float64, burst int) bool {
	if rate <= 0 || math.IsNaN(rate) {
		rate = DefaultRateLimits().MaxDispatchesPerSecond
	}
	interval := time.Duration(math.Min(float64(time.Second)/rate, float64(math.MaxInt64/4)))
	if interval <= 0 {
		interval = 1
	}
	tolerance := time.Duration(math.Min(float64(interval)*float64(burst-1), float64(math.MaxInt64/4)))
	if now.Before(b.next.Add(-tolerance)) {
		return false
	}
	if b.next.Before(now) {
		// Idle long enough to be full: tokens beyond burst are not banked.
		b.next = now
	}
	b.next = b.next.Add(interval)
	return true
}

// NewWorker returns a dispatch worker.
func NewWorker(s *Store, d *Dispatcher, clock sched.Clock, interval time.Duration) *Worker {
	if clock == nil {
		clock = sched.RealClock{}
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	return &Worker{store: s, dispatcher: d, clock: clock, interval: interval,
		inflight: map[string]bool{}, perQueue: map[string]int{}, buckets: map[string]*rateBucket{}}
}

func (w *Worker) Name() string { return "tasks-dispatcher" }

// Run dispatches due tasks until ctx is cancelled, and waits for the
// attempts it started before returning.
func (w *Worker) Run(ctx context.Context) error {
	defer w.running.Wait()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.clock.After(w.interval):
		}
		w.startDue(ctx)
	}
}

// dispatchDue runs one pass over the due tasks and waits for its attempts.
func (w *Worker) dispatchDue(ctx context.Context) {
	w.startDue(ctx)
	w.running.Wait()
}

// startDue starts an attempt for every due task that is not already in
// flight, as far as its queue's max_concurrent_dispatches and its token
// bucket, filled at max_dispatches_per_second (#716), allow. Retries take a
// token like first attempts: both are dispatches.
func (w *Worker) startDue(ctx context.Context) {
	now := w.clock.Now()
	due, err := w.store.DueTasks(now)
	if err != nil {
		return
	}
	w.startAll(ctx, now, due)
}

// startAll starts the tasks of due, a list read at now, as startDue says.
//
// The list can be stale by the time a task's turn comes: an attempt that
// was in flight when it was read may have ended since, deleting the task
// (a 2xx) or rescheduling it (a retry), and cleared its in-flight mark. So
// each task is read again once it is marked in flight, when no other
// attempt of it can be out, and is started only if it is still there and
// still due. Without that a soak saw 340 of 33,141 tasks that answered 200
// delivered twice, the more as the store grew slower (#1594).
func (w *Worker) startAll(ctx context.Context, now time.Time, due []Task) {
	// Read once a pass, so a queue's new limits apply from the next pass.
	limits := map[string]RateLimits{}
	for _, task := range due {
		if ctx.Err() != nil {
			return
		}
		limit, ok := limits[task.Queue]
		if !ok {
			limit = DefaultRateLimits()
			if q, err := w.store.GetQueue(task.Queue); err == nil {
				if q.RateLimits.MaxConcurrentDispatches > 0 {
					limit.MaxConcurrentDispatches = q.RateLimits.MaxConcurrentDispatches
				}
				if q.RateLimits.MaxDispatchesPerSecond > 0 {
					limit.MaxDispatchesPerSecond = q.RateLimits.MaxDispatchesPerSecond
				}
			}
			limits[task.Queue] = limit
		}
		forced := !task.RunRequested.IsZero()
		w.mu.Lock()
		// A forced run (RunTask, #1215) ignores the queue's rate limits,
		// as documented. One already in flight finishes first: two
		// concurrent attempts of one task would race on its record.
		if forced && !w.inflight[task.Name] {
			w.inflight[task.Name] = true
			w.perQueue[task.Queue]++
			w.mu.Unlock()
			fresh, ok := w.current(task.Name, time.Time{})
			if !ok || !fresh.RunRequested.Equal(task.RunRequested) {
				w.release(task)
				continue
			}
			w.store.TakeRun(fresh.Name, fresh.RunRequested)
			fresh.RunRequested = time.Time{}
			w.start(ctx, fresh)
			continue
		}
		if forced || w.inflight[task.Name] || w.perQueue[task.Queue] >= limit.MaxConcurrentDispatches {
			w.mu.Unlock()
			continue
		}
		b := w.buckets[task.Queue]
		if b == nil {
			b = &rateBucket{}
			w.buckets[task.Queue] = b
		}
		rate := limit.MaxDispatchesPerSecond
		if !b.take(now, rate, BurstSize(rate)) {
			// The queue's bucket is empty; its other due tasks wait too.
			w.mu.Unlock()
			continue
		}
		w.inflight[task.Name] = true
		w.perQueue[task.Queue]++
		w.mu.Unlock()
		fresh, ok := w.current(task.Name, now)
		if !ok {
			w.release(task)
			continue
		}
		w.start(ctx, fresh)
	}
}

// current is the stored task named name, and whether it is still to be
// dispatched: it exists and, when due is not zero, is scheduled at or
// before due. The caller has marked it in flight.
func (w *Worker) current(name string, due time.Time) (Task, bool) {
	t, err := w.store.GetTask(name)
	if err != nil {
		return Task{}, false
	}
	if !due.IsZero() && t.ScheduleTime.After(due) && t.RunRequested.IsZero() {
		return Task{}, false
	}
	return t, true
}

// release clears the in-flight mark of a task that was not started.
func (w *Worker) release(task Task) {
	w.mu.Lock()
	delete(w.inflight, task.Name)
	w.perQueue[task.Queue]--
	w.mu.Unlock()
}

// start runs one attempt of a task already marked in flight.
func (w *Worker) start(ctx context.Context, task Task) {
	w.running.Add(1)
	go func(task Task) {
		defer w.running.Done()
		defer func() {
			w.mu.Lock()
			delete(w.inflight, task.Name)
			w.perQueue[task.Queue]--
			w.mu.Unlock()
		}()
		if err := w.dispatcher.Dispatch(ctx, task); err != nil {
			w.reschedule(task)
		}
	}(task)
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
	if !b.ShouldRetryAfter(current.DispatchCount, w.clock.Now().Sub(current.FirstAttempt)) {
		_ = w.store.DeleteTask(current.Name)
		return
	}
	current.ScheduleTime = w.clock.Now().Add(b.Delay(current.DispatchCount))
	_ = w.store.UpdateTask(current)
}
