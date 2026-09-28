package main

import (
	"context"
	"strings"
	"testing"
	"time"

	schedulerapi "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// schedulerEditFixture is a started Scheduler service, the console's provider
// over it and the official client against its port.
func schedulerEditFixture(t *testing.T) (schedulerProvider, *schedulerapi.CloudSchedulerClient) {
	t.Helper()
	ctx := context.Background()
	svc := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	c, err := schedulerapi.NewCloudSchedulerClient(ctx, clientOpts(svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return schedulerProvider{svc: svc}, c
}

// schedulerSubmitted is what the browser submits for an edit form: every
// field's default except the immutable ones, which it does not send.
func schedulerSubmitted(form *console.EditForm) map[string]string {
	out := map[string]string{}
	for _, f := range form.Fields {
		if !f.Immutable {
			out[f.Name] = f.Default
		}
	}
	return out
}

func schedulerEditOf(t *testing.T, p schedulerProvider, project, name string) *console.EditForm {
	t.Helper()
	d, err := p.Detail(context.Background(), project, []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("the job's page offers no edit form: %v %+v", err, d)
	}
	return d.Edit
}

// A job's page carries Edit job, prefilled from the job, with the name,
// region and target type shown and not sent, and an Authorization header not
// shown at all. Saving it changes what the official client's GetJob reads,
// the next run follows the new schedule in the new zone, and a redacted
// header is kept. What UpdateJob refuses is refused with its message and
// changes nothing (#795).
func TestSchedulerEditJobThroughUpdateJob(t *testing.T) {
	ctx := context.Background()
	p, c := schedulerEditFixture(t)
	const project = "edit-proj"
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/jobs/nightly"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "0 3 * * *", TimeZone: "Etc/UTC", Description: "before",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: "http://127.0.0.1:1/a", HttpMethod: schedulerpb.HttpMethod_POST, Body: []byte("b1"),
			Headers: map[string]string{"Authorization": "Bearer s3cret", "X-Kind": "a"}}},
	}}); err != nil {
		t.Fatal(err)
	}

	form := schedulerEditOf(t, p, project, name)
	if form.Label != "Edit job" {
		t.Errorf("edit label = %q", form.Label)
	}
	for _, f := range form.Fields {
		if strings.Contains(f.Default, "s3cret") {
			t.Errorf("field %s carries the Authorization header's value: %q", f.Name, f.Default)
		}
		if (f.Name == "name" || f.Name == "location" || f.Name == "targetType") != f.Immutable {
			t.Errorf("field %s immutable = %v", f.Name, f.Immutable)
		}
		if f.Name == "topic" || f.Name == "attributes" {
			t.Errorf("an HTTP job's form offers the Pub/Sub field %s", f.Name)
		}
	}
	values := schedulerSubmitted(form)
	want := map[string]string{
		"description": "before", "schedule": "0 3 * * *", "timeZone": "Etc/UTC", "uri": "http://127.0.0.1:1/a",
		"httpMethod": "POST", "headers": `{"X-Kind":"a"}`, "body": "b1", "attemptDeadline": "3m0s",
		"retryCount": "0", "maxRetryDuration": "", "minBackoff": "5s", "maxBackoff": "1h0m0s", "maxDoublings": "5",
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s prefilled as %q, want %q", k, values[k], v)
		}
	}

	// Saved unchanged, a job given no retry_config still returns none.
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit with the prefilled values: %v", err)
	}
	if j, _ := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name}); j.GetRetryConfig() != nil {
		t.Errorf("saving the defaults the form showed gave the job a retry_config: %v", j.GetRetryConfig())
	}

	values["description"] = "after"
	values["schedule"] = "30 9 * * 1"
	values["timeZone"] = "America/New_York"
	values["uri"] = "http://127.0.0.1:1/b"
	values["httpMethod"] = "PUT"
	values["headers"] = `{"X-Kind":"b"}`
	values["body"] = "b2"
	values["attemptDeadline"] = "1m"
	values["retryCount"] = "3"
	values["maxRetryDuration"] = "10m"
	values["minBackoff"] = "2s"
	values["maxBackoff"] = "30s"
	values["maxDoublings"] = "2"
	before := time.Now()
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	h := j.GetHttpTarget()
	if j.GetDescription() != "after" || j.GetSchedule() != "30 9 * * 1" || j.GetTimeZone() != "America/New_York" ||
		h.GetUri() != "http://127.0.0.1:1/b" || h.GetHttpMethod() != schedulerpb.HttpMethod_PUT || string(h.GetBody()) != "b2" ||
		h.GetHeaders()["X-Kind"] != "b" || j.GetAttemptDeadline().AsDuration() != time.Minute {
		t.Errorf("GetJob reads %v after the edit", j)
	}
	if h.GetHeaders()["Authorization"] != "Bearer s3cret" {
		t.Errorf("the redacted Authorization header was not kept: %v", h.GetHeaders())
	}
	rc := j.GetRetryConfig()
	if rc.GetRetryCount() != 3 || rc.GetMaxRetryDuration().AsDuration() != 10*time.Minute ||
		rc.GetMinBackoffDuration().AsDuration() != 2*time.Second || rc.GetMaxBackoffDuration().AsDuration() != 30*time.Second ||
		rc.GetMaxDoublings() != 2 {
		t.Errorf("GetJob reads retry config %v", rc)
	}
	// The next run is the new schedule's, in the new zone: a Monday, 09:30 in New York.
	ny, _ := time.LoadLocation("America/New_York")
	next := j.GetScheduleTime().AsTime().In(ny)
	if next.Weekday() != time.Monday || next.Hour() != 9 || next.Minute() != 30 || !next.After(before) ||
		next.Sub(before) > 8*24*time.Hour {
		t.Errorf("the next run is %v; want the next Monday 09:30 in New York", next)
	}
	if got := schedulerSubmitted(schedulerEditOf(t, p, project, name)); got["schedule"] != "30 9 * * 1" || got["maxRetryDuration"] != "10m0s" {
		t.Errorf("the form after saving is prefilled with %v", got)
	}

	// Refused by UpdateJob, with its message, changing nothing.
	for _, tc := range []struct{ key, value, want string }{
		{"schedule", "61 * * * *", `schedule "61 * * * *" is not a unix-cron expression`},
		{"timeZone", "Mars/Olympus", `time_zone "Mars/Olympus" is not an IANA time zone`},
		{"httpMethod", "GET", "http_target.body is allowed only when http_method is POST, PUT or PATCH, not GET"},
		{"attemptDeadline", "5s", "attempt_deadline must be between 15 seconds and 30 minutes"},
		{"retryCount", "6", "retry_config.retry_count must be 0 to 5"},
	} {
		bad := map[string]string{}
		for k, v := range values {
			bad[k] = v
		}
		bad[tc.key] = tc.value
		err := p.Edit(ctx, project, []string{name}, bad)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q refused with %v; want UpdateJob's INVALID_ARGUMENT %q", tc.key, tc.value, err, tc.want)
		}
	}
	if j, _ := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name}); j.GetSchedule() != "30 9 * * 1" || j.GetTimeZone() != "America/New_York" {
		t.Errorf("after refused edits GetJob reads %v", j)
	}

	// Refused before UpdateJob: a rename, a changed target type, another
	// project's job, a value that is not a number.
	for what, v := range map[string]map[string]string{
		"a rename":            {"name": "other"},
		"a changed target":    {"targetType": "Pub/Sub"},
		"a count not a count": {"retryCount": "three"},
		"a bad duration":      {"minBackoff": "soon"},
	} {
		bad := map[string]string{}
		for k, x := range values {
			bad[k] = x
		}
		for k, x := range v {
			bad[k] = x
		}
		if err := p.Edit(ctx, project, []string{name}, bad); err == nil {
			t.Errorf("an edit with %s was accepted", what)
		}
	}
	if err := p.Edit(ctx, "other-proj", []string{name}, values); err == nil {
		t.Error("an edit of another project's job was accepted")
	}
	if err := c.DeleteJob(ctx, &schedulerpb.DeleteJobRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if err := p.Edit(ctx, project, []string{name}, values); status.Code(err) != codes.NotFound {
		t.Errorf("editing a deleted job = %v; want NOT_FOUND", err)
	}
}

// A Pub/Sub job's form holds its topic, data and attributes and no HTTP
// field, and saving it is what GetJob reads; a job whose data is not UTF-8
// text is not offered the form, which could not hold it (#795).
func TestSchedulerEditPubSubJob(t *testing.T) {
	ctx := context.Background()
	p, c := schedulerEditFixture(t)
	const project = "edit-proj"
	parent := "projects/" + project + "/locations/europe-west1"
	name := parent + "/jobs/ping"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "*/5 * * * *",
		Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: "projects/" + project + "/topics/t1", Data: []byte("d1"), Attributes: map[string]string{"k": "v"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	form := schedulerEditOf(t, p, project, name)
	for _, f := range form.Fields {
		if f.Name == "uri" || f.Name == "httpMethod" || f.Name == "headers" || f.Name == "attemptDeadline" {
			t.Errorf("a Pub/Sub job's form offers the HTTP field %s", f.Name)
		}
		if f.Name == "location" && f.Default != "europe-west1" {
			t.Errorf("region shown as %q", f.Default)
		}
	}
	values := schedulerSubmitted(form)
	if values["topic"] != "projects/"+project+"/topics/t1" || values["body"] != "d1" || values["attributes"] != `{"k":"v"}` {
		t.Errorf("the Pub/Sub form is prefilled with %v", values)
	}
	values["topic"] = "projects/" + project + "/topics/t2"
	values["body"] = "d2"
	values["attributes"] = `{"k":"w","n":"1"}`
	values["schedule"] = "0 * * * *"
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ps := j.GetPubsubTarget()
	if ps.GetTopicName() != "projects/"+project+"/topics/t2" || string(ps.GetData()) != "d2" ||
		ps.GetAttributes()["k"] != "w" || ps.GetAttributes()["n"] != "1" || j.GetSchedule() != "0 * * * *" {
		t.Errorf("GetJob reads %v after the edit", j)
	}
	values["body"], values["attributes"] = "", ""
	if err := p.Edit(ctx, project, []string{name}, values); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a Pub/Sub target with no data and no attributes = %v; want UpdateJob's INVALID_ARGUMENT", err)
	}

	binary := parent + "/jobs/binary"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: binary, Schedule: "*/5 * * * *",
		Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: "projects/" + project + "/topics/t1", Data: []byte{0xff, 0xfe}}},
	}}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, project, []string{binary})
	if err != nil || d.Edit != nil {
		t.Errorf("a job with binary data is offered an edit form (%v)", err)
	}
	if err := p.Edit(ctx, project, []string{binary}, values); err == nil {
		t.Error("an edit of a job with binary data was accepted")
	}
}
