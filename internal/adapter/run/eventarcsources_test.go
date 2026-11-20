package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

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
	// mu guards the fields below: a bus or pipeline publishes from its own
	// goroutine (eaPublishToBus), while the test polls them.
	mu     sync.Mutex
	topics []string
	data   []byte
	attrs  map[string]string
}

func (f *fakePublisher) Publish(_ context.Context, topic string, data []byte, attrs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// A document change reaches the triggers of its kind and the written ones
// whose database and document pattern match, as DocumentEventData in
// protobuf with the old value and the update mask.
func TestFirestoreEventsReachMatchingTriggers(t *testing.T) {
	pub := &fakePublisher{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: pub})
	add := func(id, typ string, filters ...*functionspb.EventFilter) {
		name := "projects/fsp/locations/us-central1/triggers/" + id
		eaState.mu.Lock()
		eaState.triggers[name] = &eaTrigger{t: &eventarcpb.Trigger{Name: name}, live: &liveTrigger{
			spec: &functionspb.EventTrigger{EventType: typ, EventFilters: filters}, topic: "projects/fsp/topics/" + id}}
		eaState.mu.Unlock()
		t.Cleanup(func() { eaState.mu.Lock(); delete(eaState.triggers, name); eaState.mu.Unlock() })
	}
	db := &functionspb.EventFilter{Attribute: "database", Value: "(default)"}
	add("upd", firestorePrefix+"updated", db, &functionspb.EventFilter{Attribute: "document", Value: "users/{u}", Operator: pathPatternOp})
	add("all", firestorePrefix+"written", db)
	add("other-db", firestorePrefix+"written", &functionspb.EventFilter{Attribute: "database", Value: "other"})
	add("posts", firestorePrefix+"updated", db, &functionspb.EventFilter{Attribute: "document", Value: "posts/{p}", Operator: pathPatternOp})
	if w := FirestoreWatches(); len(w) != 4 || w[0] != (FirestoreWatch{"fsp", "(default)", ""}) || w[1] != (FirestoreWatch{"fsp", "(default)", "posts"}) ||
		w[2] != (FirestoreWatch{"fsp", "(default)", "users"}) || w[3] != (FirestoreWatch{"fsp", "other", ""}) {
		t.Errorf("FirestoreWatches = %v", w)
	}
	name := "projects/fsp/databases/(default)/documents/users/alice"
	old := &firestorepb.Document{Name: name, Fields: map[string]*firestorepb.Value{"a": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 1}},
		"b": {ValueType: &firestorepb.Value_StringValue{StringValue: "x"}}}}
	cur := &firestorepb.Document{Name: name, Fields: map[string]*firestorepb.Value{"a": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 2}},
		"b": {ValueType: &firestorepb.Value_StringValue{StringValue: "x"}}}}
	n := f.Eventarc().EmitFirestore(context.Background(), FirestoreChange{Project: "fsp", Database: "(default)", Name: name, Old: old, New: cur})
	if n != 2 || strings.Join(pub.topics, ",") != "projects/fsp/topics/upd,projects/fsp/topics/all" {
		t.Fatalf("EmitFirestore = %d to %v", n, pub.topics)
	}
	for k, want := range map[string]string{"ce-source": "//firestore.googleapis.com/projects/fsp/databases/(default)",
		"ce-subject": "documents/users/alice", "ce-document": "users/alice", "ce-database": "(default)", "content-type": "application/protobuf"} {
		if pub.attrs[k] != want {
			t.Errorf("%s = %q; want %q", k, pub.attrs[k], want)
		}
	}
	var gotNew, gotOld firestorepb.Document
	var mask []string
	for b := pub.data; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		b = b[n:]
		if typ != protowire.BytesType {
			t.Fatalf("field %d is not bytes", num)
		}
		switch num {
		case 1:
			_ = proto.Unmarshal(v, &gotNew)
		case 2:
			_ = proto.Unmarshal(v, &gotOld)
		case 3:
			for len(v) > 0 {
				_, _, n := protowire.ConsumeTag(v)
				v = v[n:]
				s, n := protowire.ConsumeString(v)
				v = v[n:]
				mask = append(mask, s)
			}
		}
	}
	if !proto.Equal(&gotNew, cur) || !proto.Equal(&gotOld, old) || strings.Join(mask, ",") != "a" {
		t.Errorf("DocumentEventData = value %v, old %v, mask %v", &gotNew, &gotOld, mask)
	}
	pub.topics = nil
	if n := f.Eventarc().EmitFirestore(context.Background(), FirestoreChange{Project: "fsp", Database: "(default)", Name: name, New: cur}); n != 1 ||
		pub.topics[0] != "projects/fsp/topics/all" {
		t.Errorf("a create reached %v", pub.topics)
	}
}

// A trigger whose destination is a Cloud Run function delivers to the
// function's service through the ingress, with a token for its URL.
func TestEventarcDeliversToAFunction(t *testing.T) {
	var host, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, path, auth = r.Host, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	tokens := &fakeTokens{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: &fakeTransport{}, Tokens: tokens, Ingress: strings.TrimPrefix(srv.URL, "http://")})
	fnName := "projects/proj-ea/locations/us-central1/functions/ea-fn"
	functions.mu.Lock()
	functions.fns[fnName] = &storedFunction{fn: &functionspb.Function{Name: fnName,
		ServiceConfig: &functionspb.ServiceConfig{Uri: "http://ea-fn.ns.cloudburrow.localhost"}}}
	functions.mu.Unlock()
	t.Cleanup(func() { functions.mu.Lock(); delete(functions.fns, fnName); functions.mu.Unlock() })
	tr := &eventarcpb.Trigger{Name: "projects/proj-ea/locations/us-central1/triggers/t", ServiceAccount: "sa@proj-ea.iam.gserviceaccount.com",
		Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudFunction{CloudFunction: fnName}}}
	if _, err := eaSpec(&eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: pubsubPublished}},
		Destination: tr.GetDestination()}, "us-central1"); err != nil {
		t.Fatalf("eaSpec: %v", err)
	}
	spec := &functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t"}
	body := `{"message":{"data":"aGk=","messageId":"1"},"subscription":"s"}`
	code, err := f.deliverEventarc(context.Background(), tr, spec, []byte(body))
	if err != nil || code != http.StatusNoContent || host != "ea-fn.ns.cloudburrow.localhost" || path != "/" || auth != "Bearer tok" ||
		tokens.audience != "http://ea-fn.ns.cloudburrow.localhost" {
		t.Errorf("delivery = %d, %v; host %q path %q auth %q audience %q", code, err, host, path, auth, tokens.audience)
	}
}

type fakeWorkflows struct{ name, arg string }

func (w *fakeWorkflows) WorkflowExists(name string) bool {
	return name == "projects/p/locations/us-central1/workflows/w"
}
func (w *fakeWorkflows) StartWorkflow(_ context.Context, name, arg string) error {
	w.name, w.arg = name, arg
	return nil
}

// A workflow destination (#1986): refused without Workflows, and with it the
// event, as a CloudEvent in JSON, is the argument of an execution.
func TestEventarcWorkflowDestination(t *testing.T) {
	wf := "projects/p/locations/us-central1/workflows/w"
	d := &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_Workflow{Workflow: wf}}
	tr := &eventarcpb.Trigger{Name: "projects/p/locations/us-central1/triggers/t", Destination: d,
		EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: pubsubPublished}}}
	if _, err := eaSpec(tr, "us-central1"); err != nil {
		t.Fatalf("eaSpec: %v", err)
	}
	bad := &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_Workflow{Workflow: "w"}}
	if _, err := eaSpec(&eventarcpb.Trigger{EventFilters: tr.GetEventFilters(), Destination: bad}, "us-central1"); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("a bad workflow name = %v", err)
	}
	if err := (&FunctionsServer{}).WithEvents(Events{}).checkWorkflow(wf); apierror.From(err).Code != codes.FailedPrecondition {
		t.Errorf("without Workflows = %v", err)
	}
	w := &fakeWorkflows{}
	f := (&FunctionsServer{}).WithEvents(Events{Workflows: w})
	if err := f.checkWorkflow(wf + "x"); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("a missing workflow = %v", err)
	}
	spec := &functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t"}
	body := `{"message":{"data":"aGk=","messageId":"7"},"subscription":"s"}`
	code, err := f.deliverEventarc(context.Background(), tr, spec, []byte(body))
	if err != nil || code != http.StatusOK || w.name != wf {
		t.Fatalf("delivery = %d, %v to %q", code, err, w.name)
	}
	var arg map[string]any
	if err := json.Unmarshal([]byte(w.arg), &arg); err != nil || arg["id"] != "7" || arg["type"] != pubsubPublished ||
		arg["source"] != "//pubsub.googleapis.com/projects/p/topics/t" {
		t.Errorf("argument %s (%v)", w.arg, err)
	}
	if data, _ := arg["data"].(map[string]any); data["message"] == nil {
		t.Errorf("argument's data %v", arg["data"])
	}
}

// A 2nd-gen function's own audit-log and Firestore triggers (#1853): its
// event_trigger is checked as Eventarc's are, its transport is a topic
// Eventarc makes, and the events Eventarc sources reach that topic.
func TestFunctionSourcedTriggersAreServed(t *testing.T) {
	pub := &fakePublisher{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: pub, PushBase: func() string { return "http://push" }})
	audit := &functionspb.EventTrigger{EventType: auditLogWritten, TriggerRegion: "us-central1", EventFilters: []*functionspb.EventFilter{
		{Attribute: "serviceName", Value: "secretmanager.googleapis.com"},
		{Attribute: "methodName", Value: "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret"}}}
	if err := f.checkEventTrigger(audit); err != nil {
		t.Fatalf("checkEventTrigger(audit log) = %v", err)
	}
	if err := f.checkEventTrigger(&functionspb.EventTrigger{EventType: auditLogWritten}); err == nil {
		t.Error("an audit-log trigger without serviceName and methodName was accepted")
	}
	if err := f.checkEventTrigger(&functionspb.EventTrigger{EventType: auditLogWritten, PubsubTopic: "projects/p/topics/t",
		EventFilters: audit.GetEventFilters()}); err == nil {
		t.Error("an audit-log trigger with a pubsub_topic was accepted")
	}
	fsSpec := &functionspb.EventTrigger{EventType: firestorePrefix + "written", TriggerRegion: "us-central1",
		EventFilters: []*functionspb.EventFilter{{Attribute: "database", Value: "(default)"}}}
	if err := f.checkEventTrigger(fsSpec); err != nil {
		t.Fatalf("checkEventTrigger(Firestore) = %v", err)
	}
	live, err := f.makeTrigger(context.Background(), "projects/fnsp/locations/us-central1/functions/aud", audit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(live.topic, "projects/fnsp/topics/eventarc-us-central1-aud-") ||
		len(pub.calls) != 2 || !strings.HasPrefix(pub.calls[0], "topic "+live.topic) || !strings.Contains(pub.calls[1], " "+live.topic+" ") {
		t.Fatalf("the trigger made topic %q with %v", live.topic, pub.calls)
	}
	fs, err := f.makeTrigger(context.Background(), "projects/fnsp/locations/us-central1/functions/docs", fsSpec)
	if err != nil {
		t.Fatal(err)
	}
	functions.mu.Lock()
	functions.fns["projects/fnsp/locations/us-central1/functions/aud"] = &storedFunction{fn: &functionspb.Function{}, trigger: live}
	functions.fns["projects/fnsp/locations/us-central1/functions/docs"] = &storedFunction{fn: &functionspb.Function{}, trigger: fs}
	functions.mu.Unlock()
	t.Cleanup(func() {
		functions.mu.Lock()
		delete(functions.fns, "projects/fnsp/locations/us-central1/functions/aud")
		delete(functions.fns, "projects/fnsp/locations/us-central1/functions/docs")
		functions.mu.Unlock()
	})
	call := AuditCall{Project: "fnsp", ServiceName: "secretmanager.googleapis.com",
		MethodName: "google.cloud.secretmanager.v1.SecretManagerService.CreateSecret", ResourceName: "projects/fnsp/secrets/s"}
	if n := f.Eventarc().EmitAuditLog(context.Background(), call); n != 1 || pub.topics[len(pub.topics)-1] != live.topic {
		t.Errorf("EmitAuditLog = %d to %v; want the function's topic %s", n, pub.topics, live.topic)
	}
	found := false
	for _, w := range FirestoreWatches() {
		found = found || w == (FirestoreWatch{"fnsp", "(default)", ""})
	}
	if !found {
		t.Errorf("FirestoreWatches = %v; want the function's database watched", FirestoreWatches())
	}
}
