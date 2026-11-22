package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestEventarcCreatePipelineIsGooglesPage (#2098): Create pipeline's
// sections are Google's, in its order, with Add an enrollment; the Cloud
// Run job destination is jobs.run with its message binding and an OAuth
// token, the Cloud Run service destination the service's URL, each read
// back as what it was made as; Apply a transformation unchecked sends no
// mediation; Add an enrollment's rows are checked before anything is made.
func TestEventarcCreatePipelineIsGooglesPage(t *testing.T) {
	runs := eaRunChoices{services: map[string]string{"sink": "http://sink.demo.127.0.0.1.sslip.io"}, jobs: []string{"on-order"}}
	fields := eaPipelineFields(nil, runs, true, []string{"orders"})
	var sections []string
	for _, f := range fields {
		if len(sections) == 0 || sections[len(sections)-1] != f.Section {
			sections = append(sections, f.Section)
		}
		if f.Name == "enrollments" && (f.AddLabel != "Add an enrollment" || f.Group[1].Options[0] != "orders") {
			t.Errorf("Add an enrollment = %+v", f)
		}
		if f.Name == "runService" && (f.Type != "select" || f.Options[0] != "sink") {
			t.Errorf("Cloud Run service = %+v", f)
		}
	}
	if got := strings.Join(sections, "|"); got != "Pipeline details|Enrollments|Event mediation|Destination" {
		t.Errorf("sections = %s; want Google's", got)
	}
	if got := strings.Join(eaPipelineDestinations, "|"); got != "HTTP endpoint|Eventarc Advanced bus|Workflows|Pub/Sub topic|Cloud Run service (via HTTP)|Cloud Run job (via HTTP)" {
		t.Errorf("destination types = %s", got)
	}

	job := map[string]string{"destination": eaDestJob, "runJob": "on-order", "authEnabled": "true", "authType": eaAuthOAuth,
		"serviceAccount": "runner@demo.iam.gserviceaccount.com", "transform": "false", "inputFormat": "JSON"}
	if _, err := eaFormPipeline("demo", "us-central1", job); err == nil || !strings.Contains(err.Error(), "message binding") {
		t.Errorf("a job destination without a message binding = %v", err)
	}
	job["messageBinding"] = eaJobBindingExample
	pl, err := eaFormPipeline("demo", "us-central1", job)
	if err != nil {
		t.Fatal(err)
	}
	d := pl.GetDestinations()[0]
	if d.GetHttpEndpoint().GetUri() != "https://run.googleapis.com/v2/projects/demo/locations/us-central1/jobs/on-order:run" ||
		d.GetHttpEndpoint().GetMessageBindingTemplate() != eaJobBindingExample ||
		d.GetAuthenticationConfig().GetOauthToken().GetServiceAccount() != "runner@demo.iam.gserviceaccount.com" {
		t.Errorf("job destination = %v", d)
	}
	if pl.GetInputPayloadFormat() != nil || len(pl.GetMediations()) != 0 {
		t.Errorf("Apply a transformation unchecked still sent %v %v", pl.GetInputPayloadFormat(), pl.GetMediations())
	}
	pl.Name = "projects/demo/locations/us-central1/pipelines/p"
	if v := eaPipelineValues(pl, runs); v["destination"] != eaDestJob || v["runJob"] != "on-order" || v["authType"] != eaAuthOAuth || v["authEnabled"] != "true" {
		t.Errorf("job read back as %v", v)
	}

	svc, err := eaFormPipeline("demo", "us-central1", map[string]string{"destination": eaDestService, "runService": "sink",
		"uri": "http://sink.demo.127.0.0.1.sslip.io/events", "authEnabled": "true", "authType": eaAuthOIDC,
		"serviceAccount": "inv@demo.iam.gserviceaccount.com", "transform": "true", "inputFormat": "JSON", "transformation": "message"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.GetDestinations()[0].GetAuthenticationConfig().GetGoogleOidc().GetServiceAccount() == "" || svc.GetInputPayloadFormat().GetJson() == nil {
		t.Errorf("service pipeline = %v", svc)
	}
	if kind, target := runs.destination(svc.GetDestinations()[0]); kind != eaDestService || target != "sink/events" {
		t.Errorf("service destination reads %s %s", kind, target)
	}
	if v := eaPipelineValues(svc, runs); v["runService"] != "sink" || v["urlPath"] != "/events" || v["transform"] != "true" {
		t.Errorf("service read back as %v", v)
	}
	if _, err := eaFormPipeline("demo", "us-central1", map[string]string{"destination": eaDestHTTP, "uri": "https://x.example",
		"authEnabled": "true"}); err == nil {
		t.Error("authentication without a service account was accepted")
	}

	rows := console.FormatGroup([]map[string]string{{"name": "all", "bus": "orders", "celMatch": ""},
		{"name": "created", "bus": "orders", "celMatch": `message.type == "order.created"`}})
	ens, order, err := eaFormEnrollments("demo", "us-central1", "p", map[string]string{"enrollments": rows})
	if err != nil || strings.Join(order, ",") != "all,created" || ens["all"].GetCelMatch() != "true" ||
		ens["created"].GetDestination() != "projects/demo/locations/us-central1/pipelines/p" ||
		ens["created"].GetMessageBus() != "projects/demo/locations/us-central1/messageBuses/orders" {
		t.Errorf("enrollments = %v %v %v", ens, order, err)
	}
	for _, bad := range [][]map[string]string{
		{{"name": "Bad_Name", "bus": "orders"}},
		{{"name": "a", "bus": ""}},
		{{"name": "a", "bus": "orders"}, {"name": "a", "bus": "orders"}},
	} {
		if _, _, err := eaFormEnrollments("demo", "us-central1", "p", map[string]string{"enrollments": console.FormatGroup(bad)}); err == nil {
			t.Errorf("enrollments %v were accepted", bad)
		}
	}
}
