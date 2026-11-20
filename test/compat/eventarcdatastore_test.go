//go:build compat

package compat

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	datastorepb "cloud.google.com/go/datastore/apiv1/datastorepb"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// entityEventData decodes google.events.cloud.datastore.v1.EntityEventData:
// value (1) and old_value (2), whose EntityResult field numbers are
// google.datastore.v1.EntityResult's, and update_mask (3).
func entityEventData(t *testing.T, raw []byte) (value, old *datastorepb.EntityResult, mask []string) {
	t.Helper()
	for b := raw; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || typ != protowire.BytesType {
			t.Fatalf("EntityEventData is not the documented protobuf: %x", raw)
		}
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		b = b[n:]
		switch num {
		case 1, 2:
			r := &datastorepb.EntityResult{}
			if err := proto.Unmarshal(v, r); err != nil {
				t.Fatalf("EntityEventData field %d: %v", num, err)
			}
			if num == 1 {
				value = r
			} else {
				old = r
			}
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
	return value, old, mask
}

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/GetProvider
//
// TestEventarcDatastoreTriggers (#1877), with the official Eventarc and
// Datastore clients, as Google's "Extend Datastore with Cloud Functions
// (2nd gen)" documents Firestore in Datastore mode's events: a
// google.cloud.datastore.entity.v1.written trigger on the (default)
// database whose entity filter is the path pattern users/{userId}
// receives a create, an update and a delete of users/NAME, made with the
// Datastore client, as binary-mode CloudEvents with data content type
// application/protobuf, the project, database, namespace and entity
// extensions, and EntityEventData: the new value, the old value for the
// update and the delete, and the update's mask. An entity of another kind
// reaches no trigger, a trigger with no database filter is refused, and
// firestore.googleapis.com names the Datastore mode event types.
func TestEventarcDatastoreTriggers(t *testing.T) {
	h := New(t)
	dsAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_DATASTORE", EnvDatastore)
	f := newEventarcFixture(t, h, "eventarc-ds-probe")
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClient(f.ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })

	p, err := f.ec.GetProvider(f.ctx, &eventarcpb.GetProviderRequest{Name: f.parent + "/providers/firestore.googleapis.com"})
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	var named bool
	for _, et := range p.GetEventTypes() {
		if et.GetType() == "google.cloud.datastore.entity.v1.written" {
			named = true
			for _, a := range et.GetFilteringAttributes() {
				if a.GetAttribute() == "entity" && !a.GetPathPatternSupported() {
					t.Error("the entity attribute does not support path patterns")
				}
			}
		}
	}
	if !named {
		t.Errorf("firestore.googleapis.com does not name the Datastore mode events: %v", p.GetEventTypes())
	}

	typ := &eventarcpb.EventFilter{Attribute: "type", Value: "google.cloud.datastore.entity.v1.written"}
	db := &eventarcpb.EventFilter{Attribute: "database", Value: "(default)"}
	users := &eventarcpb.EventFilter{Attribute: "entity", Value: "users/{userId}", Operator: "match-path-pattern"}
	if _, err := f.ec.CreateTrigger(f.ctx, &eventarcpb.CreateTriggerRequest{Parent: f.parent, TriggerId: "ds-bad",
		Trigger: &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{typ}, Destination: f.toService("")}}); err == nil {
		t.Error("a Datastore mode trigger with no database filter was accepted")
	}
	tr := f.create(t, "ds-written", &eventarcpb.Trigger{Destination: f.toService("/entities"),
		EventFilters: []*eventarcpb.EventFilter{typ, db, users}, EventDataContentType: "application/protobuf"})
	if tr.GetEventDataContentType() != "application/protobuf" {
		t.Errorf("the trigger's eventDataContentType = %q", tr.GetEventDataContentType())
	}
	// The trigger's watch of the database starts within a few seconds.
	time.Sleep(8 * time.Second)

	user := strings.ReplaceAll("u"+time.Now().Format("150405.000000"), ".", "")
	key := datastore.NameKey("users", user, nil)
	type row struct {
		N    int64
		Name string
	}
	if _, err := dc.Put(f.ctx, key, &row{N: 1, Name: "first"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Each change is seen on its own poll.
	time.Sleep(3 * time.Second)
	if _, err := dc.Put(f.ctx, key, &row{N: 2, Name: "first"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	post := datastore.NameKey("posts", user, nil)
	if _, err := dc.Put(f.ctx, post, &row{N: 1}); err != nil {
		t.Fatalf("Put posts: %v", err)
	}
	time.Sleep(3 * time.Second)
	if err := dc.Delete(f.ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	t.Cleanup(func() { _ = dc.Delete(f.ctx, post) })

	decode := func(e probedEvent) (*datastorepb.EntityResult, *datastorepb.EntityResult, []string) {
		raw, _ := base64.StdEncoding.DecodeString(e.DataBase64)
		return entityEventData(t, raw)
	}
	ofUser := func(match func(v, old *datastorepb.EntityResult) bool) func(probedEvent) bool {
		return func(e probedEvent) bool {
			if e.Path != "/entities" || e.Headers["ce-entity"] != "users/"+user {
				return false
			}
			v, old, _ := decode(e)
			return match(v, old)
		}
	}
	created, all := probedEvents(t, f.base, f.host, ofUser(func(v, old *datastorepb.EntityResult) bool { return old == nil && v != nil }))
	if created.Headers == nil {
		t.Fatalf("no Datastore mode CloudEvent for the create of %s reached the service; it got %d events", user, len(all))
	}
	for k, want := range map[string]string{"ce-type": "google.cloud.datastore.entity.v1.written", "ce-specversion": "1.0",
		"content-type": "application/protobuf", "ce-project": h.Project(), "ce-database": "(default)", "ce-namespace": "(default)"} {
		if got := created.Headers[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if v, _, _ := decode(created); v.GetEntity().GetProperties()["Name"].GetStringValue() != "first" ||
		v.GetEntity().GetKey().GetPath()[0].GetName() != user {
		t.Errorf("the create's EntityEventData value = %v", v)
	}
	updated, _ := probedEvents(t, f.base, f.host, ofUser(func(v, old *datastorepb.EntityResult) bool { return old != nil && v != nil }))
	if updated.Headers == nil {
		t.Fatal("no CloudEvent for the update reached the trigger")
	}
	if v, old, mask := decode(updated); old.GetEntity().GetProperties()["N"].GetIntegerValue() != 1 ||
		v.GetEntity().GetProperties()["N"].GetIntegerValue() != 2 || strings.Join(mask, ",") != "N" {
		t.Errorf("the update's EntityEventData = %v, old %v, mask %v", v, old, mask)
	}
	deleted, _ := probedEvents(t, f.base, f.host, ofUser(func(v, old *datastorepb.EntityResult) bool { return old != nil && v == nil }))
	if deleted.Headers == nil {
		t.Fatal("no CloudEvent for the delete reached the trigger")
	}
	_, all = probedEvents(t, f.base, f.host, func(probedEvent) bool { return true })
	for _, e := range all {
		if strings.HasPrefix(e.Headers["ce-entity"], "posts/") {
			t.Errorf("an entity of posts reached the service: %v", e.Headers)
		}
	}
}
