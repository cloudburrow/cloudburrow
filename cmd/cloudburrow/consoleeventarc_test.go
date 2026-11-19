package main

import (
	"strings"
	"testing"
)

// TestEventarcConsoleTriggers (#1877): Create trigger's form is Google's
// (trigger name and type, event type, region, service account, event
// destination, labels), and its values are the trigger CreateTrigger takes;
// what the API would refuse is refused on the form.
func TestEventarcConsoleTriggers(t *testing.T) {
	p := eventarcProvider{run: runProvider{defaultProject: "demo"}}
	label, fields := p.CreateForm()
	names := []string{}
	for _, f := range fields {
		names = append(names, f.Label)
	}
	got := strings.Join(names, "|")
	for _, want := range []string{"Trigger name", "Trigger type", "Event type", "Region", "Service account", "Event destination", "Service URL path", "Labels"} {
		if !strings.Contains(got, want) {
			t.Errorf("Create trigger has no %q: %s", want, got)
		}
	}
	if label != "Create trigger" || p.CreateTitle() != "Create trigger" || p.CreateSubmit() != "Create" || !p.CreateOnPage() {
		t.Errorf("Create trigger's labels = %q, %q, %q", label, p.CreateTitle(), p.CreateSubmit())
	}
	tr, err := eaFormTrigger("demo", "us-central1", map[string]string{"eventType": "google.cloud.pubsub.topic.v1.messagePublished",
		"topic": "orders", "service": "hello", "path": "/e", "labels": `{"team":"a"}`})
	if err != nil || tr.GetTransport().GetPubsub().GetTopic() != "projects/demo/topics/orders" || tr.GetDestination().GetCloudRun().GetService() != "hello" ||
		tr.GetDestination().GetCloudRun().GetRegion() != "us-central1" || tr.GetLabels()["team"] != "a" {
		t.Errorf("Pub/Sub trigger = %v, %v", tr, err)
	}
	tr, err = eaFormTrigger("demo", "us-central1", map[string]string{"eventType": "google.cloud.storage.object.v1.finalized", "bucket": "b1", "service": "hello"})
	if err != nil || len(tr.GetEventFilters()) != 2 || tr.GetEventFilters()[1].GetValue() != "b1" {
		t.Errorf("Cloud Storage trigger = %v, %v", tr, err)
	}
	tr, err = eaFormTrigger("demo", "us-central1", map[string]string{"eventType": eaAuditLogType, "service": "hello",
		"serviceName": "secretmanager.googleapis.com", "methodName": "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret",
		"resource": "Path pattern", "resourceName": "projects/demo/secrets/{s}"})
	if f := tr.GetEventFilters(); err != nil || len(f) != 4 || f[3].GetOperator() != "match-path-pattern" || f[3].GetValue() != "projects/demo/secrets/{s}" ||
		eaProviderLabel(tr) != "Cloud Audit Logs: secretmanager.googleapis.com" {
		t.Errorf("Cloud Audit Logs trigger = %v, %v", tr, err)
	}
	for _, bad := range []map[string]string{
		{"eventType": eaAuditLogType, "service": "hello", "serviceName": "secretmanager.googleapis.com"},
		{"eventType": "google.cloud.storage.object.v1.finalized", "service": "hello"},
		{"eventType": "google.cloud.pubsub.topic.v1.messagePublished"},
		{"eventType": "google.cloud.pubsub.topic.v1.messagePublished", "service": "hello", "bucket": "b"},
		{"eventType": "google.example.unknown", "service": "hello"},
	} {
		if _, err := eaFormTrigger("demo", "us-central1", bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
