package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newTasksEditProvider(t *testing.T) (tasksProvider, string) {
	t.Helper()
	p := tasksProvider{svc: &tasksService{store: tasks.NewStore(store.NewMemory())}}
	name, err := p.Create(context.Background(), "edit-proj", map[string]string{"name": "edit-q", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	return p, name
}

// submittedValues is what the browser submits for a form: every field's default
// except the immutable ones.
func submittedValues(form *console.EditForm) map[string]string {
	out := map[string]string{}
	for _, f := range form.Fields {
		if !f.Immutable {
			out[f.Name] = f.Default
		}
	}
	return out
}

// The queue page carries Edit queue, prefilled from the stored queue, with
// the burst size shown and not submitted; saving it changes what the API's
// GetQueue reads, and an out-of-range value is refused with the API's own
// message (#784).
func TestTasksQueueEditThroughUpdateQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)

	d, err := p.Detail(ctx, "edit-proj", []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("queue detail offers no edit form: %v %+v", err, d)
	}
	if d.Edit.Label != "Edit queue" {
		t.Errorf("edit label = %q", d.Edit.Label)
	}
	values := submittedValues(d.Edit)
	want := map[string]string{
		"maxDispatchesPerSecond": "500", "maxConcurrentDispatches": "1000", "maxAttempts": "100",
		"minBackoff": "100ms", "maxBackoff": "1h0m0s", "maxDoublings": "16", "maxRetryDuration": "",
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s prefilled as %q, want %q", k, values[k], v)
		}
	}
	if _, ok := values["maxBurstSize"]; ok {
		t.Error("max burst size is submitted; it is output only")
	}

	values["maxDispatchesPerSecond"] = "2.5"
	values["maxConcurrentDispatches"] = "7"
	values["maxAttempts"] = "-1"
	values["minBackoff"] = "2s"
	values["maxBackoff"] = "30s"
	values["maxDoublings"] = "3"
	values["maxRetryDuration"] = "10m"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	q, err := p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	rl, rc := q.GetRateLimits(), q.GetRetryConfig()
	if rl.GetMaxDispatchesPerSecond() != 2.5 || rl.GetMaxConcurrentDispatches() != 7 {
		t.Errorf("rate limits read back as %v", rl)
	}
	if rc.GetMaxAttempts() != -1 || rc.GetMinBackoff().AsDuration() != 2*time.Second ||
		rc.GetMaxBackoff().AsDuration() != 30*time.Second || rc.GetMaxDoublings() != 3 ||
		rc.GetMaxRetryDuration().AsDuration() != 10*time.Minute {
		t.Errorf("retry config read back as %v", rc)
	}

	// The form is prefilled from what was saved.
	d, _ = p.Detail(ctx, "edit-proj", []string{name})
	if got := submittedValues(d.Edit); got["maxDispatchesPerSecond"] != "2.5" || got["maxRetryDuration"] != "10m0s" {
		t.Errorf("the form after saving is prefilled with %v", got)
	}

	values["maxAttempts"] = "-2"
	err = p.Edit(ctx, "edit-proj", []string{name}, values)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "retry_config.max_attempts -2 must be -1") {
		t.Errorf("max attempts -2 refused with %v; want the API's INVALID_ARGUMENT", err)
	}
	values["maxAttempts"] = "3"
	values["maxDispatchesPerSecond"] = "fast"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err == nil || !strings.Contains(err.Error(), "Max dispatches per second") {
		t.Errorf("a rate that is not a number refused with %v", err)
	}
	values["maxDispatchesPerSecond"] = "1"
	if err := p.Edit(ctx, "other-proj", []string{name}, values); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an edit of another project's queue = %v; want refused", err)
	}
	if err := p.Edit(ctx, "edit-proj", []string{name, name + "/tasks/x"}, values); err == nil {
		t.Error("a task was accepted as editable")
	}

	// A queue deleted meanwhile is reported, not made again by UpdateQueue.
	if err := p.Delete(ctx, "edit-proj", name); err != nil {
		t.Fatal(err)
	}
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); status.Code(err) != codes.NotFound {
		t.Errorf("editing a deleted queue = %v; want NOT_FOUND", err)
	}
	if _, err := p.svc.Store().GetQueue(name); err == nil {
		t.Error("editing a deleted queue created it again")
	}
}

// Create task is offered on a queue's page and not on a task's, and creates
// through CreateTask the task the form describes; what CreateTask refuses is
// refused with its message (#784).
func TestTasksCreateTaskFromTheQueuePage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)

	actions := p.DetailActions(ctx, "edit-proj", []string{name})
	if len(actions) != 1 || actions[0].ID != "createtask" || len(actions[0].Fields) == 0 {
		t.Fatalf("queue page actions = %+v; want Create task with fields", actions)
	}

	later := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	err := p.ActAt(ctx, "edit-proj", []string{name}, "createtask", map[string]string{
		"url": "http://127.0.0.1:9/hook", "httpMethod": "PUT",
		"headers": `{"X-Kind":"console"}`, "body": "hello",
		"scheduleTime": later.Format(time.RFC3339), "taskId": "console-task",
	})
	if err != nil {
		t.Fatalf("createtask: %v", err)
	}
	task, err := p.api().GetTask(ctx, &cloudtaskspb.GetTaskRequest{Name: name + "/tasks/console-task"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	hr := task.GetHttpRequest()
	if hr.GetUrl() != "http://127.0.0.1:9/hook" || hr.GetHttpMethod() != cloudtaskspb.HttpMethod_PUT ||
		hr.GetHeaders()["X-Kind"] != "console" || string(hr.GetBody()) != "hello" ||
		!task.GetScheduleTime().AsTime().Equal(later) {
		t.Errorf("the task reads back as %v", task)
	}
	if acts := p.DetailActions(ctx, "edit-proj", []string{name, task.GetName()}); len(acts) != 1 || acts[0].ID != "deletetask" {
		t.Errorf("a task's page offers %+v; want Delete task only", acts)
	}

	// Refused by CreateTask, with its message.
	err = p.ActAt(ctx, "edit-proj", []string{name}, "createtask", map[string]string{
		"url": "http://127.0.0.1:9/hook", "httpMethod": "GET", "body": "not allowed",
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "httpRequest.body must be empty") {
		t.Errorf("a GET with a body = %v; want CreateTask's INVALID_ARGUMENT", err)
	}
	err = p.ActAt(ctx, "edit-proj", []string{name}, "createtask", map[string]string{
		"url": "http://127.0.0.1:9/hook", "httpMethod": "POST", "taskId": "console-task",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("a duplicate task ID = %v; want ALREADY_EXISTS", err)
	}
	for _, bad := range []map[string]string{
		{"url": "http://127.0.0.1:9/", "httpMethod": "FETCH"},
		{"url": "http://127.0.0.1:9/", "httpMethod": "POST", "scheduleTime": "tomorrow"},
	} {
		if err := p.ActAt(ctx, "edit-proj", []string{name}, "createtask", bad); err == nil {
			t.Errorf("createtask with %v was accepted", bad)
		}
	}
}
