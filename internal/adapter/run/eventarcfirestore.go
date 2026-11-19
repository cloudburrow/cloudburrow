package run

// Firestore events (#1877): google.cloud.firestore.document.v1.created,
// updated, deleted and written, and their withAuthContext variants, from
// this instance's Firestore, as Google's "Route Firestore events to Cloud
// Run" documents them: a trigger filters on database (required),
// namespace and document (exact, or by path pattern), and receives
// DocumentEventData in protobuf (application/protobuf).
//
// The Firestore emulator documents no change notifications, so the CLI
// watches what each trigger names with Firestore's own Listen, on the
// collection group its document filter ends in, or, when that can be any
// collection, by querying every collection each second, and hands each
// change to EmitFirestore with the document before and after it, which
// publishes it to each matching trigger's topic as audit logs are. Two
// watches can see one change, so EmitFirestore drops a change it has
// already published.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const firestorePrefix = "google.cloud.firestore.document.v1."

func init() {
	firestoreAttrs := []eaAttr{
		{name: "database", description: "The Firestore database", required: true},
		{name: "namespace", description: "The namespace of the document"},
		{name: "document", description: "The path of the document, relative to the database", pattern: true},
	}
	for _, kind := range []string{"created", "updated", "deleted", "written"} {
		for _, typ := range []string{firestorePrefix + kind, firestorePrefix + kind + ".withAuthContext"} {
			eaTypeAttrs[typ] = firestoreAttrs
			eaSchemaURIs[typ] = "https://googleapis.github.io/google-cloudevents/proto/google/events/cloud/firestore/v1/data.proto"
		}
		eaTypeDescriptions[firestorePrefix+kind] = "A Firestore document is " + kind
		eaTypeDescriptions[firestorePrefix+kind+".withAuthContext"] = "A Firestore document is " + kind + ", with the auth context"
	}
	eaTypeDescriptions[firestorePrefix+"written"] = "A Firestore document is created, updated or deleted"
}

// FirestoreWatch is a database some trigger names, and the collection ID
// its document filter ends in: "" when the filter names none (no filter,
// a wildcard collection or a ** segment), so every collection is watched.
type FirestoreWatch struct{ Project, Database, Collection string }

// documentCollection is the collection ID of the documents a document
// filter matches, or "" when it can be any.
func documentCollection(f *functionspb.EventFilter) string {
	if f == nil {
		return ""
	}
	segs := strings.Split(strings.Trim(f.GetValue(), "/"), "/")
	if len(segs) < 2 || strings.Contains(f.GetValue(), "**") {
		return ""
	}
	c := segs[len(segs)-2]
	if f.GetOperator() == pathPatternOp && strings.ContainsAny(c, "*{}") {
		return ""
	}
	return c
}

// FirestoreWatches are the databases Firestore triggers name, which the
// CLI watches.
func FirestoreWatches() []FirestoreWatch {
	eaState.mu.Lock()
	defer eaState.mu.Unlock()
	seen := map[FirestoreWatch]bool{}
	var out []FirestoreWatch
	for name, x := range eaState.triggers {
		spec := x.live.spec
		if !isFirestoreType(spec.GetEventType()) {
			continue
		}
		w := FirestoreWatch{Project: strings.Split(name, "/")[1]}
		var doc *functionspb.EventFilter
		for _, f := range spec.GetEventFilters() {
			switch f.GetAttribute() {
			case "database":
				w.Database = f.GetValue()
			case "document":
				doc = f
			}
		}
		w.Collection = documentCollection(doc)
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Project+"/"+out[i].Database+"/"+out[i].Collection < out[j].Project+"/"+out[j].Database+"/"+out[j].Collection
	})
	return out
}

// FirestoreChange is one document's change: Old is nil for a create, New
// for a delete.
type FirestoreChange struct {
	Project, Database, Location string
	// Name is the document's full name, projects/P/databases/D/documents/PATH.
	Name     string
	Old, New *firestorepb.Document
	Time     time.Time
}

// firestoreKind is the change's event kind.
func (c FirestoreChange) kind() string {
	switch {
	case c.Old == nil:
		return "created"
	case c.New == nil:
		return "deleted"
	}
	return "updated"
}

// updateMask is the top-level fields an update changed, added or removed,
// sorted.
func updateMask(old, cur *firestorepb.Document) []string {
	var out []string
	for k, v := range cur.GetFields() {
		if ov, ok := old.GetFields()[k]; !ok || !proto.Equal(ov, v) {
			out = append(out, k)
		}
	}
	for k := range old.GetFields() {
		if _, ok := cur.GetFields()[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// documentEventData is google.events.cloud.firestore.v1.DocumentEventData
// in protobuf: value (1) and old_value (2), Documents whose field numbers
// are google.firestore.v1.Document's, and update_mask (3), a DocumentMask
// of field_paths (1).
func documentEventData(c FirestoreChange) ([]byte, error) {
	var b []byte
	for _, part := range []struct {
		num protowire.Number
		doc *firestorepb.Document
	}{{1, c.New}, {2, c.Old}} {
		if part.doc == nil {
			continue
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(part.doc)
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, part.num, protowire.BytesType)
		b = protowire.AppendBytes(b, raw)
	}
	if c.kind() == "updated" {
		var mask []byte
		for _, p := range updateMask(c.Old, c.New) {
			mask = protowire.AppendTag(mask, 1, protowire.BytesType)
			mask = protowire.AppendString(mask, p)
		}
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, mask)
	}
	return b, nil
}

// EmitFirestore publishes one document change to every trigger it
// matches: a created, updated or deleted trigger of its kind, and every
// written trigger, filtered on database, namespace and document.
func (e *EventarcServer) EmitFirestore(ctx context.Context, c FirestoreChange) int {
	prefix := "projects/" + c.Project + "/databases/" + c.Database + "/documents/"
	doc, ok := strings.CutPrefix(c.Name, prefix)
	if !ok {
		return 0
	}
	if !firstSighting(c) {
		return 0
	}
	kind := c.kind()
	types := []string{firestorePrefix + kind, firestorePrefix + kind + ".withAuthContext",
		firestorePrefix + "written", firestorePrefix + "written.withAuthContext"}
	value := func(attr string) (string, bool) {
		switch attr {
		case "database":
			return c.Database, true
		case "namespace":
			return "(default)", true
		case "document":
			return doc, true
		}
		return "", false
	}
	n := 0
	data, err := documentEventData(c)
	if err != nil {
		return 0
	}
	if c.Time.IsZero() {
		c.Time = time.Now()
	}
	location := c.Location
	if location == "" {
		location = "nam5"
	}
	ts := c.Time.UTC().Format(time.RFC3339Nano)
	attrs := map[string]string{
		"ce-id":        fmt.Sprintf("%x", c.Time.UnixNano()) + "-" + kind,
		"ce-source":    "//firestore.googleapis.com/projects/" + c.Project + "/databases/" + c.Database,
		"ce-subject":   "documents/" + doc,
		"ce-time":      ts,
		"ce-location":  location,
		"ce-project":   c.Project,
		"ce-database":  c.Database,
		"ce-namespace": "(default)",
		"ce-document":  doc,
		"content-type": "application/protobuf",
	}
	for _, typ := range types {
		topics := eaMatching(c.Project, []string{typ}, value)
		a := attrs
		if strings.HasSuffix(typ, ".withAuthContext") {
			a = map[string]string{}
			for k, v := range attrs {
				a[k] = v
			}
			// No caller is authenticated here (ADR-0006).
			a["ce-authtype"], a["ce-authid"] = "unauthenticated", ""
		}
		n += e.f.publishSourced(ctx, topics, data, a)
	}
	return n
}

// published remembers the changes published recently, by document and
// update time, so the same change seen by two watches is published once.
var published = struct {
	sync.Mutex
	seen map[string]time.Time
}{seen: map[string]time.Time{}}

func firstSighting(c FirestoreChange) bool {
	stamp := c.New.GetUpdateTime().AsTime().String()
	if c.New == nil {
		stamp = "deleted:" + c.Old.GetUpdateTime().AsTime().String()
	}
	key := c.Name + "@" + c.kind() + "@" + stamp
	published.Lock()
	defer published.Unlock()
	now := time.Now()
	for k, t := range published.seen {
		if now.Sub(t) > time.Minute {
			delete(published.seen, k)
		}
	}
	if _, ok := published.seen[key]; ok {
		return false
	}
	published.seen[key] = now
	return true
}
