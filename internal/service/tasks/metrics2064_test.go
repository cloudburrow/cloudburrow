package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// The API calls naming a queue and each dispatch attempt are recorded for
// Cloud Monitoring (#2064): method and canonical code, lower case, as the
// response_code label writes it; an attempt's code and its delay after
// its scheduled time. ListQueues names no queue and is not recorded.
func TestCallsAndAttemptsAreRecordedForMonitoring(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	g := NewGRPCServer(s)
	cc := context.Background()
	if _, err := g.CreateQueue(cc, &cloudtaskspb.CreateQueueRequest{Parent: loc, Queue: &cloudtaskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.GetQueue(cc, &cloudtaskspb.GetQueueRequest{Name: queueA + "-missing"}); err == nil {
		t.Fatal("GetQueue of a missing queue succeeded")
	}
	if _, err := g.ListQueues(cc, &cloudtaskspb.ListQueuesRequest{Parent: loc}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	created, err := g.CreateTask(cc, &cloudtaskspb.CreateTaskRequest{Parent: queueA, Task: &cloudtaskspb.Task{Name: TaskName(queueA, "t1"),
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{Url: srv.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	task, _ := s.GetTask(created.GetName())
	clock := sched.NewFakeClock(task.ScheduleTime.Add(1500 * time.Millisecond))
	if err := NewDispatcher(s, srv.Client(), clock).Dispatch(cc, task); err == nil {
		t.Fatal("a 503 succeeded")
	}

	evs := s.DrainMetrics()
	type call struct{ method, queue, code string }
	var calls []call
	var attempts []MetricEvent
	for _, e := range evs {
		switch e.Kind {
		case MetricAPICall:
			calls = append(calls, call{e.Method, e.Queue, e.Code})
		case MetricAttempt:
			attempts = append(attempts, e)
		}
	}
	want := []call{{"CreateQueue", queueA, "ok"}, {"GetQueue", queueA + "-missing", "not_found"}, {"CreateTask", queueA, "ok"}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v; want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %v; want %v", i, calls[i], want[i])
		}
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %v; want one", attempts)
	}
	if a := attempts[0]; a.Code != "unavailable" || a.Queue != queueA || a.TargetType != "HTTP" || a.Delay != 1500*time.Millisecond {
		t.Errorf("attempt = %+v; want unavailable on %s, HTTP, 1.5s after its schedule", a, queueA)
	}
	if again := s.DrainMetrics(); len(again) != 0 {
		t.Errorf("a second drain = %v; want nothing", again)
	}

	depths, err := s.Depths()
	if err != nil || len(depths) != 1 || depths[0].Queue != queueA || depths[0].Tasks != 1 || depths[0].TargetType != "HTTP" {
		t.Errorf("Depths = %+v, %v; want %s with its one task", depths, err, queueA)
	}
}

// The pending events are bounded, the oldest dropped first.
func TestPendingMetricsAreBounded(t *testing.T) {
	t.Parallel()
	var m metricsLog
	for i := 0; i < maxPendingMetrics+10; i++ {
		m.add(MetricEvent{Kind: MetricAttempt, Delay: time.Duration(i)})
	}
	if len(m.pending) != maxPendingMetrics || m.pending[0].Delay != 10 {
		t.Errorf("%d pending, first %v; want %d from the 11th", len(m.pending), m.pending[0].Delay, maxPendingMetrics)
	}
}
