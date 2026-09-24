package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// Publisher publishes one message to a Pub/Sub topic. The instance supplies
// one over the local emulator.
type Publisher func(ctx context.Context, topic string, data []byte, attributes map[string]string) error

// Attempt is one delivery attempt, for the observer.
type Attempt struct {
	Job        string
	Attempt    int
	StatusCode int // 0 for a Pub/Sub target or no response
	Err        error
}

// Runner fires jobs when they are due, and when RunJob asks.
//
// Retries use tasks.Backoff, the schedule Cloud Tasks computes, fed from the
// job's RetryConfig: Cloud Scheduler documents the same min/max backoff and
// doublings semantics, so both services back off identically here.
type Runner struct {
	store    *Store
	client   *http.Client
	clock    sched.Clock
	publish  Publisher
	interval time.Duration
	observe  func(Attempt)

	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
	ctx      context.Context
}

// NewRunner returns a runner that checks for due jobs every interval.
func NewRunner(st *Store, client *http.Client, clock sched.Clock, publish Publisher, interval time.Duration) *Runner {
	if client == nil {
		client = &http.Client{}
	}
	if clock == nil {
		clock = sched.RealClock{}
	}
	return &Runner{store: st, client: client, clock: clock, publish: publish, interval: interval,
		inflight: map[string]bool{}, ctx: context.Background()}
}

// Observe attaches an attempt observer and returns the runner.
func (r *Runner) Observe(fn func(Attempt)) *Runner { r.observe = fn; return r }

func (r *Runner) Name() string { return "scheduler-runner" }

// Run fires due jobs until ctx ends, then waits for running jobs to finish.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	defer r.wg.Wait()
	for {
		r.fireDue()
		select {
		case <-ctx.Done():
			return nil
		case <-r.clock.After(r.interval):
		}
	}
}

func (r *Runner) fireDue() {
	jobs, err := r.store.List("")
	if err != nil {
		return
	}
	now := r.clock.Now().UTC()
	for _, j := range jobs {
		if j.State != StateEnabled || j.ScheduleTime.After(now) {
			continue
		}
		// Advance the schedule first, so a slow target is not fired again on
		// the next tick for the same slot.
		next, err := nextRun(j.Schedule, j.TimeZone, now)
		if err != nil {
			continue
		}
		scheduled := j.ScheduleTime
		if _, err := r.store.Update(j.Name, func(cur *Job) error { cur.ScheduleTime = next; return nil }); err != nil {
			continue
		}
		r.fire(j, scheduled)
	}
}

// fire delivers a job in the background, with its retries. A job already
// being delivered is not started a second time.
func (r *Runner) fire(j Job, scheduled time.Time) {
	r.mu.Lock()
	if r.inflight[j.Name] {
		r.mu.Unlock()
		return
	}
	r.inflight[j.Name] = true
	ctx := r.ctx
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); delete(r.inflight, j.Name); r.mu.Unlock() }()
		r.deliver(ctx, j, scheduled)
	}()
}

func (r *Runner) deliver(ctx context.Context, j Job, scheduled time.Time) {
	backoff := tasks.Backoff(tasks.RetryConfig{
		// MaxAttempts counts the first attempt; RetryCount does not.
		MaxAttempts: j.Retry.RetryCount + 1, MinBackoff: j.Retry.MinBackoff,
		MaxBackoff: j.Retry.MaxBackoff, MaxDoublings: j.Retry.MaxDoublings,
	})
	start := r.clock.Now()
	for attempt := 1; ; attempt++ {
		code, err := r.attempt(ctx, j, scheduled, attempt)
		now := r.clock.Now().UTC()
		grpcCode, msg := codes.OK, ""
		if err != nil {
			grpcCode, msg = codes.Unknown, err.Error()
		}
		_, _ = r.store.Update(j.Name, func(cur *Job) error {
			cur.LastAttemptTime, cur.LastCode, cur.LastMessage = now, int32(grpcCode), msg
			return nil
		})
		if r.observe != nil {
			r.observe(Attempt{Job: j.Name, Attempt: attempt, StatusCode: code, Err: err})
		}
		if err == nil || !backoff.ShouldRetry(attempt) {
			return
		}
		if j.Retry.MaxRetryDuration > 0 && r.clock.Now().Sub(start) >= j.Retry.MaxRetryDuration {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(backoff.Delay(attempt)):
		}
	}
}

// attempt performs one delivery.
func (r *Runner) attempt(ctx context.Context, j Job, scheduled time.Time, attempt int) (int, error) {
	switch {
	case j.PubSub != nil:
		if r.publish == nil {
			return 0, fmt.Errorf("Pub/Sub is not enabled on this instance, so %s cannot publish", j.Name)
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return 0, r.publish(pctx, j.PubSub.Topic, j.PubSub.Data, j.PubSub.Attributes)
	case j.HTTP != nil:
		actx, cancel := context.WithTimeout(ctx, j.AttemptDeadline)
		defer cancel()
		req, err := http.NewRequestWithContext(actx, j.HTTP.Method, j.HTTP.URI, bytes.NewReader(j.HTTP.Body))
		if err != nil {
			return 0, err
		}
		for k, v := range j.HTTP.Headers {
			req.Header.Set(k, v)
		}
		// Headers the service sets, which handlers read to tell a scheduled
		// call from any other.
		req.Header.Set("User-Agent", "Google-Cloud-Scheduler")
		req.Header.Set("X-CloudScheduler", "true")
		req.Header.Set("X-CloudScheduler-JobName", j.Name[strings.LastIndex(j.Name, "/")+1:])
		req.Header.Set("X-CloudScheduler-ScheduleTime", scheduled.UTC().Format(time.RFC3339))
		if req.Header.Get("Content-Type") == "" && len(j.HTTP.Body) > 0 {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return resp.StatusCode, fmt.Errorf("%s returned HTTP %d", j.HTTP.URI, resp.StatusCode)
		}
		return resp.StatusCode, nil
	default:
		return 0, fmt.Errorf("job %s has no target", j.Name)
	}
}
