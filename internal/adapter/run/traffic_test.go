package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

const (
	tLatest   = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
	tRevision = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
)

// TestTrafficRendersOntoKnative (#1224): a split, a pinned revision and a
// tag become Knative's traffic block, one target each.
func TestTrafficRendersOntoKnative(t *testing.T) {
	s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1"})
	s.Traffic = []*runpb.TrafficTarget{
		{Type: tLatest, Percent: 50, Tag: "next"},
		{Type: tRevision, Revision: "app-00001", Percent: 50},
		{Revision: "projects/p/locations/l/services/app/revisions/app-00002", Tag: "old"},
	}
	m, err := ToKnative(s, "default", "test", nil)
	if err != nil {
		t.Fatalf("ToKnative: %v", err)
	}
	want := `  traffic:
    - latestRevision: true
      percent: 50
      tag: "next"
    - latestRevision: false
      revisionName: "app-00001"
      percent: 50
    - latestRevision: false
      revisionName: "app-00002"
      percent: 0
      tag: "old"
`
	if !strings.HasSuffix(m, want) {
		t.Errorf("manifest does not end with the traffic block:\n%s", m)
	}

	s.Traffic = nil
	if m, _ := ToKnative(s, "default", "test", nil); strings.Contains(m, "traffic:") {
		t.Errorf("no traffic rendered a traffic block:\n%s", m)
	}
}

func TestTrafficValidation(t *testing.T) {
	for name, tr := range map[string][]*runpb.TrafficTarget{
		"under 100":        {{Type: tLatest, Percent: 60}},
		"over 100":         {{Type: tLatest, Percent: 60}, {Revision: "a-1", Percent: 60}},
		"negative":         {{Type: tLatest, Percent: -1}, {Revision: "a-1", Percent: 101}},
		"revision unnamed": {{Type: tRevision, Percent: 100}},
		"latest with name": {{Type: tLatest, Revision: "a-1", Percent: 100}},
		"bad tag":          {{Type: tLatest, Percent: 100, Tag: "-x"}},
		"duplicate tag":    {{Type: tLatest, Percent: 50, Tag: "x"}, {Revision: "a-1", Percent: 50, Tag: "x"}},
		"zero without tag": {{Type: tLatest, Percent: 100}, {Revision: "a-1"}},
	} {
		if err := validateTraffic(tr); err == nil {
			t.Errorf("%s: validateTraffic(%v) = nil, want a refusal", name, tr)
		}
	}
	if err := validateTraffic([]*runpb.TrafficTarget{{Percent: 100}}); err != nil {
		t.Errorf("100%% with no type = %v, want accepted as LATEST", err)
	}
}

// TestTrafficReadsBack: spec.traffic reads back as traffic, and
// status.traffic as traffic_statuses with a tag's URL.
func TestTrafficReadsBack(t *testing.T) {
	var k ksvc
	raw := `{"metadata":{"name":"app"},
	  "spec":{"traffic":[{"latestRevision":true,"percent":30,"tag":"next"},{"latestRevision":false,"revisionName":"app-00001","percent":70}]},
	  "status":{"traffic":[{"revisionName":"app-00002","latestRevision":true,"percent":30,"tag":"next","url":"http://next-app.default.example.com"},
	    {"revisionName":"app-00001","latestRevision":false,"percent":70}]}}`
	if err := json.Unmarshal([]byte(raw), &k); err != nil {
		t.Fatal(err)
	}
	s := FromKnative(k, "projects/p/locations/l")
	if len(s.GetTraffic()) != 2 || s.GetTraffic()[0].GetType() != tLatest || s.GetTraffic()[0].GetTag() != "next" ||
		s.GetTraffic()[1].GetType() != tRevision || s.GetTraffic()[1].GetRevision() != "app-00001" || s.GetTraffic()[1].GetPercent() != 70 {
		t.Errorf("traffic = %v", s.GetTraffic())
	}
	st := s.GetTrafficStatuses()
	if len(st) != 2 || st[0].GetUri() != "http://next-app.default.example.com" || st[0].GetRevision() != "app-00002" ||
		st[0].GetType() != tLatest || st[1].GetPercent() != 70 || st[1].GetUri() != "" {
		t.Errorf("traffic_statuses = %v", st)
	}

	// No spec.traffic is Knative's default, reported as Cloud Run's.
	var plain ksvc
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"app"}}`), &plain)
	if tr := FromKnative(plain, "projects/p/locations/l").GetTraffic(); len(tr) != 1 || tr[0].GetType() != tLatest || tr[0].GetPercent() != 100 {
		t.Errorf("default traffic = %v, want 100%% to LATEST", tr)
	}
}

// TestUpdateServicePinsAnExistingRevision: a target naming a revision the
// service has is applied.
func TestUpdateServicePinsAnExistingRevision(t *testing.T) {
	s, r := updateServer()
	r.answers["get revision hello-00001"] = `{"metadata":{"name":"hello-00001","labels":{"serving.knative.dev/service":"hello"}}}`
	svc := updated("ghcr.io/knative/helloworld-go@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "v2")
	svc.Traffic = []*runpb.TrafficTarget{{Type: tRevision, Revision: "hello-00001", Percent: 90}, {Type: tLatest, Percent: 10, Tag: "canary"}}
	if _, err := s.UpdateService(context.Background(), &runpb.UpdateServiceRequest{Service: svc}); err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if len(r.applied) != 1 || !strings.Contains(r.applied[0], `revisionName: "hello-00001"`) || !strings.Contains(r.applied[0], `tag: "canary"`) {
		t.Errorf("applied = %v", r.applied)
	}
}
