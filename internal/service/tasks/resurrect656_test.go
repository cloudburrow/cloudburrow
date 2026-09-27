package tasks

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// failingTransport refuses every request, as a target that is not listening
// does, and deletes the task first when asked to: the DeleteTask that lands
// while an attempt is in flight.
type failingTransport struct{ during func() }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if f.during != nil {
		f.during()
	}
	return nil, errors.New("connection refused")
}

// A task deleted while its attempt is in flight stays deleted. The attempt's
// write-backs (the dispatch count, then reschedule's next schedule time) were
// unconditional puts, so the delete was undone and GetTask found the task
// again (#656; TestTasksTaskLifecycle caught it in CI).
func TestADeletedTaskIsNotRecreatedByItsAttempt(t *testing.T) {
	s := newStore(t)
	mustQueue(t, s, queueA)
	name := TaskName(queueA, "deleted-mid-attempt")
	task, err := s.CreateTask(Task{Name: name, HTTPRequest: &HTTPRequest{URL: "http://127.0.0.1:1/never"}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	clock := sched.NewFakeClock(time.Now())
	client := &http.Client{Transport: failingTransport{during: func() {
		if err := s.DeleteTask(name); err != nil {
			t.Errorf("DeleteTask during the attempt: %v", err)
		}
	}}}
	d := NewDispatcher(s, client, clock)
	w := NewWorker(s, d, clock, time.Second)

	if err := d.Dispatch(context.Background(), task); err == nil {
		t.Fatal("an attempt against a refused connection reported success")
	}
	w.reschedule(task)

	if _, err := s.GetTask(name); status.Code(err) != codes.NotFound {
		t.Errorf("GetTask after a delete during the attempt = %v, want NotFound", err)
	}
	if err := s.UpdateTask(task); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateTask of a deleted task = %v, want NotFound", err)
	}
}
