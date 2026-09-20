package sched

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrClosed means the scheduler is shutting down.
var ErrClosed = errors.New("scheduler is closed")

// Job is work due at a point in time.
type Job struct {
	// ID is unique within the scheduler.
	ID string
	// Due is when the job should run.
	Due time.Time
	// Attempt counts prior attempts; 0 means it has not run yet.
	Attempt int
	// Payload is caller data.
	Payload any
}

// Handler runs a job. Returning an error schedules a retry if policy allows.
type Handler func(ctx context.Context, job Job) error

// Scheduler runs due jobs, with bounded retry over an injected clock.
//
// Ownership is explicit: the scheduler owns its goroutine and stops when Run's
// context is cancelled. It does not spawn detached work.
type Scheduler struct {
	clock   Clock
	backoff Backoff
	handler Handler

	mu     sync.Mutex
	jobs   map[string]Job
	closed bool
	// claimed guards against two workers running the same attempt.
	claimed map[string]bool

	wake chan struct{}

	mu2      sync.Mutex
	failures []error
}

// New returns a scheduler.
func New(clock Clock, backoff Backoff, handler Handler) *Scheduler {
	if clock == nil {
		clock = RealClock{}
	}
	return &Scheduler{
		clock:   clock,
		backoff: backoff,
		handler: handler,
		jobs:    map[string]Job{},
		claimed: map[string]bool{},
		wake:    make(chan struct{}, 1),
	}
}

// Schedule adds or replaces a job.
func (s *Scheduler) Schedule(job Job) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.jobs[job.ID] = job
	s.mu.Unlock()
	s.notify()
	return nil
}

// Cancel removes a job. Cancelling an unknown job is not an error: the job may
// have already completed, and callers should not have to race to find out.
func (s *Scheduler) Cancel(id string) {
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
	s.notify()
}

// Pending returns the number of scheduled jobs.
func (s *Scheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// Failures returns jobs that exhausted their retries.
func (s *Scheduler) Failures() []error {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	return append([]error(nil), s.failures...)
}

func (s *Scheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// due returns the jobs ready to run and the wait until the next one.
func (s *Scheduler) due(now time.Time) (ready []Job, next time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next = time.Hour
	for _, j := range s.jobs {
		if j.Due.After(now) {
			if d := j.Due.Sub(now); d < next {
				next = d
			}
			continue
		}
		if s.claimed[j.ID] {
			// Another attempt is in flight; never run the same attempt twice.
			continue
		}
		s.claimed[j.ID] = true
		ready = append(ready, j)
	}
	// Deterministic order so tests and logs are reproducible.
	sort.Slice(ready, func(i, k int) bool { return ready[i].ID < ready[k].ID })
	return ready, next
}

// Run processes jobs until ctx is cancelled.
//
// It returns only when every in-flight attempt has finished, so a caller that
// cancels and waits knows no handler is still running.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}()

	for {
		ready, next := s.due(s.clock.Now())
		for _, job := range ready {
			wg.Add(1)
			go func(job Job) {
				defer wg.Done()
				s.attempt(ctx, job)
			}(job)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
		case <-s.clock.After(next):
		}
	}
}

// attempt runs one job and applies the retry policy.
func (s *Scheduler) attempt(ctx context.Context, job Job) {
	err := s.handler(ctx, job)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed, job.ID)

	if err == nil {
		delete(s.jobs, job.ID)
		return
	}
	if ctx.Err() != nil {
		// Shutting down: leave the job scheduled rather than consuming an
		// attempt for a cancellation that was not the job's fault.
		return
	}

	next := job
	next.Attempt++
	if !s.backoff.ShouldRetry(next.Attempt) {
		delete(s.jobs, job.ID)
		s.mu2.Lock()
		s.failures = append(s.failures, fmt.Errorf("job %s failed after %d attempts: %w", job.ID, next.Attempt, err))
		s.mu2.Unlock()
		return
	}
	next.Due = s.clock.Now().Add(s.backoff.Delay(next.Attempt))
	s.jobs[job.ID] = next
	s.notify()
}
