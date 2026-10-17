package main

// App Engine targets in the console and in seed documents (#1447): Edit
// queue holds the queue's app_engine_routing_override and clears it, Edit job
// is offered for an App Engine job, and a seed takes both, as the API does.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func aeField(form *console.EditForm, name string) (console.Field, bool) {
	for _, f := range form.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return console.Field{}, false
}

// Edit queue sets the routing override through UpdateQueue, GetQueue reads
// it back with its computed host, the Configuration tab shows it, and
// emptying its three fields clears it.
func TestTasksQueueEditAppEngineRoutingOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)
	d, err := p.Detail(ctx, "edit-proj", []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("queue detail offers no edit form: %v", err)
	}
	for _, f := range []string{"appEngineService", "appEngineVersion", "appEngineInstance"} {
		field, ok := aeField(d.Edit, f)
		if !ok || field.Section != "App Engine routing override" || field.Default != "" {
			t.Errorf("Edit queue's %s = %+v, %v", f, field, ok)
		}
	}
	values := submittedValues(d.Edit)
	values["appEngineService"], values["appEngineVersion"], values["appEngineInstance"] = "api", "v2", "i1"
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	q, err := p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	r := q.GetAppEngineRoutingOverride()
	if r.GetService() != "api" || r.GetVersion() != "v2" || r.GetInstance() != "i1" ||
		r.GetHost() != "i1.v2.api.edit-proj.appspot.com" {
		t.Errorf("GetQueue's override after the edit = %v", r)
	}

	d, err = p.Detail(ctx, "edit-proj", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := aeField(d.Edit, "appEngineService"); f.Default != "api" {
		t.Errorf("the form is not prefilled with the override: %+v", f)
	}
	shown := false
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			if g.Heading != "App Engine routing override" {
				continue
			}
			for _, prop := range g.Properties {
				shown = shown || (prop.Label == "Host" && prop.Value == "i1.v2.api.edit-proj.appspot.com")
			}
		}
	}
	if !shown {
		t.Error("the queue's Configuration does not show the override's host")
	}

	values = submittedValues(d.Edit)
	values["appEngineService"], values["appEngineVersion"], values["appEngineInstance"] = "", "", ""
	if err := p.Edit(ctx, "edit-proj", []string{name}, values); err != nil {
		t.Fatalf("clearing Edit: %v", err)
	}
	q, err = p.api().GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if q.GetAppEngineRoutingOverride() != nil {
		t.Errorf("the override survived emptying its fields: %v", q.GetAppEngineRoutingOverride())
	}
}

// Edit job is offered for an App Engine job: prefilled from it, with a
// redacted header not shown and kept, and saved through UpdateJob, which the
// official client then reads back. UpdateJob's refusals are the form's.
func TestSchedulerEditAppEngineJob(t *testing.T) {
	ctx := context.Background()
	p, c := schedulerEditFixture(t)
	const project = "edit-proj"
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/jobs/cron"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: name, Schedule: "0 * * * *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			HttpMethod: schedulerpb.HttpMethod_POST, RelativeUri: "/tasks/a", Body: []byte("b1"),
			Headers:          map[string]string{"Authorization": "Bearer s3cret", "X-Kind": "a"},
			AppEngineRouting: &schedulerpb.AppEngineRouting{Service: "worker"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	form := schedulerEditOf(t, p, project, name)
	if f, _ := aeField(form, "targetType"); f.Default != "App Engine" || !f.Immutable {
		t.Errorf("target type field = %+v", f)
	}
	for _, f := range []string{"uri", "topic", "attributes"} {
		if _, ok := aeField(form, f); ok {
			t.Errorf("an App Engine job's form offers %s", f)
		}
	}
	values := schedulerSubmitted(form)
	if values["relativeUri"] != "/tasks/a" || values["httpMethod"] != "POST" || values["body"] != "b1" ||
		values["appEngineService"] != "worker" || values["headers"] != `{"X-Kind":"a"}` {
		t.Errorf("the App Engine form is prefilled with %v", values)
	}
	values["relativeUri"] = "/tasks/b?x=1"
	values["httpMethod"] = "PUT"
	values["body"] = "b2"
	values["headers"] = `{"X-Kind":"b"}`
	values["appEngineService"], values["appEngineVersion"], values["appEngineInstance"] = "api", "v3", "i2"
	values["attemptDeadline"] = "1m0s"
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	a := j.GetAppEngineHttpTarget()
	r := a.GetAppEngineRouting()
	if a.GetRelativeUri() != "/tasks/b?x=1" || a.GetHttpMethod() != schedulerpb.HttpMethod_PUT || string(a.GetBody()) != "b2" ||
		a.GetHeaders()["X-Kind"] != "b" || a.GetHeaders()["Authorization"] != "Bearer s3cret" ||
		r.GetService() != "api" || r.GetVersion() != "v3" || r.GetInstance() != "i2" ||
		r.GetHost() != "i2.v3.api."+project+".appspot.com" || j.GetAttemptDeadline().AsDuration().Seconds() != 60 {
		t.Errorf("GetJob reads %v after the edit", j)
	}

	values["httpMethod"] = "GET"
	if err := p.Edit(ctx, project, []string{name}, values); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a GET with a body = %v; want UpdateJob's INVALID_ARGUMENT", err)
	}
	values["httpMethod"], values["targetType"] = "PUT", "HTTP"
	if err := p.Edit(ctx, project, []string{name}, values); err == nil || !strings.Contains(err.Error(), "target type") {
		t.Errorf("changing the target type = %v", err)
	}
}

// A seed takes a queue object with its appEngineRoutingOverride, stored as
// CreateQueue stores it; a plain name still works beside it.
func TestTasksSeedAppEngineRoutingOverride(t *testing.T) {
	st := tasks.NewStore(store.NewMemory())
	s := &tasksSeeder{svc: &tasksService{store: st}}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}
	seedTwice(t, seed, `{"queues": ["projects/seed-proj/locations/us-central1/queues/plain",
		{"name": "projects/seed-proj/locations/us-central1/queues/routed",
		 "appEngineRoutingOverride": {"service": "api", "version": "v1"}}]}`)
	q, err := st.GetQueue("projects/seed-proj/locations/us-central1/queues/routed")
	if err != nil {
		t.Fatal(err)
	}
	if r := q.AppEngineRouting; r == nil || r.Service != "api" || r.Version != "v1" || r.Instance != "" {
		t.Errorf("seeded override = %+v", q.AppEngineRouting)
	}
	if q, err := st.GetQueue("projects/seed-proj/locations/us-central1/queues/plain"); err != nil || q.AppEngineRouting != nil {
		t.Errorf("plain queue = %+v, %v", q, err)
	}
	for doc, want := range map[string]string{
		`{"queues": [{"name": "work"}]}`: "queues[0]",
		`{"queues": [{"name": "projects/p/locations/l/queues/q", "appEngineRoutingOverride": {"host": "h"}}]}`: "host",
		`{"queues": [{"name": "projects/p/locations/l/queues/q", "state": "PAUSED"}]}`:                         "state",
	} {
		if err := s.Validate(json.RawMessage(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want an error naming %q", doc, err, want)
		}
	}
}

// A seed takes a Scheduler job's appEngineHttpTarget, created through
// CreateJob and read back by GetJob with its computed host.
func TestSchedulerSeedAppEngineJob(t *testing.T) {
	svc := startedScheduler()
	s := &schedulerSeeder{svc: svc}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}
	seedTwice(t, seed, `{"jobs": [{"name": "projects/seed-proj/locations/us-central1/jobs/gae",
		"schedule": "0 * * * *", "appEngineHttpTarget": {"httpMethod": "PUT", "relativeUri": "/cron",
		"headers": {"X-Kind": "seed"}, "bodyBase64": "aGk=", "appEngineRouting": {"service": "worker", "version": "v2"}}}]}`)
	j, err := svc.api.GetJob(context.Background(), &schedulerpb.GetJobRequest{Name: "projects/seed-proj/locations/us-central1/jobs/gae"})
	if err != nil {
		t.Fatal(err)
	}
	a := j.GetAppEngineHttpTarget()
	if a.GetHttpMethod() != schedulerpb.HttpMethod_PUT || a.GetRelativeUri() != "/cron" || string(a.GetBody()) != "hi" ||
		a.GetHeaders()["X-Kind"] != "seed" || a.GetAppEngineRouting().GetService() != "worker" ||
		a.GetAppEngineRouting().GetHost() != "v2.worker.seed-proj.appspot.com" || j.GetState() != schedulerpb.Job_ENABLED {
		t.Errorf("seeded App Engine job = %v", j)
	}
}

// TestTasksSeedNamesWhyAQueueNameIsRefused (#1583): docs/configuration.md's
// seed example named project "p", which is too short to be a project ID; the
// refusal said only that the name was not projects/.../queues/{queue}, which
// it plainly was.
func TestTasksSeedNamesWhyAQueueNameIsRefused(t *testing.T) {
	s := &tasksSeeder{svc: &tasksService{store: tasks.NewStore(store.NewMemory())}}
	err := s.Validate(json.RawMessage(`{"queues": ["projects/p/locations/us-central1/queues/work"]}`))
	if err == nil || !strings.Contains(err.Error(), `project "p"`) {
		t.Errorf("Validate = %v, want the invalid project named", err)
	}
}
