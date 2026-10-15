//go:build compat

package compat

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/UpdateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue
//
// TestTasksQueueHTTPTarget (#1282): through the official client, a queue's
// http_target is kept, read back, and applied to every dispatch of its
// tasks, while each task keeps its own httpRequest:
//
//   - in ALWAYS mode, uri_override's scheme, host, port and path replace
//     the task's, http_method replaces its method, and header_overrides
//     replace its headers of the same name;
//   - UpdateQueue with the mask http_target switches to IF_NOT_EXISTS: a
//     task with its own url is sent there, and a task with none, which the
//     queue's host makes creatable, to the override;
//   - a negative port is INVALID_ARGUMENT.
func TestTasksQueueHTTPTarget(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	type hit struct{ method, path, header string }
	got := make(chan hit, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- hit{r.Method, r.URL.RequestURI(), r.Header.Get("X-Who")}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.ParseInt(u.Port(), 10, 64)
	next := func(what string) hit {
		t.Helper()
		select {
		case v := <-got:
			return v
		case <-time.After(15 * time.Second):
			t.Fatalf("%s was not delivered", what)
			return hit{}
		}
	}

	name := location(h) + "/queues/compat-http-target"
	target := &cloudtaskspb.HttpTarget{
		UriOverride: &cloudtaskspb.UriOverride{
			Scheme: cloudtaskspb.UriOverride_HTTP.Enum(), Host: proto.String(u.Hostname()), Port: proto.Int64(port),
			PathOverride: &cloudtaskspb.PathOverride{Path: "/from-queue"},
		},
		HttpMethod:      cloudtaskspb.HttpMethod_PUT,
		HeaderOverrides: []*cloudtaskspb.HttpTarget_HeaderOverride{{Header: &cloudtaskspb.HttpTarget_Header{Key: "X-Who", Value: "queue"}}},
	}
	q, err := c.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: location(h),
		Queue: &cloudtaskspb.Queue{Name: name, HttpTarget: target}})
	if err != nil {
		t.Fatalf("CreateQueue with http_target: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(ctx, &cloudtaskspb.DeleteQueueRequest{Name: name}) })
	if !proto.Equal(q.GetHttpTarget(), target) {
		t.Errorf("CreateQueue returned http_target %v, want %v", q.GetHttpTarget(), target)
	}
	if g, err := c.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name}); err != nil || !proto.Equal(g.GetHttpTarget(), target) {
		t.Errorf("GetQueue http_target = %v, %v", g.GetHttpTarget(), err)
	}

	own := "http://task.invalid/own?x=1"
	task, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: name, Task: &cloudtaskspb.Task{
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
			Url: own, HttpMethod: cloudtaskspb.HttpMethod_POST, Headers: map[string]string{"X-Who": "task"}}}}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.GetHttpRequest().GetUrl() != own {
		t.Errorf("the task keeps url %q, want its own %q", task.GetHttpRequest().GetUrl(), own)
	}
	if v := next("the ALWAYS task"); v != (hit{"PUT", "/from-queue?x=1", "queue"}) {
		t.Errorf("ALWAYS dispatch = %+v; want PUT /from-queue?x=1 with X-Who: queue", v)
	}

	target.UriOverride.UriOverrideEnforceMode = cloudtaskspb.UriOverride_IF_NOT_EXISTS
	target.HeaderOverrides = nil
	if _, err := c.UpdateQueue(ctx, &cloudtaskspb.UpdateQueueRequest{Queue: &cloudtaskspb.Queue{Name: name, HttpTarget: target},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"http_target"}}}); err != nil {
		t.Fatalf("UpdateQueue http_target: %v", err)
	}
	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: name, Task: &cloudtaskspb.Task{
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{Url: srv.URL + "/mine"}}}}); err != nil {
		t.Fatal(err)
	}
	if v := next("the IF_NOT_EXISTS task with a url"); v.path != "/mine" || v.method != "PUT" {
		t.Errorf("IF_NOT_EXISTS dispatch of a task with a url = %+v; want PUT /mine", v)
	}
	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: name, Task: &cloudtaskspb.Task{
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{}}}}); err != nil {
		t.Fatalf("CreateTask with no url on a queue whose http_target names a host: %v", err)
	}
	if v := next("the IF_NOT_EXISTS task with no url"); v.path != "/from-queue" {
		t.Errorf("IF_NOT_EXISTS dispatch of a task with no url = %+v; want /from-queue", v)
	}

	bad := &cloudtaskspb.HttpTarget{UriOverride: &cloudtaskspb.UriOverride{Port: proto.Int64(-1)}}
	if _, err := c.UpdateQueue(ctx, &cloudtaskspb.UpdateQueueRequest{Queue: &cloudtaskspb.Queue{Name: name, HttpTarget: bad},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"http_target"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateQueue with a negative port = %v; want INVALID_ARGUMENT", err)
	}
}
