//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvAppEngineURL is the --appengine-url the instance under test was started
// with (#1506): the test serves the stand-in App Engine app there.
const EnvAppEngineURL = "CLOUDBURROW_TEST_APPENGINE_URL"

// skipWithAppEngine skips a test of the no-App-Engine 404s on an instance
// that delivers App Engine targets to a stand-in.
func skipWithAppEngine(t *testing.T) {
	if os.Getenv(EnvAppEngineURL) != "" {
		t.Skip("the instance delivers App Engine targets to " + EnvAppEngineURL + " (#1506)")
	}
}

// appEngineReq is one request the stand-in app got.
type appEngineReq struct {
	method, uri, host, body string
	header                  http.Header
}

// standInApp serves the stand-in App Engine app at the instance's
// --appengine-url, answering each request with the next of codes (200 when
// they run out), and returns what it receives.
func standInApp(t *testing.T, codes ...int) <-chan appEngineReq {
	t.Helper()
	raw := os.Getenv(EnvAppEngineURL)
	if raw == "" {
		t.Skip(EnvAppEngineURL + " is not set: the instance was not started with --appengine-url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s: %v", EnvAppEngineURL, err)
	}
	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		t.Fatalf("listen on %s: %v", u.Host, err)
	}
	got := make(chan appEngineReq, 16)
	next := make(chan int, len(codes))
	for _, c := range codes {
		next <- c
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- appEngineReq{method: r.Method, uri: r.URL.RequestURI(), host: r.Host, body: string(b), header: r.Header.Clone()}
		code := http.StatusOK
		select {
		case code = <-next:
		default:
		}
		w.WriteHeader(code)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return got
}

func nextAppEngineReq(t *testing.T, got <-chan appEngineReq) appEngineReq {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the stand-in App Engine app got no request")
		return appEngineReq{}
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/GetTask, google.cloud.tasks.v2.CloudTasks/UpdateQueue
//
// TestTasksAppEngineDelivery (#1506), with cloud.google.com/go/cloudtasks/apiv2
// on an instance started with --appengine-url: an appEngineHttpRequest task
// reaches the stand-in app at its relative_uri, with the method, body and
// headers it was given, Host the queue override's
// (v1.worker.PROJECT.appspot.com), and X-AppEngine-QueueName, -TaskName,
// -TaskRetryCount, -TaskExecutionCount and -TaskETA; its X-AppEngine-*
// header is replaced. The app's 500 is retried, with
// X-AppEngine-TaskPreviousResponse 500, and its 200 completes the task.
func TestTasksAppEngineDelivery(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	got := standInApp(t, http.StatusInternalServerError)
	ctx := h.Context()
	q := queue(t, h, c, fmt.Sprintf("aed-%d", time.Now().UnixNano()))
	if _, err := c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{
		Queue: &taskspb.Queue{Name: q, AppEngineRoutingOverride: &taskspb.AppEngineRouting{Service: "worker", Version: "v1"},
			RetryConfig: &taskspb.RetryConfig{MaxAttempts: 3, MinBackoff: durationpb.New(time.Second), MaxBackoff: durationpb.New(time.Second)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"app_engine_routing_override", "retry_config"}}}); err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{
		Name: q + "/tasks/ae-deliver",
		MessageType: &taskspb.Task_AppEngineHttpRequest{AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{
			HttpMethod: taskspb.HttpMethod_PUT, RelativeUri: "/work?id=7", Body: []byte(`{"n":7}`),
			Headers: map[string]string{"Content-Type": "application/json", "X-Job": "7", "X-AppEngine-TaskName": "forged"}}}}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r := nextAppEngineReq(t, got)
	if r.method != "PUT" || r.uri != "/work?id=7" || r.body != `{"n":7}` || r.host != "v1.worker."+h.Project()+".appspot.com" {
		t.Errorf("the first attempt = %s %s Host %s body %q", r.method, r.uri, r.host, r.body)
	}
	for k, want := range map[string]string{"Content-Type": "application/json", "X-Job": "7", "X-AppEngine-TaskName": "ae-deliver",
		"X-AppEngine-QueueName":      q[strings.LastIndex(q, "/")+1:],
		"X-AppEngine-TaskRetryCount": "0", "X-AppEngine-TaskExecutionCount": "0",
		"User-Agent": "AppEngine-Google; (+http://code.google.com/appengine)"} {
		if v := r.header.Get(k); v != want {
			t.Errorf("first attempt %s = %q, want %q", k, v, want)
		}
	}
	if r.header.Get("X-AppEngine-TaskETA") == "" {
		t.Error("no X-AppEngine-TaskETA")
	}
	r = nextAppEngineReq(t, got)
	if r.header.Get("X-AppEngine-TaskRetryCount") != "1" || r.header.Get("X-AppEngine-TaskPreviousResponse") != "500" {
		t.Errorf("the retry's headers = %v", r.header)
	}
	for d := time.Now().Add(20 * time.Second); time.Now().Before(d); time.Sleep(200 * time.Millisecond) {
		if _, err = c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName()}); status.Code(err) == codes.NotFound {
			return
		}
	}
	t.Errorf("GetTask after the app's 200: %v; want NotFound", err)
}

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/RunJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
//
// TestSchedulerAppEngineDelivery (#1506), with
// cloud.google.com/go/scheduler/apiv1 on an instance started with
// --appengine-url: RunJob of an app_engine_http_target job reaches the
// stand-in app at its relative_uri with Host v2.cron.PROJECT.appspot.com,
// X-CloudScheduler, X-CloudScheduler-JobName and the AppEngine-Google
// User-Agent; Job.status is OK after the app's 200.
func TestSchedulerAppEngineDelivery(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_SCHEDULER", EnvScheduler)
	c := schedulerClient(t, h)
	got := standInApp(t)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/us-central1"
	job, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/ae-deliver", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			RelativeUri: "/cron/tick", Body: []byte("t"),
			AppEngineRouting: &schedulerpb.AppEngineRouting{Service: "cron", Version: "v2"}}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	r := nextAppEngineReq(t, got)
	if r.method != "POST" || r.uri != "/cron/tick" || r.body != "t" || r.host != "v2.cron."+h.Project()+".appspot.com" ||
		r.header.Get("X-CloudScheduler") != "true" || r.header.Get("X-CloudScheduler-JobName") != "ae-deliver" ||
		r.header.Get("User-Agent") != "AppEngine-Google; (+http://code.google.com/appengine)" ||
		r.header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("the job's request = %+v", r)
	}
	var j *schedulerpb.Job
	for d := time.Now().Add(15 * time.Second); time.Now().Before(d); time.Sleep(100 * time.Millisecond) {
		if j, err = c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()}); err != nil || j.GetLastAttemptTime() != nil {
			break
		}
	}
	if err != nil || j.GetStatus().GetCode() != int32(codes.OK) {
		t.Errorf("status after the app's 200 = %v, %v", j.GetStatus(), err)
	}
}
