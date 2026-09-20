//go:build compat

package compat

import (
	"fmt"
	"testing"

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
