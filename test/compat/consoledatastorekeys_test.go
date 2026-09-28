//go:build compat

package compat

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// rawDatastore is the emulator's Datastore v1 API itself, for what the
// official Go client cannot express: a key value whose partition names
// another project or database (the client's Key has neither).
func rawDatastore(t *testing.T, h *Harness) datastorepb.DatastoreClient {
	t.Helper()
	conn, err := grpc.NewClient(h.Endpoint(EnvDatastore), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return datastorepb.NewDatastoreClient(conn)
}

// TestConsoleDatastoreKeepsAKeyValuesProjectAndDatabase (#893, #894).
//
// A key value names its entity's project and database as well as its path.
// The official Go client's Key holds neither: the console read an entity
// through it and wrote the whole entity back on Add, Edit and Delete
// property, and against the emulator that rewrote a key value naming
// another-project, or database another-db, to this project's default
// database, without a word. An entity written through the v1 API holds such
// keys, at the top level and in an array, beside keys to this project's
// Order named "id=7" and numbered 7 in an array and in an embedded entity,
// which the client writes too. Its page shows each foreign key with where it
// is, and nested keys as key(Order/name=id=7) and key(Order/id=7); a
// foreign key's page offers no Edit property, and says why. Add property,
// Edit property on another property and Delete property each leave every
// other value exactly as it was, read back through the v1 API, and the
// official client reads the nested keys back unchanged.
func TestConsoleDatastoreKeepsAKeyValuesProjectAndDatabase(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	holderKey := datastore.NameKey("Holder893", "h", nil)
	holder := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
		Path: []*datastorepb.Key_PathElement{{Kind: "Holder893", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}}
	ref := func(p, db string, el *datastorepb.Key_PathElement) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: &datastorepb.Key{
			PartitionId: &datastorepb.PartitionId{ProjectId: p, DatabaseId: db},
			Path:        []*datastorepb.Key_PathElement{el}}}}
	}
	id7 := func() *datastorepb.Key_PathElement {
		return &datastorepb.Key_PathElement{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}
	}
	named7 := func() *datastorepb.Key_PathElement {
		return &datastorepb.Key_PathElement{Kind: "Order", IdType: &datastorepb.Key_PathElement_Name{Name: "id=7"}}
	}
	array := func(vs ...*datastorepb.Value) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{Values: vs}}}
	}
	stored := map[string]*datastorepb.Value{
		"otherProject": ref("another-project", "", id7()),
		"otherDB":      ref(project, "another-db", id7()),
		"otherBoth":    ref("another-project", "another-db", named7()),
		"mixed":        array(ref("another-project", "", id7()), ref(project, "", named7())),
		"refs":         array(ref(project, "", named7()), ref(project, "", id7())),
		"line": {ValueType: &datastorepb.Value_EntityValue{EntityValue: &datastorepb.Entity{
			Properties: map[string]*datastorepb.Value{"order": ref(project, "", named7())}}}},
		"note": {ValueType: &datastorepb.Value_StringValue{StringValue: "x"}},
	}
	upsert := &datastorepb.Entity{Key: holder, Properties: map[string]*datastorepb.Value{}}
	for k, v := range stored {
		upsert.Properties[k] = v
	}
	if _, err := raw.Commit(ctx, &datastorepb.CommitRequest{ProjectId: project, Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Upsert{Upsert: upsert}}}}); err != nil {
		t.Fatalf("Commit through the v1 API: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, holderKey) })

	lookup := func() map[string]*datastorepb.Value {
		t.Helper()
		r, err := raw.Lookup(ctx, &datastorepb.LookupRequest{ProjectId: project, Keys: []*datastorepb.Key{holder}})
		if err != nil || len(r.GetFound()) != 1 {
			t.Fatalf("Lookup through the v1 API = %v, %v", r, err)
		}
		return r.GetFound()[0].GetEntity().GetProperties()
	}
	// The emulator keeps what was written, so a change below is the
	// console's.
	for name, want := range stored {
		if got := lookup()[name]; !proto.Equal(got, want) {
			t.Fatalf("the emulator stored %s as %v, want %v", name, got, want)
		}
	}

	entity := []string{"Holder893", holderKey.Encode()}
	page := consoleWriteDetail(t, addr, "datastore", project, entity...)
	cells := map[string]string{}
	for _, s := range page.Sections {
		if s.ID == "properties" {
			for _, it := range s.Listing.Items {
				cells[it.Name] = it.Fields["Value"]
			}
		}
	}
	for name, want := range map[string]string{
		"otherProject": "Order/id=7 (project another-project)",
		"otherDB":      "Order/id=7 (database another-db)",
		"otherBoth":    "Order/name=id=7 (project another-project, database another-db)",
		"mixed":        "[key(Order/id=7 (project another-project)), key(Order/name=id=7)]",
		"refs":         "[key(Order/name=id=7), key(Order/id=7)]",
		"line":         `{"order": key(Order/name=id=7)}`,
	} {
		if cells[name] != want {
			t.Errorf("the entity's page shows %s as %q, want %q", name, cells[name], want)
		}
	}

	type propertyPage struct {
		Sections []struct{ ID, Text, Note string }
		Edit     *struct{}
	}
	for _, name := range []string{"otherProject", "otherDB", "otherBoth", "mixed", "refs"} {
		var pp propertyPage
		q := url.Values{"project": {project}, "name": append(append([]string{}, entity...), name)}
		consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "", &pp)
		if pp.Edit != nil {
			t.Errorf("%s's page offers Edit property, which would write it in this project's default database", name)
		}
		if len(pp.Sections) == 0 || !strings.Contains(pp.Sections[0].Note, "cannot be edited here") {
			t.Errorf("%s's page does not say why it offers no edit: %+v", name, pp.Sections)
		}
	}

	// Add property, Edit property on another one, and Delete property.
	if code, out := consoleAct(t, addr, "datastore", project, entity, "addproperty",
		map[string]string{"field": "added", "type": "string", "value": "y"}); code != http.StatusOK {
		t.Fatalf("Add property = %d: %s", code, out)
	}
	if code, out := consoleEdit(t, addr, "datastore", project, append(append([]string{}, entity...), "note"),
		map[string]string{"type": "string", "value": "changed"}); code != http.StatusOK {
		t.Fatalf("Edit property = %d: %s", code, out)
	}
	if code, out := consoleAct(t, addr, "datastore", project, append(append([]string{}, entity...), "added"),
		"deleteproperty", nil); code != http.StatusOK {
		t.Fatalf("Delete property = %d: %s", code, out)
	}

	after := lookup()
	for name, want := range stored {
		if name == "note" {
			continue
		}
		if !proto.Equal(after[name], want) {
			t.Errorf("after Add, Edit and Delete property, %s reads back %v through the v1 API, want %v as written",
				name, after[name], want)
		}
	}
	if _, has := after["added"]; has {
		t.Error("the deleted property reads back")
	}
	if len(after) != len(stored) {
		t.Errorf("the entity has %d properties, want %d", len(after), len(stored))
	}

	// The official client: the note changed, and the keys in this project
	// read back as they were written.
	var props datastore.PropertyList
	if err := c.Get(ctx, holderKey, &props); err != nil {
		t.Fatalf("Get %v: %v", holderKey, err)
	}
	got := map[string]any{}
	for _, p := range props {
		got[p.Name] = p.Value
	}
	if got["note"] != "changed" {
		t.Errorf("the client reads note %#v, want changed", got["note"])
	}
	named, numeric := datastore.NameKey("Order", "id=7", nil), datastore.IDKey("Order", 7, nil)
	if refs, _ := got["refs"].([]any); len(refs) != 2 || !keyEquals(refs[0], named) || !keyEquals(refs[1], numeric) {
		t.Errorf("the client reads refs %#v, want [%v %v]", got["refs"], named, numeric)
	}
	if line, _ := got["line"].(*datastore.Entity); line == nil || len(line.Properties) != 1 || !keyEquals(line.Properties[0].Value, named) {
		t.Errorf("the client reads line %#v, want its order %v", got["line"], named)
	}
}

func keyEquals(v any, want *datastore.Key) bool {
	k, ok := v.(*datastore.Key)
	return ok && k.Equal(want)
}
