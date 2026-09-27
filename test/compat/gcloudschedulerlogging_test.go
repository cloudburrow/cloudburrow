//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/logging/logadmin"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/ListJobs, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/UpdateJob, google.cloud.scheduler.v1.CloudScheduler/PauseJob, google.cloud.scheduler.v1.CloudScheduler/ResumeJob, google.cloud.scheduler.v1.CloudScheduler/RunJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
//
// TestGcloudScheduler (#591): through the configuration `cloudburrow
// gcloud-setup` writes and nothing else, the real gcloud creates an HTTP
// and a Pub/Sub job, lists and describes them, updates one, pauses and
// resumes it, runs it and deletes both, over Cloud Scheduler's JSON API on
// the scheduler's gRPC port. The official gRPC client reads each change
// back. gcloud needs --location: without it, it looks for an App Engine app.
func TestGcloudScheduler(t *testing.T) {
	h := New(t)
	c := schedulerClient(t, h) // skips without CLOUDBURROW_TEST_SCHEDULER
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p, loc := "--project="+h.Project(), "--location=us-central1"
	name := "projects/" + h.Project() + "/locations/us-central1/jobs/"
	t.Cleanup(func() {
		for _, j := range []string{"gcloud-http", "gcloud-pubsub"} {
			_ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: name + j})
		}
	})
	g.must("scheduler", "jobs", "create", "http", "gcloud-http", p, loc, "--schedule=*/5 * * * *",
		"--uri=http://127.0.0.1:1/", "--http-method=PUT", "--message-body=hi", "--description=first")
	g.must("scheduler", "jobs", "create", "pubsub", "gcloud-pubsub", p, loc, "--schedule=0 * * * *",
		"--topic=projects/"+h.Project()+"/topics/t", "--message-body=tick")
	if got := g.must("scheduler", "jobs", "list", p, loc, "--format=value(ID)"); !strings.Contains(got, "gcloud-http") || !strings.Contains(got, "gcloud-pubsub") {
		t.Errorf("jobs list = %q", got)
	}
	j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name + "gcloud-http"})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if ht := j.GetHttpTarget(); ht.GetHttpMethod() != schedulerpb.HttpMethod_PUT || string(ht.GetBody()) != "hi" || j.GetDescription() != "first" {
		t.Errorf("the created job reads back as %v", j)
	}
	g.must("scheduler", "jobs", "update", "http", "gcloud-http", p, loc, "--schedule=0 9 * * 1", "--description=second")
	if got := g.must("scheduler", "jobs", "describe", "gcloud-http", p, loc, "--format=value(schedule,description)"); !strings.Contains(got, "0 9 * * 1\tsecond") {
		t.Errorf("describe after update = %q", got)
	}
	g.must("scheduler", "jobs", "pause", "gcloud-http", p, loc)
	if j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name + "gcloud-http"}); err != nil || j.GetState() != schedulerpb.Job_PAUSED {
		t.Errorf("after pause, GetJob = %v, %v", j.GetState(), err)
	}
	g.must("scheduler", "jobs", "resume", "gcloud-http", p, loc)
	g.must("scheduler", "jobs", "run", "gcloud-pubsub", p, loc)
	for _, id := range []string{"gcloud-http", "gcloud-pubsub"} {
		g.must("scheduler", "jobs", "delete", id, p, loc, "--quiet")
		if _, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name + id}); status.Code(err) != codes.NotFound {
			t.Errorf("GetJob %s after delete: %v; want NotFound", id, err)
		}
	}
}

// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries, google.logging.v2.LoggingServiceV2/ListLogEntries, google.logging.v2.LoggingServiceV2/ListLogs, google.logging.v2.LoggingServiceV2/DeleteLog
//
// TestGcloudLogging (#591): through gcloud-setup's configuration, the real
// gcloud writes a text and a JSON entry, reads them back with a logName
// filter and a severity filter, lists the logs and deletes the log, over
// Cloud Logging's JSON API on the logging gRPC port. logadmin, over gRPC,
// reads what gcloud wrote.
func TestGcloudLogging(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLogging)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	logName := "projects/" + h.Project() + "/logs/gcloud-log"
	g.must("logging", "write", "gcloud-log", "hello there", p, "--severity=WARNING")
	g.must("logging", "write", "gcloud-log", `{"msg":"structured","n":2}`, p, "--payload-type=json", "--severity=ERROR")
	got := g.must("logging", "read", `logName="`+logName+`"`, p, "--format=value(severity,textPayload,jsonPayload.msg)")
	if !strings.Contains(got, "WARNING\thello there") || !strings.Contains(got, "ERROR\t\tstructured") {
		t.Errorf("logging read = %q", got)
	}
	if got := g.must("logging", "read", "severity>=ERROR", p, "--limit=5", "--format=value(jsonPayload.msg)"); strings.TrimSpace(got) != "structured" {
		t.Errorf("logging read severity>=ERROR = %q", got)
	}
	if got := g.must("logging", "logs", "list", p, "--format=value(NAME)"); !strings.Contains(got, logName) {
		t.Errorf("logs list = %q", got)
	}

	ac, err := logadmin.NewClient(ctx, "projects/"+h.Project(), option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	n := 0
	it := ac.Entries(ctx, logadmin.Filter(`logName="`+logName+`"`))
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("logadmin Entries: %v", err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("logadmin reads %d entries gcloud wrote; want 2", n)
	}

	g.must("logging", "logs", "delete", "gcloud-log", p, "--quiet")
	if got := g.must("logging", "logs", "list", p, "--format=value(NAME)"); strings.Contains(got, logName) {
		t.Errorf("logs list after delete = %q", got)
	}
}
