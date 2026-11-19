//go:build compat

package compat

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/firestore"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// documentEventData decodes google.events.cloud.firestore.v1.DocumentEventData:
// value (1) and old_value (2), whose Document field numbers are
// google.firestore.v1.Document's, and update_mask (3).
func documentEventData(t *testing.T, raw []byte) (value, old *firestorepb.Document, mask []string) {
	t.Helper()
	for b := raw; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || typ != protowire.BytesType {
			t.Fatalf("DocumentEventData is not the documented protobuf: %x", raw)
		}
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		b = b[n:]
		switch num {
		case 1, 2:
			d := &firestorepb.Document{}
			if err := proto.Unmarshal(v, d); err != nil {
				t.Fatalf("DocumentEventData field %d: %v", num, err)
			}
			if num == 1 {
				value = d
			} else {
				old = d
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

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger
//
// TestEventarcFirestoreTriggers (#1877), with the official Eventarc and
// Firestore clients, as Google's "Route Firestore events to Cloud Run"
// documents it: a google.cloud.firestore.document.v1.written trigger on
// the (default) database whose document filter is the path pattern
// users/{userId} receives a create and an update of users/NAME, made with
// the Firestore client, as binary-mode CloudEvents with data content type
// application/protobuf and DocumentEventData: the new value, and for the
// update the old value and the update mask. An updated trigger receives
// only the update; a write to posts/ reaches neither. A trigger with no
// database filter is refused.
func TestEventarcFirestoreTriggers(t *testing.T) {
	h := New(t)
	fsAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_FIRESTORE", EnvFirestore)
	f := newEventarcFixture(t, h, "eventarc-fs-probe")
	t.Setenv("FIRESTORE_EMULATOR_HOST", fsAddr)
	fc, err := firestore.NewClient(f.ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })

	typ := func(kind string) *eventarcpb.EventFilter {
		return &eventarcpb.EventFilter{Attribute: "type", Value: "google.cloud.firestore.document.v1." + kind}
	}
	db := &eventarcpb.EventFilter{Attribute: "database", Value: "(default)"}
	users := &eventarcpb.EventFilter{Attribute: "document", Value: "users/{userId}", Operator: "match-path-pattern"}
	if _, err := f.ec.CreateTrigger(f.ctx, &eventarcpb.CreateTriggerRequest{Parent: f.parent, TriggerId: "bad",
		Trigger: &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{typ("written")}, Destination: f.toService("")}}); err == nil {
		t.Error("a Firestore trigger with no database filter was accepted")
	}
	written := f.create(t, "fs-written", &eventarcpb.Trigger{Destination: f.toService("/written"),
		EventFilters: []*eventarcpb.EventFilter{typ("written"), db, users}, EventDataContentType: "application/protobuf"})
	if written.GetEventDataContentType() != "application/protobuf" {
		t.Errorf("the trigger's eventDataContentType = %q", written.GetEventDataContentType())
	}
	f.create(t, "fs-updated", &eventarcpb.Trigger{Destination: f.toService("/updated"),
		EventFilters: []*eventarcpb.EventFilter{typ("updated"), db, users}})
	// The trigger's watch of the database starts within a few seconds, as
	// a trigger on Google takes effect after a delay.
	time.Sleep(8 * time.Second)

	user := "u" + time.Now().Format("150405.000000")
	user = strings.ReplaceAll(user, ".", "")
	doc := fc.Collection("users").Doc(user)
	if _, err := doc.Set(f.ctx, map[string]any{"n": 1, "name": "first"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := doc.Set(f.ctx, map[string]any{"n": 2, "name": "first"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := fc.Collection("posts").Doc(user).Set(f.ctx, map[string]any{"n": 1}); err != nil {
		t.Fatalf("Set posts: %v", err)
	}
	t.Cleanup(func() {
		_, _ = doc.Delete(f.ctx)
		_, _ = fc.Collection("posts").Doc(user).Delete(f.ctx)
	})
	name := "projects/" + h.Project() + "/databases/(default)/documents/users/" + user
	ofUser := func(path string, n int64) func(probedEvent) bool {
		return func(e probedEvent) bool {
			if e.Path != path || e.Headers["ce-subject"] != "documents/users/"+user {
				return false
			}
			raw, _ := base64.StdEncoding.DecodeString(e.DataBase64)
			v, _, _ := documentEventData(t, raw)
			return v.GetFields()["n"].GetIntegerValue() == n
		}
	}
	created, all := probedEvents(t, f.base, f.host, ofUser("/written", 1))
	if created.Headers == nil {
		t.Fatalf("no Firestore CloudEvent for the create of %s reached the service; it got %d events", user, len(all))
	}
	for k, want := range map[string]string{"ce-type": "google.cloud.firestore.document.v1.written", "ce-specversion": "1.0",
		"ce-source":    "//firestore.googleapis.com/projects/" + h.Project() + "/databases/(default)",
		"content-type": "application/protobuf", "ce-document": "users/" + user, "ce-database": "(default)"} {
		if got := created.Headers[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(created.DataBase64)
	if v, old, _ := documentEventData(t, raw); v.GetName() != name || old != nil || v.GetFields()["name"].GetStringValue() != "first" {
		t.Errorf("the create's DocumentEventData = %v, old %v", v, old)
	}
	updated, _ := probedEvents(t, f.base, f.host, ofUser("/written", 2))
	if updated.Headers == nil {
		t.Fatal("no Firestore CloudEvent for the update reached the written trigger")
	}
	raw, _ = base64.StdEncoding.DecodeString(updated.DataBase64)
	if v, old, mask := documentEventData(t, raw); old.GetFields()["n"].GetIntegerValue() != 1 || v.GetFields()["n"].GetIntegerValue() != 2 ||
		strings.Join(mask, ",") != "n" {
		t.Errorf("the update's DocumentEventData = %v, old %v, mask %v", v, old, mask)
	}
	if u, _ := probedEvents(t, f.base, f.host, ofUser("/updated", 2)); u.Headers["ce-type"] != "google.cloud.firestore.document.v1.updated" {
		t.Errorf("the updated trigger's event = %v", u.Headers)
	}
	_, all = probedEvents(t, f.base, f.host, func(probedEvent) bool { return true })
	for _, e := range all {
		if e.Path == "/updated" && e.Headers["ce-subject"] == "documents/users/"+user {
			raw, _ := base64.StdEncoding.DecodeString(e.DataBase64)
			if _, old, _ := documentEventData(t, raw); old == nil {
				t.Errorf("the updated trigger received the create")
			}
		}
		if strings.HasPrefix(e.Headers["ce-subject"], "documents/posts/") {
			t.Errorf("a write to posts/ reached the service: %v", e.Headers)
		}
	}
}
