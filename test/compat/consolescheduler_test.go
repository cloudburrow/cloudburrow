//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// consoleJobEditForm reads a job's page from the console and returns the
// values its Edit job form would submit: every field's default except the
// immutable ones, which the browser does not send.
func consoleJobEditForm(t *testing.T, addr, project, job string) map[string]string {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/scheduler?project="+project+"&name="+url.QueryEscape(job), "")
	if code != http.StatusOK {
		t.Fatalf("console detail = %d: %s", code, body)
	}
	var detail struct {
		Unavailable string
		Edit        *struct {
			Label  string
			Fields []struct {
				Name, Default string
				Immutable     bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if detail.Edit == nil || detail.Edit.Label != "Edit job" {
		t.Fatalf("the job page offers no Edit job (unavailable: %q): %s", detail.Unavailable, body)
	}
	values := map[string]string{}
	for _, f := range detail.Edit.Fields {
		if !f.Immutable {
			values[f.Name] = f.Default
		}
	}
	return values
}

// TestConsoleSchedulerEditJob (#795): the console's Edit job changes the
// schedule, time zone, description, HTTP target and retry config of a job
// the official client created, and the official client's GetJob reads back
// what was saved, with the next run on the new schedule in the new zone. An
// invalid cron expression is refused with the message the official client's
// own UpdateJob receives for it, and changes nothing.
func TestConsoleSchedulerEditJob(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := schedulerClient(t, h)
	ctx := h.Context()
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/jobs/console-edit"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "0 3 * * *", TimeZone: "Etc/UTC", Description: "before",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: "http://127.0.0.1:1/a", HttpMethod: schedulerpb.HttpMethod_POST}},
	}}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: name}) })

	values := consoleJobEditForm(t, addr, project, name)
	if values["schedule"] != "0 3 * * *" || values["timeZone"] != "Etc/UTC" || values["uri"] != "http://127.0.0.1:1/a" {
		t.Fatalf("the edit form is not prefilled from the job: %v", values)
	}
	edit := func(v map[string]string) (int, string) {
		body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Values": v})
		return consoleDo(t, addr, http.MethodPatch, "/api/resources/scheduler?project="+project, string(body))
	}

	values["schedule"] = "30 9 * * 1"
	values["timeZone"] = "America/New_York"
	values["description"] = "after"
	values["uri"] = "http://127.0.0.1:1/b"
	values["httpMethod"] = "PUT"
	values["headers"] = `{"X-Kind":"console"}`
	values["body"] = "hello"
	values["retryCount"] = "2"
	values["minBackoff"] = "10s"
	before := time.Now()
	if code, body := edit(values); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, body)
	}
	got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	ht := got.GetHttpTarget()
	if got.GetSchedule() != "30 9 * * 1" || got.GetTimeZone() != "America/New_York" || got.GetDescription() != "after" ||
		ht.GetUri() != "http://127.0.0.1:1/b" || ht.GetHttpMethod() != schedulerpb.HttpMethod_PUT ||
		ht.GetHeaders()["X-Kind"] != "console" || string(ht.GetBody()) != "hello" {
		t.Errorf("GetJob reads %v; want what the console saved", got)
	}
	if rc := got.GetRetryConfig(); rc.GetRetryCount() != 2 || rc.GetMinBackoffDuration().AsDuration() != 10*time.Second {
		t.Errorf("GetJob reads retry config %v; want 2 retries from 10s", rc)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next := got.GetScheduleTime().AsTime().In(ny)
	if next.Weekday() != time.Monday || next.Hour() != 9 || next.Minute() != 30 || !next.After(before) ||
		next.Sub(before) > 8*24*time.Hour {
		t.Errorf("GetJob's next run is %v; want the next Monday 09:30 in New York", next)
	}

	// An invalid cron expression: the console's refusal is the API's.
	const invalid = "61 * * * *"
	_, sdkErr := c.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{
		Job: &schedulerpb.Job{Name: name, Schedule: invalid}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"schedule"}}})
	st, _ := status.FromError(sdkErr)
	if sdkErr == nil || st.Message() == "" {
		t.Fatalf("UpdateJob with schedule %q = %v; want it refused", invalid, sdkErr)
	}
	values["schedule"] = invalid
	code, body := edit(values)
	var refusal struct{ Error string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if want := st.Code().String() + ": " + st.Message(); code != http.StatusBadRequest || refusal.Error != want {
		t.Errorf("console edit with schedule %q = %d %s; want 400 with UpdateJob's own %q", invalid, code, body, want)
	}
	if got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name}); err != nil || got.GetSchedule() != "30 9 * * 1" {
		t.Errorf("after a refused edit GetJob reads schedule %q (%v), want 30 9 * * 1", got.GetSchedule(), err)
	}
}
