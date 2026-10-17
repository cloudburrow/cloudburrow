package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// With --appengine-url, an App Engine task is delivered to the stand-in
// server with its routing's Host (under the queue's override) and the
// X-AppEngine-* headers Google documents; a 500 is retried with
// X-AppEngine-TaskPreviousResponse, and a 200 deletes the task (#1506).
func TestAppEngineTaskDelivered(t *testing.T) {
	t.Parallel()
	got := make(chan *http.Request, 4)
	codes := make(chan int, 4)
	codes <- http.StatusInternalServerError
	codes <- http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		w.WriteHeader(<-codes)
	}))
	t.Cleanup(srv.Close)
	s := newStore(t)
	q := mustQueue(t, s, queueA)
	if _, _, err := s.UpdateQueue(q.Name, func(q *Queue) error {
		q.RetryConfig = RetryConfig{MaxAttempts: 3, MinBackoff: time.Second, MaxBackoff: time.Second}
		q.AppEngineRouting = &AppEngineRouting{Service: "other"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := sched.NewFakeClock(time.Unix(1700000000, 500000000))
	w := NewWorker(s, NewDispatcher(s, nil, clock).WithAppEngine(srv.URL), clock, time.Second)
	name := TaskName(queueA, "ae")
	if _, err := s.CreateTask(Task{Name: name, ScheduleTime: clock.Now(), AppEngineHTTPRequest: &AppEngineHTTPRequest{Method: "POST",
		RelativeURI: "/w", Body: []byte("{}"), Headers: map[string]string{"Content-Type": "application/json", "X-Google-Foo": "x"},
		Routing: &AppEngineRouting{Service: "worker"}}}); err != nil {
		t.Fatal(err)
	}
	w.dispatchDue(context.Background())
	r := <-got
	if r.Host != "other.my-project.appspot.com" || r.URL.Path != "/w" {
		t.Errorf("first attempt: Host %s path %s", r.Host, r.URL.Path)
	}
	for k, want := range map[string]string{"X-AppEngine-QueueName": "queue-a", "X-AppEngine-TaskName": "ae", "X-AppEngine-TaskRetryCount": "0",
		"X-AppEngine-TaskExecutionCount": "0", "X-AppEngine-TaskETA": "1700000000.500000", "X-AppEngine-TaskPreviousResponse": "",
		"User-Agent": AppEngineUserAgent, "Content-Type": "application/json", "X-Google-Foo": ""} {
		if v := r.Header.Get(k); v != want {
			t.Errorf("first attempt %s = %q, want %q", k, v, want)
		}
	}
	if _, err := s.GetTask(name); err != nil {
		t.Fatalf("after a 500 the task is gone: %v", err)
	}
	clock.Advance(2 * time.Second)
	w.dispatchDue(context.Background())
	r = <-got
	if r.Header.Get("X-AppEngine-TaskRetryCount") != "1" || r.Header.Get("X-AppEngine-TaskExecutionCount") != "1" ||
		r.Header.Get("X-AppEngine-TaskPreviousResponse") != "500" {
		t.Errorf("retry headers = %v", r.Header)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := s.GetTask(name); err != nil {
			return
		}
	}
	t.Error("the task outlived its 200")
}
