package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
)

// TestWriteFunctionIsGooglesForm (#1853): Write a function asks for the
// Service name (Google's rule: at most 49 characters), Region, Runtime,
// Trigger and Authentication in that order, and its Create goes on to the
// function's Source tab.
func TestWriteFunctionIsGooglesForm(t *testing.T) {
	a := writeFunctionLayout(functionsProvider{storage: true}.createAction())
	var order []string
	for _, f := range a.Fields {
		switch f.Name {
		case "name", "region", "runtime", "trigger", "authentication":
			order = append(order, f.Name)
		}
		if f.Name == "name" && f.Label != "Service name" {
			t.Errorf("the name's label = %q", f.Label)
		}
		if f.Name == "authentication" && (f.Type != "radio" || strings.Join(f.Options, ",") != runAllowPublic+","+runRequireAuth || f.Default != runRequireAuth) {
			t.Errorf("Authentication = %+v", f)
		}
	}
	if strings.Join(order, ",") != "name,region,runtime,trigger,authentication" {
		t.Errorf("Write a function's order = %v", order)
	}
	var sections []string
	for _, f := range a.Fields {
		s := f.Section
		if f.Tab != "" {
			s += "/" + f.Tab
		}
		if len(sections) == 0 || sections[len(sections)-1] != s {
			sections = append(sections, s)
		}
	}
	want := []string{runSectionAuth, runSectionBilling, "Service scaling", runSectionIngress, runSectionMore + "/" + runTabContainers,
		runSectionMore + "/" + runTabVariables, runSectionMore + "/" + runTabVolumes, runSectionMore + "/" + runTabSecurity}
	at := -1
	for _, w := range want {
		i := slices.Index(sections, w)
		if i <= at {
			t.Errorf("Write a function's sections = %v; want %v in order", sections, want)
			break
		}
		at = i
	}
	if a.Then != "/functions/{name}?tab=source" {
		t.Errorf("Then = %q", a.Then)
	}
	re := regexp.MustCompile(runServiceNamePattern)
	for name, ok := range map[string]bool{"hello": true, "a" + strings.Repeat("b", 48): true, "a" + strings.Repeat("b", 49): false,
		"hello-": false, "1hello": false} {
		if re.MatchString(name) != ok {
			t.Errorf("service name %q accepted = %v", name, !ok)
		}
	}
	for _, v := range []string{"", runRequireAuth} {
		if pub, err := functionPublicChoice(v); pub || err != nil {
			t.Errorf("functionPublicChoice(%q) = %v, %v", v, pub, err)
		}
	}
	if pub, err := functionPublicChoice(runAllowPublic); !pub || err != nil {
		t.Errorf("Allow public access = %v, %v", pub, err)
	}
	if _, err := functionPublicChoice("everyone"); err == nil {
		t.Error("an unknown Authentication was accepted")
	}
}

// TestShowPayloadIsTheTriggersEvent (#1853): Show Payload is an HTTP
// function's request body, or a CloudEvent of the trigger's event type
// with its provider's data, or for 1st gen the legacy data and context.
func TestShowPayloadIsTheTriggersEvent(t *testing.T) {
	name := "projects/p/locations/us-central1/functions/f"
	gen2 := &functionspb.Function{Name: name}
	if got := showPayload(gen2, nil); got.Text != functionTestBody || len(got.Items) != 0 {
		t.Errorf("HTTP payload = %+v", got)
	}
	cases := []struct {
		et              *functionspb.EventTrigger
		source, dataKey string
	}{
		{&functionspb.EventTrigger{EventType: functionPubSubEvent, PubsubTopic: "projects/p/topics/t"}, "//pubsub.googleapis.com/projects/p/topics/t", "message"},
		{&functionspb.EventTrigger{EventType: "google.cloud.storage.object.v1.finalized",
			EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: "b"}}}, "//storage.googleapis.com/projects/_/buckets/b", "bucket"},
		{&functionspb.EventTrigger{EventType: functionFirestorePrefix + "written",
			EventFilters: []*functionspb.EventFilter{{Attribute: "database", Value: "(default)"}, {Attribute: "document", Value: "users/{u}"}}},
			"//firestore.googleapis.com/projects/p/databases/(default)", "value"},
		{&functionspb.EventTrigger{EventType: functionAuditEvent,
			EventFilters: []*functionspb.EventFilter{{Attribute: "serviceName", Value: "storage.googleapis.com"}}},
			"//cloudaudit.googleapis.com/projects/p/logs/activity", "protoPayload"},
	}
	for _, c := range cases {
		got := showPayload(gen2, c.et)
		attrs := map[string]string{}
		for _, it := range got.Items {
			attrs[it.Name] = it.Fields["Value"]
		}
		if attrs["ce-type"] != c.et.GetEventType() || attrs["ce-source"] != c.source || attrs["ce-specversion"] != "1.0" {
			t.Errorf("%s: attributes = %v", c.et.GetEventType(), attrs)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(got.Text), &data); err != nil || data[c.dataKey] == nil {
			t.Errorf("%s: data = %s (%v)", c.et.GetEventType(), got.Text, err)
		}
	}
	gen1 := &functionspb.Function{Name: name, Environment: functionspb.Environment_GEN_1,
		EventTrigger: &functionspb.EventTrigger{EventType: functionPubSubEvent, PubsubTopic: "projects/p/topics/t"}}
	got := showPayload(gen1, gen1.GetEventTrigger())
	var legacy struct {
		Data    map[string]any
		Context struct{ EventType, Resource string }
	}
	if err := json.Unmarshal([]byte(got.Text), &legacy); err != nil || legacy.Context.EventType != "google.pubsub.topic.publish" ||
		legacy.Context.Resource != "pubsub.googleapis.com/projects/p/topics/t" || legacy.Data["message"] == nil {
		t.Errorf("1st gen payload = %s (%v)", got.Text, err)
	}
}

// TestFunctionSettingsAreChecked (#1853): Write a function's settings are
// refused out of range, a 1st gen function refuses 2nd gen's, and each
// reaches the API's field.
func TestFunctionSettingsAreChecked(t *testing.T) {
	for _, bad := range []map[string]string{{"billing": "Instance-based"}, {"ingress": "Internal"}, {"minInstances": "5", "maxInstances": "2"},
		{"timeout": "9999"}, {"concurrency": "x"}, {"env": "no equals sign"}} {
		if _, err := functionSettingsOf(bad); err == nil {
			t.Errorf("functionSettingsOf(%v) was accepted", bad)
		}
	}
	s, err := functionSettingsOf(map[string]string{"cpu": "1", "timeout": "60"})
	if err != nil {
		t.Fatal(err)
	}
	if s.gen1Refusal() == nil {
		t.Error("a 1st gen function took a CPU")
	}
	s, _ = functionSettingsOf(map[string]string{"timeout": "600"})
	if s.gen1Refusal() == nil {
		t.Error("a 1st gen function took a 600 s timeout")
	}
	s, _ = functionSettingsOf(map[string]string{"timeout": "60", "minInstances": "1", "maxInstances": "4", "env": `{"A":"b"}`, "serviceAccount": "sa@p.iam.gserviceaccount.com"})
	v1 := &fv1.CloudFunction{}
	s.setV1(v1)
	if v1.GetTimeout().GetSeconds() != 60 || v1.GetMinInstances() != 1 || v1.GetMaxInstances() != 4 || v1.GetEnvironmentVariables()["A"] != "b" ||
		v1.GetServiceAccountEmail() != "sa@p.iam.gserviceaccount.com" {
		t.Errorf("the v1 function = %v", v1)
	}
}
