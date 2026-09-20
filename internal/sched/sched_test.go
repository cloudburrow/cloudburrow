package sched

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func start() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// waitFor polls a condition with a bounded deadline. Used only to observe a
// goroutine reaching a point — never to let real time pass for scheduling.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFakeClockFiresOnlyWhenDue(t *testing.T) {
	t.Parallel()
	c := NewFakeClock(start())
	ch := c.After(10 * time.Second)

	c.Advance(9 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired before its deadline")
	default:
	}

	c.Advance(time.Second)
	select {
	case got := <-ch:
		if !got.Equal(start().Add(10 * time.Second)) {
			t.Errorf("fired at %v, want %v", got, start().Add(10*time.Second))
		}
	default:
		t.Fatal("timer did not fire at its deadline")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()
	b := Backoff{Min: time.Second, Max: 10 * time.Second, Multiplier: 2, MaxAttempts: 10}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
	// A large attempt count must not overflow into a negative duration that
	// would never fire.
	if got := b.Delay(1000); got != b.Max {
		t.Errorf("Delay(1000) = %v, want the cap %v", got, b.Max)
	}
}

func TestShouldRetryRespectsMaxAttempts(t *testing.T) {
	t.Parallel()
	b := Backoff{MaxAttempts: 3}
	for attempt, want := range map[int]bool{1: true, 2: true, 3: false, 4: false} {
		if got := b.ShouldRetry(attempt); got != want {
			t.Errorf("ShouldRetry(%d) = %v, want %v", attempt, got, want)
		}
	}
	if !(Backoff{MaxAttempts: 0}).ShouldRetry(1000) {
		t.Error("zero MaxAttempts should mean unlimited")
	}
}

// Retry timing is verified by advancing virtual time, not by sleeping.
func TestRetryUsesBackoffWithoutSleeping(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(start())
	var attempts int64
	fail := errors.New("transient")

	s := New(clock, Backoff{Min: time.Second, Max: time.Minute, Multiplier: 2, MaxAttempts: 4},
		func(context.Context, Job) error {
			atomic.AddInt64(&attempts, 1)
			return fail
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	if err := s.Schedule(Job{ID: "j", Due: clock.Now()}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "first attempt", func() bool { return atomic.LoadInt64(&attempts) == 1 })

	// Each advance must trigger exactly one more attempt, at the backoff delay.
	for i, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		waitFor(t, "timer armed", func() bool { return clock.Waiters() > 0 })
		clock.Advance(delay)
		want := int64(i + 2)
		waitFor(t, "retry", func() bool { return atomic.LoadInt64(&attempts) == want })
	}

	// The 4th attempt exhausts MaxAttempts, so the job is dropped and reported.
	waitFor(t, "failure recorded", func() bool { return len(s.Failures()) == 1 })
	if s.Pending() != 0 {
		t.Errorf("Pending() = %d, want 0 after exhausting retries", s.Pending())
	}
	if got := s.Failures()[0]; !errors.Is(got, fail) {
		t.Errorf("failure = %v, want it to wrap the cause", got)
	}

	cancel()
	<-done
}

func TestSuccessfulJobIsNotRetried(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(start())
	var attempts int64
	s := New(clock, DefaultBackoff(), func(context.Context, Job) error {
		atomic.AddInt64(&attempts, 1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	_ = s.Schedule(Job{ID: "ok", Due: clock.Now()})
	waitFor(t, "run", func() bool { return atomic.LoadInt64(&attempts) == 1 })
	waitFor(t, "removal", func() bool { return s.Pending() == 0 })

	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if n := atomic.LoadInt64(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1; a successful job was retried", n)
	}
}

// A job is not due until its time arrives, however long the test waits.
func TestJobNotRunBeforeDueTime(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(start())
	var ran int64
	s := New(clock, DefaultBackoff(), func(context.Context, Job) error {
		atomic.AddInt64(&ran, 1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	_ = s.Schedule(Job{ID: "later", Due: clock.Now().Add(time.Hour)})
	time.Sleep(30 * time.Millisecond)
	if atomic.LoadInt64(&ran) != 0 {
		t.Fatal("job ran before its due time")
	}
	waitFor(t, "timer armed", func() bool { return clock.Waiters() > 0 })
	clock.Advance(time.Hour)
	waitFor(t, "job run", func() bool { return atomic.LoadInt64(&ran) == 1 })
}

// Two workers must never run the same attempt concurrently.
func TestNoConcurrentDuplicateAttempts(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(start())
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	release := make(chan struct{})

	s := New(clock, DefaultBackoff(), func(context.Context, Job) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	_ = s.Schedule(Job{ID: "single", Due: clock.Now()})
	waitFor(t, "handler entered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inFlight == 1
	})
	// Poke the scheduler repeatedly while the handler is blocked: the same job
	// must not be claimed a second time.
	for i := 0; i < 50; i++ {
		clock.Advance(time.Millisecond)
	}
	mu.Lock()
	peak := maxInFlight
	mu.Unlock()
	close(release)

	if peak > 1 {
		t.Errorf("the same job ran %d times concurrently", peak)
	}
	cancel()
}

func TestCancelRemovesJob(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(start())
	var ran int64
	s := New(clock, DefaultBackoff(), func(context.Context, Job) error {
		atomic.AddInt64(&ran, 1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	_ = s.Schedule(Job{ID: "doomed", Due: clock.Now().Add(time.Minute)})
	s.Cancel("doomed")
	clock.Advance(2 * time.Minute)
	time.Sleep(30 * time.Millisecond)
	if atomic.LoadInt64(&ran) != 0 {
		t.Error("a cancelled job still ran")
	}
	// Cancelling an unknown job must not panic or error.
	s.Cancel("never-existed")
}

// Run must not return until in-flight attempts finish, so a caller that
// cancels and waits knows no handler is still running.
func TestRunDrainsInFlightWork(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(start())
	entered := make(chan struct{})
	finished := make(chan struct{})

	s := New(clock, DefaultBackoff(), func(ctx context.Context, _ Job) error {
		close(entered)
		time.Sleep(50 * time.Millisecond)
		close(finished)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	_ = s.Schedule(Job{ID: "slow", Due: clock.Now()})
	<-entered
	cancel()
	<-done

	select {
	case <-finished:
	default:
		t.Error("Run returned while a handler was still running")
	}
}
