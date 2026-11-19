package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The examples of Google's "Understand path patterns" page.
func TestPathPatternsAsGoogleDocumentsThem(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"/buckets/bucket-1/objects/*", "/buckets/bucket-1/objects/file4.jpg", true},
		{"/buckets/bucket-1/objects/*", "/buckets/bucket-1/objects/files/file4.jpg", false},
		{"/buckets/bucket-1/objects/*", "/buckets/bucket-1/objects", false},
		{"/buckets/bucket-1/objects/file-*.txt", "/buckets/bucket-1/objects/file-.txt", true},
		{"/buckets/bucket-1/objects/file-*.txt", "/buckets/bucket-1/objects/file.txt", false},
		{"/projects/_/**/{filename=file-*.txt}", "/projects/_/file-10.txt", true},
		{"/projects/_/**/{filename=file-*.txt}", "/projects/_/a/b/file-10.txt", true},
		{"/projects/_/**/{filename=file-*.txt}", "/projects/_/files/file-5.txt/file.txt", false},
		{"projects/p/secrets/{secret}", "projects/p/secrets/s1", true},
		{"projects/p/secrets/{secret}", "projects/p/secrets/s1/versions/1", false},
		{"users/{userId=**}", "users/alice/posts/p1", true},
	} {
		if got := matchPathPattern(c.pattern, c.path); got != c.want {
			t.Errorf("matchPathPattern(%q, %q) = %v; want %v", c.pattern, c.path, got, c.want)
		}
	}
	for _, bad := range []string{"/projects/_/buckets/bucket-1/objects/", "bucket-1/objects/file-*.*", "/projects/**/buckets/**",
		"{bucket=}", "{bucket/objects/file1.txt", ""} {
		if err := validPathPattern(bad); err == nil {
			t.Errorf("validPathPattern(%q) accepted it", bad)
		}
	}
}

func TestAuditLogFiltersAreChecked(t *testing.T) {
	trig := func(filters ...*eventarcpb.EventFilter) *eventarcpb.Trigger {
		return &eventarcpb.Trigger{EventFilters: append([]*eventarcpb.EventFilter{{Attribute: "type", Value: auditLogWritten}}, filters...),
			Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{Service: "s"}}}}
	}
	svc := &eventarcpb.EventFilter{Attribute: "serviceName", Value: "secretmanager.googleapis.com"}
	method := &eventarcpb.EventFilter{Attribute: "methodName", Value: "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret"}
	for _, c := range []struct {
		trig *eventarcpb.Trigger
		code codes.Code
	}{
		{trig(svc, method), codes.OK},
		{trig(svc, method, &eventarcpb.EventFilter{Attribute: "resourceName", Value: "projects/p/secrets/*", Operator: pathPatternOp}), codes.OK},
		{trig(svc), codes.InvalidArgument},
		{trig(svc, method, &eventarcpb.EventFilter{Attribute: "resourceName", Value: "/a/**/b/**", Operator: pathPatternOp}), codes.InvalidArgument},
		{trig(svc, &eventarcpb.EventFilter{Attribute: "methodName", Value: "x", Operator: pathPatternOp}), codes.InvalidArgument},
		{trig(svc, method, &eventarcpb.EventFilter{Attribute: "resourceName", Value: "x", Operator: "prefix"}), codes.InvalidArgument},
		{trig(svc, method, &eventarcpb.EventFilter{Attribute: "bucket", Value: "b"}), codes.InvalidArgument},
	} {
		_, err := eaSpec(c.trig, "us-central1")
		got := codes.OK
		if err != nil {
			got = apierror.From(err).Code
		}
		if got != c.code {
			t.Errorf("%v = %v; want %v", c.trig.GetEventFilters(), err, c.code)
		}
	}
}

type fakePublisher struct {
	fakeTransport
	topics []string
	data   []byte
	attrs  map[string]string
}

func (f *fakePublisher) Publish(_ context.Context, topic string, data []byte, attrs map[string]string) error {
	f.topics, f.data, f.attrs = append(f.topics, topic), data, attrs
	return nil
}

// An admin call matching a trigger's serviceName, methodName and
// resourceName pattern is published to the trigger's topic as Google's
// audit-log CloudEvent; a read, another method or another resource is not.
func TestAuditLogEventsReachMatchingTriggers(t *testing.T) {
	pub := &fakePublisher{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: pub})
	name := "projects/audp/locations/us-central1/triggers/aud"
	spec := &functionspb.EventTrigger{EventType: auditLogWritten, EventFilters: []*functionspb.EventFilter{
		{Attribute: "serviceName", Value: "secretmanager.googleapis.com"},
		{Attribute: "methodName", Value: "google.cloud.secretmanager.v1.SecretManagerService.DeleteSecret"},
		{Attribute: "resourceName", Value: "projects/audp/secrets/keep-*", Operator: pathPatternOp}}}
	eaState.mu.Lock()
	eaState.triggers[name] = &eaTrigger{t: &eventarcpb.Trigger{Name: name}, live: &liveTrigger{spec: spec, topic: "projects/audp/topics/eventarc-aud"}}
	eaState.mu.Unlock()
	t.Cleanup(func() { eaState.mu.Lock(); delete(eaState.triggers, name); eaState.mu.Unlock() })
	call := AuditCall{Project: "audp", ServiceName: "secretmanager.googleapis.com",
		MethodName: "google.cloud.secretmanager.v1.SecretManagerService.DeleteSecret", ResourceName: "projects/audp/secrets/keep-1",
		Time: time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)}
	if n := f.Eventarc().EmitAuditLog(context.Background(), call); n != 1 || len(pub.topics) != 1 || pub.topics[0] != "projects/audp/topics/eventarc-aud" {
		t.Fatalf("EmitAuditLog = %d to %v", n, pub.topics)
	}
	for k, want := range map[string]string{"ce-source": "//cloudaudit.googleapis.com/projects/audp/logs/activity",
		"ce-subject": "secretmanager.googleapis.com/projects/audp/secrets/keep-1", "ce-methodname": call.MethodName,
		"ce-servicename": call.ServiceName, "ce-resourcename": call.ResourceName, "content-type": "application/json; charset=utf-8"} {
		if pub.attrs[k] != want {
			t.Errorf("%s = %q; want %q", k, pub.attrs[k], want)
		}
	}
	var entry struct {
		LogName      string
		ProtoPayload map[string]any
	}
	if err := json.Unmarshal(pub.data, &entry); err != nil || entry.LogName != "projects/audp/logs/cloudaudit.googleapis.com%2Factivity" ||
		entry.ProtoPayload["methodName"] != call.MethodName || entry.ProtoPayload["@type"] != "type.googleapis.com/google.cloud.audit.AuditLog" {
		t.Errorf("entry = %s (%v)", pub.data, err)
	}
	// The push becomes the CloudEvent, with the trigger's type.
	body, _ := json.Marshal(map[string]any{"message": map[string]any{"data": base64.StdEncoding.EncodeToString(pub.data),
		"attributes": pub.attrs, "messageId": "9"}, "subscription": "s"})
	ev, err := ToCloudEvent(spec, body)
	if err != nil || ev.Headers.Get("ce-type") != auditLogWritten || ev.Headers.Get("ce-methodname") != call.MethodName ||
		ev.Headers.Get("Content-Type") != "application/json; charset=utf-8" || string(ev.Data) != string(pub.data) ||
		!strings.HasPrefix(ev.Headers.Get("ce-id"), "projects/audp/logs/") {
		t.Errorf("CloudEvent = %v, %v", ev, err)
	}
	for _, other := range []AuditCall{
		{Project: "audp", ServiceName: call.ServiceName, MethodName: "google.cloud.secretmanager.v1.SecretManagerService.GetSecret", ResourceName: call.ResourceName},
		{Project: "audp", ServiceName: call.ServiceName, MethodName: call.MethodName, ResourceName: "projects/audp/secrets/other"},
		{Project: "other", ServiceName: call.ServiceName, MethodName: call.MethodName, ResourceName: call.ResourceName},
	} {
		if n := f.Eventarc().EmitAuditLog(context.Background(), other); n != 0 {
			t.Errorf("EmitAuditLog(%v) reached %d triggers", other, n)
		}
	}
	if s, m, ok := GRPCAuditCall("/google.cloud.secretmanager.v1.SecretManagerService/CreateSecret"); !ok || s != "secretmanager.googleapis.com" ||
		m != "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret" {
		t.Errorf("GRPCAuditCall = %q, %q, %v", s, m, ok)
	}
}
