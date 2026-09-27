//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cloud Tasks across stop and up (#596), the pattern of #418: in persistent
// mode its queues and tasks are a durable store under the state directory,
// in ephemeral mode they are memory. TestTasksRestartSetup runs alone just
// before `stop`, and TestTasksAcrossRestart after each `up`.
const (
	envTasksProbe  = "CLOUDBURROW_TEST_TASKS_PROBE"
	envTasksExpect = "CLOUDBURROW_TEST_TASKS_EXPECT"
	tasksProbeBody = "written-before-stop"
)

// tasksProbe is what the setup leaves for the probe, in the file envTasksProbe names.
type tasksProbe struct {
	Queue        string    `json:"queue"`
	Task         string    `json:"task"`
	ScheduleTime time.Time `json:"scheduleTime"`
}

// TestTasksRestartSetup leaves a paused queue holding one HTTP task,
// scheduled a day ahead so nothing dispatches it, for TestTasksAcrossRestart.
func TestTasksRestartSetup(t *testing.T) {
	path := os.Getenv(envTasksProbe)
	if path == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envTasksProbe)
	}
	h := New(t)
	ctx := h.Context()
	c := tasksClient(t, h)
	q, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: location(h),
		Queue: &taskspb.Queue{Name: location(h) + "/queues/restart-probe"}})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q.GetName(), Task: &taskspb.Task{
		ScheduleTime: timestamppb.New(when),
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
			Url: "http://127.0.0.1:1/never", HttpMethod: taskspb.HttpMethod_POST, Body: []byte(tasksProbeBody)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: q.GetName()}); err != nil || p.GetState() != taskspb.Queue_PAUSED {
		t.Fatalf("PauseQueue = %v, %v", p.GetState(), err)
	}
	b, _ := json.Marshal(tasksProbe{Queue: q.GetName(), Task: task.GetName(), ScheduleTime: when})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTasksAcrossRestart: after stop and up in persistent mode the queue is
// still paused and its task reads back unchanged; in ephemeral mode the queue
// is gone.
func TestTasksAcrossRestart(t *testing.T) {
	expect := os.Getenv(envTasksExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envTasksExpect)
	}
	b, err := os.ReadFile(os.Getenv(envTasksProbe))
	if err != nil {
		t.Fatalf("the probe TestTasksRestartSetup left: %v", err)
	}
	var p tasksProbe
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	h := New(t)
	ctx := h.Context()
	c := tasksClient(t, h)
	switch expect {
	case "present":
		q, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: p.Queue})
		if err != nil {
			t.Fatalf("persistent mode after stop/up: GetQueue = %v; want the queue created before stop", err)
		}
		if q.GetState() != taskspb.Queue_PAUSED {
			t.Errorf("queue state = %v, want PAUSED", q.GetState())
		}
		task, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: p.Task, ResponseView: taskspb.Task_FULL})
		if err != nil {
			t.Fatalf("persistent mode after stop/up: GetTask = %v; want the task created before stop", err)
		}
		if got := string(task.GetHttpRequest().GetBody()); got != tasksProbeBody {
			t.Errorf("task body = %q, want %q", got, tasksProbeBody)
		}
		if got := task.GetScheduleTime().AsTime(); !got.Equal(p.ScheduleTime) {
			t.Errorf("task schedule time = %v, want %v", got, p.ScheduleTime)
		}
	case "absent":
		if _, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: p.Queue}); status.Code(err) != codes.NotFound {
			t.Fatalf("ephemeral mode after stop/up: GetQueue = %v; want NotFound", err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envTasksExpect, expect)
	}
}
