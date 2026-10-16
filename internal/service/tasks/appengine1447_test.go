package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// The host is built as AppEngineRouting.host documents it, from the app's
// domain and the routing's parts.
func TestAppEngineHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		r    AppEngineRouting
		want string
	}{
		{AppEngineRouting{}, "p.appspot.com"},
		{AppEngineRouting{Service: "worker"}, "worker.p.appspot.com"},
		{AppEngineRouting{Service: "worker", Version: "v2"}, "v2.worker.p.appspot.com"},
		{AppEngineRouting{Service: "worker", Version: "v2", Instance: "0"}, "0.v2.worker.p.appspot.com"},
		{AppEngineRouting{Version: "v2"}, "v2.p.appspot.com"},
	} {
		if got := AppEngineHost("p", tc.r); got != tc.want {
			t.Errorf("AppEngineHost(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// An App Engine task is stored as sent, with its routing's host, and a
// queue's app_engine_routing_override is stored and returned (#1447).
func TestGRPCAppEngineTaskIsStored(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	task, err := c.CreateTask(cc, &taskspb.CreateTaskRequest{Parent: queueA, Task: &taskspb.Task{
		DispatchDeadline: durationpb.New(time.Hour),
		MessageType: &taskspb.Task_AppEngineHttpRequest{AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{
			RelativeUri: "/work?x=1", Body: []byte("b"), Headers: map[string]string{"X-A": "1"},
			AppEngineRouting: &taskspb.AppEngineRouting{Service: "worker", Version: "v1"},
		}}}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	ae := task.GetAppEngineHttpRequest()
	if ae.GetRelativeUri() != "/work?x=1" || ae.GetHttpMethod() != taskspb.HttpMethod_POST || string(ae.GetBody()) != "b" ||
		ae.GetAppEngineRouting().GetHost() != "v1.worker.my-project.appspot.com" || ae.GetHeaders()["X-A"] != "1" {
		t.Errorf("created task = %v", task)
	}
	if task.GetDispatchDeadline().AsDuration() != time.Hour {
		t.Errorf("dispatch_deadline = %v, want 1h (allowed for App Engine)", task.GetDispatchDeadline())
	}

	q, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue:      &taskspb.Queue{Name: queueA, AppEngineRoutingOverride: &taskspb.AppEngineRouting{Service: "other", Host: "ignored"}},
		UpdateMask: mask("app_engine_routing_override")})
	if err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	if o := q.GetAppEngineRoutingOverride(); o.GetService() != "other" || o.GetHost() != "other.my-project.appspot.com" {
		t.Errorf("override = %v", o)
	}
	got, err := c.GetTask(cc, &taskspb.GetTaskRequest{Name: task.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if r := got.GetAppEngineHttpRequest().GetAppEngineRouting(); r.GetService() != "worker" || r.GetHost() != "other.my-project.appspot.com" {
		t.Errorf("under the override, routing = %v; want the task's own service and the override's host", r)
	}
}

// The contract's validation of appEngineHttpRequest.
func TestGRPCAppEngineTaskValidation(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		task *taskspb.Task
	}{
		{"relative uri without slash", &taskspb.Task{MessageType: &taskspb.Task_AppEngineHttpRequest{
			AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{RelativeUri: "work"}}}},
		{"body on GET", &taskspb.Task{MessageType: &taskspb.Task_AppEngineHttpRequest{
			AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{HttpMethod: taskspb.HttpMethod_GET, Body: []byte("x")}}}},
		{"deadline above 24h15s", &taskspb.Task{DispatchDeadline: durationpb.New(25 * time.Hour),
			MessageType: &taskspb.Task_AppEngineHttpRequest{AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{}}}},
	} {
		if _, err := c.CreateTask(cc, &taskspb.CreateTaskRequest{Parent: queueA, Task: tc.task}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", tc.name, err)
		}
	}
}

// Every attempt of an App Engine task fails as a request to a missing app
// does, with the reason in last_attempt, and is retried by the queue's
// retry_config until it is exhausted; the task is never dropped unseen.
func TestAppEngineAttemptsFail(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	q := mustQueue(t, s, queueA)
	if _, _, err := s.UpdateQueue(q.Name, func(q *Queue) error {
		q.RetryConfig = RetryConfig{MaxAttempts: 2, MinBackoff: time.Second, MaxBackoff: time.Second}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := sched.NewFakeClock(time.Now())
	var seen []int
	d := NewDispatcher(s, nil, clock).Observe(func(_, _ string, _ int, code int, _ error) { seen = append(seen, code) })
	w := NewWorker(s, d, clock, time.Second)
	name := TaskName(queueA, "ae")
	if _, err := s.CreateTask(Task{Name: name, AppEngineHTTPRequest: &AppEngineHTTPRequest{Method: "POST", RelativeURI: "/w",
		Routing: &AppEngineRouting{Service: "worker"}}, ScheduleTime: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	w.dispatchDue(context.Background())
	got, err := s.GetTask(name)
	if err != nil {
		t.Fatalf("after one failed attempt of two: %v", err)
	}
	pt := toProtoTaskIn(got, nil)
	la := pt.GetLastAttempt()
	if pt.GetDispatchCount() != 1 || pt.GetResponseCount() != 1 || pt.GetFirstAttempt().GetDispatchTime() == nil ||
		la.GetResponseStatus().GetCode() != int32(codes.NotFound) || la.GetResponseTime() == nil ||
		!strings.Contains(la.GetResponseStatus().GetMessage(), "worker.my-project.appspot.com/w") {
		t.Errorf("after one attempt, task = %v", pt)
	}
	clock.Advance(2 * time.Second)
	w.dispatchDue(context.Background())
	if _, err := s.GetTask(name); err == nil {
		t.Error("the task outlived max_attempts")
	}
	if len(seen) != 2 || seen[0] != 404 || seen[1] != 404 {
		t.Errorf("observed attempts = %v, want two 404s", seen)
	}
}
