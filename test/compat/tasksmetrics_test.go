//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestTasksQueueMetricsInCloudMonitoring (#2064): through the official
// clients, a queue's calls and dispatch are Google's four Cloud Tasks
// metrics in the instance's Cloud Monitoring, on the cloud_tasks_queue
// resource (project_id, location, queue_id, target_type), as
// https://docs.cloud.google.com/monitoring/api/metrics_gcp_c lists them:
// api/request_count (DELTA INT64, api_method and response_code),
// queue/depth (GAUGE INT64), queue/task_attempt_count (DELTA INT64,
// response_code) and queue/task_attempt_delays (DELTA DISTRIBUTION, ms).
// Two tasks are dispatched to a local HTTP target, one answering 200 and
// one 503 then retried; the series are read back with
// cloud.google.com/go/monitoring's ListTimeSeries and their descriptors
// with GetMetricDescriptor.
// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/CreateTask, google.monitoring.v3.MetricService/ListTimeSeries, google.monitoring.v3.MetricService/GetMetricDescriptor
func TestTasksQueueMetricsInCloudMonitoring(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := tasksClient(t, h)
	mc := monitoringClient(t, h)

	id := "metrics-q"
	name := fmt.Sprintf("%s/queues/%s", location(h), id)
	if _, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h), Queue: &taskspb.Queue{Name: name,
		RetryConfig: &taskspb.RetryConfig{MaxAttempts: 2}}}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: name}) })

	ok := newTarget(t, http.StatusOK)
	failing := newTarget(t, http.StatusServiceUnavailable)
	for i, url := range []string{ok.srv.URL + "/ok", failing.srv.URL + "/fail"} {
		if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: name, Task: &taskspb.Task{
			Name:        fmt.Sprintf("%s/tasks/metrics-task-%d", name, i),
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: url, HttpMethod: taskspb.HttpMethod_POST}}}}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	ok.await(t, 1, 15*time.Second)
	failing.await(t, 1, 15*time.Second)

	read := func(metricType string) []*monitoringpb.TimeSeries {
		now := time.Now()
		return listAll(t, mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + h.Project(),
			Filter:   fmt.Sprintf(`metric.type = %q AND resource.labels.queue_id = %q`, metricType, id),
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-10 * time.Minute)), EndTime: timestamppb.New(now)},
			View:     monitoringpb.ListTimeSeriesRequest_FULL}))
	}
	checkResource := func(ts *monitoringpb.TimeSeries) {
		t.Helper()
		r := ts.GetResource()
		want := map[string]string{"project_id": h.Project(), "location": "us-central1", "queue_id": id, "target_type": "HTTP"}
		if r.GetType() != "cloud_tasks_queue" || len(r.GetLabels()) != len(want) {
			t.Errorf("%s resource = %v; want cloud_tasks_queue %v", ts.GetMetric().GetType(), r, want)
			return
		}
		for k, v := range want {
			if r.GetLabels()[k] != v {
				t.Errorf("%s resource label %s = %q; want %q", ts.GetMetric().GetType(), k, r.GetLabels()[k], v)
			}
		}
	}
	sum := func(series []*monitoringpb.TimeSeries, label, value string) int64 {
		var n int64
		for _, ts := range series {
			if label != "" && ts.GetMetric().GetLabels()[label] != value {
				continue
			}
			for _, p := range ts.GetPoints() {
				n += p.GetValue().GetInt64Value()
			}
		}
		return n
	}

	// Attempts: one ok, at least one unavailable (503), each with its delay.
	var attempts []*monitoringpb.TimeSeries
	eventuallyTrue(t, "task_attempt_count to count the ok and the 503 attempts", func() bool {
		attempts = read("cloudtasks.googleapis.com/queue/task_attempt_count")
		return sum(attempts, "response_code", "ok") >= 1 && sum(attempts, "response_code", "unavailable") >= 1
	})
	for _, ts := range attempts {
		checkResource(ts)
		if ts.GetMetricKind() != metricpb.MetricDescriptor_DELTA || ts.GetValueType() != metricpb.MetricDescriptor_INT64 {
			t.Errorf("task_attempt_count is %v %v; want DELTA INT64", ts.GetMetricKind(), ts.GetValueType())
		}
	}
	if n := sum(attempts, "response_code", "ok"); n != 1 {
		t.Errorf("task_attempt_count{response_code=ok} = %d; want 1", n)
	}

	delays := read("cloudtasks.googleapis.com/queue/task_attempt_delays")
	var count int64
	for _, ts := range delays {
		checkResource(ts)
		if ts.GetValueType() != metricpb.MetricDescriptor_DISTRIBUTION || ts.GetUnit() != "ms" {
			t.Errorf("task_attempt_delays is %v in %q; want DISTRIBUTION in ms", ts.GetValueType(), ts.GetUnit())
		}
		for _, p := range ts.GetPoints() {
			d := p.GetValue().GetDistributionValue()
			count += d.GetCount()
			if d.GetMean() < 0 {
				t.Errorf("task_attempt_delays mean %v ms is negative", d.GetMean())
			}
		}
	}
	if count < 2 {
		t.Errorf("task_attempt_delays counted %d attempts; want at least 2", count)
	}

	// API requests: the queue's CreateQueue and its two CreateTask calls.
	calls := read("cloudtasks.googleapis.com/api/request_count")
	for _, ts := range calls {
		checkResource(ts)
	}
	if n := sum(calls, "api_method", "CreateTask"); n != 2 {
		t.Errorf("request_count{api_method=CreateTask} = %d; want 2", n)
	}
	for _, ts := range calls {
		if ts.GetMetric().GetLabels()["api_method"] == "CreateTask" && ts.GetMetric().GetLabels()["response_code"] != "ok" {
			t.Errorf("CreateTask's response_code = %q; want ok", ts.GetMetric().GetLabels()["response_code"])
		}
	}

	// Depth: the 503 task is still in the queue until its retries end; a
	// gauge point is the count when it was sampled.
	depth := read("cloudtasks.googleapis.com/queue/depth")
	if len(depth) != 1 || len(depth[0].GetPoints()) == 0 {
		t.Fatalf("queue/depth = %v; want one series with points", depth)
	}
	checkResource(depth[0])
	if depth[0].GetMetricKind() != metricpb.MetricDescriptor_GAUGE {
		t.Errorf("queue/depth is %v; want GAUGE", depth[0].GetMetricKind())
	}
	if v := depth[0].GetPoints()[0].GetValue().GetInt64Value(); v < 0 || v > 1 {
		t.Errorf("queue/depth now = %d; want 0 or 1 (the 503 task until its retries end)", v)
	}

	for typ, kind := range map[string]metricpb.MetricDescriptor_MetricKind{
		"cloudtasks.googleapis.com/api/request_count":         metricpb.MetricDescriptor_DELTA,
		"cloudtasks.googleapis.com/queue/depth":               metricpb.MetricDescriptor_GAUGE,
		"cloudtasks.googleapis.com/queue/task_attempt_count":  metricpb.MetricDescriptor_DELTA,
		"cloudtasks.googleapis.com/queue/task_attempt_delays": metricpb.MetricDescriptor_DELTA,
	} {
		md, err := mc.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + h.Project() + "/metricDescriptors/" + typ})
		if err != nil {
			t.Errorf("GetMetricDescriptor %s: %v", typ, err)
			continue
		}
		if md.GetMetricKind() != kind || len(md.GetMonitoredResourceTypes()) != 1 || md.GetMonitoredResourceTypes()[0] != "cloud_tasks_queue" {
			t.Errorf("%s descriptor = %v on %v; want %v on cloud_tasks_queue", typ, md.GetMetricKind(), md.GetMonitoredResourceTypes(), kind)
		}
	}
}
