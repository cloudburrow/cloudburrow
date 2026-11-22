package main

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// servedEventarcCatalog is the catalog the adapter's own Eventarc serves,
// read through the official client as the console reads it.
func servedEventarcCatalog(t *testing.T) eventarcPaneCatalog {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	fs := s.Functions(s.Builds(&memObjects{objs: map[string][]byte{}}, failingBuilder{}), func() string { return "" }).
		WithEvents(runadapter.Events{Transport: noTransport{}, PushBase: func() string { return "" }})
	srv := grpc.NewServer()
	fs.Eventarc().Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	c, err := eventarc.NewClient(ctx, option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	cat := eventarcCatalogFrom(ctx, c, "pane-2093", "us-central1")
	if cat.err != "" {
		t.Fatal(cat.err)
	}
	return cat
}

// TestEventarcPaneOffersEveryServedProvider (#2093): Add trigger, on a
// function's and a Cloud Run service's Triggers tab alike, offers every
// provider the served Eventarc lists, each with its event types: Cloud
// Firestore's document and Datastore mode entity events, Cloud Run
// functions' and Eventarc's own direct events, and the Cloud Audit Logs
// event of each served service; Firebase's providers, which are not
// served, are left out rather than shown.
func TestEventarcPaneOffersEveryServedProvider(t *testing.T) {
	cat := servedEventarcCatalog(t)
	fields := eventarcPane(cat, "us-central1")
	prov := paneField(t, fields, "provider")
	for _, want := range []string{"pubsub.googleapis.com", "storage.googleapis.com", "firestore.googleapis.com",
		"cloudfunctions.googleapis.com", "eventarc.googleapis.com", "secretmanager.googleapis.com", "run.googleapis.com"} {
		if !slices.Contains(prov.Options, want) {
			t.Errorf("Event provider lacks %s: %v", want, prov.Options)
		}
	}
	for _, o := range prov.Options {
		if strings.Contains(o, "firebase") {
			t.Errorf("Event provider offers %s, which is not served", o)
		}
	}
	if prov.OptionLabels["firestore.googleapis.com"] != "Cloud Firestore" {
		t.Errorf("Cloud Firestore's label = %q", prov.OptionLabels["firestore.googleapis.com"])
	}
	types := func(provider string) []string { return paneField(t, fields, eventarcEventTypeField+provider).Options }
	fsTypes := strings.Join(types("firestore.googleapis.com"), ",")
	for _, want := range []string{"google.cloud.firestore.document.v1.created", "google.cloud.firestore.document.v1.written",
		"google.cloud.datastore.entity.v1.created", eventarcAuditEvent} {
		if !strings.Contains(fsTypes, want) {
			t.Errorf("Cloud Firestore's event types lack %s: %s", want, fsTypes)
		}
	}
	if !slices.Contains(types("cloudfunctions.googleapis.com"), "google.cloud.functions.function.v2.created") ||
		!slices.Contains(types("eventarc.googleapis.com"), "google.cloud.eventarc.trigger.v1.created") {
		t.Errorf("direct events: functions %v, eventarc %v", types("cloudfunctions.googleapis.com"), types("eventarc.googleapis.com"))
	}
	if got := types("secretmanager.googleapis.com"); len(got) != 1 || got[0] != eventarcAuditEvent {
		t.Errorf("Secret Manager's event types = %v; want its audit-log event", got)
	}
	// Each type's documented filters: Firestore's database, namespace and
	// document; Datastore mode's entity; the audit log's method name and
	// Resource; a direct event's resource ID.
	for _, name := range []string{"filter.firestore.googleapis.com.database", "filter.firestore.googleapis.com.namespace",
		"filter.firestore.googleapis.com.document", "filter.firestore.googleapis.com.entity",
		"filter.secretmanager.googleapis.com.methodName", "resource.secretmanager.googleapis.com",
		"filter.secretmanager.googleapis.com.resourceName", "filter.eventarc.googleapis.com.trigger",
		"filter.cloudfunctions.googleapis.com.function"} {
		paneField(t, fields, name)
	}
	if doc := paneField(t, fields, "filter.firestore.googleapis.com.document"); !strings.Contains(doc.Help, "path pattern") ||
		!strings.Contains(doc.ShowWhen, "google.cloud.firestore.document.v1.created") || strings.Contains(doc.ShowWhen, "datastore") {
		t.Errorf("Document = %+v", doc)
	}
	if db := paneField(t, fields, "filter.firestore.googleapis.com.database"); !db.Required ||
		!strings.Contains(db.ShowWhen, "google.cloud.datastore.entity.v1.created") {
		t.Errorf("Database = %+v", db)
	}

	// Save trigger's requests for each kind, checked against the catalog.
	filters := func(v map[string]string) map[string]string {
		t.Helper()
		v["triggerType"] = eventarcGoogleSources
		tr, err := eventarcPaneTrigger(cat, "p", "us-central1", "svc", v)
		if err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		out := map[string]string{}
		for _, f := range tr.GetEventFilters() {
			out[f.GetAttribute()] = f.GetValue() + "|" + f.GetOperator()
		}
		return out
	}
	got := filters(map[string]string{"provider": "firestore.googleapis.com",
		"eventType.firestore.googleapis.com":       "google.cloud.firestore.document.v1.created",
		"filter.firestore.googleapis.com.database": "(default)", "filter.firestore.googleapis.com.document": "users/{userId}"})
	if got["database"] != "(default)|" || got["document"] != "users/{userId}|"+functionPathPatternOp {
		t.Errorf("Firestore filters = %v", got)
	}
	got = filters(map[string]string{"provider": "firestore.googleapis.com",
		"eventType.firestore.googleapis.com":       "google.cloud.datastore.entity.v1.created",
		"filter.firestore.googleapis.com.database": "(default)", "filter.firestore.googleapis.com.entity": "Task/{id}",
		"filter.firestore.googleapis.com.document": "ignored"})
	if got["entity"] != "Task/{id}|"+functionPathPatternOp || got["document"] != "" {
		t.Errorf("Datastore mode filters = %v", got)
	}
	got = filters(map[string]string{"provider": "eventarc.googleapis.com",
		"eventType.eventarc.googleapis.com": "google.cloud.eventarc.trigger.v1.created", "filter.eventarc.googleapis.com.trigger": "t2"})
	if got["trigger"] != "t2|" || got["type"] != "google.cloud.eventarc.trigger.v1.created|" {
		t.Errorf("Eventarc direct filters = %v", got)
	}
	for res, want := range map[string]string{eventarcAnyResource: "", eventarcSpecificResource: "projects/p/secrets/s|",
		eventarcPathPattern: "projects/p/secrets/s|" + functionPathPatternOp} {
		got = filters(map[string]string{"provider": "secretmanager.googleapis.com",
			"eventType.secretmanager.googleapis.com":         eventarcAuditEvent,
			"filter.secretmanager.googleapis.com.methodName": "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret",
			"resource.secretmanager.googleapis.com":          res, "filter.secretmanager.googleapis.com.resourceName": "projects/p/secrets/s"})
		if got["resourceName"] != want || got["serviceName"] != "secretmanager.googleapis.com|" {
			t.Errorf("Resource %s: filters = %v", res, got)
		}
	}
	if _, err := eventarcPaneTrigger(cat, "p", "us-central1", "svc", map[string]string{"provider": "secretmanager.googleapis.com",
		"eventType.secretmanager.googleapis.com":         eventarcAuditEvent,
		"filter.secretmanager.googleapis.com.methodName": "m", "resource.secretmanager.googleapis.com": eventarcPathPattern}); err == nil {
		t.Error("Path pattern with no pattern was accepted")
	}
}

// TestEventarcTriggerProviderNamesTheProvider (#2093): the Triggers tab's
// Event provider is the catalog's display name, an audit-log trigger's
// its service's, a channel's Custom or its provider.
func TestEventarcTriggerProviderNamesTheProvider(t *testing.T) {
	cat := testEventarcCatalog()
	typed := func(typ string, more ...*eventarcpb.EventFilter) *eventarcpb.Trigger {
		return &eventarcpb.Trigger{EventFilters: append([]*eventarcpb.EventFilter{{Attribute: "type", Value: typ}}, more...)}
	}
	for want, tr := range map[string]*eventarcpb.Trigger{
		"Cloud Storage": typed("google.cloud.storage.object.v1.finalized"),
		"Cloud Pub/Sub": typed(functionPubSubEvent),
		"Cloud Pub/Sub (via Cloud Audit Logs)": typed(eventarcAuditEvent,
			&eventarcpb.EventFilter{Attribute: "serviceName", Value: "pubsub.googleapis.com"}),
		"iam.googleapis.com (via Cloud Audit Logs)": typed(eventarcAuditEvent,
			&eventarcpb.EventFilter{Attribute: "serviceName", Value: "iam.googleapis.com"}),
		eventarcCustom: {Channel: "projects/p/locations/us-central1/channels/orders"},
		"acme":         {Channel: "projects/p/locations/us-central1/channels/partner"},
		"-":            typed("google.example.unknown"),
	} {
		if got := eventarcTriggerProvider(cat, tr); got != want {
			t.Errorf("provider of %v = %q, want %q", tr, got, want)
		}
	}
}

// TestRunAddTriggerIsThePane (#2093): a Cloud Run service's Add trigger is
// the functions' Eventarc pane, its Cloud Pub/Sub topic a select of the
// project's topics with Create a topic.
func TestRunAddTriggerIsThePane(t *testing.T) {
	cat := testEventarcCatalog()
	cat.topics = []string{"orders"}
	topic := paneField(t, eventarcPane(cat, "us-central1"), "topic")
	if topic.Type != "select" || strings.Join(topic.Options, ",") != ",orders" || !strings.Contains(topic.OptionLabels[""], "Create a topic") {
		t.Errorf("topic = %+v", topic)
	}
	cat.topics = nil
	if topic := paneField(t, eventarcPane(cat, "us-central1"), "topic"); topic.Type != "text" {
		t.Errorf("topic with no topics listed = %+v", topic)
	}
}
