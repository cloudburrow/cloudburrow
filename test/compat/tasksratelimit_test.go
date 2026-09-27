//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
)

// TestTasksMaxDispatchesPerSecondSpacesDeliveries (#716): a queue limited to
// one dispatch a second, created with the official client, spreads N tasks
// enqueued together over at least N-1 seconds, and GetQueue reports the
// burst of 1 the dispatcher enforces as max_burst_size. Before, the rate was
// stored and returned but every due task was dispatched at once. N is small
// to keep the suite fast; the pacing itself is unit-tested on a fake clock.
func TestTasksMaxDispatchesPerSecondSpacesDeliveries(t *testing.T) {
	const n = 3
	h := New(t)
	c := tasksClient(t, h)
	tg := newTarget(t, http.StatusOK)

	name := fmt.Sprintf("%s/queues/rate-q", location(h))
	if _, err := c.CreateQueue(h.Context(), &taskspb.CreateQueueRequest{
		Parent: location(h),
		// max_concurrent_dispatches is set too: without it, CreateQueue
		// before #692 replaced the whole of rate_limits with the defaults.
		Queue: &taskspb.Queue{Name: name, RateLimits: &taskspb.RateLimits{MaxDispatchesPerSecond: 1, MaxConcurrentDispatches: 10}},
	}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: name}) })

	got, err := c.GetQueue(h.Context(), &taskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if rl := got.GetRateLimits(); rl.GetMaxDispatchesPerSecond() != 1 || rl.GetMaxBurstSize() != 1 {
		t.Errorf("rate_limits = %v; want max_dispatches_per_second 1 and max_burst_size 1", rl)
	}

	for i := 0; i < n; i++ {
		if _, err := c.CreateTask(h.Context(), &taskspb.CreateTaskRequest{
			Parent: name,
			Task:   &taskspb.Task{MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL}}},
		}); err != nil {
			t.Fatalf("CreateTask %d: %v", i, err)
		}
	}

	d := tg.await(t, n, 15*time.Second)
	// The gaps are measured where the requests arrive, so each may fall
	// short of a second by the difference in two loopback round trips;
	// allow that and no more.
	const slack = 50 * time.Millisecond
	for i := 1; i < n; i++ {
		if gap := d[i].at.Sub(d[i-1].at); gap < time.Second-slack {
			t.Errorf("delivery %d came %v after the previous one; want at least 1s at 1/s", i+1, gap)
		}
	}
	if spread := d[n-1].at.Sub(d[0].at); spread < time.Duration(n-1)*time.Second-slack {
		t.Errorf("%d deliveries spread over %v; want at least %v at 1/s", n, spread, time.Duration(n-1)*time.Second)
	}
}
