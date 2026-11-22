package main

import (
	"strings"
	"testing"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// testEventarcCatalog is a catalog as the served ListProviders and
// ListChannels give one.
func testEventarcCatalog() eventarcPaneCatalog {
	loc := "projects/p/locations/us-central1"
	audit := &eventarcpb.EventType{Type: eventarcAuditEvent, FilteringAttributes: []*eventarcpb.FilteringAttribute{
		{Attribute: "type", Required: true},
		{Attribute: "serviceName", Required: true},
		{Attribute: "methodName", Required: true},
		{Attribute: "resourceName", PathPatternSupported: true, Description: "The resource"},
	}}
	return eventarcPaneCatalog{
		providers: []*eventarcpb.Provider{
			{Name: loc + "/providers/storage.googleapis.com", DisplayName: "Cloud Storage", EventTypes: []*eventarcpb.EventType{
				{Type: "google.cloud.storage.object.v1.finalized", FilteringAttributes: []*eventarcpb.FilteringAttribute{
					{Attribute: "type", Required: true}, {Attribute: "bucket", Required: true, Description: "The bucket's name"}}},
				audit}},
			{Name: loc + "/providers/pubsub.googleapis.com", DisplayName: "Cloud Pub/Sub", EventTypes: []*eventarcpb.EventType{
				{Type: functionPubSubEvent, FilteringAttributes: []*eventarcpb.FilteringAttribute{{Attribute: "type", Required: true}}},
				audit}},
		},
		custom:     []*eventarcpb.Channel{{Name: loc + "/channels/orders"}},
		thirdParty: []*eventarcpb.Channel{{Name: loc + "/channels/partner", Provider: loc + "/providers/acme"}},
	}
}

func paneField(t *testing.T, fields []console.Field, name string) console.Field {
	t.Helper()
	for _, f := range fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q", name)
	return console.Field{}
}

func TestEventarcPaneIsGooglesPane(t *testing.T) {
	fields := eventarcPane(testEventarcCatalog(), "us-central1")
	var order []string
	for _, f := range fields {
		if len(order) == 0 || order[len(order)-1] != f.Label {
			order = append(order, f.Label)
		}
	}
	got := strings.Join(order, " | ")
	// Google's order: name, type, provider, event type and its filters,
	// then region, service account, path.
	for _, want := range [][2]string{{"Trigger name", "Trigger type"}, {"Trigger type", "Event provider"},
		{"Event provider", "Event type"}, {"Event type", "Bucket"}, {"Bucket", "Region"},
		{"Region", "Service account"}, {"Service account", "Service URL path"}} {
		if i, j := strings.Index(got, want[0]), strings.LastIndex(got, want[1]); i < 0 || j < 0 || i > j {
			t.Errorf("%q should come before %q: %s", want[0], want[1], got)
		}
	}
	if last := fields[len(fields)-1]; last.Label != "Service URL path" {
		t.Errorf("last field = %q, want Service URL path", last.Label)
	}
	tt := paneField(t, fields, "triggerType")
	if strings.Join(tt.Options, ",") != "Google Sources,Custom,Third-party" || tt.Default != eventarcGoogleSources {
		t.Errorf("Trigger type = %v (default %q)", tt.Options, tt.Default)
	}
	prov := paneField(t, fields, "provider")
	if strings.Join(prov.Options, ",") != "pubsub.googleapis.com,storage.googleapis.com" || prov.Default != "pubsub.googleapis.com" ||
		prov.OptionLabels["storage.googleapis.com"] != "Cloud Storage" || prov.ShowWhen != "triggerType=Google Sources" {
		t.Errorf("Event provider = %+v", prov)
	}
	et := paneField(t, fields, "eventType.storage.googleapis.com")
	if et.ShowWhen != "triggerType=Google Sources&provider=storage.googleapis.com" || et.Default != "google.cloud.storage.object.v1.finalized" ||
		!strings.Contains(et.OptionLabels[eventarcAuditEvent], "via Cloud Audit Logs") {
		t.Errorf("storage Event type = %+v", et)
	}
	bucket := paneField(t, fields, "filter.storage.googleapis.com.bucket")
	if bucket.Label != "Bucket" || !bucket.Required ||
		bucket.ShowWhen != "triggerType=Google Sources&provider=storage.googleapis.com&eventType.storage.googleapis.com=google.cloud.storage.object.v1.finalized" {
		t.Errorf("Bucket = %+v", bucket)
	}
	method := paneField(t, fields, "filter.pubsub.googleapis.com.methodName")
	if method.Label != "Method name" || !strings.HasSuffix(method.ShowWhen, "=google.cloud.audit.log.v1.written") {
		t.Errorf("Method name = %+v", method)
	}
	// Google's audit-log Resource: Any resource, Specific resource or Path
	// pattern, then the name or pattern for the last two (#2093).
	if res := paneField(t, fields, "resource.pubsub.googleapis.com"); res.Label != "Resource" ||
		strings.Join(res.Options, ",") != "Any resource,Specific resource,Path pattern" || res.Default != eventarcAnyResource {
		t.Errorf("Resource = %+v", res)
	}
	if res := paneField(t, fields, "filter.pubsub.googleapis.com.resourceName"); res.Required ||
		!strings.HasSuffix(res.ShowWhen, "&resource.pubsub.googleapis.com=Specific resource|Path pattern") {
		t.Errorf("Resource name = %+v", res)
	}
	for _, f := range fields {
		if strings.HasSuffix(f.Name, ".serviceName") {
			t.Errorf("serviceName is the provider's service, not asked for: %s", f.Name)
		}
	}
	if topic := paneField(t, fields, "topic"); topic.ShowWhen != "triggerType=Google Sources&provider=pubsub.googleapis.com" {
		t.Errorf("topic = %+v", topic)
	}
	if ch := paneField(t, fields, "customChannel"); ch.ShowWhen != "triggerType=Custom" || ch.Options[0] != "projects/p/locations/us-central1/channels/orders" {
		t.Errorf("Custom channel = %+v", ch)
	}
	if ch := paneField(t, fields, "thirdPartyChannel"); ch.OptionLabels[ch.Options[0]] != "partner (acme)" {
		t.Errorf("Third-party channel = %+v", ch)
	}
	if r := paneField(t, fields, "region"); r.Default != "us-central1" {
		t.Errorf("Region = %+v", r)
	}
}

func TestEventarcPaneSaysWhyItIsEmpty(t *testing.T) {
	fields := eventarcPane(eventarcPaneCatalog{err: "cannot list Eventarc's event providers: down"}, "us-central1")
	if p := paneField(t, fields, "provider"); !strings.Contains(p.Help, "down") || len(p.Options) != 0 {
		t.Errorf("provider = %+v", p)
	}
	if ch := paneField(t, fields, "customChannel"); ch.OptionLabels[""] != "No channel in us-central1" {
		t.Errorf("channel = %+v", ch)
	}
}

func TestEventarcPaneTriggerIsTheRequest(t *testing.T) {
	cat := testEventarcCatalog()
	tr, err := eventarcPaneTrigger(cat, "p", "us-central1", "fn", map[string]string{
		"name": "t1", "triggerType": eventarcGoogleSources, "provider": "storage.googleapis.com",
		"eventType.storage.googleapis.com":     "google.cloud.storage.object.v1.finalized",
		"filter.storage.googleapis.com.bucket": "b", "serviceAccount": "sa@p.iam.gserviceaccount.com", "path": "/route"})
	if err != nil {
		t.Fatal(err)
	}
	run := tr.GetDestination().GetCloudRun()
	if run.GetService() != "fn" || run.GetRegion() != "us-central1" || run.GetPath() != "/route" ||
		tr.GetServiceAccount() != "sa@p.iam.gserviceaccount.com" || len(tr.GetEventFilters()) != 2 ||
		tr.GetEventFilters()[1].GetAttribute() != "bucket" || tr.GetEventFilters()[1].GetValue() != "b" {
		t.Errorf("trigger = %v", tr)
	}

	tr, err = eventarcPaneTrigger(cat, "p", "us-central1", "fn", map[string]string{"triggerType": eventarcGoogleSources,
		"provider": "pubsub.googleapis.com", "eventType.pubsub.googleapis.com": eventarcAuditEvent,
		"filter.pubsub.googleapis.com.methodName":   "google.pubsub.v1.Publisher.CreateTopic",
		"filter.pubsub.googleapis.com.resourceName": "projects/p/topics/*"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range tr.GetEventFilters() {
		got[f.GetAttribute()] = f.GetValue() + "/" + f.GetOperator()
	}
	if got["serviceName"] != "pubsub.googleapis.com/" || got["resourceName"] != "projects/p/topics/*/"+functionPathPatternOp {
		t.Errorf("audit filters = %v", got)
	}

	tr, err = eventarcPaneTrigger(cat, "p", "us-central1", "fn", map[string]string{"triggerType": eventarcGoogleSources,
		"provider": "pubsub.googleapis.com", "eventType.pubsub.googleapis.com": functionPubSubEvent, "topic": "orders"})
	if err != nil || tr.GetTransport().GetPubsub().GetTopic() != "projects/p/topics/orders" {
		t.Errorf("pubsub trigger = %v, %v", tr, err)
	}

	tr, err = eventarcPaneTrigger(cat, "p", "us-central1", "fn", map[string]string{"triggerType": eventarcCustom,
		"customChannel": "projects/p/locations/us-central1/channels/orders", "customEventType": "com.example.order.created"})
	if err != nil || tr.GetChannel() != "projects/p/locations/us-central1/channels/orders" || tr.GetEventFilters()[0].GetValue() != "com.example.order.created" {
		t.Errorf("custom trigger = %v, %v", tr, err)
	}

	for name, v := range map[string]map[string]string{
		"missing bucket":  {"provider": "storage.googleapis.com", "eventType.storage.googleapis.com": "google.cloud.storage.object.v1.finalized"},
		"unknown type":    {"provider": "storage.googleapis.com", "eventType.storage.googleapis.com": "nope"},
		"unknown provide": {"provider": "acme"},
		"other region":    {"provider": "pubsub.googleapis.com", "eventType.pubsub.googleapis.com": functionPubSubEvent, "region": "europe-west1"},
		"third-party channel as custom": {"triggerType": eventarcCustom,
			"customChannel": "projects/p/locations/us-central1/channels/partner", "customEventType": "x"},
	} {
		if _, err := eventarcPaneTrigger(cat, "p", "us-central1", "fn", v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
