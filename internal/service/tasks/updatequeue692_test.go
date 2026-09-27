package tasks

import (
	"context"
	"io"
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
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

// A mask replaces the fields it names and keeps the rest: a sub-path keeps
// its message's other fields too (#692).
func TestUpdateQueueReplacesWhatTheMaskNames(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA,
		RetryConfig: &taskspb.RetryConfig{MinBackoff: durationpb.New(5 * time.Second)},
		RateLimits:  &taskspb.RateLimits{MaxDispatchesPerSecond: 7}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseQueue(cc, &taskspb.PauseQueueRequest{Name: queueA}); err != nil {
		t.Fatal(err)
	}
	got, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue: &taskspb.Queue{Name: queueA, State: taskspb.Queue_RUNNING,
			RetryConfig: &taskspb.RetryConfig{MaxAttempts: 4, MinBackoff: durationpb.New(time.Minute)},
			RateLimits:  &taskspb.RateLimits{MaxConcurrentDispatches: 3, MaxDispatchesPerSecond: 99}},
		UpdateMask: mask("retry_config.max_attempts", "rate_limits.max_concurrent_dispatches"),
	})
	if err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	rc, rl := got.GetRetryConfig(), got.GetRateLimits()
	if rc.GetMaxAttempts() != 4 || rc.GetMinBackoff().AsDuration() != 5*time.Second {
		t.Errorf("retry_config = %v; want max_attempts 4 and min_backoff kept at 5s", rc)
	}
	if rl.GetMaxConcurrentDispatches() != 3 || rl.GetMaxDispatchesPerSecond() != 7 {
		t.Errorf("rate_limits = %v; want max_concurrent_dispatches 3 and max_dispatches_per_second kept at 7", rl)
	}
	// State is output only: UpdateQueue ignores it; only ResumeQueue resumes.
	if got.GetState() != taskspb.Queue_PAUSED {
		t.Errorf("state = %v; UpdateQueue changed it", got.GetState())
	}
	read, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA})
	if err != nil || read.GetRetryConfig().GetMaxAttempts() != 4 || read.GetRateLimits().GetMaxConcurrentDispatches() != 3 {
		t.Errorf("GetQueue after update = %v, %v", read, err)
	}

	// A whole-message path replaces the message; an unset field defaults.
	got, err = c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue:      &taskspb.Queue{Name: queueA, RetryConfig: &taskspb.RetryConfig{MaxAttempts: 2}},
		UpdateMask: mask("retry_config"),
	})
	if err != nil {
		t.Fatal(err)
	}
	def := DefaultRetryConfig()
	if rc := got.GetRetryConfig(); rc.GetMaxAttempts() != 2 || rc.GetMinBackoff().AsDuration() != def.MinBackoff {
		t.Errorf("retry_config = %v; want max_attempts 2 and min_backoff back to the default", rc)
	}
	if got.GetRateLimits().GetMaxConcurrentDispatches() != 3 {
		t.Errorf("rate_limits changed under a retry_config mask: %v", got.GetRateLimits())
	}

	// An empty mask replaces every settable field: rate limits left out of
	// the request go back to the defaults.
	got, err = c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue: &taskspb.Queue{Name: queueA, RetryConfig: &taskspb.RetryConfig{MaxAttempts: 9}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetRetryConfig().GetMaxAttempts() != 9 || got.GetRateLimits().GetMaxConcurrentDispatches() != int32(DefaultRateLimits().MaxConcurrentDispatches) ||
		got.GetRateLimits().GetMaxDispatchesPerSecond() != DefaultRateLimits().MaxDispatchesPerSecond {
		t.Errorf("empty mask gave %v", got)
	}
}

// "This method creates the queue if it does not exist" (UpdateQueue), with
// CreateQueue's name rules.
func TestUpdateQueueCreatesAMissingQueue(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	got, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue:      &taskspb.Queue{Name: queueA, RetryConfig: &taskspb.RetryConfig{MaxAttempts: 3}},
		UpdateMask: mask("retry_config.max_attempts"),
	})
	if err != nil {
		t.Fatalf("UpdateQueue on a missing queue: %v", err)
	}
	if got.GetState() != taskspb.Queue_RUNNING || got.GetRetryConfig().GetMaxAttempts() != 3 ||
		got.GetRateLimits().GetMaxConcurrentDispatches() != int32(DefaultRateLimits().MaxConcurrentDispatches) {
		t.Errorf("created queue = %v", got)
	}
	if _, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA}); err != nil {
		t.Errorf("GetQueue after the creating update: %v", err)
	}
	for _, name := range []string{"", loc + "/queues/bad_id", loc + "/topics/x", "projects/p/queues/x"} {
		if _, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{Queue: &taskspb.Queue{Name: name}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateQueue(%q) = %v, want InvalidArgument", name, err)
		}
	}
}

// A path that names the name, an output-only field or no field is
// INVALID_ARGUMENT naming it; a field CreateQueue refuses is refused here
// too, and nothing is changed by a refused request.
func TestUpdateQueueRefusesPathsItCannotApply(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		queue *taskspb.Queue
		paths []string
		code  codes.Code
		named string
	}{
		{nil, []string{"state"}, codes.InvalidArgument, `"state"`},
		{nil, []string{"name"}, codes.InvalidArgument, `"name"`},
		{nil, []string{"purge_time"}, codes.InvalidArgument, `"purge_time"`},
		{nil, []string{"rate_limits.max_burst_size"}, codes.InvalidArgument, `"rate_limits.max_burst_size"`},
		{nil, []string{"retry_config.max_attempts", "no_such_field"}, codes.InvalidArgument, `"no_such_field"`},
		{nil, []string{"retry_config.bogus"}, codes.InvalidArgument, `"retry_config.bogus"`},
		{nil, []string{"http_target"}, codes.Unimplemented, "http_target"},
		{&taskspb.Queue{StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1}},
			[]string{"stackdriver_logging_config"}, codes.Unimplemented, "stackdriver_logging_config"},
		{&taskspb.Queue{StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1}},
			nil, codes.Unimplemented, "stackdriver_logging_config"},
		{&taskspb.Queue{AppEngineRoutingOverride: &taskspb.AppEngineRouting{Service: "s"}},
			[]string{"app_engine_routing_override"}, codes.Unimplemented, "app_engine_routing_override"},
		{&taskspb.Queue{RetryConfig: &taskspb.RetryConfig{MaxAttempts: 1, MaxRetryDuration: durationpb.New(-time.Second)}},
			[]string{"retry_config"}, codes.InvalidArgument, "max_retry_duration"},
	} {
		q := tc.queue
		if q == nil {
			q = &taskspb.Queue{RetryConfig: &taskspb.RetryConfig{MaxAttempts: 1}}
		}
		q.Name = queueA
		req := &taskspb.UpdateQueueRequest{Queue: q}
		if tc.paths != nil {
			req.UpdateMask = mask(tc.paths...)
		}
		_, err := c.UpdateQueue(cc, req)
		if status.Code(err) != tc.code || !strings.Contains(status.Convert(err).Message(), tc.named) {
			t.Errorf("UpdateQueue(mask %v) = %v; want %v naming %s", tc.paths, err, tc.code, tc.named)
		}
	}
	got, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA})
	if err != nil || got.GetRetryConfig().GetMaxAttempts() != int32(DefaultRetryConfig().MaxAttempts) {
		t.Errorf("a refused update changed the queue: %v, %v", got, err)
	}
}

// An updated retry_config reaches the dispatcher: a failing task in a queue
// updated from 5 attempts to 1 is dropped after its first (#692).
func TestAnUpdatedRetryConfigChangesRetrying(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	g := NewGRPCServer(s)
	if _, err := g.CreateQueue(context.Background(), &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA,
		RetryConfig: &taskspb.RetryConfig{MaxAttempts: 5, MinBackoff: durationpb.New(time.Second)}}}); err != nil {
		t.Fatal(err)
	}
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	name := TaskName(queueA, "failing")
	if _, err := s.CreateTask(Task{Name: name, HTTPRequest: &HTTPRequest{URL: srv.URL}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.UpdateQueue(context.Background(), &taskspb.UpdateQueueRequest{
		Queue:      &taskspb.Queue{Name: queueA, RetryConfig: &taskspb.RetryConfig{MaxAttempts: 1}},
		UpdateMask: mask("retry_config.max_attempts"),
	}); err != nil {
		t.Fatal(err)
	}
	clock := sched.NewFakeClock(time.Now().UTC())
	w := NewWorker(s, NewDispatcher(s, srv.Client(), clock), clock, time.Second)
	w.dispatchDue(context.Background())
	if _, err := s.GetTask(name); status.Code(err) != codes.NotFound {
		t.Errorf("after one failed attempt under max_attempts 1 the task is still queued (%v); the update did not reach the dispatcher", err)
	}
	clock.Advance(time.Minute)
	w.dispatchDue(context.Background())
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

// PATCH is UpdateQueue by Google's binding, with updateMask a
// lowerCamelCase FieldMask, as Terraform and gcloud send it (#692).
func TestRESTUpdateQueue(t *testing.T) {
	r := rest.NewRouter()
	NewRESTServer(NewStore(store.NewMemory())).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const q = "/v2/" + queueA
	for _, c := range []struct {
		method, path, body string
		code               int
		has                []string
	}{
		{"POST", "/v2/" + loc + "/queues", `{"name":"` + queueA + `","retryConfig":{"minBackoff":"5s"}}`, 200, []string{`"minBackoff":"5s"`}},
		// gcloud tasks queues update --max-attempts=4 --max-concurrent-dispatches=3
		{"PATCH", q + "?updateMask=retryConfig.maxAttempts,rateLimits.maxConcurrentDispatches",
			`{"retryConfig":{"maxAttempts":4},"rateLimits":{"maxConcurrentDispatches":3}}`, 200,
			[]string{`"maxAttempts":4`, `"minBackoff":"5s"`, `"maxConcurrentDispatches":3`, `"maxDispatchesPerSecond":500`}},
		// Terraform's in-place update of a retry_config block.
		{"PATCH", q + "?alt=json&updateMask=retryConfig", `{"name":"` + queueA + `","retryConfig":{"maxAttempts":7,"minBackoff":"0.100s","maxBackoff":"3600s","maxDoublings":16}}`, 200,
			[]string{`"maxAttempts":7`, `"minBackoff":"0.100s"`, `"maxConcurrentDispatches":3`}},
		{"GET", q, "", 200, []string{`"maxAttempts":7`, `"maxConcurrentDispatches":3`}},
		{"PATCH", q + "?updateMask=state", `{"state":"PAUSED"}`, 400, []string{"INVALID_ARGUMENT", `\"state\"`}},
		{"PATCH", q + "?updateMask=rateLimits&bogus=1", `{}`, 400, []string{"INVALID_ARGUMENT", "bogus"}},
		{"PATCH", q + "?updateMask=retryConfig", `{"retryConfig":{"noSuchField":1}}`, 400, []string{"INVALID_ARGUMENT"}},
		{"PATCH", q + "?updateMask=stackdriverLoggingConfig", `{"stackdriverLoggingConfig":{"samplingRatio":1}}`, 501, []string{"UNIMPLEMENTED", "stackdriver_logging_config"}},
		// No mask replaces every settable field: the rate limits the body
		// leaves out go back to the defaults.
		{"PATCH", q, `{"retryConfig":{"maxAttempts":7}}`, 200, []string{`"maxAttempts":7`, `"maxConcurrentDispatches":1000`}},
		{"GET", q, "", 200, []string{`"state":"RUNNING"`, `"maxAttempts":7`}},
		// A missing queue is created.
		{"PATCH", "/v2/" + loc + "/queues/made-by-patch?updateMask=retryConfig.maxAttempts", `{"retryConfig":{"maxAttempts":2}}`, 200, []string{`"maxAttempts":2`}},
		{"GET", "/v2/" + loc + "/queues/made-by-patch", "", 200, []string{`"state":"RUNNING"`}},
	} {
		code, body := do(c.method, c.path, c.body)
		ok := code == c.code
		for _, h := range c.has {
			ok = ok && strings.Contains(body, h)
		}
		if !ok {
			t.Errorf("%s %s: %d %s; want %d containing %q", c.method, c.path, code, body, c.code, c.has)
		}
	}
}
