package main

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// offlineFirestore is a client for building references. It dials nothing
// until a call is made, and none is.
func offlineFirestore(t *testing.T) *firestore.Client {
	t.Helper()
	c, err := firestore.NewClient(context.Background(), "demo", localOpts("127.0.0.1:1")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestEditFormsRoundTripTheStoredType (#796).
//
// An edit form is prefilled with the stored value in the form it is parsed
// from, so saving a field unchanged writes the same type and value back: a
// double 2.0 must not come back as the integer 2, a geopoint as a string, or a
// double inside a map as an integer. The same holds on Datastore, whose types
// are Google's Datastore console's.
func TestEditFormsRoundTripTheStoredType(t *testing.T) {
	c := offlineFirestore(t)
	stamp := time.Date(2026, 9, 27, 15, 4, 5, 123456000, time.UTC)
	for _, v := range []any{
		nil, true, false, int64(42), int64(-7), 2.0, 1.5, -0.25, 1e300, "hello", "42", "",
		stamp, &latlng.LatLng{Latitude: 51.5, Longitude: -0.12}, c.Doc("users/alice"),
		map[string]any{"x": int64(1), "y": 2.0, "z": []any{"a", nil, true}, "w": map[string]any{"q": "r"}},
		[]any{int64(1), 2.5, "x"},
	} {
		typ, raw, ok := formatFirestoreValue(v)
		if !ok {
			t.Errorf("Firestore %#v offers no edit", v)
			continue
		}
		back, err := parseFirestoreValue(c, "demo", typ, raw)
		if err != nil {
			t.Errorf("Firestore %#v: the form's own %s %q is refused: %v", v, typ, raw, err)
			continue
		}
		if ref, isRef := v.(*firestore.DocumentRef); isRef {
			if got, _ := back.(*firestore.DocumentRef); got == nil || got.Path != ref.Path {
				t.Errorf("Firestore reference %s came back as %#v", ref.Path, back)
			}
			continue
		}
		if !reflect.DeepEqual(back, v) || firestoreType(back) != firestoreType(v) {
			t.Errorf("Firestore %#v (%s) came back from %s %q as %#v (%s)",
				v, firestoreType(v), typ, raw, back, firestoreType(back))
		}
	}

	key := datastore.IDKey("Child", 7, datastore.NameKey("Parent", "p", nil))
	for _, v := range []any{
		nil, true, int64(42), 2.0, 1.5, "hello", "42", stamp, key,
		datastore.GeoPoint{Lat: 51.5, Lng: -0.12},
		[]any{int64(1), 2.5, "x"},
		&datastore.Entity{Properties: []datastore.Property{{Name: "a", Value: int64(1)}, {Name: "b", Value: 2.0}}},
	} {
		typ, raw, ok := formatDatastoreValue(v, false)
		if !ok {
			t.Errorf("Datastore %#v offers no edit", v)
			continue
		}
		back, err := parseDatastoreValue(typ, raw)
		if err != nil {
			t.Errorf("Datastore %#v: the form's own %s %q is refused: %v", v, typ, raw, err)
			continue
		}
		if !reflect.DeepEqual(back, v) || datastoreType(back) != datastoreType(v) {
			t.Errorf("Datastore %#v (%s) came back from %s %q as %#v (%s)",
				v, datastoreType(v), typ, raw, back, datastoreType(back))
		}
	}
}

// TestValuesTheFormCannotHoldOfferNoEdit (#796).
//
// A map or array is written as JSON, which has no timestamp, geopoint,
// reference or bytes: an edit of one holding them would save them back as
// strings. They are shown and deletable, and not editable.
func TestValuesTheFormCannotHoldOfferNoEdit(t *testing.T) {
	c := offlineFirestore(t)
	for _, v := range []any{
		[]byte("x"),
		map[string]any{"at": time.Now()},
		[]any{c.Doc("users/alice")},
		map[string]any{"nan": nanValue()},
	} {
		if typ, raw, ok := formatFirestoreValue(v); ok {
			t.Errorf("Firestore %#v offers an edit as %s %q, which would change it", v, typ, raw)
		}
	}
	for _, v := range []any{
		[]byte("x"),
		[]any{time.Now()},
		&datastore.Entity{Key: datastore.NameKey("K", "a", nil)},
		&datastore.Entity{Properties: []datastore.Property{{Name: "a", Value: "x", NoIndex: true}}},
	} {
		if typ, raw, ok := formatDatastoreValue(v, false); ok {
			t.Errorf("Datastore %#v offers an edit as %s %q, which would change it", v, typ, raw)
		}
	}
	// An embedded entity in an excluded property reads back with its own
	// properties excluded, and is editable there.
	excluded := &datastore.Entity{Properties: []datastore.Property{{Name: "a", Value: "x", NoIndex: true}}}
	if _, _, ok := formatDatastoreValue(excluded, true); !ok {
		t.Error("an embedded entity in an excluded property offers no edit")
	}
	namespaced := datastore.NameKey("K", "a", nil)
	namespaced.Namespace = "ns"
	if _, _, ok := formatDatastoreValue(namespaced, false); ok {
		t.Error("a key in a namespace offers an edit that would drop the namespace")
	}
}

func nanValue() float64 {
	zero := 0.0
	return zero / zero
}

// TestTypedValuesAreReadAsTheirTypeNeverGuessed (#796).
//
// The form names the type, and the value is read as that type: "42" as a
// string is a string, and "yes" is not a boolean.
func TestTypedValuesAreReadAsTheirTypeNeverGuessed(t *testing.T) {
	c := offlineFirestore(t)
	for _, tc := range []struct {
		typ, raw string
		want     any
	}{
		{"string", "42", "42"},
		{"string", " padded ", " padded "},
		{"number", "42", int64(42)},
		{"number", "42.0", 42.0},
		{"number", "1e3", 1000.0},
		{"boolean", "false", false},
		{"null", "", nil},
		{"timestamp", "2026-09-27T15:04:05Z", time.Date(2026, 9, 27, 15, 4, 5, 0, time.UTC)},
		{"geopoint", "1, 2", &latlng.LatLng{Latitude: 1, Longitude: 2}},
		{"map", `{"n": 1, "d": 1.0}`, map[string]any{"n": int64(1), "d": 1.0}},
		{"array", `[]`, []any{}},
	} {
		got, err := parseFirestoreValue(c, "demo", tc.typ, tc.raw)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Firestore %s %q = %#v, %v; want %#v", tc.typ, tc.raw, got, err, tc.want)
		}
	}
	ref, err := parseFirestoreValue(c, "demo", "reference",
		"projects/demo/databases/(default)/documents/users/alice")
	if r, _ := ref.(*firestore.DocumentRef); err != nil || r == nil || r.ID != "alice" {
		t.Errorf("a full document name is not read as a reference: %#v, %v", ref, err)
	}
	for _, tc := range [][2]string{
		{"boolean", "yes"}, {"number", "forty"}, {"number", "99999999999999999999"}, {"null", "0"},
		{"timestamp", "yesterday"}, {"geopoint", "1"}, {"reference", "users"},
		{"reference", "projects/other/databases/(default)/documents/users/alice"},
		{"map", "[1]"}, {"array", `{"a": 1}`}, {"map", "{"}, {"bytes", "x"},
	} {
		if v, err := parseFirestoreValue(c, "demo", tc[0], tc[1]); err == nil {
			t.Errorf("Firestore %s %q was accepted as %#v", tc[0], tc[1], v)
		}
	}

	for _, tc := range []struct {
		typ, raw string
		want     any
	}{
		{"integer", "42", int64(42)},
		{"float", "42", 42.0},
		{"string", "true", "true"},
		{"key", "Order/id=7", datastore.IDKey("Order", 7, nil)},
		{"key", "Customer/alice/Order/x", datastore.NameKey("Order", "x", datastore.NameKey("Customer", "alice", nil))},
		{"entity", `{"a": 1}`, &datastore.Entity{Properties: []datastore.Property{{Name: "a", Value: int64(1)}}}},
	} {
		got, err := parseDatastoreValue(tc.typ, tc.raw)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Datastore %s %q = %#v, %v; want %#v", tc.typ, tc.raw, got, err, tc.want)
		}
	}
	for _, tc := range [][2]string{
		{"integer", "4.5"}, {"key", "Order"}, {"key", "Order/id=x"}, {"entity", "[1]"}, {"array", "{}"}, {"blob", "x"},
	} {
		if v, err := parseDatastoreValue(tc[0], tc[1]); err == nil {
			t.Errorf("Datastore %s %q was accepted as %#v", tc[0], tc[1], v)
		}
	}
}

// TestDocumentAndEntityActionsAreOfferedWhereTheyApply (#796).
//
// Add document and Create entity on a collection or kind; Add field or
// property and Delete on a document or entity; Delete on a field or property.
// Every delete has no inputs, so the page confirms it by the name typed back,
// and leaves the page of what it deleted.
func TestDocumentAndEntityActionsAreOfferedWhereTheyApply(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		actor console.PathActor
		want  map[int][]string
	}{
		{firestoreProvider{}, map[int][]string{
			1: {"adddocument"}, 2: {"addfield", "startcollection", "deletedocument"}, 3: {"deletefield"}}},
		{datastoreProvider{}, map[int][]string{
			1: {"createentity"}, 2: {"addproperty", "createchild", "deleteentity"}, 3: {"deleteproperty"}}},
	} {
		if got := tc.actor.DetailActions(ctx, "", []string{"a"}); len(got) != 0 {
			t.Errorf("%T offers %v with no project chosen", tc.actor, got)
		}
		for depth := 1; depth <= 4; depth++ {
			var ids []string
			for _, a := range tc.actor.DetailActions(ctx, "demo", make([]string, depth)) {
				ids = append(ids, a.ID)
				deletes := strings.HasPrefix(a.ID, "delete")
				if a.Destructive != deletes || a.Leaves != deletes {
					t.Errorf("%T %s: destructive %v, leaves %v", tc.actor, a.ID, a.Destructive, a.Leaves)
				}
				if deletes && len(a.Fields) > 0 {
					t.Errorf("%T %s has inputs, so the client would not ask for the name back", tc.actor, a.ID)
				}
				if !deletes && len(a.Fields) == 0 {
					t.Errorf("%T %s has no inputs", tc.actor, a.ID)
				}
			}
			if !reflect.DeepEqual(ids, tc.want[depth]) {
				t.Errorf("%T at depth %d offers %v, want %v", tc.actor, depth, ids, tc.want[depth])
			}
		}
	}
	// Edit is a field's and a property's, never a document's or an entity's.
	for _, e := range []console.Editor{firestoreProvider{}, datastoreProvider{}} {
		if err := e.Edit(ctx, "demo", []string{"a", "b"}, nil); err == nil {
			t.Errorf("%T edits a whole document or entity", e)
		}
	}
}

// TestAddFormsRefuseAValueWithNoName (#796): a value typed with no field name
// is a mistake, not a document with no fields, and is refused before any
// emulator is reached.
func TestAddFormsRefuseAValueWithNoName(t *testing.T) {
	ctx := context.Background()
	if _, err := (firestoreProvider{endpoint: "127.0.0.1:1"}).addDocument(ctx, "demo", "c",
		map[string]string{"type": "string", "value": "x"}); err == nil || !strings.Contains(err.Error(), "field name") {
		t.Errorf("Firestore Add document with a nameless value = %v", err)
	}
	if _, err := (datastoreProvider{endpoint: "127.0.0.1:1"}).createEntity(ctx, "demo", "", "K", nil,
		map[string]string{"type": "string", "value": "x"}); err == nil || !strings.Contains(err.Error(), "property name") {
		t.Errorf("Datastore Create entity with a nameless value = %v", err)
	}
}

// TestDatastorePathsAddressNamespacesAndAncestors (#854).
//
// A page in a namespace other than the default is addressed with the
// namespace first, behind a segment no kind can be named (a kind that begins
// and ends with two underscores is Datastore's), and a child entity by its
// whole key path. The name a listing gives an entity is read back to the same
// key, in its namespace and with its ancestors, so a row and the page it
// opens address one entity; the breadcrumb of a namespaced page names the
// namespace rather than the segment that addresses it.
func TestDatastorePathsAddressNamespacesAndAncestors(t *testing.T) {
	for _, tc := range []struct {
		path []string
		want datastoreScope
		rest []string
	}{
		{[]string{"Order", "id=7"}, datastoreScope{}, []string{"Order", "id=7"}},
		{[]string{"__namespace__"}, datastoreScope{index: true, namespaced: true}, nil},
		{[]string{"__namespace__", "tenant-a"}, datastoreScope{ns: "tenant-a", namespaced: true}, []string{}},
		{[]string{"__namespace__", "tenant-a", "Order", "k", "p"}, datastoreScope{ns: "tenant-a", namespaced: true},
			[]string{"Order", "k", "p"}},
	} {
		scope, rest := parseDatastorePath(tc.path)
		if scope != tc.want || len(rest) != len(tc.rest) || (len(rest) > 0 && !reflect.DeepEqual(rest, tc.rest)) {
			t.Errorf("%v parsed as %+v %v, want %+v %v", tc.path, scope, rest, tc.want, tc.rest)
		}
	}
	ns := datastoreScope{ns: "tenant-a", namespaced: true}
	if got := ns.at("Order", "k"); !reflect.DeepEqual(got, []string{"__namespace__", "tenant-a", "Order", "k"}) {
		t.Errorf("a namespaced path = %v", got)
	}
	if got := (datastoreScope{}).at("Order"); !reflect.DeepEqual(got, []string{"Order"}) {
		t.Errorf("a default-namespace path = %v", got)
	}
	trail := ns.trail("Order", "k")
	var labels []string
	for _, c := range trail {
		labels = append(labels, c.Label)
	}
	if strings.Join(labels, " / ") != "Namespaces / tenant-a / Order / k" || trail[len(trail)-1].Path != nil ||
		!reflect.DeepEqual(trail[2].Path, []string{"__namespace__", "tenant-a", "Order"}) {
		t.Errorf("the namespaced trail is %+v", trail)
	}
	if (datastoreScope{}).trail("Order") != nil {
		t.Error("a default-namespace page draws its own trail; one crumb per segment is already right")
	}

	alice := datastore.NameKey("Customer", "alice", nil)
	order := datastore.IDKey("Order", 7, alice)
	line := datastore.NameKey("Line", "l/1", order)
	for _, tc := range []struct {
		key  *datastore.Key
		seg  string
		ns   string
		kind string
	}{
		{alice, "alice", "", "Customer"},
		{datastore.IDKey("Customer", 3, nil), "id=3", "", "Customer"},
		{order, "Customer/alice/Order/id=7", "", "Order"},
		{order, "Customer/alice/Order/id=7", "tenant-a", "Order"},
		// A name with a slash in a child's path is escaped, so it stays one
		// segment.
		{line, "Customer/alice/Order/id=7/Line/l%2F1", "", "Line"},
		// A root name with a slash that is not shaped like a key path is a
		// name.
		{datastore.NameKey("Doc", "a/b", nil), "a/b", "", "Doc"},
	} {
		if got := datastoreKeySegment(tc.key); got != tc.seg {
			t.Errorf("%v is listed as %q, want %q", tc.key, got, tc.seg)
		}
		back, err := datastoreEntityKey(tc.ns, tc.kind, datastoreKeySegment(tc.key))
		if err != nil {
			t.Fatalf("%q: %v", tc.seg, err)
		}
		for k := back; k != nil; k = k.Parent {
			if k.Namespace != tc.ns {
				t.Errorf("%q read back in namespace %q, want %q", tc.seg, k.Namespace, tc.ns)
			}
		}
		if datastoreKeyPath(back) != datastoreKeyPath(tc.key) {
			t.Errorf("%q read back as %s, want %s", tc.seg, datastoreKeyPath(back), datastoreKeyPath(tc.key))
		}
	}
	// A root entity's name may hold slashes; one that is not shaped like a
	// child's path is read as the name it is.
	if k, err := datastoreEntityKey("", "Order", "Customer/alice/x/Order/id=7"); err != nil || k.Parent != nil ||
		k.Name != "Customer/alice/x/Order/id=7" {
		t.Errorf("a root name with five slash-separated parts read back as %v, %v", k, err)
	}
}

// TestDatastoreEntityAddressIsUnambiguous (#875).
//
// A root entity whose own name is shaped like a child's key path — name
// Customer/alice/Order/x in kind Order — was read back as the child
// Customer/alice → Order/x, so its page could not be opened. An entity's
// page is now addressed by its key encoded as Google's console encodes it
// (Key.Encode), which reads back to that entity and no other: the root, the
// child it looks like, a root named id=7 and the numeric id 7, in the
// default namespace and another. An encoded key of another kind or
// namespace is not taken for this page's, and a link in the older form
// still opens what it did.
func TestDatastoreEntityAddressIsUnambiguous(t *testing.T) {
	alice := datastore.NameKey("Customer", "alice", nil)
	for _, ns := range []string{"", "tenant-a"} {
		root := datastore.NameKey("Order", "Customer/alice/Order/x", nil)
		child := datastore.NameKey("Order", "x", alice)
		named := datastore.NameKey("Order", "id=7", nil)
		numeric := datastore.IDKey("Order", 7, nil)
		for _, k := range []*datastore.Key{root, child, child.Parent, named, numeric} {
			for a := k; a != nil; a = a.Parent {
				a.Namespace = ns
			}
		}
		seen := map[string]bool{}
		for _, k := range []*datastore.Key{root, child, named, numeric} {
			addr := datastoreEntityAddress(k)
			if seen[addr] {
				t.Errorf("%v shares its address %q with another entity", k, addr)
			}
			seen[addr] = true
			back, err := datastoreEntityKey(ns, "Order", addr)
			if err != nil {
				t.Fatalf("%v: %v", k, err)
			}
			if !back.Equal(k) {
				t.Errorf("in namespace %q, %v is addressed as %q, which reads back as %v", ns, k, addr, back)
			}
			if got := datastoreEntityLabel(ns, "Order", addr); got != datastoreKeySegment(k) {
				t.Errorf("%v's page is named %q, want %q", k, got, datastoreKeySegment(k))
			}
		}
		// The encoded key is URL-safe, so it is one path segment as it is.
		for addr := range seen {
			if url.PathEscape(addr) != addr {
				t.Errorf("address %q is not URL-safe", addr)
			}
		}
	}

	// Another kind's or namespace's key is not this page's: the segment is
	// read as a name instead, which is what it would be.
	other := datastoreEntityAddress(datastore.NameKey("Customer", "alice", nil))
	if k, err := datastoreEntityKey("", "Order", other); err != nil || k.Kind != "Order" || k.Name != other {
		t.Errorf("a Customer key on an Order page read back as %v, %v", k, err)
	}
	inNS := datastore.NameKey("Order", "x", nil)
	inNS.Namespace = "tenant-a"
	if k, err := datastoreEntityKey("", "Order", datastoreEntityAddress(inNS)); err != nil || k.Namespace != "" || k.Name == "x" {
		t.Errorf("a tenant-a key on a default-namespace page read back as %v, %v", k, err)
	}

	// A link from before still opens what it did: the child's key path.
	if k, err := datastoreEntityKey("", "Order", "Customer/alice/Order/x"); err != nil || k.Parent == nil ||
		k.Parent.Name != "alice" || k.Name != "x" {
		t.Errorf("the old key-path link read back as %v, %v", k, err)
	}
}

// TestFirestoreSubcollectionPagesNameEveryLevel (#854).
//
// A subcollection's page is addressed by its whole path in one segment, so
// its breadcrumb is drawn from that path: each collection and document above
// it opens its own page, and the page itself is the last crumb.
func TestFirestoreSubcollectionPagesNameEveryLevel(t *testing.T) {
	if firestoreTrail("users", "alice") != nil {
		t.Error("a top-level collection's page draws its own trail; one crumb per segment is already right")
	}
	trail := firestoreTrail("users/alice/orders", "o1", "total")
	want := []console.Crumb{
		{Label: "users", Path: []string{"users"}},
		{Label: "alice", Path: []string{"users", "alice"}},
		{Label: "orders", Path: []string{"users/alice/orders"}},
		{Label: "o1", Path: []string{"users/alice/orders", "o1"}},
		{Label: "total"},
	}
	if !reflect.DeepEqual(trail, want) {
		t.Errorf("the trail is %+v\nwant %+v", trail, want)
	}
	c := offlineFirestore(t)
	if _, err := firestoreCollection(c, "users/alice"); err == nil {
		t.Error("a document path was accepted as a collection")
	}
	if col, err := firestoreCollection(c, "users/alice/orders"); err != nil || col.ID != "orders" {
		t.Errorf("the subcollection path = %v, %v", col, err)
	}
	// Start collection on a document takes an ID, never a path.
	err := firestoreProvider{endpoint: "127.0.0.1:1"}.ActAt(context.Background(), "demo",
		[]string{"users", "alice"}, "startcollection", map[string]string{"collection": "a/b"})
	if err == nil || !strings.Contains(err.Error(), "no slash") {
		t.Errorf("Start collection with a slash = %v", err)
	}
}
