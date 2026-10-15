package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// stackdriver_logging_config round-trips through CreateQueue, GetQueue and
// UpdateQueue (#1256).
func TestQueueLoggingConfigRoundTrips(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	q, err := c.CreateQueue(cc, &cloudtaskspb.CreateQueueRequest{Parent: loc, Queue: &cloudtaskspb.Queue{Name: queueA,
		StackdriverLoggingConfig: &cloudtaskspb.StackdriverLoggingConfig{SamplingRatio: 0.25}}})
	if err != nil || q.GetStackdriverLoggingConfig().GetSamplingRatio() != 0.25 {
		t.Fatalf("CreateQueue = %v, %v", q.GetStackdriverLoggingConfig(), err)
	}
	q, err = c.UpdateQueue(cc, &cloudtaskspb.UpdateQueueRequest{Queue: &cloudtaskspb.Queue{Name: queueA,
		StackdriverLoggingConfig: &cloudtaskspb.StackdriverLoggingConfig{SamplingRatio: 1}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stackdriver_logging_config.sampling_ratio"}}})
	if err != nil || q.GetStackdriverLoggingConfig().GetSamplingRatio() != 1 {
		t.Fatalf("UpdateQueue = %v, %v", q.GetStackdriverLoggingConfig(), err)
	}
	q, err = c.UpdateQueue(cc, &cloudtaskspb.UpdateQueueRequest{Queue: &cloudtaskspb.Queue{Name: queueA},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stackdriver_logging_config"}}})
	if err != nil || q.GetStackdriverLoggingConfig() != nil {
		t.Fatalf("clearing = %v, %v", q.GetStackdriverLoggingConfig(), err)
	}
	if got, _ := c.GetQueue(cc, &cloudtaskspb.GetQueueRequest{Name: queueA}); got.GetStackdriverLoggingConfig() != nil {
		t.Errorf("GetQueue after clearing = %v", got.GetStackdriverLoggingConfig())
	}
}

// Each attempt on a queue sampling every operation is written as Cloud
// Tasks writes it; a queue sampling none writes nothing.
func TestDispatchAttemptsAreLogged(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	queueB := queueA + "-quiet"
	for _, q := range []Queue{{Name: queueA, Logging: &LoggingConfig{SamplingRatio: 1}}, {Name: queueB, Logging: &LoggingConfig{}}} {
		if _, err := s.CreateQueue(q); err != nil {
			t.Fatal(err)
		}
	}
	codes := []int{http.StatusServiceUnavailable, http.StatusOK}
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(codes[n%2])
		n++
	}))
	defer srv.Close()
	var entries []*loggingpb.LogEntry
	d := NewDispatcher(s, srv.Client(), sched.NewFakeClock(time.Now())).WithLogging(func(_ context.Context, e *loggingpb.LogEntry) error {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, e)
		return nil
	})
	task := Task{Name: TaskName(queueA, "t1"), HTTPRequest: &HTTPRequest{URL: srv.URL + "/work", Method: "PUT"}}
	if _, err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := d.Dispatch(context.Background(), task); err == nil {
		t.Fatal("a 503 succeeded")
	}
	task, _ = s.GetTask(task.Name)
	if err := d.Dispatch(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	quiet := Task{Name: TaskName(queueB, "t2"), HTTPRequest: &HTTPRequest{URL: srv.URL}}
	if _, err := s.CreateTask(quiet); err != nil {
		t.Fatal(err)
	}
	_ = d.Dispatch(context.Background(), quiet)

	if len(entries) != 2 {
		t.Fatalf("%d entries; want the two attempts on the sampled queue", len(entries))
	}
	e := entries[0]
	if e.GetLogName() != "projects/my-project/logs/cloudtasks.googleapis.com%2Ftask_operations_log" {
		t.Errorf("logName = %s", e.GetLogName())
	}
	if r := e.GetResource(); r.GetType() != "cloud_tasks_queue" || r.GetLabels()["queue_id"] != "queue-a" ||
		r.GetLabels()["project_id"] != "my-project" || r.GetLabels()["location"] != "us-central1" || r.GetLabels()["target_type"] != "HTTP" {
		t.Errorf("resource = %v", r)
	}
	if e.GetSeverity() != ltype.LogSeverity_ERROR || entries[1].GetSeverity() != ltype.LogSeverity_INFO {
		t.Errorf("severities = %v, %v", e.GetSeverity(), entries[1].GetSeverity())
	}
	p := e.GetJsonPayload().AsMap()
	if p["@type"] != "type.googleapis.com/google.cloud.tasks.logging.v1.TaskActivityLog" || p["task"] != task.Name {
		t.Errorf("payload = %v", p)
	}
	a := p["attemptResponseLog"].(map[string]any)
	if a["status"] != "UNAVAILABLE" || a["targetAddress"] != "PUT "+srv.URL+"/work" || a["targetType"] != "HTTP" ||
		a["dispatchCount"] != "1" || a["responseCount"] != "1" || a["retryTime"] == nil || a["maxAttempts"] == nil {
		t.Errorf("attemptResponseLog = %v", a)
	}
	b := entries[1].GetJsonPayload().AsMap()["attemptResponseLog"].(map[string]any)
	if b["status"] != "OK" || b["dispatchCount"] != "2" || b["retryTime"] != nil {
		t.Errorf("second attemptResponseLog = %v", b)
	}
}
