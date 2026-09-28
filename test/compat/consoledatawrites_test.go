//go:build compat

package compat

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// consoleWritePage is the part of a console detail page these tests read.
type consoleWritePage struct {
	Unavailable string
	Summary     []struct{ Label, Value string }
	Actions     []struct {
		ID          string
		Destructive bool
		Leaves      bool
		Confirm     string
		Fields      []struct{ Name, Type string }
	}
	Sections []struct {
		ID      string
		Note    string
		Listing struct {
			Items []struct {
				Name   string
				Fields map[string]string
				Opens  []string
				Absent bool
			}
			More   bool
			Cursor string
			Note   string
		}
	}
	Trail []struct {
		Label string
		Path  []string
	}
	Title string
}

func consoleWriteDetail(t *testing.T, addr, service, project string, path ...string) consoleWritePage {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	var page consoleWritePage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if page.Unavailable != "" {
		t.Fatalf("console detail %v is unavailable: %s", path, page.Unavailable)
	}
	return page
}

func hasKey(m map[string][]string, k string) bool {
	_, ok := m[k]
	return ok
}

func (p consoleWritePage) actionIDs() []string {
	var ids []string
	for _, a := range p.Actions {
		ids = append(ids, a.ID)
	}
	return ids
}

func (p consoleWritePage) summary(label string) string {
	for _, s := range p.Summary {
		if s.Label == label {
			return s.Value
		}
	}
	return ""
}

// rows is the named section's rows by name, with the path each opens.
func (p consoleWritePage) rows(t *testing.T, section string) map[string][]string {
	t.Helper()
	for _, s := range p.Sections {
		if s.ID == section {
			out := map[string][]string{}
			for _, it := range s.Listing.Items {
				out[it.Name] = it.Opens
			}
			return out
		}
	}
	t.Fatalf("the page has no %q section: %+v", section, p.Sections)
	return nil
}

// TestConsoleBigQueryDatasetTableAndRowWrites.
//
// The BigQuery screen's writes (#854), each read back through the official Go
// client. Create dataset makes a dataset with its location, description and
// labels; a second one of the same ID is refused at once as already existing
// (the emulator answers 500, which the client would retry until its
// deadline), and a hyphenated ID, which the emulator accepts and BigQuery does
// not, is refused on the form. Create table on the dataset's page makes a
// table with the schema the editor sent, names, types and modes; a column
// named twice is refused. Insert rows streams rows the client then reads, with
// their types; a batch holding one bad row writes none of them, where the
// emulator alone would have written the good one, and a row missing a
// REQUIRED value is refused. Delete table and Delete dataset, the second from
// the list with a table still in it, leave the client NOT_FOUND.
func TestConsoleBigQueryDatasetTableAndRowWrites(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	q := "?project=" + url.QueryEscape(project)
	id := "console_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(id)
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })

	body, _ := json.Marshal(map[string]string{"datasetId": id, "location": "EU",
		"description": "made by the console", "labels": `{"team":"data"}`})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigquery"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console Create dataset = %d: %s", code, out)
	}
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("the console's dataset is not there: %v", err)
	}
	if md.Location != "EU" || md.Description != "made by the console" || md.Labels["team"] != "data" {
		t.Errorf("the dataset reads back as location %q, description %q, labels %v", md.Location, md.Description, md.Labels)
	}
	start := time.Now()
	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigquery"+q, string(body))
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "already exists") {
		t.Errorf("a second dataset %s = %d %s, want already exists", id, code, out)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the duplicate was refused after %s: the emulator's 500 was retried", took)
	}
	code, out = consoleDo(t, addr, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"bad-name"}`)
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "not a dataset ID") {
		t.Errorf("dataset ID bad-name = %d %s, want the form's refusal", code, out)
	}
	if _, err := c.Dataset("bad-name").Metadata(ctx); err == nil {
		t.Error("the refused dataset bad-name was created")
	}

	page := consoleWriteDetail(t, addr, "bigquery", project, id)
	if got := strings.Join(page.actionIDs(), ","); got != "createtable,deletedataset" {
		t.Errorf("the dataset's page offers %s", got)
	}
	schema := `[{"name":"id","type":"INTEGER","mode":"REQUIRED"},{"name":"region","type":"STRING","mode":"NULLABLE"},` +
		`{"name":"tags","type":"STRING","mode":"REPEATED"},{"name":"at","type":"TIMESTAMP","mode":"NULLABLE"},` +
		`{"name":"price","type":"NUMERIC","mode":"NULLABLE"}]`
	if code, out := consoleAct(t, addr, "bigquery", project, []string{id}, "createtable",
		map[string]string{"tableId": "orders", "description": "console", "schema": schema}); code != http.StatusOK {
		t.Fatalf("console Create table = %d: %s", code, out)
	}
	tbl := ds.Table("orders")
	tm, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatalf("the console's table is not there: %v", err)
	}
	var cols []string
	for _, f := range tm.Schema {
		mode := "NULLABLE"
		if f.Required {
			mode = "REQUIRED"
		} else if f.Repeated {
			mode = "REPEATED"
		}
		cols = append(cols, f.Name+":"+string(f.Type)+":"+mode)
	}
	if want := "id:INTEGER:REQUIRED,region:STRING:NULLABLE,tags:STRING:REPEATED,at:TIMESTAMP:NULLABLE,price:NUMERIC:NULLABLE"; strings.Join(cols, ",") != want {
		t.Errorf("the table's schema reads back as %s, want %s", strings.Join(cols, ","), want)
	}
	code, out = consoleAct(t, addr, "bigquery", project, []string{id}, "createtable",
		map[string]string{"tableId": "twice", "schema": `[{"name":"x","type":"STRING"},{"name":"X","type":"STRING"}]`})
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "named twice") {
		t.Errorf("a column named twice = %d %s", code, out)
	}

	page = consoleWriteDetail(t, addr, "bigquery", project, id, "orders")
	if got := strings.Join(page.actionIDs(), ","); got != "insertrows,deletetable" {
		t.Errorf("the table's page offers %s", got)
	}
	rows := `{"id": 1, "region": "eu", "tags": ["a", "b"], "at": "2026-09-27T15:04:05Z", "price": "1.25"}` + "\n" +
		`{"id": "2", "region": null}`
	if code, out := consoleAct(t, addr, "bigquery", project, []string{id, "orders"}, "insertrows",
		map[string]string{"rows": rows}); code != http.StatusOK {
		t.Fatalf("console Insert rows = %d: %s", code, out)
	}
	read := func() []string {
		t.Helper()
		it := tbl.Read(ctx)
		var got []string
		for {
			var r []bigquery.Value
			err := it.Next(&r)
			if errors.Is(err, iterator.Done) {
				return got
			}
			if err != nil {
				t.Fatalf("read the table: %v", err)
			}
			b, _ := json.Marshal(r)
			got = append(got, string(b))
		}
	}
	got := read()
	if len(got) != 2 || !strings.HasPrefix(got[0], `[1,"eu",["a","b"],"2026-09-27T15:04:05Z"`) || !strings.HasPrefix(got[1], `[2,null,`) {
		t.Errorf("the table reads back %v, want the two rows with their types", got)
	}

	for rows, why := range map[string]string{
		`{"id": 3}` + "\n" + `{"id": 4, "tags": ["ok", 5]}`: "row 2: tags[1]",
		`{"region": "no id"}`:                               "id is REQUIRED",
		`{"id": 5, "nosuch": 1}`:                            "no such field: nosuch",
	} {
		code, out := consoleAct(t, addr, "bigquery", project, []string{id, "orders"}, "insertrows",
			map[string]string{"rows": rows})
		if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), why) {
			t.Errorf("Insert rows %q = %d %s, want a refusal naming %q", rows, code, out, why)
		}
	}
	if after := read(); len(after) != 2 {
		t.Errorf("after refused inserts the table holds %d rows, want 2: a refusal wrote %v", len(after), after)
	}

	if code, out := consoleAct(t, addr, "bigquery", project, []string{id, "orders"}, "deletetable", nil); code != http.StatusOK {
		t.Fatalf("console Delete table = %d: %s", code, out)
	}
	if _, err := tbl.Metadata(ctx); !isNotFound(err) {
		t.Errorf("after Delete table the client reads %v, want NOT_FOUND", err)
	}

	// Delete dataset from the list, with a table still in it.
	if code, out := consoleAct(t, addr, "bigquery", project, []string{id}, "createtable",
		map[string]string{"tableId": "left", "schema": `[{"name":"x","type":"STRING"}]`}); code != http.StatusOK {
		t.Fatalf("console Create table left = %d: %s", code, out)
	}
	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console Delete dataset = %d: %s", code, out)
	}
	if _, err := ds.Metadata(ctx); !isNotFound(err) {
		t.Errorf("after Delete dataset the client reads %v, want NOT_FOUND", err)
	}
}

// TestConsoleFirestoreSubcollections.
//
// A document's page lists its subcollections (#854), which the official
// client's DocumentRef.Collections reads the same way, and each opens to its
// own page by its path. Start collection on a document makes a subcollection
// with its first document, which the client reads at its path; a
// subcollection's page lists its documents and names its parent, Add document
// works in it, and a document in it has its fields, Add field, its own
// subcollections, a breadcrumb through every level, and Delete document.
//
// covers: google.firestore.v1.Firestore/ListCollectionIds
func TestConsoleFirestoreSubcollections(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()

	alice := c.Doc("users/alice")
	if _, err := alice.Create(ctx, map[string]any{"name": "Alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Doc("users/alice/pets/rex").Create(ctx, map[string]any{"kind": "dog"}); err != nil {
		t.Fatal(err)
	}

	page := consoleWriteDetail(t, addr, "firestore", project, "users", "alice")
	if got := page.rows(t, "collections"); len(got) != 1 || strings.Join(got["pets"], "|") != "users/alice/pets" {
		t.Errorf("alice's Collections tab is %v, want pets opening users/alice/pets", got)
	}
	if !strings.Contains(strings.Join(page.actionIDs(), ","), "startcollection") {
		t.Errorf("a document's page offers %v, not Start collection", page.actionIDs())
	}

	if code, out := consoleAct(t, addr, "firestore", project, []string{"users", "alice"}, "startcollection",
		map[string]string{"collection": "orders", "documentId": "o1", "field": "total", "type": "number", "value": "12"}); code != http.StatusOK {
		t.Fatalf("console Start collection on a document = %d: %s", code, out)
	}
	snap, err := c.Doc("users/alice/orders/o1").Get(ctx)
	if err != nil || snap.Data()["total"] != int64(12) {
		t.Fatalf("users/alice/orders/o1 reads back %v, %v; want total 12", snap, err)
	}
	var ids []string
	it := alice.Collections(ctx)
	for {
		col, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Collections: %v", err)
		}
		ids = append(ids, col.ID)
	}
	if strings.Join(ids, ",") != "orders,pets" {
		t.Errorf("the client lists alice's subcollections as %v, want orders and pets", ids)
	}
	if got := consoleWriteDetail(t, addr, "firestore", project, "users", "alice").rows(t, "collections"); len(got) != 2 {
		t.Errorf("alice's Collections tab is %v after Start collection, want orders and pets", got)
	}

	sub := consoleWriteDetail(t, addr, "firestore", project, "users/alice/orders")
	if docs := sub.rows(t, "documents"); len(docs) != 1 || !hasKey(docs, "o1") {
		t.Errorf("the subcollection's page lists %v, want o1", docs)
	}
	if sub.summary("Parent document") != "users/alice" {
		t.Errorf("the subcollection's page names its parent %q", sub.summary("Parent document"))
	}
	if code, out := consoleAct(t, addr, "firestore", project, []string{"users/alice/orders"}, "adddocument",
		map[string]string{"documentId": "o2"}); code != http.StatusOK {
		t.Fatalf("console Add document in a subcollection = %d: %s", code, out)
	}
	if _, err := c.Doc("users/alice/orders/o2").Get(ctx); err != nil {
		t.Errorf("users/alice/orders/o2: %v", err)
	}
	if code, out := consoleAct(t, addr, "firestore", project, []string{"users/alice/orders", "o1"}, "addfield",
		map[string]string{"field": "paid", "type": "boolean", "value": "true"}); code != http.StatusOK {
		t.Fatalf("console Add field in a subcollection's document = %d: %s", code, out)
	}
	if snap, err := c.Doc("users/alice/orders/o1").Get(ctx); err != nil || snap.Data()["paid"] != true {
		t.Errorf("o1 reads back %v, %v; want paid true", snap, err)
	}

	doc := consoleWriteDetail(t, addr, "firestore", project, "users/alice/orders", "o1")
	var crumbs []string
	for _, c := range doc.Trail {
		crumbs = append(crumbs, c.Label+"="+strings.Join(c.Path, "|"))
	}
	if want := "users=users,alice=users|alice,orders=users/alice/orders,o1="; strings.Join(crumbs, ",") != want {
		t.Errorf("the nested document's trail is %s, want %s", strings.Join(crumbs, ","), want)
	}
	if code, out := consoleAct(t, addr, "firestore", project, []string{"users/alice/orders", "o2"}, "deletedocument", nil); code != http.StatusOK {
		t.Fatalf("console Delete document in a subcollection = %d: %s", code, out)
	}
	if _, err := c.Doc("users/alice/orders/o2").Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete document the client reads %v, want NOT_FOUND", err)
	}
	code, out := consoleAct(t, addr, "firestore", project, []string{"users", "alice"}, "startcollection",
		map[string]string{"collection": "a/b"})
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "no slash") {
		t.Errorf("Start collection named with a slash = %d %s", code, out)
	}
}

// TestConsoleDatastoreNamespacesAndChildren.
//
// Datastore namespaces and ancestors on the console (#854), each read back
// through the official client. Create entity with a namespace writes the
// entity in it; the Datastore screen lists every namespace's kinds with the
// namespace named, and a row in another namespace opens its kind there; the
// Namespaces page lists it, and its page its kinds. On an entity's page,
// Create child entity writes an entity whose parent is that one, in the same
// namespace; the parent's Children tab lists it by its key path and opens it;
// the child's page shows its key path and parent, Add property changes it, and
// Delete entity removes it. A child of an entity that does not exist is
// refused.
func TestConsoleDatastoreNamespacesAndChildren(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()
	const ns = "tenant-a"

	body, _ := json.Marshal(map[string]string{"namespace": ns, "kind": "Widget", "key": "w1",
		"field": "n", "type": "integer", "value": "1"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console Create entity in a namespace = %d: %s", code, out)
	}
	w1 := datastore.NameKey("Widget", "w1", nil)
	w1.Namespace = ns
	var props datastore.PropertyList
	if err := c.Get(ctx, w1, &props); err != nil || len(props) != 1 || props[0].Value != int64(1) {
		t.Fatalf("the namespaced entity reads back %v, %v", props, err)
	}
	var none datastore.PropertyList
	if err := c.Get(ctx, datastore.NameKey("Widget", "w1", nil), &none); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("the default namespace has Widget w1 too: %v", err)
	}
	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore?project="+project,
		`{"namespace":"__reserved__","kind":"K"}`)
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "reserved") {
		t.Errorf("a reserved namespace = %d %s", code, out)
	}

	var list struct {
		Items []struct {
			Name   string
			Fields map[string]string
			Opens  []string
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/resources/datastore?project="+project, "", &list)
	found := false
	for _, it := range list.Items {
		if it.Name == "Widget" && it.Fields["Namespace"] == ns {
			found = strings.Join(it.Opens, "|") == "__namespace__|"+ns+"|Widget"
		}
	}
	if !found {
		t.Errorf("the Datastore screen does not list Widget in %s opening its kind there: %+v", ns, list.Items)
	}
	if got := consoleWriteDetail(t, addr, "datastore", project, "__namespace__").rows(t, "namespaces"); got == nil || len(got) != 1 {
		t.Errorf("the Namespaces page lists %v, want %s", got, ns)
	}
	if got := consoleWriteDetail(t, addr, "datastore", project, "__namespace__", ns).rows(t, "kinds"); len(got) != 1 {
		t.Errorf("namespace %s's page lists %v, want Widget", ns, got)
	}
	nsPath := []string{"__namespace__", ns}
	at := func(rest ...string) []string { return append(append([]string{}, nsPath...), rest...) }
	if got := consoleWriteDetail(t, addr, "datastore", project, at("Widget")...).rows(t, "entities"); len(got) != 1 {
		t.Errorf("Widget in %s lists %v, want w1", ns, got)
	}

	// A child, in the namespace.
	if code, out := consoleAct(t, addr, "datastore", project, at("Widget", "w1"), "createchild",
		map[string]string{"kind": "Part", "key": "p1", "field": "size", "type": "string", "value": "large"}); code != http.StatusOK {
		t.Fatalf("console Create child entity = %d: %s", code, out)
	}
	p1 := datastore.NameKey("Part", "p1", w1)
	p1.Namespace = ns
	var part datastore.PropertyList
	if err := c.Get(ctx, p1, &part); err != nil || len(part) != 1 || part[0].Value != "large" {
		t.Fatalf("the child reads back %v, %v", part, err)
	}
	children := consoleWriteDetail(t, addr, "datastore", project, at("Widget", "w1")...).rows(t, "children")
	const seg = "Widget/w1/Part/p1"
	// Listed by its Name/ID (#885), and opened by its encoded key (#875).
	if strings.Join(children["name=p1"], "|") != strings.Join(at("Part", p1.Encode()), "|") {
		t.Errorf("w1's Children tab is %v, want name=p1 opening %v", children, at("Part", p1.Encode()))
	}
	// The key-path form the page was addressed by before still opens it.
	child := consoleWriteDetail(t, addr, "datastore", project, at("Part", seg)...)
	if child.summary("Key path") != "Widget/name=w1/Part/name=p1" || child.summary("Parent") != "Widget/name=w1" ||
		child.summary("Namespace") != ns {
		t.Errorf("the child's page says key path %q, parent %q, namespace %q", child.summary("Key path"),
			child.summary("Parent"), child.summary("Namespace"))
	}
	if code, out := consoleAct(t, addr, "datastore", project, at("Part", seg), "addproperty",
		map[string]string{"field": "count", "type": "integer", "value": "3"}); code != http.StatusOK {
		t.Fatalf("console Add property on a child = %d: %s", code, out)
	}
	part = nil
	if err := c.Get(ctx, p1, &part); err != nil || len(part) != 2 {
		t.Errorf("after Add property the child reads back %v, %v", part, err)
	}
	if code, out := consoleAct(t, addr, "datastore", project, at("Part", seg), "deleteentity", nil); code != http.StatusOK {
		t.Fatalf("console Delete entity on a child = %d: %s", code, out)
	}
	if err := c.Get(ctx, p1, &part); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("after Delete entity the child reads %v, want no such entity", err)
	}

	// In the default namespace, a child of an SDK-written root.
	root := datastore.NameKey("Customer", "bob", nil)
	if _, err := c.Put(ctx, root, &datastore.PropertyList{{Name: "n", Value: "Bob"}}); err != nil {
		t.Fatal(err)
	}
	if code, out := consoleAct(t, addr, "datastore", project, []string{"Customer", "bob"}, "createchild",
		map[string]string{"kind": "Order"}); code != http.StatusOK {
		t.Fatalf("console Create child entity with an allocated ID = %d: %s", code, out)
	}
	keys, err := c.GetAll(ctx, datastore.NewQuery("Order").Ancestor(root).KeysOnly(), nil)
	if err != nil || len(keys) != 1 || keys[0].ID == 0 {
		t.Fatalf("bob's Order children are %v, %v; want one with an allocated ID", keys, err)
	}
	kindRows := consoleWriteDetail(t, addr, "datastore", project, "Order").rows(t, "entities")
	orderSeg := "Customer/bob/Order/id=" + strconv.FormatInt(keys[0].ID, 10)
	if got := kindRows["id="+strconv.FormatInt(keys[0].ID, 10)]; strings.Join(got, "|") != "Order|"+keys[0].Encode() {
		t.Errorf("the Order kind lists %v, want the child by its Name/ID opening its encoded key", kindRows)
	}
	if got := consoleWriteDetail(t, addr, "datastore", project, "Order", orderSeg); got.summary("Parent") != "Customer/name=bob" {
		t.Errorf("the child's page names parent %q", got.summary("Parent"))
	}
	code, out = consoleAct(t, addr, "datastore", project, []string{"Customer", "nobody"}, "createchild",
		map[string]string{"kind": "Order"})
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "no such entity") {
		t.Errorf("a child of a missing entity = %d %s, want no such entity", code, out)
	}
	t.Cleanup(func() {
		_ = c.DeleteMulti(ctx, append(keys, root))
		_ = c.Delete(ctx, w1)
	})
}

// TestConsoleFirestoreListsMissingDocumentsWithSubcollections.
//
// A document that does not exist but has subcollections (#875) is listed on
// its collection's page, as the official client's CollectionRef.DocumentRefs
// lists it — ListDocuments with show_missing — marked absent and described
// as having no fields and subcollections, so its subcollections stay
// reachable: its page is its Collections tab, whose rows open them. Add
// field on it creates it, which the client reads back; Delete document on
// one says there is nothing to delete. The page after a full first page
// continues with ListDocuments' page token and still lists a missing
// document.
//
// covers: google.firestore.v1.Firestore/ListDocuments
func TestConsoleFirestoreListsMissingDocumentsWithSubcollections(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()

	// users/ghost does not exist; users/ghost/orders/o1 does.
	for path, data := range map[string]map[string]any{
		"users/alice":           {"name": "Alice"},
		"users/ghost/orders/o1": {"total": 3},
	} {
		if _, err := c.Doc(path).Create(ctx, data); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	if _, err := c.Doc("users/ghost").Get(ctx); status.Code(err) != codes.NotFound {
		t.Fatalf("users/ghost reads %v, want NOT_FOUND: the fixture needs it missing", err)
	}
	var refs []string
	it := c.Collection("users").DocumentRefs(ctx)
	for {
		ref, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("DocumentRefs: %v", err)
		}
		refs = append(refs, ref.ID)
	}
	if strings.Join(refs, ",") != "alice,ghost" {
		t.Fatalf("the client's DocumentRefs lists %v, want alice and the missing ghost", refs)
	}

	page := consoleWriteDetail(t, addr, "firestore", project, "users")
	var listed []string
	for _, sec := range page.Sections {
		for _, it := range sec.Listing.Items {
			listed = append(listed, it.Name)
			switch it.Name {
			case "ghost":
				if !it.Absent || it.Fields["Fields"] != "no fields — has subcollections" {
					t.Errorf("the missing document's row is %+v, want it absent with no fields and subcollections", it)
				}
			case "alice":
				if it.Absent || !strings.Contains(it.Fields["Fields"], "Alice") {
					t.Errorf("alice's row is %+v, want her fields and not absent", it)
				}
			}
		}
	}
	if strings.Join(listed, ",") != "alice,ghost" {
		t.Errorf("the users page lists %v, want alice and ghost, as DocumentRefs does", listed)
	}

	ghost := consoleWriteDetail(t, addr, "firestore", project, "users", "ghost")
	if got := ghost.rows(t, "collections"); strings.Join(got["orders"], "|") != "users/ghost/orders" {
		t.Errorf("the missing document's Collections tab is %v, want orders opening users/ghost/orders", got)
	}
	if !strings.HasPrefix(ghost.summary("Exists"), "No") {
		t.Errorf("the missing document's page says Exists %q", ghost.summary("Exists"))
	}
	if docs := consoleWriteDetail(t, addr, "firestore", project, "users/ghost/orders").rows(t, "documents"); !hasKey(docs, "o1") {
		t.Errorf("the missing document's subcollection lists %v, want o1", docs)
	}
	code, out := consoleAct(t, addr, "firestore", project, []string{"users", "ghost"}, "deletedocument", nil)
	if code == http.StatusOK || !strings.Contains(consoleError(t, out), "does not exist") {
		t.Errorf("Delete document on a missing document = %d %s", code, out)
	}
	if code, out := consoleAct(t, addr, "firestore", project, []string{"users", "ghost"}, "addfield",
		map[string]string{"field": "name", "type": "string", "value": "Ghost"}); code != http.StatusOK {
		t.Fatalf("Add field on a missing document = %d: %s", code, out)
	}
	if snap, err := c.Doc("users/ghost").Get(ctx); err != nil || snap.Data()["name"] != "Ghost" {
		t.Errorf("after Add field users/ghost reads %v, %v; want name Ghost", snap, err)
	}

	// A second page, through the page token, lists a missing document too.
	// 200 is the page size; zz-ghost sorts after every one of them.
	b := c.BulkWriter(ctx)
	for i := 0; i < 200; i++ {
		if _, err := b.Create(c.Doc(fmt.Sprintf("many/d%03d", i)), map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Create(c.Doc("many/zz-ghost/sub/s1"), map[string]any{"i": -1}); err != nil {
		t.Fatal(err)
	}
	b.End()
	first := consoleWriteDetail(t, addr, "firestore", project, "many")
	var sec = first.Sections[0].Listing
	if len(sec.Items) != 200 || !sec.More || sec.Cursor == "" {
		t.Fatalf("the first page has %d documents, more %v, cursor %q; want 200 and a cursor", len(sec.Items), sec.More, sec.Cursor)
	}
	q := url.Values{"project": {project}, "name": {"many"}, "cursor": {sec.Cursor}}
	var next struct {
		Items []struct {
			Name   string
			Absent bool
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/page/firestore?"+q.Encode(), "", &next)
	if len(next.Items) != 1 || next.Items[0].Name != "zz-ghost" || !next.Items[0].Absent {
		t.Errorf("the second page is %+v, want only the missing zz-ghost", next.Items)
	}
}

// TestConsoleDatastoreOpensARootEntityNamedLikeAKeyPath.
//
// A root entity whose name is shaped like a child's key path — Order
// "Customer/alice/Order/x" — beside the child that path names (#875). Each
// row opens its entity's page by the key encoded as the official client's
// Key.Encode encodes it (Google's console's entity URLs use the same), each
// page shows its own entity, and Add property through each changes that
// entity and not the other, read back through the client. A link in the
// key-path form still opens the child.
func TestConsoleDatastoreOpensARootEntityNamedLikeAKeyPath(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	alice := datastore.NameKey("Customer", "alice", nil)
	root := datastore.NameKey("Order", "Customer/alice/Order/x", nil)
	child := datastore.NameKey("Order", "x", alice)
	if _, err := c.PutMulti(ctx, []*datastore.Key{alice, root, child}, []datastore.PropertyList{
		{{Name: "who", Value: "alice"}}, {{Name: "who", Value: "root"}}, {{Name: "who", Value: "child"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteMulti(ctx, []*datastore.Key{alice, root, child}) })

	kind := consoleWriteDetail(t, addr, "datastore", project, "Order")
	opens := map[string]bool{}
	for _, it := range kind.Sections[0].Listing.Items {
		opens[strings.Join(it.Opens, "|")] = true
	}
	for _, k := range []*datastore.Key{root, child} {
		if !opens["Order|"+k.Encode()] {
			t.Errorf("the Order kind has no row opening %v by its encoded key: %+v", k, kind.Sections[0].Listing.Items)
		}
	}

	for _, tc := range []struct {
		key    *datastore.Key
		who    string
		path   string
		parent string
		title  string
	}{
		{root, "root", "Order/name=Customer%2Falice%2FOrder%2Fx", "", "name=Customer/alice/Order/x"},
		{child, "child", "Customer/name=alice/Order/name=x", "Customer/name=alice", "Customer/name=alice/Order/name=x"},
	} {
		page := consoleWriteDetail(t, addr, "datastore", project, "Order", tc.key.Encode())
		if page.summary("Key path") != tc.path || page.summary("Parent") != tc.parent {
			t.Errorf("%s's page says key path %q, parent %q; want %q, %q", tc.who, page.summary("Key path"),
				page.summary("Parent"), tc.path, tc.parent)
		}
		if page.Title != tc.title {
			t.Errorf("%s's page is headed %q, want %q", tc.who, page.Title, tc.title)
		}
		if props := page.rows(t, "properties"); !hasKey(props, "who") {
			t.Errorf("%s's page lists properties %v", tc.who, props)
		}
		if code, out := consoleAct(t, addr, "datastore", project, []string{"Order", tc.key.Encode()}, "addproperty",
			map[string]string{"field": "seen", "type": "string", "value": tc.who}); code != http.StatusOK {
			t.Fatalf("Add property on %s = %d: %s", tc.who, code, out)
		}
	}
	for _, tc := range []struct {
		key *datastore.Key
		who string
	}{{root, "root"}, {child, "child"}} {
		var props datastore.PropertyList
		if err := c.Get(ctx, tc.key, &props); err != nil {
			t.Fatal(err)
		}
		got := map[string]any{}
		for _, p := range props {
			got[p.Name] = p.Value
		}
		if got["who"] != tc.who || got["seen"] != tc.who {
			t.Errorf("%s reads back %v; Add property on its page wrote another entity", tc.who, got)
		}
	}

	old := consoleWriteDetail(t, addr, "datastore", project, "Order", "Customer/alice/Order/x")
	if old.summary("Parent") != "Customer/name=alice" {
		t.Errorf("the key-path link opens a page with parent %q, want the child's", old.summary("Parent"))
	}
}

// consoleRows is the part of a console listing the #882 tests read.
type consoleRows struct {
	Columns    []string
	NameColumn string
	Note       string
	Items      []consoleRow
}

// consoleRow is one row of a listing, as consoleWritePage's sections hold it.
type consoleRow = struct {
	Name   string
	Fields map[string]string
	Opens  []string
	Absent bool
}

// TestConsoleDatastoreRowsNameTheirParent (#882).
//
// A root entity named Customer/alice/Order/x and the child that key path
// names, written with the official client, are listed on kind Order's page
// and in the query builder's results; each row's Parent cell tells them
// apart — none for the root, Customer/name=alice for the child — as does its
// Name/ID cell (#885), and each opens its own entity by its encoded key. An encoded key that names
// the page's project opens the child too; one naming another project is not
// taken for this project's key and is read as a name, which no entity has.
func TestConsoleDatastoreRowsNameTheirParent(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	alice := datastore.NameKey("Customer", "alice", nil)
	root := datastore.NameKey("Order", "Customer/alice/Order/x", nil)
	child := datastore.NameKey("Order", "x", alice)
	if _, err := c.PutMulti(ctx, []*datastore.Key{alice, root, child}, []datastore.PropertyList{
		{{Name: "who", Value: "alice"}}, {{Name: "who", Value: "root"}}, {{Name: "who", Value: "child"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteMulti(ctx, []*datastore.Key{alice, root, child}) })

	want := map[string]string{
		root.Encode():  "name=Customer/alice/Order/x | none (root entity)",
		child.Encode(): "name=x | Customer/name=alice",
	}
	check := func(where string, items []consoleRow) {
		t.Helper()
		got := map[string]string{}
		for _, it := range items {
			if len(it.Opens) != 2 {
				t.Errorf("%s lists %+v, want each entity opening its own address", where, it)
				continue
			}
			got[it.Opens[1]] = it.Name + " | " + it.Fields["Parent"]
		}
		for addr, row := range want {
			if got[addr] != row {
				t.Errorf("%s: the row opening %s reads %q, want %q (rows %v)", where, addr, got[addr], row, got)
			}
		}
	}
	kind := consoleWriteDetail(t, addr, "datastore", project, "Order")
	check("kind Order's page", kind.Sections[0].Listing.Items)

	body, _ := json.Marshal(map[string]any{"Path": []string{"Order"}, "Values": map[string]string{}})
	var query struct{ Listing consoleRows }
	consoleJSON(t, addr, http.MethodPost, "/api/query/datastore?project="+url.QueryEscape(project), string(body), &query)
	if strings.Join(query.Listing.Columns, ",") != "Parent,Properties" || query.Listing.NameColumn != "Name/ID" {
		t.Errorf("the query builder's columns are %q + %v, want Name/ID + Parent and Properties",
			query.Listing.NameColumn, query.Listing.Columns)
	}
	check("the query builder", query.Listing.Items)

	// The same key with the page's project named in it, and with another's.
	withProject := func(p string) string {
		pk := &datastorepb.Key{
			PartitionId: &datastorepb.PartitionId{ProjectId: p},
			Path: []*datastorepb.Key_PathElement{
				{Kind: "Customer", IdType: &datastorepb.Key_PathElement_Name{Name: "alice"}},
				{Kind: "Order", IdType: &datastorepb.Key_PathElement_Name{Name: "x"}},
			},
		}
		b, err := proto.Marshal(pk)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	if page := consoleWriteDetail(t, addr, "datastore", project, "Order", withProject(project)); page.summary("Parent") != "Customer/name=alice" {
		t.Errorf("the child's key naming this project opens a page with parent %q", page.summary("Parent"))
	}
	q := url.Values{"project": {project}, "name": {"Order", withProject("another-project")}}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "")
	var other consoleWritePage
	if err := json.Unmarshal([]byte(out), &other); code != http.StatusOK || err != nil ||
		!strings.Contains(other.Unavailable, "no such entity") {
		t.Errorf("another project's key = %d %s; want it read as a name no entity has", code, out)
	}
}

// TestConsoleDatastoreNameIDTellsANameFromAnID (#885).
//
// A root entity whose key name is the string "id=7", made with Create
// entity's Key identifier name=id=7, and the root entity whose numeric ID is
// 7, written with the official client, both have no parent, and were listed with the same Key cell, id=7. Kind Order's page
// and the query builder's results now name them as Google's console's
// Name/ID column does, name=id=7 and id=7; each row opens its own entity by
// its encoded key, whose page is headed by that Name/ID and changes only
// that entity, read back through the client; Delete entity asks for the
// Name/ID back. An old-form link, Order/id=7, opens the numeric one.
func TestConsoleDatastoreNameIDTellsANameFromAnID(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	named := datastore.NameKey("Order", "id=7", nil)
	numeric := datastore.IDKey("Order", 7, nil)
	t.Cleanup(func() { _ = c.DeleteMulti(ctx, []*datastore.Key{named, numeric}) })
	if _, err := c.Put(ctx, numeric, &datastore.PropertyList{{Name: "who", Value: "numeric"}}); err != nil {
		t.Fatal(err)
	}
	// Create entity's Key identifier takes what the Name/ID column shows:
	// name=id=7 is the name "id=7", which id=7 alone is not.
	if code, out := consoleAct(t, addr, "datastore", project, []string{"Order"}, "createentity",
		map[string]string{"key": "name=id=7", "field": "who", "type": "string", "value": "named"}); code != http.StatusOK {
		t.Fatalf("console Create entity name=id=7 = %d: %s", code, out)
	}
	var made datastore.PropertyList
	if err := c.Get(ctx, named, &made); err != nil || len(made) != 1 || made[0].Value != "named" {
		t.Fatalf("the entity named id=7 reads back %v, %v", made, err)
	}

	want := map[string]string{named.Encode(): "name=id=7 | none (root entity)", numeric.Encode(): "id=7 | none (root entity)"}
	check := func(where string, items []consoleRow) {
		t.Helper()
		got := map[string]string{}
		for _, it := range items {
			if len(it.Opens) == 2 {
				got[it.Opens[1]] = it.Name + " | " + it.Fields["Parent"]
			}
		}
		for a, row := range want {
			if got[a] != row {
				t.Errorf("%s: the row opening %s reads %q, want %q (rows %v)", where, a, got[a], row, got)
			}
		}
	}
	var kind struct {
		Sections []struct{ Listing consoleRows }
	}
	q := url.Values{"project": {project}, "name": {"Order"}}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "", &kind)
	if len(kind.Sections) == 0 || kind.Sections[0].Listing.NameColumn != "Name/ID" {
		t.Fatalf("kind Order's page is %+v, want a Name/ID column", kind)
	}
	check("kind Order's page", kind.Sections[0].Listing.Items)
	body, _ := json.Marshal(map[string]any{"Path": []string{"Order"}, "Values": map[string]string{}})
	var query struct{ Listing consoleRows }
	consoleJSON(t, addr, http.MethodPost, "/api/query/datastore?project="+url.QueryEscape(project), string(body), &query)
	check("the query builder", query.Listing.Items)

	for _, tc := range []struct {
		key       *datastore.Key
		who, head string
	}{{named, "named", "name=id=7"}, {numeric, "numeric", "id=7"}} {
		page := consoleWriteDetail(t, addr, "datastore", project, "Order", tc.key.Encode())
		if page.Title != tc.head || page.summary("Name/ID") != tc.head || page.summary("Key path") != "Order/"+tc.head {
			t.Errorf("%s's page is headed %q with Name/ID %q and key path %q; want %s", tc.who, page.Title,
				page.summary("Name/ID"), page.summary("Key path"), tc.head)
		}
		if !slices.Contains(page.actionIDs(), "deleteentity") {
			t.Errorf("%s's page does not offer Delete entity: %v", tc.who, page.actionIDs())
		}
		if code, out := consoleAct(t, addr, "datastore", project, []string{"Order", tc.key.Encode()}, "addproperty",
			map[string]string{"field": "seen", "type": "string", "value": tc.who}); code != http.StatusOK {
			t.Fatalf("Add property on %s = %d: %s", tc.who, code, out)
		}
	}
	for _, tc := range []struct {
		key *datastore.Key
		who string
	}{{named, "named"}, {numeric, "numeric"}} {
		var props datastore.PropertyList
		if err := c.Get(ctx, tc.key, &props); err != nil {
			t.Fatal(err)
		}
		got := map[string]any{}
		for _, p := range props {
			got[p.Name] = p.Value
		}
		if got["who"] != tc.who || got["seen"] != tc.who {
			t.Errorf("%s reads back %v; Add property on its page wrote another entity", tc.who, got)
		}
	}

	// The old-form link opens the numeric ID, as it did before (#875); the
	// entity named id=7 would read name=id=7.
	if old := consoleWriteDetail(t, addr, "datastore", project, "Order", "id=7"); old.summary("Name/ID") != "id=7" {
		t.Errorf("the old link Order/id=7 opens an entity with Name/ID %q, want the numeric ID 7", old.summary("Name/ID"))
	}
}

// TestConsoleFirestoreCountsMissingDocuments (#882).
//
// The Firestore screen's Documents column, and a document's Collections
// tab's, count what the collection's page lists — ListDocuments with
// show_missing, the client's CollectionRef.DocumentRefs — so a document that
// does not exist but has subcollections is counted, and each list's note
// says so.
//
// covers: google.firestore.v1.Firestore/ListDocuments
func TestConsoleFirestoreCountsMissingDocuments(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()

	// users/alice exists; users/ghost does not, and has orders. Under
	// users/alice, orders/o1 does not exist and has lines.
	for path, data := range map[string]map[string]any{
		"users/alice":                    {"name": "Alice"},
		"users/ghost/orders/o1":          {"total": 3},
		"users/alice/orders/o2":          {"total": 4},
		"users/alice/orders/o1/lines/l1": {"sku": "a"},
	} {
		if _, err := c.Doc(path).Create(ctx, data); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	refs := func(col *firestore.CollectionRef) int {
		n := 0
		it := col.DocumentRefs(ctx)
		for {
			_, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return n
			}
			if err != nil {
				t.Fatalf("DocumentRefs %s: %v", col.Path, err)
			}
			n++
		}
	}
	if n := refs(c.Collection("users")); n != 2 {
		t.Fatalf("the client's DocumentRefs lists %d users, want alice and the missing ghost", n)
	}
	if n := refs(c.Collection("users/alice/orders")); n != 2 {
		t.Fatalf("the client's DocumentRefs lists %d of alice's orders, want o2 and the missing o1", n)
	}

	var list consoleRows
	consoleJSON(t, addr, http.MethodGet, "/api/resources/firestore?project="+url.QueryEscape(project), "", &list)
	counted := false
	for _, it := range list.Items {
		if it.Name == "users" {
			counted = true
			if it.Fields["Documents"] != "2" {
				t.Errorf("users counts %q documents, want 2: alice and the missing ghost", it.Fields["Documents"])
			}
		}
	}
	if !counted || !strings.Contains(list.Note, "does not exist but has subcollections") {
		t.Errorf("the collections list is %+v; want users, and a note saying a missing document is counted", list)
	}

	alice := consoleWriteDetail(t, addr, "firestore", project, "users", "alice")
	for _, sec := range alice.Sections {
		if sec.ID != "collections" {
			continue
		}
		if len(sec.Listing.Items) != 1 || sec.Listing.Items[0].Fields["Documents"] != "2" {
			t.Errorf("alice's Collections tab is %+v, want orders counting o2 and the missing o1", sec.Listing.Items)
		}
		if !strings.Contains(sec.Listing.Note, "does not exist but has subcollections") {
			t.Errorf("alice's Collections tab note is %q", sec.Listing.Note)
		}
		return
	}
	t.Errorf("alice's page has no Collections tab: %+v", alice.Sections)
}

// TestConsoleDatastoreKeyValuesRoundTripThroughEditProperty (#887).
//
// A key-valued property was shown and edited in the older Kind/name or
// Kind/id=123 form, which cannot tell a name from an ID: a key to Order named
// "id=7" read Order/id=7, as the key to the numeric ID 7 does, and saving
// Edit property unchanged wrote the key to the numeric ID 7. Entities
// written with the official client hold key values of every shape — a name
// that looks like an ID, the numeric ID, names with a slash, a percent sign
// and an equals sign, an ancestor, a namespace, and, from an entity in a
// namespace, a key in the default one. Each property's page shows the key in
// the rendering #885 gave key paths (Order/name=id=7), its Edit property
// form is prefilled with the same, and saving that form unchanged is a no-op
// the client reads back: the same key, still a key.
func TestConsoleDatastoreKeyValuesRoundTripThroughEditProperty(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	inNS := func(k *datastore.Key, ns string) *datastore.Key {
		for e := k; e != nil; e = e.Parent {
			e.Namespace = ns
		}
		return k
	}
	alice := func() *datastore.Key { return datastore.NameKey("Customer", "alice", nil) }
	values := []struct {
		prop  string
		value *datastore.Key
		shown string
	}{
		{"named", datastore.NameKey("Order", "id=7", nil), "Order/name=id=7"},
		{"numeric", datastore.IDKey("Order", 7, nil), "Order/id=7"},
		{"digits", datastore.NameKey("Order", "7", nil), "Order/name=7"},
		{"odd", datastore.NameKey("Order", "a/b%c=d", nil), "Order/name=a%2Fb%25c=d"},
		{"child", datastore.NameKey("Order", "id=7", alice()), "Customer/name=alice/Order/name=id=7"},
		{"namespaced", inNS(datastore.IDKey("Line", 3, datastore.NameKey("Order", "x/y", alice())), "tenant-a"),
			"__namespace__/tenant-a/Customer/name=alice/Order/name=x%2Fy/Line/id=3"},
	}
	holders := []struct {
		key  *datastore.Key
		path []string
	}{
		{datastore.NameKey("Holder887", "h", nil), nil},
		{inNS(datastore.NameKey("Holder887", "h", nil), "tenant-b"), []string{"__namespace__", "tenant-b"}},
	}
	for _, hd := range holders {
		var props datastore.PropertyList
		for _, v := range values {
			props = append(props, datastore.Property{Name: v.prop, Value: v.value})
		}
		// From an entity in tenant-b, a key in the default namespace
		// stays in the default namespace.
		props = append(props, datastore.Property{Name: "home", Value: datastore.NameKey("Order", "id=7", nil)})
		if _, err := c.Put(ctx, hd.key, &props); err != nil {
			t.Fatalf("Put %v: %v", hd.key, err)
		}
		key := hd.key
		t.Cleanup(func() { _ = c.Delete(ctx, key) })
	}
	values = append(values, struct {
		prop  string
		value *datastore.Key
		shown string
	}{"home", datastore.NameKey("Order", "id=7", nil), "Order/name=id=7"})

	type propertyPage struct {
		Sections []struct{ ID, Text string }
		Edit     *struct {
			Fields []struct{ Name, Default string }
		}
	}
	for _, hd := range holders {
		entity := append(append([]string{}, hd.path...), "Holder887", hd.key.Encode())
		for _, v := range values {
			where := fmt.Sprintf("%v's %s", hd.key, v.prop)
			path := append(append([]string{}, entity...), v.prop)
			var page propertyPage
			q := url.Values{"project": {project}, "name": path}
			consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "", &page)
			if len(page.Sections) == 0 || page.Sections[0].Text != v.shown {
				t.Errorf("%s's page shows %+v, want %q", where, page.Sections, v.shown)
			}
			if page.Edit == nil {
				t.Errorf("%s offers no Edit property", where)
				continue
			}
			form := map[string]string{}
			for _, f := range page.Edit.Fields {
				form[f.Name] = f.Default
			}
			if form["type"] != "key" || form["value"] != v.shown {
				t.Errorf("%s's Edit property is prefilled %s %q, want key %q", where, form["type"], form["value"], v.shown)
			}
			// Saved unchanged, as the dialog submits it.
			if code, out := consoleEdit(t, addr, "datastore", project, path, form); code != http.StatusOK {
				t.Errorf("%s: Edit property saved unchanged = %d: %s", where, code, out)
			}
		}
		var props datastore.PropertyList
		if err := c.Get(ctx, hd.key, &props); err != nil {
			t.Fatalf("Get %v: %v", hd.key, err)
		}
		got := map[string]any{}
		for _, p := range props {
			got[p.Name] = p.Value
		}
		for _, v := range values {
			k, isKey := got[v.prop].(*datastore.Key)
			if !isKey || !k.Equal(v.value) {
				t.Errorf("after an unchanged Edit property, %v's %s reads back %#v, want %v", hd.key, v.prop, got[v.prop], v.value)
			}
		}
	}
}
