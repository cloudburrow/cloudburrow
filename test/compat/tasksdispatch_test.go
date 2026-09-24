//go:build compat

package compat

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// delivery is one request the dispatcher made to a test target.
type delivery struct {
	at     time.Time
	method string
	path   string
	body   string
	header http.Header
}

// target is a host HTTP server that records every delivery and answers with a
// fixed status. The dispatcher runs in the CloudBurrow process on this machine,
// so a loopback server is reachable from it.
type target struct {
	mu   sync.Mutex
	got  []delivery
	srv  *httptest.Server
	code int
}

func newTarget(t *testing.T, code int) *target {
	tg := &target{code: code}
	tg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		tg.mu.Lock()
		tg.got = append(tg.got, delivery{time.Now(), r.Method, r.URL.Path, string(body), r.Header.Clone()})
		tg.mu.Unlock()
		w.WriteHeader(tg.code)
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

func (tg *target) deliveries() []delivery {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]delivery(nil), tg.got...)
}

// await polls until n deliveries have arrived or the deadline passes.
func (tg *target) await(t *testing.T, n int, within time.Duration) []delivery {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := tg.deliveries(); len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := tg.deliveries()
	t.Fatalf("%d deliveries within %v, want %d", len(got), within, n)
	return nil
}

// TestTasksHTTPDispatchCarriesCloudTasksHeaders.
//
// HTTP dispatch was unit-tested but never proven through the SDK (#276). A task
// created with the official client must arrive with its method, body and
// headers intact, and with the four X-CloudTasks-* headers set as the service
// sets them: short names, and counts that start at zero.
func TestTasksHTTPDispatchCarriesCloudTasksHeaders(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	q := queue(t, h, c, "dispatch-q")
	tg := newTarget(t, http.StatusOK)

	if _, err := c.CreateTask(h.Context(), &taskspb.CreateTaskRequest{
		Parent: q,
		Task: &taskspb.Task{
			Name: q + "/tasks/deliver-me",
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
				Url:        tg.srv.URL + "/work",
				HttpMethod: taskspb.HttpMethod_PUT,
				Headers:    map[string]string{"Content-Type": "application/json", "X-Custom": "kept"},
				Body:       []byte(`{"order": 7}`),
			}},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	d := tg.await(t, 1, 15*time.Second)[0]
	if d.method != http.MethodPut || d.path != "/work" || d.body != `{"order": 7}` {
		t.Errorf("delivered %s %s %q", d.method, d.path, d.body)
	}
	if d.header.Get("Content-Type") != "application/json" || d.header.Get("X-Custom") != "kept" {
		t.Errorf("task headers not delivered: %v", d.header)
	}
	for k, want := range map[string]string{
		"X-CloudTasks-QueueName":          "dispatch-q",
		"X-CloudTasks-TaskName":           "deliver-me",
		"X-CloudTasks-TaskRetryCount":     "0",
		"X-CloudTasks-TaskExecutionCount": "0",
	} {
		if got := d.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestTasksScheduleTimeIsHonoured: a task scheduled 3s ahead is neither
// delivered early nor left undelivered.
func TestTasksScheduleTimeIsHonoured(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	q := queue(t, h, c, "schedule-q")
	tg := newTarget(t, http.StatusOK)

	at := time.Now().Add(3 * time.Second)
	if _, err := c.CreateTask(h.Context(), &taskspb.CreateTaskRequest{
		Parent: q,
		Task: &taskspb.Task{
			ScheduleTime: timestamppb.New(at),
			MessageType:  &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL}},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	d := tg.await(t, 1, 15*time.Second)[0]
	if d.at.Before(at) {
		t.Errorf("delivered %v before its scheduleTime", at.Sub(d.at))
	}
	if late := d.at.Sub(at); late > 3*time.Second {
		t.Errorf("delivered %v after its scheduleTime", late)
	}
}

// TestTasksAFailingTargetIsRetriedPerRetryConfig: a target answering 500 is
// retried after minBackoff, then after the doubled delay capped at
// maxBackoff, and dispatch stops at maxAttempts.
func TestTasksAFailingTargetIsRetriedPerRetryConfig(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	tg := newTarget(t, http.StatusInternalServerError)

	name := fmt.Sprintf("%s/queues/retry-q", location(h))
	if _, err := c.CreateQueue(h.Context(), &taskspb.CreateQueueRequest{
		Parent: location(h),
		Queue: &taskspb.Queue{Name: name, RetryConfig: &taskspb.RetryConfig{
			MaxAttempts: 3,
			MinBackoff:  durationpb.New(time.Second),
			MaxBackoff:  durationpb.New(1500 * time.Millisecond),
		}},
	}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: name}) })

	if _, err := c.CreateTask(h.Context(), &taskspb.CreateTaskRequest{
		Parent: name,
		Task:   &taskspb.Task{MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL}}},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got := tg.await(t, 3, 20*time.Second)
	// minBackoff, then 2s capped at maxBackoff. The worker polls, so a gap
	// may run over by its interval; it may never run under.
	for i, want := range []time.Duration{time.Second, 1500 * time.Millisecond} {
		gap := got[i+1].at.Sub(got[i].at)
		if gap < want-20*time.Millisecond || gap > want+time.Second {
			t.Errorf("retry %d came %v after the previous attempt, want about %v", i+1, gap, want)
		}
	}
	for i, d := range got {
		if rc := d.header.Get("X-CloudTasks-TaskRetryCount"); rc != fmt.Sprint(i) {
			t.Errorf("attempt %d carried X-CloudTasks-TaskRetryCount %q", i+1, rc)
		}
	}

	// Past maxAttempts nothing more arrives, however long we wait for it.
	time.Sleep(4 * time.Second)
	if n := len(tg.deliveries()); n != 3 {
		t.Errorf("%d deliveries, want dispatch to stop at maxAttempts 3", n)
	}
}
