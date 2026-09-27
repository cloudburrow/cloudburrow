package tasks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Tasks over JSON (#591): the official client's REST transport creates a
// queue by the hand-written route, then creates, gets, lists and deletes a
// task through the transcoder, over the same GRPCServer.
func TestRESTTasksWithTheOfficialRESTClient(t *testing.T) {
	r := rest.NewRouter()
	NewRESTServer(NewStore(store.NewMemory())).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	ctx := context.Background()
	c, err := cloudtasks.NewRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const loc = "projects/demo-proj/locations/us-central1"
	q, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: loc + "/queues/rest"}})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	// Paused, so the task stays put for the reads.
	if _, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: q.GetName()}); err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q.GetName(), Task: &taskspb.Task{
		Name: q.GetName() + "/tasks/t1",
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
			Url: "http://127.0.0.1:1/", HttpMethod: taskspb.HttpMethod_PUT, Body: []byte("hi"),
			Headers: map[string]string{"X-Test": "1"}}}}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.GetHttpRequest().GetHttpMethod() != taskspb.HttpMethod_PUT || task.GetScheduleTime() == nil {
		t.Errorf("created task = %v", task)
	}
	if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q.GetName(), Task: &taskspb.Task{
		Name:        task.GetName(),
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://127.0.0.1:1/"}}}}); httpCode(err) != http.StatusConflict {
		t.Errorf("CreateTask again: %v; want 409", err)
	}
	got, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName(), ResponseView: taskspb.Task_FULL})
	if err != nil || string(got.GetHttpRequest().GetBody()) != "hi" || got.GetHttpRequest().GetHeaders()["X-Test"] != "1" {
		t.Fatalf("GetTask = %v, %v", got, err)
	}
	it := c.ListTasks(ctx, &taskspb.ListTasksRequest{Parent: q.GetName(), PageSize: 10})
	if l, err := it.Next(); err != nil || l.GetName() != task.GetName() {
		t.Errorf("ListTasks = %v, %v", l, err)
	} else if _, err := it.Next(); err != iterator.Done {
		t.Errorf("ListTasks has more than one task: %v", err)
	}
	if err := c.DeleteTask(ctx, &taskspb.DeleteTaskRequest{Name: task.GetName()}); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName()}); httpCode(err) != http.StatusNotFound {
		t.Errorf("GetTask after delete: %v; want 404", err)
	}
	if _, err := c.RunTask(ctx, &taskspb.RunTaskRequest{Name: task.GetName()}); httpCode(err) != http.StatusNotImplemented {
		t.Errorf("RunTask: %v; want 501", err)
	}
}

// Task paths take the transcoder's query policy; queue paths keep theirs.
func TestRESTTaskPathsRefuseUnknownParameters(t *testing.T) {
	r := rest.NewRouter()
	NewRESTServer(NewStore(store.NewMemory())).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	const q = "/v2/projects/demo-proj/locations/us-central1/queues/p"
	for _, c := range []struct {
		method, path, body string
		code               int
		has                string
	}{
		{"POST", "/v2/projects/demo-proj/locations/us-central1/queues", `{"name":"projects/demo-proj/locations/us-central1/queues/p"}`, 200, ""},
		{"GET", q + "?noSuchParameter=1", "", 200, `"name"`},
		{"GET", q + "/tasks?noSuchParameter=1", "", 400, "noSuchParameter"},
		{"GET", q + "/tasks?pageSize=5&responseView=FULL", "", 200, "{}"},
		{"POST", q + "/tasks", `{"task":{"httpRequest":{"url":"http://127.0.0.1:1/"}},"noSuchField":1}`, 400, "INVALID_ARGUMENT"},
		{"GET", "/v2/projects/demo-proj/locations/us-central1/queues/absent/tasks", "", 404, "NOT_FOUND"},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.code || !strings.Contains(string(b), c.has) {
			t.Errorf("%s %s: %d %s; want %d containing %q", c.method, c.path, resp.StatusCode, b, c.code, c.has)
		}
	}
}

// httpCode is a REST client error's HTTP status, or 0.
func httpCode(err error) int {
	var g *googleapi.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return 0
}
