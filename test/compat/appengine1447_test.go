//go:build compat

package compat

import (
	"context"
	"fmt"
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

// App Engine targets (#1447). An instance has no App Engine, and Google
// documents no mapping from an App Engine service to anything else, so a
// task or job with one is stored as the API documents it, with
// AppEngineRouting.host computed from <project>.appspot.com, and every
// attempt fails as a request to an app that does not exist does: HTTP 404,
// NOT_FOUND with the reason.

// covers: google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/GetTask, google.cloud.tasks.v2.CloudTasks/UpdateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue
//
// TestTasksAppEngineTargets, with cloud.google.com/go/cloudtasks/apiv2: a
// queue's app_engine_routing_override reads back with its host; an
// appEngineHttpRequest task reads back as sent, its host the override's;
// its first attempt fails, last_attempt.response_status NOT_FOUND naming the
// host, dispatch_count and response_count 1; and once max_attempts (2) are
// spent the task is gone, as on Google.
func TestTasksAppEngineTargets(t *testing.T) {
	h := New(t)
	skipWithAppEngine(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	q := queue(t, h, c, fmt.Sprintf("ae-%d", time.Now().UnixNano()))
	project := h.Project()

	uq, err := c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{
		Queue: &taskspb.Queue{Name: q, AppEngineRoutingOverride: &taskspb.AppEngineRouting{Service: "worker", Version: "v1"},
			RetryConfig: &taskspb.RetryConfig{MaxAttempts: 2, MinBackoff: durationpb.New(time.Second), MaxBackoff: durationpb.New(time.Second)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"app_engine_routing_override", "retry_config"}}})
	if err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	wantHost := "v1.worker." + project + ".appspot.com"
	if o := uq.GetAppEngineRoutingOverride(); o.GetService() != "worker" || o.GetHost() != wantHost {
		t.Errorf("app_engine_routing_override = %v; want service worker, host %s", o, wantHost)
	}
	if g, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); err != nil || g.GetAppEngineRoutingOverride().GetHost() != wantHost {
		t.Errorf("GetQueue = %v, %v", g.GetAppEngineRoutingOverride(), err)
	}

	task, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{
		Name: q + "/tasks/ae-task",
		MessageType: &taskspb.Task_AppEngineHttpRequest{AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{
			HttpMethod: taskspb.HttpMethod_PUT, RelativeUri: "/work?id=1", Body: []byte("payload"),
			Headers:          map[string]string{"X-Job": "1"},
			AppEngineRouting: &taskspb.AppEngineRouting{Service: "ignored-by-override"},
		}}}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	ae := task.GetAppEngineHttpRequest()
	if ae.GetHttpMethod() != taskspb.HttpMethod_PUT || ae.GetRelativeUri() != "/work?id=1" ||
		ae.GetAppEngineRouting().GetHost() != wantHost {
		t.Errorf("created task = %v", task)
	}

	var got *taskspb.Task
	for d := time.Now().Add(20 * time.Second); time.Now().Before(d); time.Sleep(100 * time.Millisecond) {
		got, err = c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName(), ResponseView: taskspb.Task_FULL})
		if err != nil || got.GetLastAttempt().GetResponseStatus() != nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("GetTask after the first attempt: %v", err)
	}
	la := got.GetLastAttempt()
	if got.GetDispatchCount() < 1 || got.GetResponseCount() < 1 || got.GetFirstAttempt().GetDispatchTime() == nil ||
		la.GetResponseStatus().GetCode() != int32(codes.NotFound) || la.GetResponseTime() == nil ||
		!strings.Contains(la.GetResponseStatus().GetMessage(), wantHost+"/work?id=1") {
		t.Errorf("after an attempt: dispatch %d, response %d, first %v, last %v",
			got.GetDispatchCount(), got.GetResponseCount(), got.GetFirstAttempt(), la)
	}
	if string(got.GetAppEngineHttpRequest().GetBody()) != "payload" || got.GetAppEngineHttpRequest().GetHeaders()["X-Job"] != "1" {
		t.Errorf("the stored request = %v", got.GetAppEngineHttpRequest())
	}

	for d := time.Now().Add(20 * time.Second); time.Now().Before(d); time.Sleep(200 * time.Millisecond) {
		if _, err = c.GetTask(ctx, &taskspb.GetTaskRequest{Name: task.GetName()}); status.Code(err) == codes.NotFound {
			break
		}
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetTask after max_attempts: %v; want NotFound", err)
	}

	if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{
		MessageType: &taskspb.Task_AppEngineHttpRequest{AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{RelativeUri: "no-slash"}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("relative_uri without a leading / = %v; want InvalidArgument", err)
	}
}

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/RunJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
//
// TestSchedulerAppEngineTarget, with cloud.google.com/go/scheduler/apiv1:
// an app_engine_http_target job reads back with its routing's host, and
// RunJob's attempt sets Job.status to NOT_FOUND naming it.
func TestSchedulerAppEngineTarget(t *testing.T) {
	h := New(t)
	skipWithAppEngine(t)
	c := schedulerClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/us-central1"
	job, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/ae-job", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			RelativeUri: "/cron/tick", Body: []byte("t"),
			AppEngineRouting: &schedulerpb.AppEngineRouting{Service: "cron", Version: "v2"}}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })
	wantHost := "v2.cron." + h.Project() + ".appspot.com"
	ae := job.GetAppEngineHttpTarget()
	if ae.GetHttpMethod() != schedulerpb.HttpMethod_POST || ae.GetRelativeUri() != "/cron/tick" ||
		ae.GetAppEngineRouting().GetHost() != wantHost {
		t.Errorf("created job = %v", job)
	}
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	var got *schedulerpb.Job
	for d := time.Now().Add(15 * time.Second); time.Now().Before(d); time.Sleep(100 * time.Millisecond) {
		if got, err = c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()}); err != nil || got.GetLastAttemptTime() != nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.GetStatus().GetCode() != int32(codes.NotFound) || !strings.Contains(got.GetStatus().GetMessage(), wantHost+"/cron/tick") {
		t.Errorf("status after RunJob = %v; want NOT_FOUND naming %s", got.GetStatus(), wantHost)
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/GetTask, google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob
//
// TestGcloudAppEngineTargets: through gcloud-setup's configuration, with
// egress blocked, `gcloud tasks create-app-engine-task` on a paused queue
// and `gcloud scheduler jobs create app-engine` create what the official
// clients read back.
func TestGcloudAppEngineTargets(t *testing.T) {
	h := New(t)
	tc := tasksClient(t, h)
	sc := schedulerClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p, loc := "--project="+h.Project(), "--location=us-central1"
	parent := "projects/" + h.Project() + "/locations/us-central1"
	q := fmt.Sprintf("gcloud-ae-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = tc.DeleteQueue(context.Background(), &taskspb.DeleteQueueRequest{Name: parent + "/queues/" + q})
		_ = sc.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: parent + "/jobs/gcloud-ae"})
	})
	g.must("tasks", "queues", "create", q, p, loc, "--routing-override=service:worker")
	g.must("tasks", "queues", "pause", q, p, loc)
	g.must("tasks", "create-app-engine-task", "ae1", "--queue="+q, p, loc, "--relative-uri=/work",
		"--method=PUT", "--body-content=hi", "--header=X-Test:1", "--routing=service:worker,version:v1")
	task, err := tc.GetTask(ctx, &taskspb.GetTaskRequest{Name: parent + "/queues/" + q + "/tasks/ae1", ResponseView: taskspb.Task_FULL})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	ae := task.GetAppEngineHttpRequest()
	if ae.GetRelativeUri() != "/work" || ae.GetHttpMethod() != taskspb.HttpMethod_PUT || string(ae.GetBody()) != "hi" ||
		ae.GetAppEngineRouting().GetVersion() != "v1" || ae.GetAppEngineRouting().GetHost() != "worker."+h.Project()+".appspot.com" {
		t.Errorf("gcloud's task reads back as %v", task)
	}
	if got := g.must("tasks", "describe", "ae1", "--queue="+q, p, loc,
		"--format=value(appEngineHttpRequest.relativeUri,appEngineHttpRequest.appEngineRouting.host)"); !strings.Contains(got, "/work\tworker."+h.Project()+".appspot.com") {
		t.Errorf("tasks describe = %q", got)
	}

	g.must("scheduler", "jobs", "create", "app-engine", "gcloud-ae", p, loc, "--schedule=0 9 * * 1",
		"--relative-url=/cron", "--service=cron", "--version=v3", "--http-method=PUT", "--message-body=tick")
	job, err := sc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: parent + "/jobs/gcloud-ae"})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j := job.GetAppEngineHttpTarget(); j.GetRelativeUri() != "/cron" || j.GetHttpMethod() != schedulerpb.HttpMethod_PUT ||
		string(j.GetBody()) != "tick" || j.GetAppEngineRouting().GetHost() != "v3.cron."+h.Project()+".appspot.com" {
		t.Errorf("gcloud's job reads back as %v", job)
	}
	g.must("scheduler", "jobs", "delete", "gcloud-ae", p, loc, "--quiet")
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.tasks.v2.CloudTasks/UpdateQueue, google.cloud.tasks.v2.CloudTasks/DeleteQueue, google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/UpdateJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
//
// TestTerraformAppEngineTargets: a google_cloud_tasks_queue with an
// app_engine_routing_override and a google_cloud_scheduler_job with an
// app_engine_http_target apply through `cloudburrow terraform`, read back
// with the official clients, plan clean, take a change in place, plan clean
// again and destroy.
func TestTerraformAppEngineTargets(t *testing.T) {
	h := New(t)
	tc := tasksClient(t, h)
	sc := schedulerClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	queueID := fmt.Sprintf("tf-ae-%d", time.Now().UnixNano())
	queue := "projects/" + project + "/locations/us-central1/queues/" + queueID
	job := "projects/" + project + "/locations/us-central1/jobs/tf-ae-job"
	module := func(service, uri string) string {
		return fmt.Sprintf(`
resource "google_cloud_tasks_queue" "q" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
  app_engine_routing_override {
    service = %[3]q
    version = "v1"
  }
}
resource "google_cloud_scheduler_job" "j" {
  project   = %[1]q
  region    = "us-central1"
  name      = "tf-ae-job"
  schedule  = "0 9 * * 1"
  time_zone = "Etc/UTC"
  app_engine_http_target {
    http_method  = "POST"
    relative_uri = %[4]q
    body         = base64encode("tick")
    app_engine_routing {
      service = %[3]q
      version = "v1"
    }
  }
}
`, project, queueID, service, uri)
	}
	check := func(when, service, uri string) {
		t.Helper()
		q, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: queue})
		if err != nil {
			t.Fatalf("%s, GetQueue: %v", when, err)
		}
		if o := q.GetAppEngineRoutingOverride(); o.GetService() != service || o.GetHost() != "v1."+service+"."+project+".appspot.com" {
			t.Errorf("%s, the override = %v", when, o)
		}
		j, err := sc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job})
		if err != nil {
			t.Fatalf("%s, GetJob: %v", when, err)
		}
		if a := j.GetAppEngineHttpTarget(); a.GetRelativeUri() != uri || a.GetAppEngineRouting().GetService() != service || string(a.GetBody()) != "tick" {
			t.Errorf("%s, the job's target = %v", when, a)
		}
	}
	m.write(module("worker", "/cron"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "worker", "/cron")
	m.planClean()
	m.write(module("other", "/cron2"))
	m.apply()
	check("after the change", "other", "/cron2")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: queue}); status.Code(err) != codes.NotFound {
		t.Errorf("GetQueue after destroy: %v; want NotFound", err)
	}
	if _, err := sc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy: %v; want NotFound", err)
	}
}
