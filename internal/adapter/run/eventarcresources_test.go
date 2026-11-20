package run

import (
	"strings"
	"testing"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/proto"
)

// TestEventarcResourceDiff: a resource is created once it settles, updated
// when a settled read differs, kept while it is unsettled, and deleted
// with its last settled value when it is gone.
func TestEventarcResourceDiff(t *testing.T) {
	a := &redispb.Instance{Name: "projects/p/locations/us-central1/instances/a", State: redispb.Instance_READY, MemorySizeGb: 1}
	b := &redispb.Instance{Name: "projects/p/locations/us-central1/instances/b", State: redispb.Instance_CREATING}
	next, ch := diffResources(map[string]proto.Message{}, map[string]eaResource{a.Name: {a, true}, b.Name: {b, false}})
	if len(ch) != 1 || ch[0].kind != "created" || ch[0].name != a.Name || len(next) != 1 {
		t.Fatalf("first read: %v, %v", ch, next)
	}
	a2 := proto.Clone(a).(*redispb.Instance)
	a2.MemorySizeGb = 2
	updating := proto.Clone(a2).(*redispb.Instance)
	updating.State = redispb.Instance_UPDATING
	next, ch = diffResources(next, map[string]eaResource{a.Name: {updating, false}})
	if len(ch) != 0 || !proto.Equal(next[a.Name], a) {
		t.Fatalf("while updating: %v, %v", ch, next)
	}
	next, ch = diffResources(next, map[string]eaResource{a.Name: {a2, true}})
	if len(ch) != 1 || ch[0].kind != "updated" {
		t.Fatalf("after the update: %v", ch)
	}
	_, ch = diffResources(next, map[string]eaResource{})
	if len(ch) != 1 || ch[0].kind != "deleted" || !proto.Equal(ch[0].msg, a2) {
		t.Fatalf("after the delete: %v", ch)
	}
}

// TestEventarcResourceEventData: the catalogue's XxxEventData in JSON, the
// payload left out of a Redis or function deletion and kept in Eventarc's.
func TestEventarcResourceEventData(t *testing.T) {
	redis, _ := eaResourceSourceOf("google.cloud.redis.instance.v1.deleted")
	fn, _ := eaResourceSourceOf("google.cloud.functions.function.v2.created")
	tr, _ := eaResourceSourceOf("google.cloud.eventarc.trigger.v1.deleted")
	if b, _ := resourceEventData(redis, "deleted", &redispb.Instance{Name: "x"}); string(b) != "{}" {
		t.Errorf("a Redis deletion's data = %s", b)
	}
	if b, _ := resourceEventData(fn, "created", &functionspb.Function{Name: "f", State: functionspb.Function_ACTIVE}); !strings.Contains(string(b), `"payload":{"name":"f"`) {
		t.Errorf("a function creation's data = %s", b)
	}
	if b, _ := resourceEventData(tr, "deleted", &eventarcpb.Trigger{Name: "t"}); !strings.Contains(string(b), `"payload":{"name":"t"}`) {
		t.Errorf("a trigger deletion's data = %s", b)
	}
	if a, ok := ResourceEventAttribute("google.cloud.eventarc.channelConnection.v1.created"); !ok || a != "channelconnection" {
		t.Errorf("channelConnection's attribute = %q, %v", a, ok)
	}
	if _, ok := eaTypeAttrs["google.cloud.eventarc.channelConnection.v1.updated"]; ok {
		t.Error("channelConnection.v1.updated is served; Google lists only created and deleted")
	}
}

// TestEventarcRefusedSources: the Firebase providers' and Speech-to-Text's
// direct events are UNIMPLEMENTED with the reason, and Redis's while
// Memorystore is not served.
func TestEventarcRefusedSources(t *testing.T) {
	SetRedisInstanceLister(nil)
	for typ, want := range map[string]string{
		"google.firebase.database.ref.v1.written":              "Realtime Database",
		"google.firebase.remoteconfig.remoteConfig.v1.updated": "Firebase",
		"google.cloud.speech.phraseSet.v1.created":             "Adaptation",
		"google.cloud.redis.instance.v1.created":               "memorystore",
	} {
		_, err := eaSpec(&eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}},
			Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{Service: "s"}}}}, "us-central1")
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(strings.ToLower(err.Error()), "unimplemented") {
			t.Errorf("%s: %v; want UNIMPLEMENTED naming %s", typ, err, want)
		}
	}
	if types := eaDirectTypes("redis.googleapis.com"); len(types) != 0 {
		t.Errorf("redis.googleapis.com lists %v without Memorystore", types)
	}
	if types := eaDirectTypes("eventarc.googleapis.com"); len(types) != 8 {
		t.Errorf("eventarc.googleapis.com lists %v; want Google's eight", types)
	}
}
