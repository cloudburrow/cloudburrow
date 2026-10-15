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
	// A token needs its service account (#1243).
	_, err := c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/", AuthorizationHeader: &taskspb.HttpRequest_OidcToken{
		OidcToken: &taskspb.OidcToken{Audience: "a"}}}, nil))
	wantCodeNaming(t, err, codes.InvalidArgument, "oidcToken.serviceAccountEmail")
	_, err = c.CreateTask(cc, task(&taskspb.HttpRequest{Url: "http://x/", AuthorizationHeader: &taskspb.HttpRequest_OauthToken{
		OauthToken: &taskspb.OAuthToken{Scope: "s"}}}, nil))
	wantCodeNaming(t, err, codes.InvalidArgument, "oauthToken.serviceAccountEmail")
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

// max_retry_duration round-trips; a sampling_ratio out of range (#1256) and
// a list filter are refused rather than ignored (#578).
func TestQueueFieldsAreRefusedOrKept(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	_, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA,
		StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1.5}}})
	wantCodeNaming(t, err, codes.InvalidArgument, "stackdriver_logging_config")
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
	for _, f := range []string{"state < PAUSED", "state: SLEEPING", "labels.a = b", "rate_limits = 1",
		"rate_limits.max_burst_size = many", "retry_config.min_backoff > soon", "(state: PAUSED", "state:", "name"} {
		_, err = c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: f})
		wantCodeNaming(t, err, codes.InvalidArgument, "filter")
	}
	_, err = c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: `name =~ "q.*"`})
	wantCodeNaming(t, err, codes.Unimplemented, "regular expressions")
}

// Any Queue field can be filtered on, combined with AND, OR and NOT (#1255).
func TestListQueuesFiltersOnAnyQueueField(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	fast, slow := loc+"/queues/fast", loc+"/queues/slow-lane"
	for _, q := range []*taskspb.Queue{
		{Name: fast, RateLimits: &taskspb.RateLimits{MaxDispatchesPerSecond: 50, MaxConcurrentDispatches: 20},
			RetryConfig: &taskspb.RetryConfig{MaxAttempts: 3, MinBackoff: durationpb.New(time.Second)}},
		{Name: slow, RateLimits: &taskspb.RateLimits{MaxDispatchesPerSecond: 1, MaxConcurrentDispatches: 2},
			RetryConfig: &taskspb.RetryConfig{MaxAttempts: 10, MinBackoff: durationpb.New(30 * time.Second)}},
	} {
		if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: q}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.PauseQueue(cc, &taskspb.PauseQueueRequest{Name: slow}); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{
		"name: slow":            slow,
		`name = "` + fast + `"`: fast,
		"rate_limits.max_dispatches_per_second > 10":       fast,
		"rateLimits.maxConcurrentDispatches <= 2":          slow,
		"retry_config.max_attempts >= 5 AND state: PAUSED": slow,
		"retry_config.min_backoff < 10s":                   fast,
		"retry_config.min_backoff = 30":                    slow,
		"NOT name: fast":                                   slow,
		"-state: PAUSED":                                   fast,
		"(name: nothing OR name: fast) state = RUNNING":    fast,
		"name: SLOW-LANE":                                  slow,
		"retry_config.max_attempts: 3":                     fast,
		"name: queues state: PAUSED":                       slow,
		"rate_limits.max_burst_size:* name: fast":          fast,
	} {
		resp, err := c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: f})
		if err != nil {
			t.Fatalf("filter %q: %v", f, err)
		}
		if len(resp.GetQueues()) != 1 || resp.GetQueues()[0].GetName() != want {
			t.Errorf("filter %q listed %v; want %s alone", f, names(resp.GetQueues()), want)
		}
	}
	resp, err := c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: "name: q OR name: fast", PageSize: 1})
	if err != nil || len(resp.GetQueues()) != 1 || resp.GetNextPageToken() == "" {
		t.Errorf("paged filter = %v, %q, %v; want one queue and a next page", names(resp.GetQueues()), resp.GetNextPageToken(), err)
	}
}

func names(qs []*taskspb.Queue) []string {
	var out []string
	for _, q := range qs {
		out = append(out, q.GetName())
	}
	return out
}

// A state filter is honoured, as Google's sample "state: PAUSED" (#1222).
func TestListQueuesFiltersByState(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	for _, n := range []string{queueA, loc + "/queues/b"} {
		if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: n}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.PauseQueue(cc, &taskspb.PauseQueueRequest{Name: queueA}); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{
		"state: PAUSED": queueA, "state=PAUSED": queueA, `state = "PAUSED"`: queueA,
		"state != PAUSED": loc + "/queues/b", "state: RUNNING": loc + "/queues/b",
	} {
		resp, err := c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: f})
		if err != nil {
			t.Fatalf("filter %q: %v", f, err)
		}
		if len(resp.GetQueues()) != 1 || resp.GetQueues()[0].GetName() != want {
			t.Errorf("filter %q listed %v; want %s alone", f, resp.GetQueues(), want)
		}
	}
	resp, err := c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, Filter: "state: DISABLED"})
	if err != nil || len(resp.GetQueues()) != 0 {
		t.Errorf("state: DISABLED = %v, %v; want none", resp.GetQueues(), err)
	}
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
