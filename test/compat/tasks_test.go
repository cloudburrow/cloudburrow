//go:build compat

package compat

import (
	"fmt"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// tasksClient returns the official Cloud Tasks client pointed at the local
// instance.
//
// Cloud Tasks has no emulator environment variable in any official client, so
// the endpoint and insecure credentials must be passed explicitly. That is the
// documented ergonomic limit, demonstrated here rather than described.
func tasksClient(t *testing.T, h *Harness) *cloudtasks.Client {
	t.Helper()
	endpoint := h.Endpoint(EnvTasks)
	c, err := cloudtasks.NewClient(h.Context(),
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("cloudtasks.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func location(h *Harness) string {
	return fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
}

// queue creates a uniquely named queue and removes it afterwards.
func queue(t *testing.T, h *Harness, c *cloudtasks.Client, id string) string {
	t.Helper()
	name := fmt.Sprintf("%s/queues/%s", location(h), id)
	if _, err := c.CreateQueue(h.Context(), &taskspb.CreateQueueRequest{
		Parent: location(h),
		Queue:  &taskspb.Queue{Name: name},
	}); err != nil {
		t.Fatalf("CreateQueue %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.DeleteQueue(h.Context(), &taskspb.DeleteQueueRequest{Name: name})
	})
	return name
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue
func TestTasksQueueLifecycle(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()

	name := queue(t, h, c, "lifecycle-queue")

	got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if got.GetName() != name {
		t.Errorf("name = %q, want %q", got.GetName(), name)
	}
	if got.GetState() != taskspb.Queue_RUNNING {
		t.Errorf("state = %v, want RUNNING", got.GetState())
	}

	// A missing queue must map to NotFound.
	_, err = c.GetQueue(ctx, &taskspb.GetQueueRequest{
		Name: fmt.Sprintf("%s/queues/absent-queue", location(h)),
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetQueue(absent) = %v, want NotFound", status.Code(err))
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/ListQueues
func TestTasksListQueues(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)

	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		want[queue(t, h, c, fmt.Sprintf("list-queue-%d", i))] = true
	}

	it := c.ListQueues(h.Context(), &taskspb.ListQueuesRequest{Parent: location(h)})
	got := map[string]bool{}
	for {
		q, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListQueues: %v", err)
		}
		got[q.GetName()] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("ListQueues omitted %s", name)
		}
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/PauseQueue, google.cloud.tasks.v2.CloudTasks/ResumeQueue
func TestTasksPauseAndResume(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	name := queue(t, h, c, "pause-queue")

	paused, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}
	if paused.GetState() != taskspb.Queue_PAUSED {
		t.Errorf("state = %v, want PAUSED", paused.GetState())
	}

	resumed, err := c.ResumeQueue(ctx, &taskspb.ResumeQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	if resumed.GetState() != taskspb.Queue_RUNNING {
		t.Errorf("state = %v, want RUNNING", resumed.GetState())
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/GetTask, google.cloud.tasks.v2.CloudTasks/DeleteTask
func TestTasksTaskLifecycle(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "task-queue")

	// A target that will not be reached, so the task stays queued for the
	// assertions below rather than being dispatched and deleted.
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{
		Parent: q,
		Task: &taskspb.Task{
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
				Url:        "http://127.0.0.1:1/never",
				HttpMethod: taskspb.HttpMethod_PUT,
				Body:       []byte("payload"),
				Headers:    map[string]string{"X-Test": "yes"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.GetName() == "" {
		t.Fatal("no task name was generated")
	}
	if task.GetHttpRequest().GetHttpMethod() != taskspb.HttpMethod_PUT {
		t.Errorf("method = %v, want PUT", task.GetHttpRequest().GetHttpMethod())
	}

	got, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName()})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if string(got.GetHttpRequest().GetBody()) != "payload" {
		t.Errorf("body = %q, want payload", got.GetHttpRequest().GetBody())
	}
	if got.GetHttpRequest().GetHeaders()["X-Test"] != "yes" {
		t.Errorf("headers did not round-trip: %v", got.GetHttpRequest().GetHeaders())
	}

	if err := c.DeleteTask(ctx, &taskspb.DeleteTaskRequest{Name: task.GetName()}); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTask after delete = %v, want NotFound", status.Code(err))
	}
}

// Creating a task in a queue that does not exist must be NotFound, not a
// silently created queue.
func TestTasksTaskInMissingQueue(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	_, err := c.CreateTask(h.Context(), &taskspb.CreateTaskRequest{
		Parent: fmt.Sprintf("%s/queues/no-such-queue", location(h)),
		Task: &taskspb.Task{MessageType: &taskspb.Task_HttpRequest{
			HttpRequest: &taskspb.HttpRequest{Url: "http://127.0.0.1:1/"},
		}},
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("CreateTask in missing queue = %v, want NotFound", status.Code(err))
	}
}

// Unsupported operations must say so through the SDK, not return a plausible
// empty success.
// covers: google.cloud.tasks.v2.CloudTasks/RunTask (unimplemented)
func TestTasksUnsupportedOperationsAreHonest(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "honest-queue")

	if _, err := c.RunTask(ctx, &taskspb.RunTaskRequest{Name: q + "/tasks/whatever"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("RunTask = %v, want Unimplemented", status.Code(err))
	}

	// App Engine targets are out of scope and must be reported as such.
	_, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{
		Parent: q,
		Task: &taskspb.Task{MessageType: &taskspb.Task_AppEngineHttpRequest{
			AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{RelativeUri: "/work"},
		}},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("App Engine target = %v, want Unimplemented", status.Code(err))
	}
}

// TestTasksRefusesWhatItWouldDrop: through the official client, a task with
// an OIDC or OAuth token is UNIMPLEMENTED naming the field rather than
// dispatched without an Authorization header, a GET with a body is
// INVALID_ARGUMENT, dispatch_deadline and max_retry_duration read back as
// set, and stackdriver_logging_config and a ListQueues filter are
// UNIMPLEMENTED (#578).
func TestTasksRefusesWhatItWouldDrop(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "compat-fidelity")
	create := func(hr *taskspb.HttpRequest, dd *durationpb.Duration) (*taskspb.Task, error) {
		return c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{DispatchDeadline: dd,
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: hr}}})
	}
	for field, hr := range map[string]*taskspb.HttpRequest{
		"oidcToken":  {Url: "http://127.0.0.1:1/", AuthorizationHeader: &taskspb.HttpRequest_OidcToken{OidcToken: &taskspb.OidcToken{ServiceAccountEmail: "sa@" + h.Project() + ".iam.gserviceaccount.com"}}},
		"oauthToken": {Url: "http://127.0.0.1:1/", AuthorizationHeader: &taskspb.HttpRequest_OauthToken{OauthToken: &taskspb.OAuthToken{ServiceAccountEmail: "sa@" + h.Project() + ".iam.gserviceaccount.com"}}},
	} {
		if _, err := create(hr, nil); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), field) {
			t.Errorf("CreateTask with %s = %v; want Unimplemented naming it", field, err)
		}
	}
	if _, err := create(&taskspb.HttpRequest{Url: "http://127.0.0.1:1/", HttpMethod: taskspb.HttpMethod_GET, Body: []byte("x")}, nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a GET task with a body = %v; want InvalidArgument", err)
	}
	// Scheduled an hour out so nothing dispatches it during the test.
	later := &taskspb.HttpRequest{Url: "http://127.0.0.1:1/", HttpMethod: taskspb.HttpMethod_POST}
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{DispatchDeadline: durationpb.New(2 * time.Minute),
		ScheduleTime: timestamppb.New(time.Now().Add(time.Hour)), MessageType: &taskspb.Task_HttpRequest{HttpRequest: later}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName()})
	if err != nil || got.GetDispatchDeadline().AsDuration() != 2*time.Minute {
		t.Errorf("GetTask dispatch_deadline = %v (%v); want 2m", got.GetDispatchDeadline(), err)
	}

	named := location(h) + "/queues/compat-retry-duration"
	if _, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h), Queue: &taskspb.Queue{Name: named,
		RetryConfig: &taskspb.RetryConfig{MaxRetryDuration: durationpb.New(90 * time.Second)}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(ctx, &taskspb.DeleteQueueRequest{Name: named}) })
	if rq, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: named}); err != nil || rq.GetRetryConfig().GetMaxRetryDuration().AsDuration() != 90*time.Second {
		t.Errorf("GetQueue max_retry_duration = %v (%v); want 90s", rq.GetRetryConfig().GetMaxRetryDuration(), err)
	}
	if _, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h), Queue: &taskspb.Queue{Name: location(h) + "/queues/compat-logging",
		StackdriverLoggingConfig: &taskspb.StackdriverLoggingConfig{SamplingRatio: 1}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateQueue with stackdriver_logging_config = %v; want Unimplemented", err)
	}
	it := c.ListQueues(ctx, &taskspb.ListQueuesRequest{Parent: location(h), Filter: "state: PAUSED"})
	if _, err := it.Next(); status.Code(err) != codes.Unimplemented {
		t.Errorf("ListQueues with a filter = %v; want Unimplemented", err)
	}
}

// TestTasksPurgeQueue: PurgeQueue removes every task in the queue and no
// other queue's, keeps the queue and its state, and reports purge_time; a
// missing queue is NOT_FOUND.
// covers: google.cloud.tasks.v2.CloudTasks/PurgeQueue
func TestTasksPurgeQueue(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "purge-queue")
	other := queue(t, h, c, "purge-other")
	// Paused, so no task is dispatched away before the purge.
	for _, n := range []string{q, other} {
		if _, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(parent string) int {
		t.Helper()
		n := 0
		it := c.ListTasks(ctx, &taskspb.ListTasksRequest{Parent: parent})
		for {
			_, err := it.Next()
			if err == iterator.Done {
				return n
			}
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			n++
		}
	}
	add := func(parent string) {
		t.Helper()
		if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: parent, Task: &taskspb.Task{
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://127.0.0.1:1/never"}}}}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	for range 3 {
		add(q)
	}
	add(other)
	if n := count(q); n != 3 {
		t.Fatalf("tasks before purge = %d, want 3", n)
	}

	before := time.Now().Add(-time.Minute)
	got, err := c.PurgeQueue(ctx, &taskspb.PurgeQueueRequest{Name: q})
	if err != nil {
		t.Fatalf("PurgeQueue: %v", err)
	}
	if got.GetName() != q || got.GetState() != taskspb.Queue_PAUSED {
		t.Errorf("PurgeQueue returned %s in state %v; want %s, still PAUSED", got.GetName(), got.GetState(), q)
	}
	if pt := got.GetPurgeTime(); pt == nil || pt.AsTime().Before(before) {
		t.Errorf("purge_time = %v; want the time of the purge", pt)
	}
	if n := count(q); n != 0 {
		t.Errorf("tasks after purge = %d, want 0", n)
	}
	if n := count(other); n != 1 {
		t.Errorf("the other queue's tasks after purge = %d, want 1", n)
	}
	if read, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); err != nil || read.GetPurgeTime() == nil {
		t.Errorf("GetQueue after purge = %v, %v; want the queue with its purge_time", read, err)
	}
	// The queue still takes tasks.
	add(q)
	if n := count(q); n != 1 {
		t.Errorf("tasks created after the purge = %d, want 1", n)
	}

	if _, err := c.PurgeQueue(ctx, &taskspb.PurgeQueueRequest{Name: location(h) + "/queues/no-such-queue"}); status.Code(err) != codes.NotFound {
		t.Errorf("PurgeQueue on a missing queue = %v, want NotFound", err)
	}
}
