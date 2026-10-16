//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
)

// App Engine targets from the console and from a seed document (#1447). The
// console's Edit queue holds a queue's app_engine_routing_override and
// clears it, Edit job is offered for an App Engine job, and a seed document
// takes both; each is read back with the official clients.

// covers: google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.tasks.v2.CloudTasks/UpdateQueue
//
// TestConsoleTasksEditQueueAppEngineRouting: the console's Edit queue sets
// the override, which cloud.google.com/go/cloudtasks/apiv2 reads back with
// its computed host, and emptying the three fields clears it.
func TestConsoleTasksEditQueueAppEngineRouting(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := tasksClient(t, h)
	ctx := h.Context()
	project := h.Project()
	q := queue(t, h, c, fmt.Sprintf("console-ae-%d", time.Now().UnixNano()))

	values := consoleQueueEditForm(t, addr, project, q)
	for _, f := range []string{"appEngineService", "appEngineVersion", "appEngineInstance"} {
		if v, ok := values[f]; !ok || v != "" {
			t.Fatalf("Edit queue's %s = %q, %v; want an empty field", f, v, ok)
		}
	}
	values["appEngineService"], values["appEngineVersion"], values["appEngineInstance"] = "worker", "v1", "i1"
	if code, body := consoleEdit(t, addr, "tasks", project, []string{q}, values); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, body)
	}
	got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	o := got.GetAppEngineRoutingOverride()
	if o.GetService() != "worker" || o.GetVersion() != "v1" || o.GetInstance() != "i1" ||
		o.GetHost() != "i1.v1.worker."+project+".appspot.com" {
		t.Errorf("GetQueue's override = %v; want what the console saved", o)
	}

	values = consoleQueueEditForm(t, addr, project, q)
	if values["appEngineService"] != "worker" {
		t.Errorf("the form is not prefilled with the override: %v", values)
	}
	values["appEngineService"], values["appEngineVersion"], values["appEngineInstance"] = "", "", ""
	if code, body := consoleEdit(t, addr, "tasks", project, []string{q}, values); code != http.StatusOK {
		t.Fatalf("clearing console edit = %d: %s", code, body)
	}
	if got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); err != nil || got.GetAppEngineRoutingOverride() != nil {
		t.Errorf("GetQueue after clearing = %v, %v; want no override", got.GetAppEngineRoutingOverride(), err)
	}
}

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/UpdateJob
//
// TestConsoleSchedulerEditAppEngineJob: an App Engine job the official
// client created offers Edit job, prefilled from it; saving it changes what
// cloud.google.com/go/scheduler/apiv1 reads back, the routing's host
// computed from the new service, version and instance. A body with GET is
// refused with UpdateJob's message and changes nothing.
func TestConsoleSchedulerEditAppEngineJob(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := schedulerClient(t, h)
	ctx := h.Context()
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + fmt.Sprintf("/jobs/console-ae-%d", time.Now().UnixNano())
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "0 3 * * *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			HttpMethod: schedulerpb.HttpMethod_POST, RelativeUri: "/cron/a", Body: []byte("b1"),
			Headers: map[string]string{"X-Kind": "a"}, AppEngineRouting: &schedulerpb.AppEngineRouting{Service: "worker"}}},
	}}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: name}) })

	values := consoleJobEditForm(t, addr, project, name)
	if values["relativeUri"] != "/cron/a" || values["httpMethod"] != "POST" || values["body"] != "b1" ||
		values["appEngineService"] != "worker" || values["headers"] != `{"X-Kind":"a"}` {
		t.Fatalf("the edit form is not prefilled from the job: %v", values)
	}
	values["relativeUri"] = "/cron/b?x=1"
	values["httpMethod"] = "PUT"
	values["body"] = "b2"
	values["headers"] = `{"X-Kind":"b"}`
	values["appEngineService"], values["appEngineVersion"], values["appEngineInstance"] = "api", "v2", "i3"
	values["schedule"] = "0 4 * * *"
	if code, body := consoleEdit(t, addr, "scheduler", project, []string{name}, values); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, body)
	}
	got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	a := got.GetAppEngineHttpTarget()
	r := a.GetAppEngineRouting()
	if got.GetSchedule() != "0 4 * * *" || a.GetRelativeUri() != "/cron/b?x=1" || a.GetHttpMethod() != schedulerpb.HttpMethod_PUT ||
		string(a.GetBody()) != "b2" || a.GetHeaders()["X-Kind"] != "b" || r.GetService() != "api" ||
		r.GetVersion() != "v2" || r.GetInstance() != "i3" || r.GetHost() != "i3.v2.api."+project+".appspot.com" {
		t.Errorf("GetJob reads %v; want what the console saved", got)
	}

	values["httpMethod"] = "GET"
	code, body := consoleEdit(t, addr, "scheduler", project, []string{name}, values)
	if code == http.StatusOK || !strings.Contains(body, "POST or PUT") {
		t.Errorf("a GET with a body = %d %s; want UpdateJob's refusal", code, body)
	}
	if got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name}); err != nil ||
		got.GetAppEngineHttpTarget().GetHttpMethod() != schedulerpb.HttpMethod_PUT {
		t.Errorf("a refused edit changed the job: %v, %v", got, err)
	}
}

// covers: google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.scheduler.v1.CloudScheduler/GetJob
//
// TestAppEngineSeedIsReadBackByTheOfficialClients: a seed document's queue
// with an appEngineRoutingOverride and job with an appEngineHttpTarget are
// read back by the official tasks and scheduler clients, hosts computed; a
// relativeUri not beginning with / refuses the document, which seeds nothing.
func TestAppEngineSeedIsReadBackByTheOfficialClients(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	tc := tasksClient(t, h)
	sc := schedulerClient(t, h)
	ctx := h.Context()
	project := h.Project()
	suffix := time.Now().UnixNano()
	q := fmt.Sprintf("%s/queues/seed-ae-%d", location(h), suffix)
	job := fmt.Sprintf("projects/%s/locations/us-central1/jobs/seed-ae-%d", project, suffix)
	t.Cleanup(func() {
		_ = tc.DeleteQueue(context.Background(), &taskspb.DeleteQueueRequest{Name: q})
		_ = sc.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job})
	})
	doc := func(uri string) string {
		u, _ := json.Marshal(uri)
		return fmt.Sprintf(`{"components": {
		"tasks": {"queues": [{"name": %q, "appEngineRoutingOverride": {"service": "worker", "version": "v1"}}]},
		"scheduler": {"jobs": [{"name": %q, "schedule": "0 * * * *",
		  "appEngineHttpTarget": {"httpMethod": "PUT", "relativeUri": %s, "body": "hi",
		    "appEngineRouting": {"service": "cron"}}}]}}}`, q, job, u)
	}
	if code, body := adminSeed(t, control, doc("cron")); code != http.StatusBadRequest || !strings.Contains(body, "must begin with /") {
		t.Fatalf("seed with a relativeUri not beginning with / = %d %s; want 400", code, body)
	}
	if _, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); err == nil {
		t.Fatal("the queue of a refused seed exists")
	}
	if code, body := adminSeed(t, control, doc("/cron")); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, body)
	}
	got, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if o := got.GetAppEngineRoutingOverride(); o.GetService() != "worker" || o.GetHost() != "v1.worker."+project+".appspot.com" {
		t.Errorf("seeded queue's override = %v", o)
	}
	j, err := sc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	a := j.GetAppEngineHttpTarget()
	if a.GetHttpMethod() != schedulerpb.HttpMethod_PUT || a.GetRelativeUri() != "/cron" || string(a.GetBody()) != "hi" ||
		a.GetAppEngineRouting().GetHost() != "cron."+project+".appspot.com" || j.GetState() != schedulerpb.Job_ENABLED {
		t.Errorf("seeded App Engine job = %v", j)
	}
}
