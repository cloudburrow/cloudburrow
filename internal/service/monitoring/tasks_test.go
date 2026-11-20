package monitoring

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeTasks struct {
	events []TasksEvent
	depths []TasksQueueDepth
}

func (f *fakeTasks) Drain() []TasksEvent {
	out := f.events
	f.events = nil
	return out
}

func (f *fakeTasks) Depths() []TasksQueueDepth { return f.depths }

// Cloud Tasks' four metric types (#2064) are served with Google's kinds,
// units and labels on cloud_tasks_queue, from the calls and attempts each
// sample drains: DELTA points over (last sample, this one], a GAUGE point
// of the depth at each sample, the delays a distribution in ms. Another
// project's queue is not in this project's series.
func TestTasksBuiltinServesGooglesMetrics(t *testing.T) {
	s, now := newTestServer(t, "")
	const q = "projects/p1/locations/us-central1/queues/q1"
	src := &fakeTasks{depths: []TasksQueueDepth{{Queue: q, TargetType: "HTTP", Tasks: 3},
		{Queue: "projects/p2/locations/us-central1/queues/other", TargetType: "HTTP", Tasks: 9}}}
	b := NewTasksBuiltin(src, func() time.Time { return *now })
	s.AddBuiltin(b)

	*now = t0.Add(30 * time.Second)
	src.events = []TasksEvent{
		{Time: *now, Queue: q, TargetType: "HTTP", Method: "CreateTask", Code: "ok"},
		{Time: *now, Queue: q, TargetType: "HTTP", Method: "CreateTask", Code: "ok"},
		{Time: *now, Queue: q, TargetType: "HTTP", Code: "ok", DelayMs: 10},
		{Time: *now, Queue: q, TargetType: "HTTP", Code: "unavailable", DelayMs: 30},
	}
	b.Sample()
	*now = t0.Add(90 * time.Second)

	descs, err := s.ListMetricDescriptors(context.Background(), &monitoringpb.ListMetricDescriptorsRequest{Name: "projects/p1",
		Filter: `metric.type = starts_with("cloudtasks.googleapis.com/")`})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, md := range descs.GetMetricDescriptors() {
		kinds[md.GetType()] = md.GetMetricKind().String() + " " + md.GetValueType().String() + " " + md.GetUnit()
	}
	for typ, want := range map[string]string{TasksAPIRequests: "DELTA INT64 1", TasksDepth: "GAUGE INT64 1",
		TasksAttemptCount: "DELTA INT64 1", TasksAttemptDelay: "DELTA DISTRIBUTION ms"} {
		if kinds[typ] != want {
			t.Errorf("%s = %q; want %q", typ, kinds[typ], want)
		}
	}

	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(t0), EndTime: timestamppb.New(*now)}
	read := func(typ string) []*monitoringpb.TimeSeries {
		return listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "` + typ + `"`, Interval: iv})
	}
	calls := read(TasksAPIRequests)
	if len(calls) != 1 || calls[0].GetMetric().GetLabels()["api_method"] != "CreateTask" ||
		calls[0].GetMetric().GetLabels()["response_code"] != "ok" || calls[0].GetPoints()[0].GetValue().GetInt64Value() != 2 {
		t.Fatalf("request_count = %v; want CreateTask ok 2", calls)
	}
	if r := calls[0].GetResource(); r.GetType() != "cloud_tasks_queue" || r.GetLabels()["queue_id"] != "q1" ||
		r.GetLabels()["location"] != "us-central1" || r.GetLabels()["project_id"] != "p1" || r.GetLabels()["target_type"] != "HTTP" {
		t.Errorf("resource = %v", r)
	}
	if p := calls[0].GetPoints()[0].GetInterval(); !p.GetStartTime().AsTime().Equal(t0) || !p.GetEndTime().AsTime().Equal(t0.Add(30*time.Second)) {
		t.Errorf("the delta's interval = %v; want (start, first sample]", p)
	}
	attempts := map[string]int64{}
	for _, ts := range read(TasksAttemptCount) {
		attempts[ts.GetMetric().GetLabels()["response_code"]] += ts.GetPoints()[0].GetValue().GetInt64Value()
	}
	if attempts["ok"] != 1 || attempts["unavailable"] != 1 {
		t.Errorf("task_attempt_count = %v; want one ok, one unavailable", attempts)
	}
	delays := read(TasksAttemptDelay)
	if len(delays) != 1 {
		t.Fatalf("task_attempt_delays = %v; want one series", delays)
	}
	d := delays[0].GetPoints()[0].GetValue().GetDistributionValue()
	if d.GetCount() != 2 || d.GetMean() != 20 || delays[0].GetValueType() != metricpb.MetricDescriptor_DISTRIBUTION {
		t.Errorf("task_attempt_delays = %v; want 2 attempts, mean 20 ms", d)
	}
	depth := read(TasksDepth)
	if len(depth) != 1 || depth[0].GetMetricKind() != metricpb.MetricDescriptor_GAUGE {
		t.Fatalf("queue/depth = %v; want this project's one queue", depth)
	}
	for _, p := range depth[0].GetPoints() {
		if p.GetValue().GetInt64Value() != 3 {
			t.Errorf("queue/depth point = %v; want 3", p)
		}
	}
	if len(depth[0].GetPoints()) < 2 {
		t.Errorf("queue/depth has %d points; want the sample and the read's", len(depth[0].GetPoints()))
	}
}
