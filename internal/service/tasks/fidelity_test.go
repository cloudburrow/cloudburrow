package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

func wantCodeNaming(t *testing.T, err error, code codes.Code, field string) {
	t.Helper()
	if status.Code(err) != code || !strings.Contains(err.Error(), field) {
		t.Errorf("err = %v; want %s naming %s", err, code, field)
	}
}

// Fields Cloud Tasks honours and this server would drop are refused by name
// (#578); a dispatch_deadline is validated, stored and returned.
func TestTaskFieldsAreRefusedOrKept(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	task := func(hr *taskspb.HttpRequest, dd *durationpb.Duration) *taskspb.CreateTaskRequest {
		return &taskspb.CreateTaskRequest{Parent: queueA, Task: &taskspb.Task{DispatchDeadline: dd,
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: hr}}}
	}
	_, err := c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/", AuthorizationHeader: &taskspb.HttpRequest_OidcToken{
		OidcToken: &taskspb.OidcToken{ServiceAccountEmail: "sa@p.iam.gserviceaccount.com"}}}, nil))
	wantCodeNaming(t, err, codes.Unimplemented, "oidcToken")
	_, err = c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/", AuthorizationHeader: &taskspb.HttpRequest_OauthToken{
		OauthToken: &taskspb.OAuthToken{ServiceAccountEmail: "sa@p.iam.gserviceaccount.com"}}}, nil))
	wantCodeNaming(t, err, codes.Unimplemented, "oauthToken")
	for _, m := range []taskspb.HttpMethod{taskspb.HttpMethod_GET, taskspb.HttpMethod_HEAD, taskspb.HttpMethod_DELETE, taskspb.HttpMethod_OPTIONS} {
		_, err = c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/", HttpMethod: m, Body: []byte("x")}, nil))
		wantCodeNaming(t, err, codes.InvalidArgument, "body")
	}
	for _, d := range []time.Duration{14 * time.Second, 31 * time.Minute} {
		_, err = c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/"}, durationpb.New(d)))
		wantCodeNaming(t, err, codes.InvalidArgument, "dispatch_deadline")
	}
	created, err := c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/", HttpMethod: taskspb.HttpMethod_POST, Body: []byte("x")}, durationpb.New(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTask(cc, &taskspb.GetTaskRequest{Name: created.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDispatchDeadline().AsDuration() != 2*time.Minute {
		t.Errorf("GetTask dispatch_deadline = %v; want 2m", got.GetDispatchDeadline())
	}
}

// max_retry_duration round-trips; stackdriver_logging_config and a list
// filter are refused rather than ignored (#578).
func TestQueueFieldsAreRefusedOrKept(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	_, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA,
		StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1}}})
	wantCodeNaming(t, err, codes.Unimplemented, "stackdriver_logging_config")
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA,
		RetryConfig: &taskspb.RetryConfig{MaxRetryDuration: durationpb.New(90 * time.Second)}}}); err != nil {
		t.Fatal(err)
	}
	q, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA})
	if err != nil {
		t.Fatal(err)
	}
	if q.GetRetryConfig().GetMaxRetryDuration().AsDuration() != 90*time.Second {
		t.Errorf("max_retry_duration = %v; want 90s", q.GetRetryConfig().GetMaxRetryDuration())
	}
	_, err = c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: "state: PAUSED"})
	wantCodeNaming(t, err, codes.Unimplemented, "filter")
}

// With max_retry_duration set, a task is retried past max_attempts until
// the duration since its first attempt has also passed, then dropped (#578).
func TestMaxRetryDurationExtendsRetryingUntilBothLimitsAreReached(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, err := s.CreateQueue(Queue{Name: queueA, RetryConfig: RetryConfig{
		MaxAttempts: 2, MinBackoff: time.Second, MaxBackoff: time.Second, MaxRetryDuration: 10 * time.Second}}); err != nil {
		t.Fatal(err)
	}
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	name := TaskName(queueA, "long")
	if _, err := s.CreateTask(Task{Name: name, HTTPRequest: &HTTPRequest{URL: srv.URL}}); err != nil {
		t.Fatal(err)
	}
	clock := sched.NewFakeClock(time.Now().UTC())
	w := NewWorker(s, NewDispatcher(s, srv.Client(), clock), clock, time.Second)
	// Attempts at t=0, 2, 4, 6, 8, 10 and 12 s: the attempt limit (2) is
	// passed at the second, the duration (10 s) only after t=10.
	for i := 0; i < 8; i++ {
		w.dispatchDue(context.Background())
		clock.Advance(2 * time.Second)
	}
	if got := atomic.LoadInt64(&hits); got < 5 {
		t.Errorf("hits = %d; max_retry_duration should keep the task past max_attempts", got)
	}
	if _, err := s.GetTask(name); status.Code(err) != codes.NotFound {
		t.Errorf("the task survived both limits: %v", err)
	}
	if b := Backoff(RetryConfig{MaxAttempts: 2}); b.ShouldRetryAfter(2, time.Hour) {
		t.Error("with no max_retry_duration, the attempt limit alone decides")
	}
}
