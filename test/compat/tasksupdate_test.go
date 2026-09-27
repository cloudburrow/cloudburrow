//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.tasks.v2.CloudTasks/UpdateQueue
//
// TestTasksUpdateQueue (#692): through the official client, UpdateQueue with
// a mask changes the rate limits and retry config it names and keeps the
// rest, GetQueue reads them back, a path naming an output-only field is
// INVALID_ARGUMENT naming it, a missing queue is created, as the API
// documents, and the updated retry config is the one a failing task is
// retried under.
func TestTasksUpdateQueue(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	tg := newTarget(t, http.StatusInternalServerError)

	name := fmt.Sprintf("%s/queues/update-q", location(h))
	if _, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h), Queue: &taskspb.Queue{Name: name,
		RetryConfig: &taskspb.RetryConfig{MaxAttempts: 5, MinBackoff: durationpb.New(time.Second), MaxBackoff: durationpb.New(time.Second)}}}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: name}) })

	if _, err := c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{
		Queue: &taskspb.Queue{Name: name,
			RetryConfig: &taskspb.RetryConfig{MaxAttempts: 2, MinBackoff: durationpb.New(time.Hour)},
			RateLimits:  &taskspb.RateLimits{MaxConcurrentDispatches: 2, MaxDispatchesPerSecond: 10}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retry_config.max_attempts", "rate_limits"}},
	}); err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if rc := got.GetRetryConfig(); rc.GetMaxAttempts() != 2 || rc.GetMinBackoff().AsDuration() != time.Second {
		t.Errorf("retry_config reads back as %v; want max_attempts 2, min_backoff kept at 1s", rc)
	}
	if rl := got.GetRateLimits(); rl.GetMaxConcurrentDispatches() != 2 || rl.GetMaxDispatchesPerSecond() != 10 {
		t.Errorf("rate_limits reads back as %v; want 2 concurrent, 10 per second", rl)
	}

	_, err = c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{Queue: &taskspb.Queue{Name: name, State: taskspb.Queue_PAUSED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), `"state"`) {
		t.Errorf("UpdateQueue with mask state = %v; want INVALID_ARGUMENT naming it", err)
	}

	// The dispatcher reads the updated config: two attempts, not five.
	if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: name,
		Task: &taskspb.Task{MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL}}}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	tg.await(t, 2, 20*time.Second)
	time.Sleep(3 * time.Second)
	if n := len(tg.deliveries()); n != 2 {
		t.Errorf("%d deliveries; want dispatch to stop at the updated max_attempts 2", n)
	}

	created := fmt.Sprintf("%s/queues/update-creates-q", location(h))
	t.Cleanup(func() { _ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: created}) })
	q, err := c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{
		Queue:      &taskspb.Queue{Name: created, RetryConfig: &taskspb.RetryConfig{MaxAttempts: 3}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retry_config.max_attempts"}}})
	if err != nil || q.GetState() != taskspb.Queue_RUNNING || q.GetRetryConfig().GetMaxAttempts() != 3 {
		t.Errorf("UpdateQueue on a missing queue = %v, %v; want it created, RUNNING, max_attempts 3", q, err)
	}
}
