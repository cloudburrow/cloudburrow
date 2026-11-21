package monitoring

import (
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---- Cloud Tasks (#2064) ----

// Google's four Cloud Tasks metric types, on cloud_tasks_queue, as
// https://docs.cloud.google.com/monitoring/api/metrics_gcp_c lists them
// (read 2026-11-20).
const (
	TasksAPIRequests  = "cloudtasks.googleapis.com/api/request_count"
	TasksDepth        = "cloudtasks.googleapis.com/queue/depth"
	TasksAttemptCount = "cloudtasks.googleapis.com/queue/task_attempt_count"
	TasksAttemptDelay = "cloudtasks.googleapis.com/queue/task_attempt_delays"
)

// TasksEvent is one Cloud Tasks API call naming a queue (Method set) or one
// dispatch attempt (Method empty).
type TasksEvent struct {
	Time time.Time
	// Queue is projects/P/locations/L/queues/Q.
	Queue, TargetType string
	Method            string
	// Code is the response_code label: a canonical code, lower case.
	Code string
	// DelayMs is an attempt's delay after its scheduled time.
	DelayMs float64
}

// TasksQueueDepth is one queue's task count.
type TasksQueueDepth struct {
	Queue, TargetType string
	Tasks             int
}

// TasksSource is the instance's Cloud Tasks.
type TasksSource interface {
	// Drain is the calls and attempts since the last Drain, oldest first.
	Drain() []TasksEvent
	// Depths are every queue's task counts now.
	Depths() []TasksQueueDepth
}

// tasksRing bounds each series: a week of one-minute samples.
const tasksRing = 7 * 24 * 60

// tasksDelayBuckets are task_attempt_delays' buckets, in ms: exponential,
// 1 ms growing by 2 (Google does not publish its bounds).
var tasksDelayBuckets = &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExponentialBuckets{
	ExponentialBuckets: &distribution.Distribution_BucketOptions_Exponential{NumFiniteBuckets: 32, GrowthFactor: 2, Scale: 1}}}

// TasksBuiltin serves cloudtasks.googleapis.com/* from periodic samples of
// a TasksSource, as Google samples them every 60 seconds: each sample's
// calls and attempts are one DELTA point per series over (last sample,
// this one], and each queue's depth one GAUGE point. A read samples first,
// so it sees what happened up to it.
type TasksBuiltin struct {
	src TasksSource
	now func() time.Time

	mu     sync.Mutex
	lastAt time.Time
	series map[string]*monitoringpb.TimeSeries
}

// NewTasksBuiltin returns Cloud Tasks' metrics, sampled from src.
func NewTasksBuiltin(src TasksSource, now func() time.Time) *TasksBuiltin {
	return &TasksBuiltin{src: src, now: now, lastAt: now(), series: map[string]*monitoringpb.TimeSeries{}}
}

// queueResource is a queue's cloud_tasks_queue resource.
func queueResource(queue, targetType string) (*monitoredres.MonitoredResource, string) {
	p := strings.Split(queue, "/")
	if len(p) != 6 {
		return nil, ""
	}
	return &monitoredres.MonitoredResource{Type: "cloud_tasks_queue", Labels: map[string]string{
		"project_id": p[1], "location": p[3], "queue_id": p[5], "target_type": targetType}}, p[1]
}

// Sample turns the source's calls and attempts since the last sample into
// points, and records each queue's depth.
func (b *TasksBuiltin) Sample() { b.sampleAt(b.now()) }

// sampleAt samples, the points ending at now.
func (b *TasksBuiltin) sampleAt(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := b.lastAt
	if !now.After(start) {
		return
	}
	events := b.src.Drain()
	depths := b.src.Depths()
	counts := map[string]int64{}
	delays := map[string][]float64{}
	heads := map[string]*monitoringpb.TimeSeries{}
	for _, e := range events {
		res, _ := queueResource(e.Queue, e.TargetType)
		if res == nil {
			continue
		}
		var ts *monitoringpb.TimeSeries
		if e.Method != "" {
			ts = &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: TasksAPIRequests,
				Labels: map[string]string{"api_method": e.Method, "response_code": e.Code}},
				Resource: res, MetricKind: delta, ValueType: tInt, Unit: "1"}
		} else {
			ts = &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: TasksAttemptCount,
				Labels: map[string]string{"response_code": e.Code}},
				Resource: res, MetricKind: delta, ValueType: tInt, Unit: "1"}
			d := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: TasksAttemptDelay, Labels: map[string]string{}},
				Resource: res, MetricKind: delta, ValueType: tDist, Unit: "ms"}
			dk := seriesKey(d)
			heads[dk] = d
			delays[dk] = append(delays[dk], e.DelayMs)
		}
		k := seriesKey(ts)
		heads[k] = ts
		counts[k]++
	}
	interval := &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(now)}
	for k, n := range counts {
		b.add(k, heads[k], &monitoringpb.Point{Interval: interval, Value: intValue(int(n))})
	}
	for k, vs := range delays {
		b.add(k, heads[k], &monitoringpb.Point{Interval: interval, Value: distValue(newDistribution(tasksDelayBuckets, vs))})
	}
	at := &monitoringpb.TimeInterval{StartTime: timestamppb.New(now), EndTime: timestamppb.New(now)}
	for _, d := range depths {
		res, _ := queueResource(d.Queue, d.TargetType)
		if res == nil {
			continue
		}
		ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: TasksDepth, Labels: map[string]string{}},
			Resource: res, MetricKind: gauge, ValueType: tInt, Unit: "1"}
		b.add(seriesKey(ts), ts, &monitoringpb.Point{Interval: at, Value: intValue(d.Tasks)})
	}
	b.lastAt = now
}

// add appends p to the series k, starting it with head. The caller holds mu.
func (b *TasksBuiltin) add(k string, head *monitoringpb.TimeSeries, p *monitoringpb.Point) {
	ts := b.series[k]
	if ts == nil {
		ts = head
		b.series[k] = ts
	}
	ts.Points = append(ts.Points, p)
	if len(ts.Points) > tasksRing {
		ts.Points = ts.Points[len(ts.Points)-tasksRing:]
	}
}

func (b *TasksBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	res := []string{"cloud_tasks_queue"}
	code := &labelpb.LabelDescriptor{Key: "response_code", ValueType: labelpb.LabelDescriptor_STRING,
		Description: "Canonical response code as string (e.g. 'ok')."}
	return []*metricpb.MetricDescriptor{
		{Name: descriptorName(project, TasksAPIRequests), Type: TasksAPIRequests, MetricKind: delta, ValueType: tInt, Unit: "1",
			DisplayName: "API requests", Description: "Count of Cloud Tasks API calls.",
			Labels: []*labelpb.LabelDescriptor{{Key: "api_method", ValueType: labelpb.LabelDescriptor_STRING,
				Description: "The API method called (e.g. CreateTask)."}, clone(code)},
			MonitoredResourceTypes: res},
		{Name: descriptorName(project, TasksDepth), Type: TasksDepth, MetricKind: gauge, ValueType: tInt, Unit: "1",
			DisplayName: "Queue depth", Description: "The number of tasks in the queue.", MonitoredResourceTypes: res},
		{Name: descriptorName(project, TasksAttemptCount), Type: TasksAttemptCount, MetricKind: delta, ValueType: tInt, Unit: "1",
			DisplayName: "Task attempt count", Description: "Count of task attempts broken down by response code.",
			Labels: []*labelpb.LabelDescriptor{clone(code)}, MonitoredResourceTypes: res},
		{Name: descriptorName(project, TasksAttemptDelay), Type: TasksAttemptDelay, MetricKind: delta, ValueType: tDist, Unit: "ms",
			DisplayName: "Task attempt delays", Description: "Delay between each scheduled attempt time and actual attempt time.",
			MonitoredResourceTypes: res},
	}
}

func (b *TasksBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	switch metricType {
	case TasksAPIRequests, TasksDepth, TasksAttemptCount, TasksAttemptDelay:
	default:
		return nil, nil
	}
	// A read samples first, its points ending no later than the read's
	// interval, so a read ending now sees what happened up to it.
	at := b.now()
	if end.Before(at) {
		at = end
	}
	b.sampleAt(at)
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*monitoringpb.TimeSeries
	for _, ts := range b.series {
		if ts.GetMetric().GetType() != metricType || ts.GetResource().GetLabels()["project_id"] != project {
			continue
		}
		c := &monitoringpb.TimeSeries{Metric: clone(ts.Metric), Resource: clone(ts.Resource),
			MetricKind: ts.MetricKind, ValueType: ts.ValueType, Unit: ts.Unit}
		for _, p := range ts.Points {
			e := p.GetInterval().GetEndTime().AsTime()
			if !e.Before(start) && !e.After(end) {
				c.Points = append(c.Points, clone(p))
			}
		}
		if len(c.Points) > 0 {
			out = append(out, c)
		}
	}
	return out, nil
}
