package tasks

import (
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The limits the contract documents are refused, by CreateQueue and by an
// UpdateQueue whose mask names the field, with INVALID_ARGUMENT naming it;
// a field the mask does not name is not checked, since it is not applied
// (#784).
func TestQueueLimitsTheContractDocumentsAreRefused(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		queue *taskspb.Queue
		path  string
		want  string
	}{
		{"max_attempts below -1", &taskspb.Queue{RetryConfig: &taskspb.RetryConfig{MaxAttempts: -2}},
			"retry_config.max_attempts", "retry_config.max_attempts -2 must be -1 (unlimited) or greater"},
		{"rate above 500", &taskspb.Queue{RateLimits: &taskspb.RateLimits{MaxDispatchesPerSecond: 500.5}},
			"rate_limits.max_dispatches_per_second", "above the maximum of 500"},
		{"concurrency above 5000", &taskspb.Queue{RateLimits: &taskspb.RateLimits{MaxConcurrentDispatches: 5001}},
			"rate_limits", "rate_limits.max_concurrent_dispatches 5001 is above the maximum of 5000"},
		{"negative min_backoff", &taskspb.Queue{RetryConfig: &taskspb.RetryConfig{MinBackoff: durationpb.New(-time.Second)}},
			"retry_config", "retry_config.min_backoff must not be negative"},
		{"negative max_doublings", &taskspb.Queue{RetryConfig: &taskspb.RetryConfig{MaxDoublings: -1}},
			"retry_config.max_doublings", "retry_config.max_doublings -1 must not be negative"},
	} {
		tc.queue.Name = queueA
		_, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{Queue: tc.queue, UpdateMask: mask(tc.path)})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), tc.want) {
			t.Errorf("UpdateQueue %s = %v; want INVALID_ARGUMENT containing %q", tc.name, err, tc.want)
		}
	}

	// Not named by the mask, so not applied and not checked.
	if _, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue:      &taskspb.Queue{Name: queueA, RetryConfig: &taskspb.RetryConfig{MaxAttempts: -5, MinBackoff: durationpb.New(2 * time.Second)}},
		UpdateMask: mask("retry_config.min_backoff"),
	}); err != nil {
		t.Errorf("UpdateQueue naming only min_backoff checked max_attempts too: %v", err)
	}

	// -1, 500 and 5000 are the limits themselves, and accepted.
	got, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{
		Queue: &taskspb.Queue{Name: queueA, RetryConfig: &taskspb.RetryConfig{MaxAttempts: -1},
			RateLimits: &taskspb.RateLimits{MaxDispatchesPerSecond: 500, MaxConcurrentDispatches: 5000}},
		UpdateMask: mask("retry_config.max_attempts", "rate_limits"),
	})
	if err != nil {
		t.Fatalf("UpdateQueue at the limits: %v", err)
	}
	if got.GetRetryConfig().GetMaxAttempts() != -1 || got.GetRateLimits().GetMaxConcurrentDispatches() != 5000 {
		t.Errorf("UpdateQueue at the limits read back %v", got)
	}

	_, err = c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: loc + "/queues/too-many",
		RetryConfig: &taskspb.RetryConfig{MaxAttempts: -3}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateQueue with max_attempts -3 = %v; want INVALID_ARGUMENT", err)
	}
}
