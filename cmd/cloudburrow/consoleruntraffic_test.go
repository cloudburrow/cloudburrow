package main

import (
	"context"
	"encoding/json"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// TestTrafficLinesRoundTrip (#1224): the Manage traffic form is prefilled
// from the Knative Service's spec.traffic, and its text reads back as the
// Cloud Run targets it names.
func TestTrafficLinesRoundTrip(t *testing.T) {
	var svc ksvcStatus
	if err := json.Unmarshal([]byte(`{"spec":{"traffic":[
	  {"latestRevision":true,"percent":30,"tag":"next"},
	  {"latestRevision":false,"revisionName":"hello-00001","percent":70},
	  {"latestRevision":false,"revisionName":"hello-00002","tag":"old"}]}}`), &svc); err != nil {
		t.Fatal(err)
	}
	text := trafficLines(&svc)
	if want := "LATEST 30 next\nhello-00001 70\nhello-00002 0 old"; text != want {
		t.Errorf("trafficLines = %q, want %q", text, want)
	}
	got, err := parseTrafficLines(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].GetType() != runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST ||
		got[0].GetTag() != "next" || got[1].GetRevision() != "hello-00001" || got[1].GetPercent() != 70 ||
		got[2].GetTag() != "old" || got[2].GetPercent() != 0 {
		t.Errorf("parseTrafficLines = %v", got)
	}
	if text := trafficLines(&ksvcStatus{}); text != "LATEST 100" {
		t.Errorf("no traffic prefilled as %q, want LATEST 100", text)
	}
	for _, bad := range []string{"", "LATEST", "LATEST fifty", "a 1 b c"} {
		if _, err := parseTrafficLines(bad); err == nil {
			t.Errorf("parseTrafficLines(%q) = nil error", bad)
		}
	}
}

// TestTrafficListingShowsTags: the service page's Traffic section names
// each target's tag and the URL Knative gave it.
func TestTrafficListingShowsTags(t *testing.T) {
	var svc ksvcStatus
	_ = json.Unmarshal([]byte(`{"status":{"traffic":[
	  {"revisionName":"hello-00002","latestRevision":true,"percent":50,"tag":"next","url":"http://next-hello.default.example.com"},
	  {"revisionName":"hello-00001","latestRevision":false,"percent":50}]}}`), &svc)
	l := trafficListing(&svc)
	if len(l.Items) != 2 || l.Items[0].Fields["Tag URL"] != "http://next-hello.default.example.com" ||
		l.Items[0].Fields["Latest"] != "yes" || l.Items[1].Fields["Percent"] != "50%" || l.Items[1].Fields["Latest"] != "no" {
		t.Errorf("trafficListing = %+v", l.Items)
	}
	if !svc.servingRevisions()["hello-00001"] || !svc.servingRevisions()["hello-00002"] {
		t.Errorf("servingRevisions = %v, want both halves of the split", svc.servingRevisions())
	}
}

// TestRunManageTrafficSendsATrafficMask (#1260): Manage traffic sends the
// service's name and traffic alone, with an update_mask of traffic, so the
// adapter changes nothing else.
func TestRunManageTrafficSendsATrafficMask(t *testing.T) {
	fake := &fakeRunServices{service: deployedService()}
	p := fake.serve(t)
	if err := p.manageTraffic(context.Background(), "demo", "api",
		map[string]string{runTrafficField: "LATEST 60\napi-00001 40 old"}); err != nil {
		t.Fatalf("manageTraffic: %v", err)
	}
	if len(fake.updates) != 1 {
		t.Fatalf("%d UpdateService calls, want 1", len(fake.updates))
	}
	req := fake.updates[0]
	if paths := req.GetUpdateMask().GetPaths(); len(paths) != 1 || paths[0] != "traffic" {
		t.Errorf("update mask = %v, want [traffic]", paths)
	}
	svc := req.GetService()
	if svc.GetName() != editedServiceName || svc.GetTemplate() != nil || len(svc.GetLabels()) != 0 || svc.GetEtag() != "" {
		t.Errorf("sent %v; want only the name and the traffic", svc)
	}
	tr := svc.GetTraffic()
	if len(tr) != 2 || tr[0].GetPercent() != 60 || tr[1].GetRevision() != "api-00001" || tr[1].GetTag() != "old" {
		t.Errorf("traffic = %v", tr)
	}
}
