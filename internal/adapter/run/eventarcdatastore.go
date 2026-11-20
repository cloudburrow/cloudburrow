package run

// Firestore in Datastore mode events (#1877): google.cloud.datastore.entity.v1
// created, updated, deleted and written, and their withAuthContext
// variants, from this instance's Datastore, as Google's "Extend Datastore
// with Cloud Functions (2nd gen)" documents them: a trigger filters on
// database (required), namespace and entity (exact, or by path pattern,
// kind/id pairs such as users/{userId}), and receives EntityEventData in
// protobuf (value, old_value, update_mask) with the project, location,
// database, namespace and entity extensions.
//
// The Datastore emulator documents no change notifications, so the CLI
// queries every kind of every namespace of each database a trigger names
// each second and hands each change to EmitDatastore, which publishes it
// to each matching trigger's topic as Firestore's are.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	datastorepb "cloud.google.com/go/datastore/apiv1/datastorepb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const datastorePrefix = "google.cloud.datastore.entity.v1."

func init() {
	attrs := []eaAttr{
		{name: "database", description: "The Firestore database", required: true},
		{name: "namespace", description: "The namespace of the entity"},
		{name: "entity", description: "The path of the entity: its kinds and IDs", pattern: true},
	}
	for _, kind := range []string{"created", "updated", "deleted", "written"} {
		for _, typ := range []string{datastorePrefix + kind, datastorePrefix + kind + ".withAuthContext"} {
			eaTypeAttrs[typ] = attrs
			eaSchemaURIs[typ] = "https://googleapis.github.io/google-cloudevents/proto/google/events/cloud/datastore/v1/data.proto"
		}
		eaTypeDescriptions[datastorePrefix+kind] = "A Firestore entity is " + kind + " in Datastore mode"
		eaTypeDescriptions[datastorePrefix+kind+".withAuthContext"] = "A Firestore entity is " + kind + " in Datastore mode, with the auth context"
	}
	eaTypeDescriptions[datastorePrefix+"written"] = "A Firestore entity is created, updated or deleted in Datastore mode"
}

func isDatastoreType(typ string) bool { return strings.HasPrefix(typ, datastorePrefix) }

// DatastoreWatch is a database some Datastore mode trigger names.
type DatastoreWatch struct{ Project, Database string }

// DatastoreWatches are the databases Datastore mode triggers name, which
// the CLI watches.
func DatastoreWatches() []DatastoreWatch {
	eaState.mu.Lock()
	specs := map[string]*functionspb.EventTrigger{}
	for name, x := range eaState.triggers {
		specs[name] = x.live.spec
	}
	eaState.mu.Unlock()
	for _, live := range functionTriggers() {
		specs[live.spec.GetTrigger()] = live.spec
	}
	seen := map[DatastoreWatch]bool{}
	var out []DatastoreWatch
	for name, spec := range specs {
		if !isDatastoreType(spec.GetEventType()) {
			continue
		}
		w := DatastoreWatch{Project: strings.Split(name, "/")[1]}
		for _, f := range spec.GetEventFilters() {
			if f.GetAttribute() == "database" {
				w.Database = f.GetValue()
			}
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project+"/"+out[i].Database < out[j].Project+"/"+out[j].Database })
	return out
}

// DatastoreChange is one entity's change: Old is nil for a create, New
// for a delete.
type DatastoreChange struct {
	Project, Database, Location string
	Old, New                    *datastorepb.EntityResult
	Time                        time.Time
}

func (c DatastoreChange) kind() string {
	switch {
	case c.Old == nil:
		return "created"
	case c.New == nil:
		return "deleted"
	}
	return "updated"
}

func (c DatastoreChange) key() *datastorepb.Key {
	if c.New != nil {
		return c.New.GetEntity().GetKey()
	}
	return c.Old.GetEntity().GetKey()
}

// EntityPath is a key's path as the entity filter names it: each kind and
// its name or ID, separated by slashes.
func EntityPath(k *datastorepb.Key) string {
	var parts []string
	for _, e := range k.GetPath() {
		id := e.GetName()
		if id == "" {
			id = strconv.FormatInt(e.GetId(), 10)
		}
		parts = append(parts, e.GetKind(), id)
	}
	return strings.Join(parts, "/")
}

// propertyMask is the top-level properties an update changed, added or
// removed, sorted.
func propertyMask(old, cur *datastorepb.Entity) []string {
	var out []string
	for k, v := range cur.GetProperties() {
		if ov, ok := old.GetProperties()[k]; !ok || !proto.Equal(ov, v) {
			out = append(out, k)
		}
	}
	for k := range old.GetProperties() {
		if _, ok := cur.GetProperties()[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// entityEventData is google.events.cloud.datastore.v1.EntityEventData in
// protobuf: value (1) and old_value (2), EntityResults whose field numbers
// are google.datastore.v1.EntityResult's (entity, version, update_time,
// create_time; no cursor), and update_mask (3), a PropertyMask of
// property_paths (1).
func entityEventData(c DatastoreChange) ([]byte, error) {
	var b []byte
	for _, part := range []struct {
		num protowire.Number
		r   *datastorepb.EntityResult
	}{{1, c.New}, {2, c.Old}} {
		if part.r == nil {
			continue
		}
		r := proto.Clone(part.r).(*datastorepb.EntityResult)
		r.Cursor = nil
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, part.num, protowire.BytesType)
		b = protowire.AppendBytes(b, raw)
	}
	if c.kind() == "updated" {
		var mask []byte
		for _, p := range propertyMask(c.Old.GetEntity(), c.New.GetEntity()) {
			mask = protowire.AppendTag(mask, 1, protowire.BytesType)
			mask = protowire.AppendString(mask, p)
		}
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, mask)
	}
	return b, nil
}

// EmitDatastore publishes one entity change to every trigger it matches:
// a created, updated or deleted trigger of its kind, and every written
// trigger, filtered on database, namespace and entity.
func (e *EventarcServer) EmitDatastore(ctx context.Context, c DatastoreChange) int {
	k := c.key()
	path := EntityPath(k)
	namespace := k.GetPartitionId().GetNamespaceId()
	if namespace == "" {
		namespace = "(default)"
	}
	database := c.Database
	if database == "" {
		database = "(default)"
	}
	kind := c.kind()
	types := []string{datastorePrefix + kind, datastorePrefix + kind + ".withAuthContext",
		datastorePrefix + "written", datastorePrefix + "written.withAuthContext"}
	value := func(attr string) (string, bool) {
		switch attr {
		case "database":
			return database, true
		case "namespace":
			return namespace, true
		case "entity":
			return path, true
		}
		return "", false
	}
	data, err := entityEventData(c)
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
	attrs := map[string]string{
		"ce-id":        fmt.Sprintf("%x", c.Time.UnixNano()) + "-" + kind,
		"ce-source":    "//firestore.googleapis.com/projects/" + c.Project + "/databases/" + database,
		"ce-subject":   "entities/" + path,
		"ce-time":      c.Time.UTC().Format(time.RFC3339Nano),
		"ce-location":  location,
		"ce-project":   c.Project,
		"ce-database":  database,
		"ce-namespace": namespace,
		"ce-entity":    path,
		"content-type": "application/protobuf",
	}
	n := 0
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
