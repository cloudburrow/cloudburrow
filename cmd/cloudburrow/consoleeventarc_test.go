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
	tr, err = eaFormTrigger("demo", "us-central1", map[string]string{"eventType": "google.cloud.firestore.document.v1.written", "service": "hello",
		"document": "users/{userId}"})
	if f := tr.GetEventFilters(); err != nil || len(f) != 3 || f[1].GetValue() != "(default)" || f[2].GetOperator() != "match-path-pattern" {
		t.Errorf("Cloud Firestore trigger = %v, %v", tr, err)
	}
	tr, err = eaFormTrigger("demo", "us-central1", map[string]string{"eventType": "google.cloud.pubsub.topic.v1.messagePublished",
		"destination": "Cloud Run functions", "function": "fn"})
	if err != nil || tr.GetDestination().GetCloudFunction() != "projects/demo/locations/us-central1/functions/fn" {
		t.Errorf("function destination = %v, %v", tr, err)
	}
	tr, err = eaFormTrigger("demo", "us-central1", map[string]string{"eventType": "google.cloud.pubsub.topic.v1.messagePublished",
		"destination": "HTTP endpoint", "uri": "http://10.0.0.1/e", "networkAttachment": "projects/demo/regions/us-central1/networkAttachments/a"})
	if err != nil || tr.GetDestination().GetHttpEndpoint().GetUri() != "http://10.0.0.1/e" || tr.GetDestination().GetNetworkConfig() == nil {
		t.Errorf("HTTP endpoint destination = %v, %v", tr, err)
	}
	for _, bad := range []map[string]string{
		{"eventType": "google.cloud.pubsub.topic.v1.messagePublished", "destination": "HTTP endpoint", "uri": "http://x"},
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

// Create trigger's Datastore mode event types take Database, Namespace and
// Entity (a path pattern unless Exact), and its Google Kubernetes Engine
// destination the cluster, namespace, service and path (#1877).
func TestEventarcConsoleDatastoreAndGKE(t *testing.T) {
	tr, err := eaFormTrigger("p", "us-central1", map[string]string{"eventType": "google.cloud.datastore.entity.v1.written",
		"database": "(default)", "namespace": "shop", "entity": "users/{u}", "destination": "Google Kubernetes Engine",
		"gkeCluster": "c1", "gkeNamespace": "events", "gkeService": "probe", "path": "/in"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range tr.GetEventFilters() {
		got = append(got, f.GetAttribute()+"="+f.GetValue()+"/"+f.GetOperator())
	}
	if strings.Join(got, ",") != "type=google.cloud.datastore.entity.v1.written/,database=(default)/,namespace=shop/,entity=users/{u}/match-path-pattern" {
		t.Errorf("filters = %v", got)
	}
	g := tr.GetDestination().GetGke()
	if g.GetCluster() != "c1" || g.GetLocation() != "us-central1" || g.GetNamespace() != "events" || g.GetService() != "probe" || g.GetPath() != "/in" {
		t.Errorf("GKE destination = %v", g)
	}
	if kind, target, path := eaDestination(tr.GetDestination()); kind != "Google Kubernetes Engine" || target != "events/probe" || path != "/in" {
		t.Errorf("shown as %q %q %q", kind, target, path)
	}
	if _, err := eaFormDestination("p", "us-central1", map[string]string{"destination": "Google Kubernetes Engine", "gkeCluster": "c1"}); err == nil {
		t.Error("a GKE destination with no service was accepted")
	}
}
