package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// Each attempt runs under the task's own dispatch_deadline (#579): a slow
// handler within it succeeds, one beyond it is a failed attempt, recorded
// when the deadline ran out. Scaled down from the acceptance's 45 s and
// 10 s; the dispatcher reads the stored value, and the API's 15 s floor is
// validated separately.
func TestAttemptsRunUnderTheTaskDeadline(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := NewDispatcher(s, srv.Client(), sched.NewFakeClock(time.Now()))

	within := Task{Name: TaskName(queueA, "within"), HTTPRequest: &HTTPRequest{URL: srv.URL}, DispatchDeadline: 3 * time.Second}
	beyond := Task{Name: TaskName(queueA, "beyond"), HTTPRequest: &HTTPRequest{URL: srv.URL}, DispatchDeadline: 100 * time.Millisecond}
	for _, task := range []Task{within, beyond} {
		if _, err := s.CreateTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Dispatch(context.Background(), within); err != nil {
		t.Errorf("a 400 ms handler under a 3 s deadline failed: %v", err)
	}
	start := time.Now()
	if err := d.Dispatch(context.Background(), beyond); err == nil {
		t.Error("a 400 ms handler under a 100 ms deadline succeeded")
	}
	if took := time.Since(start); took > 350*time.Millisecond {
		t.Errorf("the failed attempt took %v; it should end at the 100 ms deadline", took)
	}
	if got, _ := s.GetTask(beyond.Name); got.DispatchCount != 1 || got.LastResponseCode != 0 {
		t.Errorf("the timed-out attempt recorded as %+v", got)
	}
}

// Due tasks in different queues are in flight at the same time; before,
// one slow handler held up every queue (#579).
func TestQueuesDispatchConcurrently(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	queueB := strings.Replace(queueA, "/queues/", "/queues/b-", 1)
	mustQueue(t, s, queueA)
	mustQueue(t, s, queueB)
	var arrived int64
	both := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&arrived, 1) == 2 {
			once.Do(func() { close(both) })
		}
		select {
		case <-both:
		case <-time.After(3 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	for _, q := range []string{queueA, queueB} {
		if _, err := s.CreateTask(Task{Name: TaskName(q, "slow"), HTTPRequest: &HTTPRequest{URL: srv.URL}}); err != nil {
			t.Fatal(err)
		}
	}
	clock := sched.NewFakeClock(time.Now())
	w := NewWorker(s, NewDispatcher(s, srv.Client(), clock), clock, time.Second)
	start := time.Now()
	w.dispatchDue(context.Background())
	select {
	case <-both:
	default:
		t.Fatal("the two queues' tasks were never in flight together")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the pass took %v; the attempts ran one after the other", took)
	}
}

// max_concurrent_dispatches bounds a queue: with 1, a second due task
// waits for the first to finish.
func TestMaxConcurrentDispatchesBoundsAQueue(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, err := s.CreateQueue(Queue{Name: queueA, RateLimits: RateLimits{MaxConcurrentDispatches: 1, MaxDispatchesPerSecond: 500}}); err != nil {
		t.Fatal(err)
	}
	var inFlight, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	for _, id := range []string{"one", "two", "three"} {
		if _, err := s.CreateTask(Task{Name: TaskName(queueA, id), HTTPRequest: &HTTPRequest{URL: srv.URL}}); err != nil {
			t.Fatal(err)
		}
	}
	clock := sched.NewFakeClock(time.Now())
	w := NewWorker(s, NewDispatcher(s, srv.Client(), clock), clock, time.Second)
	for i := 0; i < 3; i++ {
		w.dispatchDue(context.Background())
	}
	if p := atomic.LoadInt64(&peak); p != 1 {
		t.Errorf("peak concurrency %d; want 1", p)
	}
	if left, _ := s.ListTasks(queueA); len(left) != 0 {
		t.Errorf("%d tasks left after three passes", len(left))
	}
}

// Cloud Tasks' own ID rules, and NOT_FOUND for the tasks of a missing queue.
func TestCloudTasksIDRulesAndMissingQueue(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	parent := strings.TrimSuffix(queueA, "/queues/"+queueA[strings.LastIndex(queueA, "/")+1:])
	for _, id := range []string{"with_underscore", strings.Repeat("q", 101), "dot.ted"} {
		if _, err := s.CreateQueue(Queue{Name: parent + "/queues/" + id}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("queue ID %q = %v; want InvalidArgument", id, err)
		}
	}
	if _, err := s.CreateQueue(Queue{Name: parent + "/queues/" + strings.Repeat("q", 100)}); err != nil {
		t.Errorf("a 100-character queue ID: %v", err)
	}
	mustQueue(t, s, queueA)
	if _, err := s.CreateTask(Task{Name: TaskName(queueA, "dot.ted"), HTTPRequest: &HTTPRequest{URL: "http://x/"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("task ID with a dot = %v; want InvalidArgument", err)
	}
	if _, err := s.CreateTask(Task{Name: TaskName(queueA, "under_score-1"), HTTPRequest: &HTTPRequest{URL: "http://x/"}}); err != nil {
		t.Errorf("task ID with an underscore: %v", err)
	}
	if _, err := s.ListTasks(parent + "/queues/no-such-queue"); status.Code(err) != codes.NotFound {
		t.Errorf("ListTasks on a missing queue = %v; want NotFound", err)
	}
}

// A task name executed or deleted cannot be reused for TombstoneTTL, then
// can; deleting the queue, as reset does, clears the tombstones (#579).
func TestTaskNamesAreTombstoned(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	mustQueue(t, s, queueA)
	name := TaskName(queueA, "once")
	task := Task{Name: name, HTTPRequest: &HTTPRequest{URL: "http://x/"}}
	if _, err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(task); status.Code(err) != codes.AlreadyExists {
		t.Errorf("reusing a deleted task's name = %v; want AlreadyExists", err)
	}
	now = now.Add(TombstoneTTL + time.Second)
	if _, err := s.CreateTask(task); err != nil {
		t.Errorf("after the tombstone window: %v", err)
	}
	if err := s.DeleteTask(name); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteQueue(queueA); err != nil {
		t.Fatal(err)
	}
	mustQueue(t, s, queueA)
	if _, err := s.CreateTask(task); err != nil {
		t.Errorf("after the queue was deleted and recreated: %v", err)
	}
}
