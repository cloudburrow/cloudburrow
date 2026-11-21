package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The queue's Metrics tab charts Google's four Cloud Tasks metrics from
// the instance's Cloud Monitoring (#2064), in Google's order, a point a
// minute over the last hour: the queue's API calls and its dispatch
// attempt are there. Without Cloud Monitoring the tab says why it has no
// charts.
func TestTasksMetricsTabChartsCloudMonitoring(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &tasksService{store: tasks.NewStore(store.NewMemory())}
	api, err := monitoring.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	api.AddBuiltin(monitoring.NewTasksBuiltin(monitoringTasks{svc: svc}, time.Now))
	p := tasksProvider{svc: svc, metrics: monitoringScreen{api: func() monitoringpb.MetricServiceServer { return api }}}

	const name = "projects/metrics-proj/locations/us-central1/queues/charted"
	g := tasks.NewGRPCServer(svc.Store())
	if _, err := g.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: "projects/metrics-proj/locations/us-central1",
		Queue: &cloudtaskspb.Queue{Name: name}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	created, err := g.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: name, Task: &cloudtaskspb.Task{
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{Url: srv.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	task, _ := svc.Store().GetTask(created.GetName())
	if err := tasks.NewDispatcher(svc.Store(), srv.Client(), sched.RealClock{}).Dispatch(ctx, task); err != nil {
		t.Fatal(err)
	}

	d, err := p.Detail(ctx, "metrics-proj", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	var sec console.Section
	for _, s := range d.Sections {
		if s.ID == "metrics" {
			sec = s
		}
	}
	if sec.Kind != console.KindChart || sec.Unavailable != "" {
		t.Fatalf("Metrics tab = %+v; want charts", sec)
	}
	want := []string{"API requests", "Queue depth", "Task attempt count", "Task attempt delays"}
	if len(sec.Series) != len(want) {
		t.Fatalf("charts = %d; want %v", len(sec.Series), want)
	}
	total := map[string]float64{}
	for i, s := range sec.Series {
		if s.Label != want[i] {
			t.Errorf("chart %d is %q; want %q", i, s.Label, want[i])
		}
		if len(s.Points) != tasksMetricsWindow {
			t.Errorf("%s has %d points; want one a minute for an hour", s.Label, len(s.Points))
		}
		for _, pt := range s.Points {
			if pt.Value != nil {
				total[s.Label] += *pt.Value
			}
		}
	}
	if total["API requests"] != 2 || total["Task attempt count"] != 1 {
		t.Errorf("charted %v; want the CreateQueue and CreateTask calls and the one attempt", total)
	}

	// Without Cloud Monitoring.
	bare := tasksProvider{svc: svc}
	d, err = bare.Detail(ctx, "metrics-proj", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Sections {
		if s.ID == "metrics" && s.Unavailable == "" {
			t.Errorf("Metrics tab without Cloud Monitoring = %+v; want why there are no charts", s)
		}
	}
}

// Disable logs is in the Logs tab's ellipsis menu, as Google's "Click the
// ellipsis icon > Disable logs"; Enable logs is a button (#2064).
func TestTasksDisableLogsIsInTheTabsMenu(t *testing.T) {
	t.Parallel()
	off := tasksLogsSection(tasks.Queue{})
	on := tasksLogsSection(tasks.Queue{Logging: &tasks.LoggingConfig{SamplingRatio: 1}})
	if len(off.Actions) != 1 || off.Actions[0].ID != tasksEnableLogs || off.Actions[0].Menu {
		t.Errorf("logging off, the Logs tab has %+v; want the Enable logs button", off.Actions)
	}
	if len(on.Actions) != 1 || on.Actions[0].ID != tasksDisableLogs || !on.Actions[0].Menu {
		t.Errorf("logging on, the Logs tab has %+v; want Disable logs in its menu", on.Actions)
	}
}

// A chart's minute is the sum of its deltas, a gauge's last reading, a
// distribution's mean weighted by count; a minute with nothing is nil.
func TestTasksChartSeriesBuckets(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(m int, sec int) *monitoringpb.TimeInterval {
		return &monitoringpb.TimeInterval{EndTime: timestamppb.New(end.Add(time.Duration(-m)*time.Minute + time.Duration(sec)*time.Second))}
	}
	ints := func(v int64) *monitoringpb.TypedValue {
		return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}}
	}
	counts := []*monitoringpb.TimeSeries{{Points: []*monitoringpb.Point{{Interval: at(2, 10), Value: ints(2)}, {Interval: at(2, 20), Value: ints(3)}}}}
	c := tasksChartSeries("c", "", false, counts, end)
	if v := c.Points[58].Value; v == nil || *v != 5 {
		t.Errorf("counts' minute = %v; want 5", v)
	}
	if c.Points[0].Value != nil {
		t.Errorf("an empty minute = %v; want nil", *c.Points[0].Value)
	}
	gauge := []*monitoringpb.TimeSeries{{Points: []*monitoringpb.Point{{Interval: at(2, 20), Value: ints(7)}, {Interval: at(2, 10), Value: ints(4)}}}}
	if v := tasksChartSeries("g", "", true, gauge, end).Points[58].Value; v == nil || *v != 7 {
		t.Errorf("gauge's minute = %v; want its last reading, 7", v)
	}
}
