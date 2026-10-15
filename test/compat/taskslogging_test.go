//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestTasksStackdriverLoggingConfig (#1256): through the official clients,
// a queue's stackdriver_logging_config is kept and returned by CreateQueue,
// GetQueue and UpdateQueue, and with a sampling_ratio of 1 each dispatch
// attempt is written to the instance's Cloud Logging as Cloud Tasks writes
// it: log cloudtasks.googleapis.com/task_operations_log, resource
// cloud_tasks_queue with project_id, location, queue_id and target_type,
// and a TaskActivityLog jsonPayload with the task and its
// attemptResponseLog, read back with logadmin's ListLogEntries. A ratio
// outside 0..1 is INVALID_ARGUMENT.
// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.tasks.v2.CloudTasks/UpdateQueue, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestTasksStackdriverLoggingConfig(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	logEndpoint := runShardEndpoint(h, "CLOUDBURROW_TEST_TASKS_LOGGING", EnvLogging)
	c := tasksClient(t, h)

	id := "logged-q"
	name := fmt.Sprintf("%s/queues/%s", location(h), id)
	_, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h), Queue: &taskspb.Queue{Name: name + "-bad",
		StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1.5}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateQueue with a sampling_ratio of 1.5 = %v; want INVALID_ARGUMENT", err)
	}
	created, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h), Queue: &taskspb.Queue{Name: name,
		StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 0.5}}})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: name}) })
	if created.GetStackdriverLoggingConfig().GetSamplingRatio() != 0.5 {
		t.Errorf("created sampling_ratio = %v", created.GetStackdriverLoggingConfig())
	}
	if _, err := c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{Queue: &taskspb.Queue{Name: name,
		StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stackdriver_logging_config"}}}); err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: name})
	if err != nil || got.GetStackdriverLoggingConfig().GetSamplingRatio() != 1 {
		t.Fatalf("GetQueue sampling_ratio = %v, %v", got.GetStackdriverLoggingConfig(), err)
	}

	tg := newTarget(t, http.StatusOK)
	task := name + "/tasks/logged-task"
	if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: name, Task: &taskspb.Task{Name: task,
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL + "/logged", HttpMethod: taskspb.HttpMethod_POST}}}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	tg.await(t, 1, 15*time.Second)

	ac, err := logadmin.NewClient(ctx, "projects/"+h.Project(), option.WithEndpoint(logEndpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	logName := "projects/" + h.Project() + "/logs/cloudtasks.googleapis.com%2Ftask_operations_log"
	var entry *logging.Entry
	for deadline := time.Now().Add(10 * time.Second); entry == nil && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		// The instance's filter takes logName, severity, resource.type and
		// timestamp; the queue is picked out of the payload.
		it := ac.Entries(ctx, logadmin.Filter(`logName="`+logName+`" AND resource.type="cloud_tasks_queue"`))
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListLogEntries: %v", err)
			}
			if p, ok := e.Payload.(*structpb.Struct); ok && p.AsMap()["task"] == task {
				entry = e
			}
		}
	}
	if entry == nil {
		t.Fatalf("no entry in %s for queue %s", logName, id)
	}
	if entry.Resource.GetType() != "cloud_tasks_queue" {
		t.Errorf("resource type = %q", entry.Resource.GetType())
	}
	_, loc, _ := strings.Cut(location(h), "/locations/")
	for k, want := range map[string]string{"project_id": h.Project(), "location": loc, "queue_id": id, "target_type": "HTTP"} {
		if got := entry.Resource.GetLabels()[k]; got != want {
			t.Errorf("resource.labels.%s = %q; want %q", k, got, want)
		}
	}
	if entry.Severity != logging.Info {
		t.Errorf("severity = %v; want INFO for a 200", entry.Severity)
	}
	payload, ok := entry.Payload.(*structpb.Struct)
	if !ok {
		t.Fatalf("payload is %T; want a jsonPayload", entry.Payload)
	}
	p := payload.AsMap()
	if p["@type"] != "type.googleapis.com/google.cloud.tasks.logging.v1.TaskActivityLog" || p["task"] != task {
		t.Errorf("jsonPayload = %v", p)
	}
	a, _ := p["attemptResponseLog"].(map[string]any)
	if a["status"] != "OK" || a["targetType"] != "HTTP" || a["targetAddress"] != "POST "+tg.srv.URL+"/logged" ||
		a["dispatchCount"] != "1" || a["responseCount"] != "1" || a["scheduleTime"] == nil || a["attemptDuration"] == nil {
		t.Errorf("attemptResponseLog = %v", a)
	}
}
