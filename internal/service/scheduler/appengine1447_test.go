package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// An App Engine job is stored with its routing's host, and every attempt
// fails with NOT_FOUND and the reason in Job.status (#1447).
func TestAppEngineJobAttemptsFail(t *testing.T) {
	c, _, _, _ := start(t)
	ctx := context.Background()
	in := &schedulerpb.Job{Name: parent + "/jobs/ae", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			RelativeUri: "/cron", Body: []byte("b"), Headers: map[string]string{"X-A": "1"},
			AppEngineRouting: &schedulerpb.AppEngineRouting{Service: "worker", Version: "v1", Host: "ignored"}}}}
	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: in})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	project := strings.Split(parent, "/")[1]
	ae := j.GetAppEngineHttpTarget()
	if ae.GetHttpMethod() != schedulerpb.HttpMethod_POST || ae.GetRelativeUri() != "/cron" || string(ae.GetBody()) != "b" ||
		ae.GetAppEngineRouting().GetHost() != "v1.worker."+project+".appspot.com" {
		t.Errorf("created job = %v", j)
	}
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
		t.Fatal(err)
	}
	var got *schedulerpb.Job
	for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(10 * time.Millisecond) {
		if got, err = c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: j.GetName()}); err == nil && got.GetLastAttemptTime() != nil {
			break
		}
	}
	if got.GetStatus().GetCode() != int32(codes.NotFound) ||
		!strings.Contains(got.GetStatus().GetMessage(), "v1.worker."+project+".appspot.com/cron") {
		t.Errorf("status after the attempt = %v", got.GetStatus())
	}

	bad := &schedulerpb.Job{Name: parent + "/jobs/ae-get", Schedule: "* * * * *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			HttpMethod: schedulerpb.HttpMethod_GET, Body: []byte("x")}}}
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GET with a body = %v, want InvalidArgument", err)
	}
}
