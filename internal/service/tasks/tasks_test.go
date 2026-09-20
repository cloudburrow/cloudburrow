package tasks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/identity-wael/cloudburrow/internal/apierror"
	"github.com/identity-wael/cloudburrow/internal/sched"
	"github.com/identity-wael/cloudburrow/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	loc    = "projects/my-project/locations/us-central1"
	queueA = loc + "/queues/queue-a"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(store.NewMemory())
}

func mustQueue(t *testing.T, s *Store, name string) Queue {
	t.Helper()
	q, err := s.CreateQueue(Queue{Name: name})
	if err != nil {
		t.Fatalf("CreateQueue(%s): %v", name, err)
	}
	return q
}

func TestQueueLifecycle(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	q := mustQueue(t, s, queueA)
	if q.State != StateRunning {
		t.Errorf("new queue state = %q, want RUNNING", q.State)
	}
	// Defaults must come from the contract, not be left zero.
	if q.RetryConfig.MaxAttempts == 0 || q.RateLimits.MaxConcurrentDispatches == 0 {
		t.Errorf("defaults not applied: %+v", q)
	}

	got, err := s.GetQueue(queueA)
	if err != nil || got.Name != queueA {
		t.Fatalf("GetQueue = %+v, %v", got, err)
	}

	if _, err := s.CreateQueue(Queue{Name: queueA}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate queue = %v, want AlreadyExists", status.Code(err))
	}

	if err := s.DeleteQueue(queueA); err != nil {
		t.Fatalf("DeleteQueue: %v", err)
	}
	if _, err := s.GetQueue(queueA); status.Code(err) != codes.NotFound {
		t.Errorf("GetQueue after delete = %v, want NotFound", status.Code(err))
	}
}

func TestMalformedNamesAreRejected(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	bad := []string{
		"",
		"queues/q",
		loc + "/queues/..",
		"projects/p/queues/q",
		loc + "/topics/t", // right shape, wrong collection
	}
	for _, name := range bad {
		if _, err := s.CreateQueue(Queue{Name: name}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateQueue(%q) = %v, want InvalidArgument", name, status.Code(err))
		}
	}
}

// Identical queue IDs in different projects must not collide.
func TestProjectIsolation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a := "projects/project-aaa/locations/us-central1/queues/shared"
	b := "projects/project-bbb/locations/us-central1/queues/shared"
	mustQueue(t, s, a)
	mustQueue(t, s, b)

	if _, err := s.GetQueue(a); err != nil {
		t.Errorf("queue a: %v", err)
	}
	if _, err := s.GetQueue(b); err != nil {
		t.Errorf("queue b: %v", err)
	}
	if err := s.DeleteQueue(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetQueue(b); err != nil {
		t.Error("deleting one project's queue removed another project's")
	}
}

func TestTaskLifecycle(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)

	name := TaskName(queueA, "task-1")
	task, err := s.CreateTask(Task{
		Name:        name,
		HTTPRequest: &HTTPRequest{URL: "http://127.0.0.1:1/work"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.HTTPRequest.Method != "POST" {
		t.Errorf("default method = %q, want POST", task.HTTPRequest.Method)
	}
	if task.Queue != queueA {
		t.Errorf("queue = %q, want %q", task.Queue, queueA)
	}

	if _, err := s.GetTask(name); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	list, err := s.ListTasks(queueA)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTasks = %d, %v", len(list), err)
	}
	if err := s.DeleteTask(name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(name); status.Code(err) != codes.NotFound {
		t.Errorf("GetTask after delete = %v, want NotFound", status.Code(err))
	}
}

func TestTaskRequiresQueueAndTarget(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	// A task in a queue that does not exist.
	_, err := s.CreateTask(Task{Name: TaskName(queueA, "orphan"), HTTPRequest: &HTTPRequest{URL: "http://x/"}})
	if status.Code(err) != codes.NotFound {
		t.Errorf("task in missing queue = %v, want NotFound", status.Code(err))
	}

	mustQueue(t, s, queueA)
	// A task with no HTTP target has nothing to dispatch to.
	if _, err := s.CreateTask(Task{Name: TaskName(queueA, "no-target")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("task without httpRequest = %v, want InvalidArgument", status.Code(err))
	}
}

// Deleting a queue must remove its tasks, or a later queue of the same name
// would inherit work it never accepted.
func TestDeleteQueueRemovesItsTasks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateTask(Task{
			Name:        TaskName(queueA, fmt.Sprintf("t%d", i)),
			HTTPRequest: &HTTPRequest{URL: "http://127.0.0.1:1/"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteQueue(queueA); err != nil {
		t.Fatal(err)
	}
	mustQueue(t, s, queueA)
	list, err := s.ListTasks(queueA)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("recreated queue inherited %d tasks", len(list))
	}
}

func TestPurgeKeepsTheQueue(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	if _, err := s.CreateTask(Task{Name: TaskName(queueA, "t1"), HTTPRequest: &HTTPRequest{URL: "http://x/"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeQueue(queueA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetQueue(queueA); err != nil {
		t.Error("PurgeQueue deleted the queue")
	}
	list, _ := s.ListTasks(queueA)
	if len(list) != 0 {
		t.Errorf("purge left %d tasks", len(list))
	}
}

// A paused queue must genuinely stop dispatching.
func TestPausedQueueYieldsNoDueTasks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	if _, err := s.CreateTask(Task{Name: TaskName(queueA, "t1"), HTTPRequest: &HTTPRequest{URL: "http://x/"}}); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueTasks(time.Now().UTC())
	if err != nil || len(due) != 1 {
		t.Fatalf("running queue due = %d, %v", len(due), err)
	}

	if _, err := s.SetQueueState(queueA, StatePaused); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueTasks(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("paused queue still yielded %d due tasks", len(due))
	}
}

// A scheduled task must not be due before its time.
func TestScheduleTimeIsRespected(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	future := time.Now().UTC().Add(time.Hour)
	if _, err := s.CreateTask(Task{
		Name:         TaskName(queueA, "later"),
		ScheduleTime: future,
		HTTPRequest:  &HTTPRequest{URL: "http://x/"},
	}); err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueTasks(time.Now().UTC())
	if len(due) != 0 {
		t.Errorf("task due before its scheduleTime")
	}
	due, _ = s.DueTasks(future.Add(time.Second))
	if len(due) != 1 {
		t.Errorf("task not due after its scheduleTime")
	}
}

// Dispatch tests.

func TestDispatchSuccessRemovesTask(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)

	var got struct {
		sync.Mutex
		headers http.Header
		body    string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got.Lock()
		got.headers = r.Header.Clone()
		got.body = string(buf)
		got.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	name := TaskName(queueA, "t1")
	task, err := s.CreateTask(Task{
		Name:        name,
		HTTPRequest: &HTTPRequest{URL: srv.URL, Body: []byte("payload")},
	})
	if err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(s, srv.Client(), nil)
	if err := d.Dispatch(context.Background(), task); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := s.GetTask(name); status.Code(err) != codes.NotFound {
		t.Error("a successful task was not removed")
	}

	got.Lock()
	defer got.Unlock()
	if got.body != "payload" {
		t.Errorf("body = %q, want payload", got.body)
	}
	// Handlers read these to distinguish a retry from a first delivery.
	for _, h := range []string{"X-Cloudtasks-Taskname", "X-Cloudtasks-Queuename", "X-Cloudtasks-Taskretrycount"} {
		if got.headers.Get(h) == "" {
			t.Errorf("missing %s header", h)
		}
	}
}

// Cloud Tasks retries non-2xx, including 4xx. Matching that is the point.
func TestDispatchNon2xxIsRetried(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 404, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			mustQueue(t, s, queueA)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			name := TaskName(queueA, "t")
			task, err := s.CreateTask(Task{Name: name, HTTPRequest: &HTTPRequest{URL: srv.URL}})
			if err != nil {
				t.Fatal(err)
			}
			d := NewDispatcher(s, srv.Client(), nil)
			if err := d.Dispatch(context.Background(), task); err == nil {
				t.Fatalf("HTTP %d was treated as success", code)
			}
			after, err := s.GetTask(name)
			if err != nil {
				t.Fatalf("task was removed despite a failure: %v", err)
			}
			if after.DispatchCount != 1 || after.LastResponseCode != code {
				t.Errorf("attempt not recorded: %+v", after)
			}
		})
	}
}

// An unreachable target records the attempt and asks for a retry.
func TestDispatchConnectionFailureIsRetried(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	name := TaskName(queueA, "t")
	task, err := s.CreateTask(Task{
		Name:        name,
		HTTPRequest: &HTTPRequest{URL: "http://127.0.0.1:1/unreachable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(s, &http.Client{Timeout: time.Second}, nil)
	if err := d.Dispatch(context.Background(), task); err == nil {
		t.Fatal("an unreachable target was treated as success")
	}
	after, err := s.GetTask(name)
	if err != nil {
		t.Fatalf("task removed after a connection failure: %v", err)
	}
	if after.DispatchCount != 1 {
		t.Errorf("DispatchCount = %d, want 1", after.DispatchCount)
	}
	if after.ResponseCount != 0 {
		t.Errorf("ResponseCount = %d, want 0 with no response", after.ResponseCount)
	}
}

// Retry timing comes from the queue's own configuration, verified by advancing
// virtual time rather than sleeping.
func TestWorkerRetriesWithQueueBackoff(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, err := s.CreateQueue(Queue{
		Name: queueA,
		RetryConfig: RetryConfig{
			MaxAttempts: 3,
			MinBackoff:  time.Second,
			MaxBackoff:  time.Minute,
		},
	}); err != nil {
		t.Fatal(err)
	}

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	name := TaskName(queueA, "flaky")
	if _, err := s.CreateTask(Task{Name: name, HTTPRequest: &HTTPRequest{URL: srv.URL}}); err != nil {
		t.Fatal(err)
	}

	clock := sched.NewFakeClock(time.Now().UTC())
	d := NewDispatcher(s, srv.Client(), clock)
	w := NewWorker(s, d, clock, time.Second)

	// First pass.
	w.dispatchDue(context.Background())
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("hits = %d, want 1", atomic.LoadInt64(&hits))
	}
	after, _ := s.GetTask(name)
	if !after.ScheduleTime.After(clock.Now()) {
		t.Error("failed task was not deferred; it would spin")
	}

	// Not due yet: another pass must do nothing.
	w.dispatchDue(context.Background())
	if atomic.LoadInt64(&hits) != 1 {
		t.Errorf("hits = %d; a deferred task was dispatched early", atomic.LoadInt64(&hits))
	}

	// Advance past the backoff and run the remaining attempts.
	clock.Advance(2 * time.Second)
	w.dispatchDue(context.Background())
	clock.Advance(10 * time.Second)
	w.dispatchDue(context.Background())

	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("hits = %d, want 3 (MaxAttempts)", got)
	}
	// Exhausted tasks are dropped; keeping them would leave a queue that never
	// drains.
	if _, err := s.GetTask(name); status.Code(err) != codes.NotFound {
		t.Error("task survived after exhausting its attempts")
	}
}

func TestBackoffFromRetryConfig(t *testing.T) {
	t.Parallel()
	b := Backoff(RetryConfig{MaxAttempts: 5, MinBackoff: time.Second, MaxBackoff: 4 * time.Second})
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 4 * time.Second} {
		if got := b.Delay(attempt); got != want {
			t.Errorf("Delay(%d) = %v, want %v", attempt, got, want)
		}
	}
	if b.ShouldRetry(5) {
		t.Error("ShouldRetry allowed an attempt past MaxAttempts")
	}
}

func TestErrorsAreGoogleShaped(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.GetQueue(queueA)
	var ae *apierror.Error
	if !errors.As(err, &ae) {
		t.Fatalf("GetQueue error = %T, want *apierror.Error", err)
	}
	if ae.HTTPStatus() != http.StatusNotFound {
		t.Errorf("HTTPStatus = %d, want 404", ae.HTTPStatus())
	}
}
