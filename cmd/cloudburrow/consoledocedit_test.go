package main

import (
	"context"
	"encoding/base64"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/protobuf/proto"

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
		// A blob, as base64 (#912), empty and not.
		[]byte{}, []byte{0, 1, 2, 0xfe, 0xff}, []byte("hello"),
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
		// A blob is edited as base64 since #912; JSON cannot hold one.
		[]any{[]byte("x")},
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
	// A key in a namespace is editable since #887: its form names the
	// namespace (TestDatastoreKeyValuesRoundTripThroughEditProperty). A key
	// in an array is not, as JSON has no key.
	if typ, raw, ok := formatDatastoreValue([]any{datastore.NameKey("K", "a", nil)}, false); ok {
		t.Errorf("an array holding a key offers an edit as %s %q, which would change it", typ, raw)
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
		// Base64, standard encoding (#912); white space is ignored.
		{"blob", "AQID", []byte{1, 2, 3}},
		{"blob", "AQ\nID\n", []byte{1, 2, 3}},
		{"blob", "", []byte{}},
		{"blob", "+/8=", []byte{0xfb, 0xff}},
	} {
		got, err := parseDatastoreValue(tc.typ, tc.raw)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Datastore %s %q = %#v, %v; want %#v", tc.typ, tc.raw, got, err, tc.want)
		}
	}
	for _, tc := range [][2]string{
		{"integer", "4.5"}, {"key", "Order"}, {"key", "Order/id=x"}, {"entity", "[1]"}, {"array", "{}"}, {"blob", "x"},
		// The URL-safe alphabet and missing padding are not the standard
		// encoding.
		{"blob", "-_8="}, {"blob", "AQ"},
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
		back, err := datastoreEntityKey("p", tc.ns, tc.kind, datastoreKeySegment(tc.key))
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
	if k, err := datastoreEntityKey("p", "", "Order", "Customer/alice/x/Order/id=7"); err != nil || k.Parent != nil ||
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
			back, err := datastoreEntityKey("p", ns, "Order", addr)
			if err != nil {
				t.Fatalf("%v: %v", k, err)
			}
			if !back.Equal(k) {
				t.Errorf("in namespace %q, %v is addressed as %q, which reads back as %v", ns, k, addr, back)
			}
			if got := datastoreEntityLabel("p", ns, "Order", addr); got != datastoreEntityHeading(k) {
				t.Errorf("%v's page is named %q, want %q", k, got, datastoreEntityHeading(k))
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
	if k, err := datastoreEntityKey("p", "", "Order", other); err != nil || k.Kind != "Order" || k.Name != other {
		t.Errorf("a Customer key on an Order page read back as %v, %v", k, err)
	}
	inNS := datastore.NameKey("Order", "x", nil)
	inNS.Namespace = "tenant-a"
	if k, err := datastoreEntityKey("p", "", "Order", datastoreEntityAddress(inNS)); err != nil || k.Namespace != "" || k.Name == "x" {
		t.Errorf("a tenant-a key on a default-namespace page read back as %v, %v", k, err)
	}

	// A link from before still opens what it did: the child's key path.
	if k, err := datastoreEntityKey("p", "", "Order", "Customer/alice/Order/x"); err != nil || k.Parent == nil ||
		k.Parent.Name != "alice" || k.Name != "x" {
		t.Errorf("the old key-path link read back as %v, %v", k, err)
	}
}

// TestDatastoreEncodedKeyPrecedence (#882).
//
// A path segment can be an old-form link — a name, id=N or a key path — and
// a valid encoded key at once. It is read as an encoded key only when that
// key belongs on the page: its project, if it names one, the page's; no
// database other than the default; the page's namespace and kind; complete;
// and the canonical encoding, byte for byte. Otherwise it is read as the old
// form, which is what it was before encoded addresses existed.
func TestDatastoreEncodedKeyPrecedence(t *testing.T) {
	enc := func(pk *datastorepb.Key) string {
		b, err := proto.Marshal(pk)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
	}
	path := func(els ...*datastorepb.Key_PathElement) []*datastorepb.Key_PathElement { return els }
	named := func(kind, name string) *datastorepb.Key_PathElement {
		return &datastorepb.Key_PathElement{Kind: kind, IdType: &datastorepb.Key_PathElement_Name{Name: name}}
	}
	x := path(named("Customer", "alice"), named("Order", "x"))
	child := datastore.NameKey("Order", "x", datastore.NameKey("Customer", "alice", nil))

	for _, tc := range []struct {
		what, ns, id string
		want         *datastore.Key // nil: read as the old form, a root name
	}{
		{"Key.Encode, which names no project", "", child.Encode(), child},
		{"the page's project named", "", enc(&datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: "p"}, Path: x}), child},
		{"another project's key", "", enc(&datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: "other"}, Path: x}), nil},
		{"a database other than the default", "", enc(&datastorepb.Key{PartitionId: &datastorepb.PartitionId{DatabaseId: "db2"}, Path: x}), nil},
		{"another namespace's key", "", enc(&datastorepb.Key{PartitionId: &datastorepb.PartitionId{NamespaceId: "tenant-a"}, Path: x}), nil},
		{"the default namespace's key on a namespaced page", "tenant-a", child.Encode(), nil},
		{"another kind's key", "", enc(&datastorepb.Key{Path: path(named("Order", "x"), named("Line", "l1"))}), nil},
		{"an incomplete key", "", enc(&datastorepb.Key{Path: path(named("Customer", "alice"), &datastorepb.Key_PathElement{Kind: "Order"})}), nil},
		// Canonical only: the key's bytes followed by an empty partition
		// (field 1) parse as the same key, but are not what Key.Encode
		// writes, which puts field 1 first or leaves it out.
		{"a non-canonical encoding", "", nonCanonical(t, child), nil},
	} {
		got, err := datastoreEntityKey("p", tc.ns, "Order", tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		want := tc.want
		if want == nil {
			want = datastore.NameKey("Order", tc.id, nil)
		} else if tc.ns != "" {
			t.Fatalf("%s: a namespaced want is not set up", tc.what)
		}
		want.Namespace = tc.ns
		if !got.Equal(want) {
			t.Errorf("%s: %q read back as %v, want %v", tc.what, tc.id, got, want)
		}
	}

	// A root entity whose name is the canonical encoding of another entity
	// of its kind is the one old-form link the precedence changes: the
	// segment opens that other entity. Its own row opens it by its own
	// encoded key, so it is still reachable.
	odd := datastore.NameKey("Order", child.Encode(), nil)
	if got, _ := datastoreEntityKey("p", "", "Order", datastoreEntityAddress(odd)); !got.Equal(odd) {
		t.Errorf("a root named like an encoded key, by its own address, read back as %v", got)
	}
}

// nonCanonical is k's encoding with an empty partition appended: a string
// that decodes to k and is not k.Encode().
func nonCanonical(t *testing.T, k *datastore.Key) string {
	b, err := base64.RawURLEncoding.DecodeString(k.Encode())
	if err != nil {
		t.Fatal(err)
	}
	s := base64.RawURLEncoding.EncodeToString(append(b, 0x0a, 0x00))
	if back, err := datastore.DecodeKey(s); err != nil || !back.Equal(k) {
		t.Fatalf("the non-canonical fixture %q decodes to %v, %v; want %v", s, back, err, k)
	}
	return s
}

// TestDatastoreEntityRowsTellARootFromTheChild (#882).
//
// A root entity named Customer/alice/Order/x and the child that key path
// names: the kind's listing gives each row its parent — none for the root,
// Customer/name=alice for the child — as Google's console lists an entity's
// parent in a column of its own, and names each in the Name/ID column,
// name=Customer/alice/Order/x and name=x (#885). Each still opens its own
// page by its encoded key.
func TestDatastoreEntityRowsTellARootFromTheChild(t *testing.T) {
	for _, scope := range []datastoreScope{{}, {ns: "tenant-a", namespaced: true}} {
		root := datastore.NameKey("Order", "Customer/alice/Order/x", nil)
		child := datastore.NameKey("Order", "x", datastore.NameKey("Customer", "alice", nil))
		r := datastoreEntityRow(scope, "Order", root, []string{"who: root"})
		c := datastoreEntityRow(scope, "Order", child, []string{"who: child"})
		if r.Name != "name=Customer/alice/Order/x" || c.Name != "name=x" {
			t.Errorf("the rows are named %q and %q, want name=Customer/alice/Order/x and name=x", r.Name, c.Name)
		}
		if r.Fields["Parent"] != datastoreRootParent || c.Fields["Parent"] != "Customer/name=alice" {
			t.Errorf("in %q the root's Parent is %q and the child's %q; want %q and Customer/name=alice",
				scope.ns, r.Fields["Parent"], c.Fields["Parent"], datastoreRootParent)
		}
		if reflect.DeepEqual(r.Opens, c.Opens) {
			t.Errorf("both rows open %v", r.Opens)
		}
		if r.Fields["Properties"] != "who: root" {
			t.Errorf("the root's Properties cell is %q", r.Fields["Properties"])
		}
		if !reflect.DeepEqual(datastoreEntityColumns, []string{"Parent", "Properties"}) {
			t.Errorf("the entity listing's columns are %v", datastoreEntityColumns)
		}
	}
}

// TestDatastoreNameIDTellsANameFromAnID (#885).
//
// A root entity whose key name is the string "id=7" and the root entity
// whose numeric ID is 7 were both listed as id=7, with the same Parent
// (none). As in Google's console's Name/ID column, a name is rendered
// name=… and an ID id=…, so the rows read name=id=7 and id=7; their pages
// are headed so, and the key path in the Parent column, an entity's Key
// path and a child's heading use the same rendering. An old-form link,
// /datastore/Order/id=7, still opens the numeric one.
func TestDatastoreNameIDTellsANameFromAnID(t *testing.T) {
	named := datastore.NameKey("Order", "id=7", nil)
	numeric := datastore.IDKey("Order", 7, nil)
	for _, scope := range []datastoreScope{{}, {ns: "tenant-a", namespaced: true}} {
		n := datastoreEntityRow(scope, "Order", named, nil)
		i := datastoreEntityRow(scope, "Order", numeric, nil)
		if n.Name != "name=id=7" || i.Name != "id=7" {
			t.Errorf("in %q the rows are named %q and %q, want name=id=7 and id=7", scope.ns, n.Name, i.Name)
		}
		if n.Fields["Parent"] != datastoreRootParent || i.Fields["Parent"] != datastoreRootParent {
			t.Errorf("the rows' parents are %q and %q", n.Fields["Parent"], i.Fields["Parent"])
		}
		if reflect.DeepEqual(n.Opens, i.Opens) {
			t.Errorf("both rows open %v", n.Opens)
		}
	}
	if datastoreEntityHeading(named) != "name=id=7" || datastoreEntityHeading(numeric) != "id=7" {
		t.Errorf("the pages are headed %q and %q", datastoreEntityHeading(named), datastoreEntityHeading(numeric))
	}

	alice := datastore.NameKey("Customer", "alice", nil)
	order := datastore.IDKey("Order", 7, alice)
	line := datastore.NameKey("Line", "l/1", order)
	for _, tc := range []struct {
		key        *datastore.Key
		path, head string
	}{
		{alice, "Customer/name=alice", "name=alice"},
		{order, "Customer/name=alice/Order/id=7", "Customer/name=alice/Order/id=7"},
		{line, "Customer/name=alice/Order/id=7/Line/name=l%2F1", "Customer/name=alice/Order/id=7/Line/name=l%2F1"},
		{datastore.NameKey("Doc", "50%", nil), "Doc/name=50%25", "name=50%"},
	} {
		if got := datastoreKeyPathLabel(tc.key); got != tc.path {
			t.Errorf("%v's key path reads %q, want %q", tc.key, got, tc.path)
		}
		if got := datastoreEntityHeading(tc.key); got != tc.head {
			t.Errorf("%v is headed %q, want %q", tc.key, got, tc.head)
		}
	}
	if got := datastoreEntityRow(datastoreScope{}, "Line", line, nil); got.Name != "name=l/1" ||
		got.Fields["Parent"] != "Customer/name=alice/Order/id=7" {
		t.Errorf("the child's row is %q with parent %q", got.Name, got.Fields["Parent"])
	}

	// Create entity's Key identifier reads what the Name/ID column shows,
	// so the entity named id=7 can be made; a bare name is still a name.
	for _, tc := range []struct {
		in   string
		want *datastore.Key
	}{
		{"name=id=7", named},
		{"id=7", numeric},
		{"alice", datastore.NameKey("Order", "alice", nil)},
		{"name=alice", datastore.NameKey("Order", "alice", nil)},
		{"name=name=x", datastore.NameKey("Order", "name=x", nil)},
	} {
		if k, err := datastoreKeyIdentifier("Order", tc.in); err != nil || !k.Equal(tc.want) {
			t.Errorf("Key identifier %q makes %v, %v; want %v", tc.in, k, err, tc.want)
		}
	}
	if _, err := datastoreKeyIdentifier("Order", "name="); err == nil {
		t.Error("Key identifier name= with no name was accepted")
	}

	// The old-form link id=7 opens the numeric ID, as it always did; the
	// entity named id=7 is opened by its own encoded key.
	if k, err := datastoreEntityKey("p", "", "Order", "id=7"); err != nil || !k.Equal(numeric) {
		t.Errorf("the old link id=7 reads back as %v, %v; want the numeric ID 7", k, err)
	}
	if got := datastoreEntityLabel("p", "", "Order", "id=7"); got != "id=7" {
		t.Errorf("the old link id=7 is headed %q", got)
	}
	if k, err := datastoreEntityKey("p", "", "Order", datastoreEntityAddress(named)); err != nil || !k.Equal(named) {
		t.Errorf("the entity named id=7, by its address, reads back as %v, %v", k, err)
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

// TestDatastoreKeyValuesRoundTripThroughEditProperty (#887).
//
// A key-valued property was shown and prefilled in the older Kind/name or
// Kind/id=123 form, which cannot tell a name from an ID: a key to Order
// named "id=7" read Order/id=7, as the key to the numeric ID 7 does, and
// saving Edit property unchanged wrote the numeric one. Key values now use
// the rendering #885 gave key paths — name=… and id=… — with a slash or
// percent sign in a kind or name escaped, and a namespace other than the
// default written first as __namespace__/{namespace}, so every key's form
// reads back to that key: a name that looks like an ID, or holds a slash, a
// percent sign or an equals sign, an ancestor, and a namespace.
func TestDatastoreKeyValuesRoundTripThroughEditProperty(t *testing.T) {
	inNS := func(k *datastore.Key, ns string) *datastore.Key {
		for e := k; e != nil; e = e.Parent {
			e.Namespace = ns
		}
		return k
	}
	alice := func() *datastore.Key { return datastore.NameKey("Customer", "alice", nil) }
	for _, tc := range []struct {
		key  *datastore.Key
		want string
	}{
		{datastore.NameKey("Order", "id=7", nil), "Order/name=id=7"},
		{datastore.IDKey("Order", 7, nil), "Order/id=7"},
		{datastore.NameKey("Order", "7", nil), "Order/name=7"},
		{datastore.NameKey("Order", "name=x", nil), "Order/name=name=x"},
		{datastore.NameKey("Order", "a/b%c=d", nil), "Order/name=a%2Fb%25c=d"},
		{datastore.NameKey("Order", "%2F", nil), "Order/name=%252F"},
		{datastore.NameKey("Order", " padded ", nil), "Order/name= padded "},
		{datastore.NameKey("a/b%", "x", nil), "a%2Fb%25/name=x"},
		{datastore.NameKey("Order", "id=7", alice()), "Customer/name=alice/Order/name=id=7"},
		{datastore.IDKey("Order", 7, alice()), "Customer/name=alice/Order/id=7"},
		{inNS(datastore.NameKey("Order", "id=7", nil), "tenant-a"), "__namespace__/tenant-a/Order/name=id=7"},
		{inNS(datastore.IDKey("Line", 3, datastore.NameKey("Order", "x/y", alice())), "tenant-a"),
			"__namespace__/tenant-a/Customer/name=alice/Order/name=x%2Fy/Line/id=3"},
		// A key to a namespace's metadata entity, whose kind is the
		// reserved __namespace__: its first element has an =, which no
		// namespace name has.
		{datastore.NameKey("__namespace__", "tenant-a", nil), "__namespace__/name=tenant-a"},
		{inNS(datastore.NameKey("__namespace__", "x", nil), "ns"), "__namespace__/ns/__namespace__/name=x"},
	} {
		typ, raw, ok := formatDatastoreValue(tc.key, false)
		if !ok || typ != "key" || raw != tc.want {
			t.Errorf("%v is prefilled as %s %q (editable %v), want key %q", tc.key, typ, raw, ok, tc.want)
			continue
		}
		if shown := renderDatastoreValue(tc.key, false); shown != tc.want {
			t.Errorf("%v is shown as %q, want %q", tc.key, shown, tc.want)
		}
		back, err := parseDatastoreValue(typ, raw)
		if err != nil {
			t.Errorf("%v: the form's own %q is refused: %v", tc.key, raw, err)
			continue
		}
		if k, _ := back.(*datastore.Key); k == nil || !k.Equal(tc.key) || !reflect.DeepEqual(k, tc.key) {
			t.Errorf("%v came back from %q as %#v", tc.key, raw, back)
		}
	}

	// What a user types: a bare name is a name, as before, and id=… an ID.
	for _, tc := range []struct {
		in   string
		want *datastore.Key
	}{
		{"Order/x", datastore.NameKey("Order", "x", nil)},
		{"Customer/alice/Order/id=7", datastore.IDKey("Order", 7, alice())},
		{"/Order/name=id=7/", datastore.NameKey("Order", "id=7", nil)},
	} {
		got, err := parseDatastoreValue("key", tc.in)
		if k, _ := got.(*datastore.Key); err != nil || k == nil || !reflect.DeepEqual(k, tc.want) {
			t.Errorf("key %q = %#v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"Order", "Order/name=", "Order/id=x", "/name=x", "__namespace__/Bad!/Order/x", "__namespace__/ns",
		"Order/50%", "Order/id=7/Line",
	} {
		if v, err := parseDatastoreValue("key", in); err == nil {
			t.Errorf("key %q was accepted as %#v", in, v)
		}
	}
}
