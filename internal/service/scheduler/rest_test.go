package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// restServer is the Cloud Scheduler JSON API over a memory store, registered
// as cmd/cloudburrow registers it.
func restServer(t *testing.T) *httptest.Server {
	t.Helper()
	clock := sched.NewFakeClock(time.Date(2026, 9, 24, 10, 0, 30, 0, time.UTC))
	st := NewStore(store.NewMemory())
	api := NewGRPCServer(st, NewRunner(st, nil, clock, (&published{}).publish, time.Second), clock)
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { api.Register(g) }))
	t.Cleanup(srv.Close)
	return srv
}

// Google's bindings are the routes: CloudScheduler's, and the two Locations
// bindings of cloudscheduler_v1.yaml.
func TestRESTRoutesAreGooglesBindings(t *testing.T) {
	t.Parallel()
	have := map[string]bool{}
	for _, r := range Routes() {
		have[r.Method+" "+r.Pattern+" "+r.RPC] = true
	}
	for _, want := range []string{
		"POST /v1/{parent=projects/*/locations/*}/jobs CloudScheduler/CreateJob",
		"GET /v1/{parent=projects/*/locations/*}/jobs CloudScheduler/ListJobs",
		"GET /v1/{name=projects/*/locations/*/jobs/*} CloudScheduler/GetJob",
		"PATCH /v1/{job.name=projects/*/locations/*/jobs/*} CloudScheduler/UpdateJob",
		"DELETE /v1/{name=projects/*/locations/*/jobs/*} CloudScheduler/DeleteJob",
		"POST /v1/{name=projects/*/locations/*/jobs/*}:pause CloudScheduler/PauseJob",
		"POST /v1/{name=projects/*/locations/*/jobs/*}:resume CloudScheduler/ResumeJob",
		"POST /v1/{name=projects/*/locations/*/jobs/*}:run CloudScheduler/RunJob",
		"GET /v1/{name=projects/*/locations/*} Locations/GetLocation",
		"GET /v1/{name=projects/*}/locations Locations/ListLocations",
	} {
		if !have[want] {
			t.Errorf("no route %s", want)
		}
	}
}

// The official client's REST transport drives a job's whole lifecycle over
// JSON: create, get, list, an update with a mask of lowerCamelCase paths,
// pause, resume and delete. What it reads back is what gRPC would return.
func TestRESTJobLifecycleWithTheOfficialRESTClient(t *testing.T) {
	t.Parallel()
	srv := restServer(t)
	ctx := context.Background()
	c, err := scheduler.NewCloudSchedulerRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := parent + "/jobs/rest"
	job, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "*/5 * * * *", TimeZone: "Etc/UTC", Description: "one",
		Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: "projects/demo-project/topics/t", Data: []byte("hi")}},
	}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.GetState() != schedulerpb.Job_ENABLED || job.GetScheduleTime() == nil {
		t.Errorf("created job = state %v, scheduleTime %v", job.GetState(), job.GetScheduleTime())
	}
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "*/5 * * * *", Target: job.GetTarget()}}); httpCode(err) != http.StatusConflict {
		t.Errorf("CreateJob again: %v; want 409", err)
	}
	got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil || string(got.GetPubsubTarget().GetData()) != "hi" {
		t.Fatalf("GetJob = %v, %v", got, err)
	}
	if it := c.ListJobs(ctx, &schedulerpb.ListJobsRequest{Parent: parent}); true {
		j, err := it.Next()
		if err != nil || j.GetName() != name {
			t.Errorf("ListJobs = %v, %v", j, err)
		}
	}
	up, err := c.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{
		Job:        &schedulerpb.Job{Name: name, Description: "two", Schedule: "0 * * * *", TimeZone: "Europe/Paris"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "schedule", "time_zone"}}})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if up.GetDescription() != "two" || up.GetSchedule() != "0 * * * *" || up.GetTimeZone() != "Europe/Paris" ||
		up.GetPubsubTarget().GetTopicName() == "" {
		t.Errorf("updated job = %v", up)
	}
	if j, err := c.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: name}); err != nil || j.GetState() != schedulerpb.Job_PAUSED {
		t.Errorf("PauseJob = %v, %v", j.GetState(), err)
	}
	if j, err := c.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: name}); err != nil || j.GetState() != schedulerpb.Job_ENABLED {
		t.Errorf("ResumeJob = %v, %v", j.GetState(), err)
	}
	if err := c.DeleteJob(ctx, &schedulerpb.DeleteJobRequest{Name: name}); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name}); httpCode(err) != http.StatusNotFound {
		t.Errorf("GetJob after delete: %v; want 404", err)
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

// The wire: the Terraform provider's resume of the job it just created and
// its camelCase updateMask, an unknown query parameter, the unimplemented
// Locations mixin and an unbound path.
func TestRESTWire(t *testing.T) {
	t.Parallel()
	srv := restServer(t)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const jobs = "/v1/" + parent + "/jobs"
	const job = jobs + "/wire"
	for _, c := range []struct {
		method, path, body string
		code               int
		has                string
	}{
		{"POST", jobs, `{"name":"` + parent + `/jobs/wire","schedule":"* * * * *","timeZone":"Etc/UTC",` +
			`"httpTarget":{"uri":"http://127.0.0.1:1/","httpMethod":"POST"},"retryConfig":{"retryCount":1}}`, 200, `"state":"ENABLED"`},
		// What the Terraform provider sends after creating an unpaused job.
		{"POST", job + ":resume", "", 200, `"state":"ENABLED"`},
		{"PATCH", job + "?updateMask=description,retryConfig", `{"name":"` + parent + `/jobs/wire","description":"d","retryConfig":{"retryCount":2}}`, 200, `"retryCount":2`},
		{"GET", job, "", 200, `"description":"d"`},
		{"PATCH", job + "?updateMask=state", `{"state":"PAUSED"}`, 400, "state"},
		{"GET", job + "?noSuchParameter=1", "", 400, "noSuchParameter"},
		{"POST", jobs, `{"name":"` + parent + `/jobs/x","noSuchField":1}`, 400, "INVALID_ARGUMENT"},
		{"POST", job + ":pause", "", 200, `"state":"PAUSED"`},
		{"GET", "/v1/projects/demo-project/locations", "", 501, "UNIMPLEMENTED"},
		{"GET", "/v1/projects/demo-project/locations/us-central1", "", 501, "UNIMPLEMENTED"},
		{"GET", "/v1/projects/demo-project/jobs", "", 404, "NOT_FOUND"},
		{"DELETE", job, "", 200, "{}"},
		{"GET", job, "", 404, "NOT_FOUND"},
	} {
		code, body := do(c.method, c.path, c.body)
		if code != c.code || !strings.Contains(body, c.has) || !json.Valid([]byte(body)) {
			t.Errorf("%s %s: %d %s; want %d containing %q", c.method, c.path, code, body, c.code, c.has)
		}
	}
}
