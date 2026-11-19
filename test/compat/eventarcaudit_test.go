//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// eventarcFixture is a probe Cloud Run service Eventarc triggers deliver
// to, with the official Eventarc and Pub/Sub clients.
type eventarcFixture struct {
	ctx          context.Context
	ec           *eventarc.Client
	pc           *pubsub.Client
	parent       string
	service      string
	base, host   string
	pubsubAddr   string
	storageAddr  string
	createdNames []string
}

func newEventarcFixture(t *testing.T, h *Harness, service string) *eventarcFixture {
	t.Helper()
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	loadEventProbe(ctx, t)
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	parent := runParent(h)
	sop, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: eventProbeImage}},
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 1}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: parent + "/services/" + service})
	})
	svc, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("the probe service never became ready: %v", err)
	}
	return &eventarcFixture{ctx: ctx, ec: ec, pc: pc, parent: parent, service: service, base: ingress(t), host: hostOf(t, svc.GetUri()),
		pubsubAddr: pubsubAddr}
}

func (f *eventarcFixture) create(t *testing.T, id string, tr *eventarcpb.Trigger) *eventarcpb.Trigger {
	t.Helper()
	op, err := f.ec.CreateTrigger(f.ctx, &eventarcpb.CreateTriggerRequest{Parent: f.parent, TriggerId: id, Trigger: tr})
	if err != nil {
		t.Fatalf("CreateTrigger %s: %v", id, err)
	}
	got, err := op.Wait(f.ctx)
	if err != nil {
		t.Fatalf("CreateTrigger %s: %v", id, err)
	}
	t.Cleanup(func() {
		if op, err := f.ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: got.GetName(), AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	return got
}

func (f *eventarcFixture) toService(path string) *eventarcpb.Destination {
	return &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{
		Service: f.service, Region: "us-central1", Path: path}}}
}

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/GetProvider
//
// TestEventarcAuditLogTriggers (#1877), with the official Eventarc and
// Pub/Sub clients, as Google's "Route Cloud Audit Logs events to Cloud
// Run" documents it. GetProvider lists google.cloud.audit.log.v1.written
// with serviceName and methodName required and resourceName supporting
// path patterns. A trigger on pubsub.googleapis.com's CreateTopic whose
// resourceName is the match-path-pattern projects/P/topics/audited-*
// receives the Admin Activity entry of a CreateTopic the Pub/Sub client
// makes of an audited-* topic, as a binary-mode CloudEvent: ce-type,
// ce-source //cloudaudit.googleapis.com/projects/P/logs/activity,
// ce-subject, the methodname, servicename and resourcename extensions,
// and LogEntryData with its AuditLog protoPayload. Another topic's
// CreateTopic, and a read (GetTopic), fire nothing. A missing methodName,
// an invalid path pattern and an operator Google does not document are
// refused.
func TestEventarcAuditLogTriggers(t *testing.T) {
	h := New(t)
	f := newEventarcFixture(t, h, "eventarc-audit-probe")
	p, err := f.ec.GetProvider(f.ctx, &eventarcpb.GetProviderRequest{Name: f.parent + "/providers/pubsub.googleapis.com"})
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	var audit *eventarcpb.EventType
	for _, et := range p.GetEventTypes() {
		if et.GetType() == "google.cloud.audit.log.v1.written" {
			audit = et
		}
	}
	attrs := map[string]*eventarcpb.FilteringAttribute{}
	for _, a := range audit.GetFilteringAttributes() {
		attrs[a.GetAttribute()] = a
	}
	if audit == nil || !attrs["serviceName"].GetRequired() || !attrs["methodName"].GetRequired() || !attrs["resourceName"].GetPathPatternSupported() {
		t.Errorf("pubsub.googleapis.com's audit-log event type = %v", audit)
	}

	typ := &eventarcpb.EventFilter{Attribute: "type", Value: "google.cloud.audit.log.v1.written"}
	svc := &eventarcpb.EventFilter{Attribute: "serviceName", Value: "pubsub.googleapis.com"}
	method := &eventarcpb.EventFilter{Attribute: "methodName", Value: "google.pubsub.v1.Publisher.CreateTopic"}
	for name, filters := range map[string][]*eventarcpb.EventFilter{
		"no methodName":           {typ, svc},
		"an invalid path pattern": {typ, svc, method, {Attribute: "resourceName", Value: "/a/**/b/**", Operator: "match-path-pattern"}},
		"an unknown operator":     {typ, svc, method, {Attribute: "resourceName", Value: "x", Operator: "prefix"}},
	} {
		_, err := f.ec.CreateTrigger(f.ctx, &eventarcpb.CreateTriggerRequest{Parent: f.parent, TriggerId: "bad",
			Trigger: &eventarcpb.Trigger{EventFilters: filters, Destination: f.toService("")}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateTrigger with %s: %v; want InvalidArgument", name, err)
		}
	}

	pattern := "projects/" + h.Project() + "/topics/audited-*"
	tr := f.create(t, "audit-topics", &eventarcpb.Trigger{Destination: f.toService("/audit"), EventFilters: []*eventarcpb.EventFilter{
		typ, svc, method, {Attribute: "resourceName", Value: pattern, Operator: "match-path-pattern"}}})
	if !strings.HasPrefix(tr.GetTransport().GetPubsub().GetTopic(), "projects/"+h.Project()+"/topics/eventarc-us-central1-audit-topics-") ||
		tr.GetEventDataContentType() != "application/json" {
		t.Errorf("the audit-log trigger = %v", tr)
	}
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	audited := topic(t, h, f.pc, "audited-"+suffix)
	other := topic(t, h, f.pc, "other-"+suffix)
	if _, err := f.pc.TopicAdminClient.GetTopic(f.ctx, &pubsubpb.GetTopicRequest{Topic: audited}); err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	ev, all := probedEvents(t, f.base, f.host, func(e probedEvent) bool { return e.Headers["ce-resourcename"] == audited })
	if ev.Headers == nil {
		t.Fatalf("no audit-log CloudEvent for %s reached the service; it got %v", audited, all)
	}
	for k, want := range map[string]string{"ce-type": "google.cloud.audit.log.v1.written", "ce-specversion": "1.0",
		"ce-source":  "//cloudaudit.googleapis.com/projects/" + h.Project() + "/logs/activity",
		"ce-subject": "pubsub.googleapis.com/" + audited, "ce-methodname": "google.pubsub.v1.Publisher.CreateTopic",
		"ce-servicename": "pubsub.googleapis.com", "content-type": "application/json; charset=utf-8"} {
		if got := ev.Headers[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if ev.Path != "/audit" {
		t.Errorf("the event reached %q; want /audit", ev.Path)
	}
	var entry struct {
		LogName      string `json:"logName"`
		InsertID     string `json:"insertId"`
		ProtoPayload struct {
			Type         string `json:"@type"`
			ServiceName  string `json:"serviceName"`
			MethodName   string `json:"methodName"`
			ResourceName string `json:"resourceName"`
		} `json:"protoPayload"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &entry); err != nil || entry.InsertID == "" ||
		entry.LogName != "projects/"+h.Project()+"/logs/cloudaudit.googleapis.com%2Factivity" ||
		entry.ProtoPayload.Type != "type.googleapis.com/google.cloud.audit.AuditLog" || entry.ProtoPayload.ResourceName != audited ||
		entry.ProtoPayload.MethodName != "google.pubsub.v1.Publisher.CreateTopic" || entry.ProtoPayload.ServiceName != "pubsub.googleapis.com" {
		t.Errorf("LogEntryData = %s (%v)", ev.Data, err)
	}
	// The other topic's entry, written at the same time, never matched.
	time.Sleep(5 * time.Second)
	_, all = probedEvents(t, f.base, f.host, func(probedEvent) bool { return true })
	for _, e := range all {
		if e.Headers["ce-resourcename"] == other || e.Headers["ce-methodname"] == "google.pubsub.v1.Publisher.GetTopic" {
			t.Errorf("an unmatched call reached the service: %v", e.Headers)
		}
	}
}
