package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func hasProperty(props []console.Property, label, value string) bool {
	for _, p := range props {
		if p.Label == label && p.Value == value {
			return true
		}
	}
	return false
}

// Create task and Create job take an OIDC or OAuth token (#1243), which the
// API keeps and the detail page shows; Edit job keeps the token, and a
// field of the other kind of token is refused rather than dropped.
func TestConsoleTaskAndJobTokens(t *testing.T) {
	ctx := context.Background()

	st := tasks.NewStore(store.NewMemory())
	tp := tasksProvider{svc: &tasksService{store: st}}
	queue := "projects/demo-proj/locations/us-central1/queues/q"
	if _, err := tasks.NewGRPCServer(st).CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/demo-proj/locations/us-central1", Queue: &cloudtaskspb.Queue{Name: queue}}); err != nil {
		t.Fatal(err)
	}
	form := map[string]string{"url": "http://127.0.0.1:1/x", "httpMethod": "POST", "taskId": "t1",
		"scheduleTime": "2099-01-01T00:00:00Z", "authHeader": authOIDC, "serviceAccountEmail": "sa@demo-proj.iam.gserviceaccount.com"}
	if err := tp.createTask(ctx, "demo-proj", queue, form); err != nil {
		t.Fatalf("Create task with an OIDC token: %v", err)
	}
	task, err := st.GetTask(queue + "/tasks/t1")
	if err != nil {
		t.Fatal(err)
	}
	if o := task.HTTPRequest.OIDCToken; o == nil || o.ServiceAccountEmail != "sa@demo-proj.iam.gserviceaccount.com" || o.Audience != "" {
		t.Errorf("stored oidcToken = %+v", o)
	}
	d, err := tp.Detail(ctx, "demo-proj", []string{queue, queue + "/tasks/t1"})
	if err != nil {
		t.Fatal(err)
	}
	var shown []console.Property
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			shown = append(shown, g.Properties...)
		}
	}
	if !hasProperty(shown, "Auth header", "OIDC token") || !hasProperty(shown, "Audience", "http://127.0.0.1:1/x (the URL)") {
		t.Errorf("the task page shows %v; want its OIDC token", shown)
	}
	bad := map[string]string{"url": "http://127.0.0.1:1/", "httpMethod": "POST", "authHeader": authOIDC,
		"serviceAccountEmail": "sa@p", "scope": "s"}
	if err := tp.createTask(ctx, "demo-proj", queue, bad); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Errorf("an OIDC token with a scope = %v; want it refused naming scope", err)
	}
	if err := tp.createTask(ctx, "demo-proj", queue, map[string]string{"url": "http://127.0.0.1:1/", "httpMethod": "POST",
		"authHeader": authOAuth}); err == nil {
		t.Error("an OAuth token without a service account was accepted")
	}

	svc := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	sp := schedulerProvider{svc: svc}
	name, err := sp.Create(ctx, "demo-proj", map[string]string{"name": "j", "location": "us-central1",
		"schedule": "0 3 * * *", "timeZone": "Etc/UTC", "uri": "http://127.0.0.1:1/",
		"authHeader": authOAuth, "serviceAccountEmail": "sa@p", "scope": "https://www.googleapis.com/auth/pubsub"})
	if err != nil {
		t.Fatalf("Create job with an OAuth token: %v", err)
	}
	j, err := svc.Store().Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if o := j.HTTP.OAuthToken; o == nil || o.ServiceAccountEmail != "sa@p" || o.Scope != "https://www.googleapis.com/auth/pubsub" {
		t.Fatalf("stored oauthToken = %+v", o)
	}
	if _, err := sp.Create(ctx, "demo-proj", map[string]string{"name": "ps", "schedule": "0 3 * * *",
		"topic": "projects/demo-proj/topics/t", "body": "x", "authHeader": authOIDC, "serviceAccountEmail": "sa@p"}); err == nil {
		t.Error("a Pub/Sub job with a token was accepted")
	}

	edit := schedulerEditForm(j)
	values := map[string]string{}
	for _, f := range edit.Fields {
		values[f.Name] = f.Default
	}
	if values["authHeader"] != authOAuth || values["serviceAccountEmail"] != "sa@p" {
		t.Fatalf("Edit job is not prefilled with the token: %v", values)
	}
	values["description"] = "edited"
	if err := sp.Edit(ctx, "demo-proj", []string{name}, values); err != nil {
		t.Fatalf("Edit job: %v", err)
	}
	if j, _ = svc.Store().Get(name); j.Description != "edited" || j.HTTP.OAuthToken == nil || j.HTTP.OAuthToken.Scope != "https://www.googleapis.com/auth/pubsub" {
		t.Errorf("after Edit job: description %q, oauthToken %+v; want the token kept", j.Description, j.HTTP.OAuthToken)
	}
}
