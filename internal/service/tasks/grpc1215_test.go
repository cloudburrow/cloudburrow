package tasks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// RunTask dispatches a task scheduled for later, on a paused queue, on the
// worker's next pass, and a task that is gone is NOT_FOUND (#1215).
func TestRunTaskDispatchesNowEvenWhenPaused(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	clock := sched.NewFakeClock(time.Now())
	w := NewWorker(s, NewDispatcher(s, srv.Client(), clock), clock, time.Second)
	later := Task{Name: TaskName(queueA, "later"), HTTPRequest: &HTTPRequest{URL: srv.URL}, ScheduleTime: time.Now().Add(time.Hour)}
	if _, err := s.CreateTask(later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetQueueState(queueA, StatePaused); err != nil {
		t.Fatal(err)
	}
	w.dispatchDue(context.Background())
	if hits.Load() != 0 {
		t.Fatal("a task an hour away on a paused queue was dispatched before RunTask")
	}
	g := NewGRPCServer(s)
	got, err := g.RunTask(context.Background(), &taskspb.RunTaskRequest{Name: later.Name})
	if err != nil || got.GetName() != later.Name {
		t.Fatalf("RunTask = %v, %v", got, err)
	}
	w.dispatchDue(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("after RunTask the target saw %d requests, want 1", hits.Load())
	}
	if _, err := s.GetTask(later.Name); err == nil {
		t.Error("a task whose forced run succeeded is still there")
	}
	if _, err := g.RunTask(context.Background(), &taskspb.RunTaskRequest{Name: later.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("RunTask on a task that succeeded = %v, want NotFound", err)
	}
}

// A RunTask that arrives while an attempt is out survives the attempt's
// write-back.
func TestRunTaskSurvivesAnInFlightWriteBack(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	task := Task{Name: TaskName(queueA, "t"), HTTPRequest: &HTTPRequest{URL: "http://x"}}
	if _, err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	stale, _ := s.GetTask(task.Name)
	if _, err := s.RunTask(task.Name); err != nil {
		t.Fatal(err)
	}
	stale.DispatchCount = 1
	if err := s.UpdateTask(stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(task.Name); got.RunRequested.IsZero() || got.DispatchCount != 1 {
		t.Errorf("after a stale write-back the task is %+v; want its forced run kept", got)
	}
}

func batchTask(id string) *taskspb.CreateTaskRequest {
	return &taskspb.CreateTaskRequest{Parent: queueA, Task: &taskspb.Task{
		Name:        TaskName(queueA, id),
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://x"}},
	}}
}

// BatchCreateTasks and BatchDeleteTasks are non-atomic, report failures by
// index, enforce their limits, and replay a request_id.
func TestBatchCreateAndDeleteTasks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	g := NewGRPCServer(s)
	cc := context.Background()

	op, err := g.BatchCreateTasks(cc, &taskspb.BatchCreateTasksRequest{Parent: queueA, RequestId: "r1",
		Requests: []*taskspb.CreateTaskRequest{batchTask("a"), batchTask("b"), batchTask("a")}})
	if err != nil || !op.GetDone() {
		t.Fatalf("BatchCreateTasks = %v, %v", op, err)
	}
	var meta taskspb.BatchCreateTasksMetadata
	if err := op.GetMetadata().UnmarshalTo(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.GetState() != taskspb.BatchCreateTasksMetadata_PARTIALLY_SUCCEEDED || len(meta.GetFailedRequests()) != 1 ||
		meta.GetFailedRequests()[2].GetCode() != int32(codes.AlreadyExists) {
		t.Errorf("metadata = %v; want request 2 failed ALREADY_EXISTS", &meta)
	}
	var resp taskspb.BatchCreateTasksResponse
	if err := op.GetResponse().UnmarshalTo(&resp); err != nil || len(resp.GetTasks()) != 2 {
		t.Errorf("response = %v, %v; want 2 tasks", &resp, err)
	}
	again, _ := g.BatchCreateTasks(cc, &taskspb.BatchCreateTasksRequest{Parent: queueA, RequestId: "r1",
		Requests: []*taskspb.CreateTaskRequest{batchTask("c")}})
	if again.GetName() != op.GetName() {
		t.Errorf("a repeated request_id ran again as %s", again.GetName())
	}
	if _, err := s.GetTask(TaskName(queueA, "c")); err == nil {
		t.Error("a repeated request_id created a task")
	}

	many := make([]*taskspb.CreateTaskRequest, MaxBatchCreate+1)
	for i := range many {
		many[i] = batchTask("x")
	}
	if _, err := g.BatchCreateTasks(cc, &taskspb.BatchCreateTasksRequest{Parent: queueA, Requests: many}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("101 requests = %v, want InvalidArgument", err)
	}
	other := batchTask("y")
	other.Parent = strings.Replace(queueA, "/queues/", "/queues/b-", 1)
	if _, err := g.BatchCreateTasks(cc, &taskspb.BatchCreateTasksRequest{Parent: queueA,
		Requests: []*taskspb.CreateTaskRequest{other}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a request for another queue = %v, want InvalidArgument", err)
	}

	del, err := g.BatchDeleteTasks(cc, &taskspb.BatchDeleteTasksRequest{Parent: queueA,
		Names: []string{TaskName(queueA, "a"), TaskName(queueA, "nope")}})
	if err != nil {
		t.Fatal(err)
	}
	var dmeta taskspb.BatchDeleteTasksMetadata
	_ = del.GetMetadata().UnmarshalTo(&dmeta)
	if dmeta.GetState() != taskspb.BatchDeleteTasksMetadata_PARTIALLY_SUCCEEDED || dmeta.GetFailedRequests()[1].GetCode() != int32(codes.NotFound) {
		t.Errorf("delete metadata = %v; want request 1 NOT_FOUND", &dmeta)
	}
	if _, err := s.GetTask(TaskName(queueA, "a")); err == nil {
		t.Error("task a survived BatchDeleteTasks")
	}
	all, _ := g.BatchDeleteTasks(cc, &taskspb.BatchDeleteTasksRequest{Parent: queueA, Names: []string{TaskName(queueA, "nope")}})
	if all.GetError().GetCode() != int32(codes.Aborted) {
		t.Errorf("a batch whose every delete failed = %v, want ABORTED", all.GetResult())
	}
}

// The CMEK config is stored and returned, never used.
func TestCmekConfigIsStored(t *testing.T) {
	t.Parallel()
	g := NewGRPCServer(newStore(t))
	cc := context.Background()
	name := loc + "/cmekConfig"
	if got, err := g.GetCmekConfig(cc, &taskspb.GetCmekConfigRequest{Name: name}); err != nil || got.GetKmsKey() != "" {
		t.Fatalf("default GetCmekConfig = %v, %v; want no key", got, err)
	}
	key := loc + "/keyRings/r/cryptoKeys/k"
	if _, err := g.UpdateCmekConfig(cc, &taskspb.UpdateCmekConfigRequest{CmekConfig: &taskspb.CmekConfig{Name: name, KmsKey: key},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.GetCmekConfig(cc, &taskspb.GetCmekConfigRequest{Name: name}); got.GetKmsKey() != key {
		t.Errorf("GetCmekConfig after update = %v", got)
	}
	if _, err := g.UpdateCmekConfig(cc, &taskspb.UpdateCmekConfigRequest{CmekConfig: &taskspb.CmekConfig{Name: name, KmsKey: "bad"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed key = %v, want InvalidArgument", err)
	}
	if _, err := g.UpdateCmekConfig(cc, &taskspb.UpdateCmekConfigRequest{CmekConfig: &taskspb.CmekConfig{Name: name}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.GetCmekConfig(cc, &taskspb.GetCmekConfigRequest{Name: name}); got.GetKmsKey() != "" {
		t.Errorf("after clearing the key, GetCmekConfig = %v", got)
	}
}

// The JSON API serves the batch methods and the CMEK config by Google's
// bindings.
func TestREST1215Bindings(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	r := rest.NewRouter()
	NewRESTServer(s).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	for _, tc := range []struct{ method, path, body, want string }{
		{"POST", "/v2/" + queueA + "/tasks:batchCreate", `{"requests":[{"parent":"` + queueA + `","task":{"httpRequest":{"url":"http://x"}}}]}`, `"done":true`},
		{"POST", "/v2/" + queueA + "/tasks:batchDelete", `{"names":["` + queueA + `/tasks/none"]}`, `"code":10`},
		{"PATCH", "/v2/" + loc + "/cmekConfig?updateMask=kmsKey", `{"name":"` + loc + `/cmekConfig","kmsKey":"` + loc + `/keyRings/r/cryptoKeys/k"}`, "cryptoKeys/k"},
		{"GET", "/v2/" + loc + "/cmekConfig", "", "cryptoKeys/k"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), tc.want) {
			t.Errorf("%s %s = %d %s; want 200 containing %s", tc.method, tc.path, resp.StatusCode, b, tc.want)
		}
	}
}
