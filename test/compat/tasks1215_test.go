//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestTasksRunTaskDispatchesNow (#1215). A task scheduled an hour away, on a
// paused queue, is delivered once RunTask is called through the official
// client, which returns the task; once it has succeeded it is gone, and
// RunTask on it is NOT_FOUND, as documented.
// covers: google.cloud.tasks.v2.CloudTasks/RunTask
func TestTasksRunTaskDispatchesNow(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "run-now")
	tg := newTarget(t, http.StatusOK)
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{
		Name:         q + "/tasks/later",
		ScheduleTime: timestamppb.New(time.Now().Add(time.Hour)),
		MessageType:  &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL + "/run"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: q}); err != nil {
		t.Fatal(err)
	}
	got, err := c.RunTask(ctx, &taskspb.RunTaskRequest{Name: task.GetName()})
	if err != nil || got.GetName() != task.GetName() {
		t.Fatalf("RunTask = %v, %v", got, err)
	}
	if d := tg.await(t, 1, 10*time.Second); d[0].path != "/run" {
		t.Errorf("delivered to %s", d[0].path)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := c.RunTask(ctx, &taskspb.RunTaskRequest{Name: task.GetName()})
		if status.Code(err) == codes.NotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("RunTask on a task that succeeded = %v, want NotFound", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestTasksBatchCreateAndDelete (#1215). The official client's LRO wrappers
// return BatchCreateTasks' tasks and its partial failures by index, and
// BatchDeleteTasks deletes what exists and reports the rest; when every
// delete fails the operation is ABORTED. More than 100 creates are refused.
// covers: google.cloud.tasks.v2.CloudTasks/BatchCreateTasks
// covers: google.cloud.tasks.v2.CloudTasks/BatchDeleteTasks
func TestTasksBatchCreateAndDelete(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "batch")
	req := func(id string) *taskspb.CreateTaskRequest {
		return &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{Name: q + "/tasks/" + id,
			ScheduleTime: timestamppb.New(time.Now().Add(time.Hour)),
			MessageType:  &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://127.0.0.1:1/never"}}}}
	}
	op, err := c.BatchCreateTasks(ctx, &taskspb.BatchCreateTasksRequest{Parent: q,
		Requests: []*taskspb.CreateTaskRequest{req("a"), req("b"), req("a")}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := op.Wait(ctx)
	if err != nil || len(resp.GetTasks()) != 2 {
		t.Fatalf("BatchCreateTasks Wait = %v, %v; want two tasks", resp, err)
	}
	meta, err := op.Metadata()
	if err != nil || meta.GetState() != taskspb.BatchCreateTasksMetadata_PARTIALLY_SUCCEEDED ||
		meta.GetFailedRequests()[2].GetCode() != int32(codes.AlreadyExists) {
		t.Errorf("BatchCreateTasks metadata = %v, %v; want request 2 ALREADY_EXISTS", meta, err)
	}
	many := make([]*taskspb.CreateTaskRequest, 101)
	for i := range many {
		many[i] = req("x")
	}
	if _, err := c.BatchCreateTasks(ctx, &taskspb.BatchCreateTasksRequest{Parent: q, Requests: many}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("101 creates = %v, want InvalidArgument", err)
	}

	dop, err := c.BatchDeleteTasks(ctx, &taskspb.BatchDeleteTasksRequest{Parent: q,
		Names: []string{q + "/tasks/a", q + "/tasks/b", q + "/tasks/none"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("BatchDeleteTasks Wait = %v", err)
	}
	if dmeta, _ := dop.Metadata(); dmeta.GetFailedRequests()[2].GetCode() != int32(codes.NotFound) || len(dmeta.GetFailedRequests()) != 1 {
		t.Errorf("BatchDeleteTasks metadata = %v; want request 2 NOT_FOUND", dmeta)
	}
	if _, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: q + "/tasks/a"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTask after batch delete = %v, want NotFound", err)
	}
	all, err := c.BatchDeleteTasks(ctx, &taskspb.BatchDeleteTasksRequest{Parent: q, Names: []string{q + "/tasks/none"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := all.Wait(ctx); status.Code(err) != codes.Aborted {
		t.Errorf("a batch whose every delete failed = %v, want Aborted", err)
	}
}

// TestTasksCmekConfig (#1215). The location's CMEK config has no key by
// default; UpdateCmekConfig stores one, GetCmekConfig returns it, and an
// empty key clears it. Nothing is encrypted with it.
// covers: google.cloud.tasks.v2.CloudTasks/GetCmekConfig
// covers: google.cloud.tasks.v2.CloudTasks/UpdateCmekConfig
func TestTasksCmekConfig(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	name := location(h) + "/cmekConfig"
	got, err := c.GetCmekConfig(ctx, &taskspb.GetCmekConfigRequest{Name: name})
	if err != nil || got.GetName() != name || got.GetKmsKey() != "" {
		t.Fatalf("GetCmekConfig = %v, %v; want no key", got, err)
	}
	key := location(h) + "/keyRings/ring/cryptoKeys/key"
	if _, err := c.UpdateCmekConfig(ctx, &taskspb.UpdateCmekConfigRequest{
		CmekConfig: &taskspb.CmekConfig{Name: name, KmsKey: key},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key"}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.UpdateCmekConfig(ctx, &taskspb.UpdateCmekConfigRequest{CmekConfig: &taskspb.CmekConfig{Name: name}})
	})
	if got, _ := c.GetCmekConfig(ctx, &taskspb.GetCmekConfigRequest{Name: name}); got.GetKmsKey() != key {
		t.Errorf("GetCmekConfig after update = %v", got)
	}
	if _, err := c.UpdateCmekConfig(ctx, &taskspb.UpdateCmekConfigRequest{
		CmekConfig: &taskspb.CmekConfig{Name: name, KmsKey: "not-a-key"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed key = %v, want InvalidArgument", err)
	}
}

// TestConsoleTasksRunNow (#1215). A task the official client created an
// hour out is offered Run now on its page; the console's Run now, which is
// RunTask, delivers it to a test target at once.
func TestConsoleTasksRunNow(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "console-run-now")
	tg := newTarget(t, http.StatusOK)
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{
		Name:         q + "/tasks/console-later",
		ScheduleTime: timestamppb.New(time.Now().Add(time.Hour)),
		MessageType:  &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL + "/console"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/tasks?project="+h.Project()+"&name="+q+"&name="+task.GetName(), "")
	if code != http.StatusOK || !strings.Contains(body, `"runtask"`) {
		t.Errorf("the task page = %d and offers no Run now: %s", code, body)
	}
	req, _ := json.Marshal(map[string]any{"Path": []string{q, task.GetName()}, "Action": "runtask"})
	if code, body := consoleDo(t, addr, http.MethodPost, "/api/actions/tasks?project="+h.Project(), string(req)); code != http.StatusOK {
		t.Fatalf("Run now = %d %s", code, body)
	}
	if d := tg.await(t, 1, 10*time.Second); d[0].path != "/console" {
		t.Errorf("delivered to %s", d[0].path)
	}
}
