package main

import (
	"strings"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestEventarcAdvancedConsoleForms (#1947): the Buses, Pipelines,
// Enrollments, Google API sources and Channels pages' Create forms carry
// Google's fields, and their values are what the Eventarc API takes; what
// the API would refuse is refused on the form. A third-party trigger names
// its channel and event type.
func TestEventarcAdvancedConsoleForms(t *testing.T) {
	base := eaAdvBase{run: runProvider{defaultProject: "demo"}}
	check := func(page, label string, fieldLabels []string, want ...string) {
		t.Helper()
		got := strings.Join(fieldLabels, "|")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s (%s) has no %q: %s", page, label, w, got)
			}
		}
	}
	names := func(label string, fs []console.Field) (string, []string) {
		out := []string{}
		for _, f := range fs {
			out = append(out, f.Label)
		}
		return label, out
	}
	l, f := names(eaBusesProvider{base}.CreateForm())
	check("Buses", l, f, "Bus name", "Region", "Display name", "Log severity", "Cloud KMS key", "Labels")
	l, f = names(eaPipelinesProvider{base}.CreateForm())
	check("Pipelines", l, f, "Pipeline name", "Destination type", "Destination URI", "Network attachment", "Pub/Sub topic", "Message bus",
		"Enrollments", "Apply a transformation", "Inbound format", "Outbound format", "Transformation (CEL)", "Maximum attempts",
		"Minimum retry delay", "Log severity", "Cloud Run service", "Cloud Run job", "Message binding", "Enable authentication")
	l, f = names(eaEnrollmentsProvider{base}.CreateForm())
	check("Enrollments", l, f, "Enrollment name", "Message bus", "CEL expression", "Destination pipeline")
	l, f = names(eaSourcesProvider{base}.CreateForm())
	check("Google API sources", l, f, "Google API source name", "Destination bus", "Log severity")
	l, f = names(eaChannelsProvider{base}.CreateForm())
	check("Channels", l, f, "Channel name", "Region", "Provider")

	b, err := eaFormBus(map[string]string{"displayName": "Orders", "logSeverity": "INFO", "labels": `{"team":"a"}`})
	if err != nil || b.GetLoggingConfig().GetLogSeverity() != eventarcpb.LoggingConfig_INFO || b.GetLabels()["team"] != "a" || b.GetDisplayName() != "Orders" {
		t.Errorf("bus = %v, %v", b, err)
	}
	if _, err := eaFormBus(map[string]string{"logSeverity": "LOUD"}); err == nil {
		t.Error("an unknown log severity was accepted")
	}

	p, err := eaFormPipeline("demo", "us-central1", map[string]string{"destination": "Pub/Sub topic", "topic": "out",
		"inputFormat": "JSON", "outputFormat": "Avro", "outputSchema": `{"type":"record","name":"R","fields":[]}`,
		"transformation": `message.setField("data.seen", true)`, "maxAttempts": "3", "minRetryDelay": "2", "maxRetryDelay": "30", "logSeverity": "DEBUG"})
	if err != nil || p.GetDestinations()[0].GetTopic() != "projects/demo/topics/out" || p.GetInputPayloadFormat().GetJson() == nil ||
		p.GetDestinations()[0].GetOutputPayloadFormat().GetAvro() == nil || p.GetRetryPolicy().GetMaxAttempts() != 3 ||
		p.GetRetryPolicy().GetMinRetryDelay().AsDuration() != 2*time.Second ||
		p.GetMediations()[0].GetTransformation().GetTransformationTemplate() != `message.setField("data.seen", true)` {
		t.Errorf("Pub/Sub pipeline = %v, %v", p, err)
	}
	p, err = eaFormPipeline("demo", "us-central1", map[string]string{"destination": "HTTP endpoint", "uri": "https://svc.example/e",
		"serviceAccount": "sa@demo.iam.gserviceaccount.com"})
	if err != nil || p.GetDestinations()[0].GetHttpEndpoint().GetUri() != "https://svc.example/e" ||
		p.GetDestinations()[0].GetAuthenticationConfig().GetGoogleOidc().GetServiceAccount() != "sa@demo.iam.gserviceaccount.com" || p.GetRetryPolicy() != nil {
		t.Errorf("HTTP pipeline = %v, %v", p, err)
	}
	p, err = eaFormPipeline("demo", "us-central1", map[string]string{"destination": "Message bus", "bus": "b2"})
	if err != nil || p.GetDestinations()[0].GetMessageBus() != "projects/demo/locations/us-central1/messageBuses/b2" {
		t.Errorf("bus pipeline = %v, %v", p, err)
	}
	for _, bad := range []map[string]string{
		{"destination": "HTTP endpoint"},
		{"destination": "Pub/Sub topic"},
		{"destination": "Pub/Sub topic", "topic": "t", "inputFormat": "Avro"},
		{"destination": "Pub/Sub topic", "topic": "t", "maxAttempts": "101"},
		{"destination": "Pub/Sub topic", "topic": "t", "minRetryDelay": "0"},
		{"destination": "Carrier pigeon"},
	} {
		if _, err := eaFormPipeline("demo", "us-central1", bad); err == nil {
			t.Errorf("pipeline %v accepted", bad)
		}
	}

	en, err := eaFormEnrollment("demo", "us-central1", map[string]string{"bus": "b1", "pipeline": "p1", "celMatch": `message.type == "x"`})
	if err != nil || en.GetMessageBus() != "projects/demo/locations/us-central1/messageBuses/b1" ||
		en.GetDestination() != "projects/demo/locations/us-central1/pipelines/p1" || en.GetCelMatch() != `message.type == "x"` {
		t.Errorf("enrollment = %v, %v", en, err)
	}
	if _, err := eaFormEnrollment("demo", "us-central1", map[string]string{"bus": "b1", "pipeline": "p1"}); err == nil {
		t.Error("an enrollment with no CEL expression was accepted")
	}
	s, err := eaFormSource("demo", "us-central1", map[string]string{"bus": "b1", "logSeverity": "ERROR"})
	if err != nil || s.GetDestination() != "projects/demo/locations/us-central1/messageBuses/b1" || s.GetLoggingConfig().GetLogSeverity() != eventarcpb.LoggingConfig_ERROR {
		t.Errorf("Google API source = %v, %v", s, err)
	}
	if _, err := eaFormSource("demo", "us-central1", map[string]string{}); err == nil {
		t.Error("a Google API source with no bus was accepted")
	}

	tr, err := eaFormTrigger("demo", "us-central1", map[string]string{"triggerType": "Third-party", "channel": "orders",
		"customEventType": "order.created", "service": "hello"})
	if err != nil || tr.GetChannel() != "projects/demo/locations/us-central1/channels/orders" || tr.GetEventFilters()[0].GetValue() != "order.created" ||
		tr.GetDestination().GetCloudRun().GetService() != "hello" {
		t.Errorf("third-party trigger = %v, %v", tr, err)
	}
	if _, err := eaFormTrigger("demo", "us-central1", map[string]string{"triggerType": "Third-party", "service": "hello"}); err == nil {
		t.Error("a third-party trigger with no channel was accepted")
	}
	if eaChannelState(eventarcpb.Channel_PENDING) != "Pending" || eaSeverityText(nil) != "NONE" || eaKeyText("") != "Google-managed" {
		t.Error("the pages' value texts are not Google's")
	}
}
