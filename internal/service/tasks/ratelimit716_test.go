package tasks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// rateHarness is a worker on a fake clock whose target counts deliveries.
type rateHarness struct {
	t         *testing.T
	s         *Store
	w         *Worker
	clock     *sched.FakeClock
	delivered atomic.Int64
	url       string
}

func newRateHarness(t *testing.T) *rateHarness {
	t.Helper()
	h := &rateHarness{t: t, s: newStore(t), clock: sched.NewFakeClock(time.Now())}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	h.w = NewWorker(h.s, NewDispatcher(h.s, srv.Client(), h.clock), h.clock, time.Second)
	return h
}

// queue creates a queue limited to rate dispatches a second.
func (h *rateHarness) queue(name string, rate float64) {
	h.t.Helper()
	if _, err := h.s.CreateQueue(Queue{Name: name, RateLimits: RateLimits{MaxDispatchesPerSecond: rate, MaxConcurrentDispatches: 1000}}); err != nil {
		h.t.Fatal(err)
	}
}

// tasks enqueues n tasks due now.
func (h *rateHarness) tasks(queue string, n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%d-%d", i, h.clock.Now().UnixNano())
		if _, err := h.s.CreateTask(Task{Name: TaskName(queue, id), HTTPRequest: &HTTPRequest{URL: h.url}, ScheduleTime: h.clock.Now()}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// pass runs one dispatch pass after advancing the clock by d and returns
// how many deliveries it made.
func (h *rateHarness) pass(d time.Duration) int64 {
	h.clock.Advance(d)
	before := h.delivered.Load()
	h.w.dispatchDue(context.Background())
	return h.delivered.Load() - before
}

// A queue limited to 1/s dispatches one task a second, however many are due
// and however often the worker polls (#716). Before, every due task went at
// once.
func TestMaxDispatchesPerSecondSpacesDispatches(t *testing.T) {
	t.Parallel()
	h := newRateHarness(t)
	h.queue(queueA, 1)
	h.tasks(queueA, 3)
	for i, step := range []struct {
		advance time.Duration
		want    int64
	}{
		{0, 1},
		{0, 0},
		{999 * time.Millisecond, 0},
		{time.Millisecond, 1},
		{500 * time.Millisecond, 0},
		{500 * time.Millisecond, 1},
	} {
		if got := h.pass(step.advance); got != step.want {
			t.Fatalf("pass %d, after %v: %d deliveries, want %d", i, step.advance, got, step.want)
		}
	}
	if left, _ := h.s.ListTasks(queueA); len(left) != 0 {
		t.Errorf("%d tasks left", len(left))
	}
}

// A rate below 1/s spaces dispatches by more than a second.
func TestAFractionalRateSpacesDispatchesBeyondASecond(t *testing.T) {
	t.Parallel()
	h := newRateHarness(t)
	h.queue(queueA, 0.5)
	h.tasks(queueA, 2)
	if got := h.pass(0); got != 1 {
		t.Fatalf("first pass: %d deliveries, want 1", got)
	}
	if got := h.pass(1999 * time.Millisecond); got != 0 {
		t.Fatalf("1.999 s later: %d deliveries, want 0", got)
	}
	if got := h.pass(time.Millisecond); got != 1 {
		t.Fatalf("2 s later: %d deliveries, want 1", got)
	}
}

// The bucket holds max_burst_size tokens: a full bucket dispatches that many
// at once, then the queue is held to its rate.
func TestMaxBurstSizeBoundsABurstThenTheRateApplies(t *testing.T) {
	t.Parallel()
	h := newRateHarness(t)
	h.queue(queueA, 5) // BurstSize(5) is 5.
	h.tasks(queueA, 9)
	if got := h.pass(0); got != 5 {
		t.Fatalf("a full bucket dispatched %d, want the burst of 5", got)
	}
	if got := h.pass(0); got != 0 {
		t.Fatalf("an empty bucket dispatched %d", got)
	}
	if got := h.pass(200 * time.Millisecond); got != 1 {
		t.Fatalf("200 ms at 5/s dispatched %d, want 1", got)
	}
	if got := h.pass(400 * time.Millisecond); got != 2 {
		t.Fatalf("400 ms at 5/s dispatched %d, want 2", got)
	}
}

// An idle queue does not bank tokens beyond its burst.
func TestAnIdleQueueBanksNoMoreThanItsBurst(t *testing.T) {
	t.Parallel()
	h := newRateHarness(t)
	h.queue(queueA, 2) // BurstSize(2) is 2.
	h.tasks(queueA, 1)
	if got := h.pass(0); got != 1 {
		t.Fatalf("first pass: %d deliveries, want 1", got)
	}
	h.clock.Advance(time.Minute)
	h.tasks(queueA, 5)
	if got := h.pass(0); got != 2 {
		t.Fatalf("after a minute idle: %d deliveries, want the burst of 2", got)
	}
}

// Each queue has its own bucket: a queue held to 1/s does not hold back
// another at the default rate.
func TestEachQueueHasItsOwnBucket(t *testing.T) {
	t.Parallel()
	h := newRateHarness(t)
	queueB := strings.Replace(queueA, "/queues/", "/queues/b-", 1)
	h.queue(queueA, 1)
	mustQueue(t, h.s, queueB)
	h.tasks(queueA, 3)
	h.tasks(queueB, 3)
	if got := h.pass(0); got != 4 {
		t.Fatalf("%d deliveries, want 1 from the limited queue and 3 from the default one", got)
	}
}

// BurstSize is one second of the rate, rounded up, at least 1; an unset
// rate is the default, 500/s. GetQueue returns it as max_burst_size.
func TestBurstSizeIsOneSecondOfTheRate(t *testing.T) {
	t.Parallel()
	for rate, want := range map[float64]int{0: 500, 0.1: 1, 0.5: 1, 1: 1, 1.5: 2, 5: 5, 500: 500} {
		if got := BurstSize(rate); got != want {
			t.Errorf("BurstSize(%v) = %d, want %d", rate, got, want)
		}
	}
	q := toProtoQueue(Queue{Name: queueA, RateLimits: RateLimits{MaxDispatchesPerSecond: 1.5, MaxConcurrentDispatches: 1}})
	if got := q.GetRateLimits().GetMaxBurstSize(); got != 2 {
		t.Errorf("max_burst_size = %d, want 2", got)
	}
}
