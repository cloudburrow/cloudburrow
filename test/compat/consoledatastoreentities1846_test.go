//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

type ent1846 struct {
	Done bool
	Tags []string
	N    int
}

// TestConsoleDatastoreEntitiesPage (#1846): the Datastore page as Google's
// Entities page. In a Datastore mode database, it lists the entities of
// the kind its Kind picker chose (the first kind when none is) in the
// namespace its Namespace picker chose, each row opening its entity; the
// pickers list the namespaces and the namespace's kinds; Delete on the
// page deletes a checked entity by its Name/ID in that kind and namespace.
// A kind's Query by kind, built from query clauses (WHERE and ORDER BY,
// typed values), lists the matching entities in order, and the tab
// Query by GQL is chosen by tab:Query. The Indexes page's Entries and Size
// are computed from the kind's entities. The official client reads every
// change back.
func TestConsoleDatastoreEntitiesPage(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	dsAddr := h.Endpoint(EnvDatastore)
	h.Endpoint(EnvFirestore)
	ac := datastoreAdminClient(t, h)
	ctx := h.Context()
	project := h.Project()
	id := uniqueDB("cents")
	if _, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	at := func(kind, name, ns string) *datastore.Key {
		k := datastore.NameKey(kind, name, nil)
		k.Namespace = ns
		return k
	}
	keys := []*datastore.Key{at("Alpha", "a1", ""), at("Task", "t1", ""), at("Task", "t2", ""), at("Task", "t3", ""), at("Task", "x1", "tenant")}
	if _, err := dc.PutMulti(ctx, keys, []*ent1846{{true, []string{"red", "blue"}, 1}, {false, []string{"red"}, 3},
		{true, []string{"blue"}, 2}, {true, nil, 1}, {true, []string{"green"}, 9}}); err != nil {
		t.Fatal(err)
	}

	type listing struct {
		Items []struct {
			Name   string
			Fields map[string]string
			Opens  []string
		}
		Filters []struct {
			Key, Selected, Default string
			Options                []struct{ ID, Label string }
		}
	}
	list := func(params string) listing {
		t.Helper()
		var l listing
		consoleJSON(t, addr, http.MethodGet, "/api/resources/datastore?project="+url.QueryEscape(project)+"&database="+id+params, "", &l)
		return l
	}
	names := func(l listing) string {
		var out []string
		for _, it := range l.Items {
			out = append(out, it.Name)
		}
		return strings.Join(out, ",")
	}
	// No kind chosen: the first kind, Alpha.
	l := list("")
	if got := names(l); got != "name=a1" {
		t.Errorf("the Entities page with no kind chosen lists %q, want Alpha's name=a1", got)
	}
	if len(l.Filters) != 2 || l.Filters[0].Key != "namespace" || l.Filters[1].Key != "kind" || l.Filters[1].Selected != "Alpha" {
		t.Fatalf("the Entities page's pickers = %+v, want Namespace and Kind (Alpha)", l.Filters)
	}
	var nss, kinds []string
	for _, o := range l.Filters[0].Options {
		nss = append(nss, o.ID+"="+o.Label)
	}
	for _, o := range l.Filters[1].Options {
		kinds = append(kinds, o.ID)
	}
	if got := strings.Join(nss, ","); got != "=[default],tenant=" {
		t.Errorf("the Namespace picker lists %q, want [default] and tenant", got)
	}
	if got := strings.Join(kinds, ","); got != "Alpha,Task" {
		t.Errorf("the Kind picker lists %q, want Alpha,Task", got)
	}
	l = list("&kind=Task")
	if got := names(l); got != "name=t1,name=t2,name=t3" {
		t.Errorf("the Entities page for Task lists %q", got)
	}
	if len(l.Items) > 0 && (len(l.Items[0].Opens) != 2 || l.Items[0].Opens[0] != "Task") {
		t.Errorf("an entity's row opens %v, want Task/<its key>", l.Items[0].Opens)
	}
	l = list("&namespace=tenant")
	if got := names(l); got != "name=x1" || len(l.Items[0].Opens) != 4 || l.Items[0].Opens[0] != "__namespace__" {
		t.Errorf("the Entities page in tenant lists %q, opening %+v", got, l.Items)
	}

	// Delete on the page: t3, by its Name/ID, in Task.
	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/datastore?project="+url.QueryEscape(project)+
		"&database="+id+"&kind=Task&name="+url.QueryEscape("name=t3"), ""); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("Delete name=t3 = %d: %s", code, out)
	}
	var e ent1846
	if err := dc.Get(ctx, at("Task", "t3", ""), &e); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("t3 after Delete on the Entities page: %v", err)
	}
	if code, _ := consoleDo(t, addr, http.MethodDelete, "/api/resources/datastore?project="+url.QueryEscape(project)+
		"&database="+id+"&kind=Task&name="+url.QueryEscape("name=gone"), ""); code == http.StatusOK {
		t.Error("Delete of an entity that is not there succeeded")
	}

	// Query by kind, from query clauses.
	query := func(values map[string]string) []string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{"Task"}, "Values": values})
		var res struct{ Listing datastoreRows }
		code, raw := consoleDo(t, addr, http.MethodPost, "/api/query/datastore?project="+url.QueryEscape(project)+"&database="+id, string(body))
		if code != http.StatusOK {
			t.Errorf("query %v = %d: %s", values, code, raw)
			return nil
		}
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range res.Listing.Items {
			out = append(out, it.Name)
		}
		return out
	}
	clauses := func(rows ...map[string]string) string {
		b, _ := json.Marshal(rows)
		return string(b)
	}
	for _, c := range []struct {
		clauses string
		want    string
	}{
		{clauses(map[string]string{"clause": "WHERE", "property": "Done", "op": "==", "type": "boolean", "value": "false"}), "name=t1"},
		{clauses(map[string]string{"clause": "WHERE", "property": "Tags", "op": "==", "type": "string", "value": "red"},
			map[string]string{"clause": "WHERE", "property": "Done", "op": "==", "type": "boolean", "value": "false"}), "name=t1"},
		{clauses(map[string]string{"clause": "ORDER BY", "property": "N", "op": "Descending"}), "name=t1,name=t2"},
		{clauses(map[string]string{"clause": "WHERE", "property": "N", "op": "<=", "type": "integer", "value": "2"}), "name=t2"},
		{clauses(map[string]string{"clause": "WHERE", "property": "N", "op": ">", "type": "integer", "value": "2"}), "name=t1"},
	} {
		got := query(map[string]string{"tab:Query": "Query by kind", "clauses": c.clauses})
		if strings.Join(got, ",") != c.want {
			t.Errorf("Query by kind %s = %v, want %s", c.clauses, got, c.want)
		}
	}
	if got := query(map[string]string{"tab:Query": "Query by GQL", "gql": "SELECT * FROM Task WHERE N = 2"}); strings.Join(got, ",") != "name=t2" {
		t.Errorf("Query by GQL = %v, want name=t2", got)
	}

	// The Indexes page's Entries and Size: Task on Tags and N, in every
	// namespace: t1 (red), t2 (blue) and tenant's x1 (green) one entry
	// each; t3 is gone.
	actx := inDatabase(ctx, project, id)
	op, err := ac.CreateIndex(actx, &dsadminpb.CreateIndexRequest{ProjectId: project, Index: &dsadminpb.Index{Kind: "Task",
		Ancestor: dsadminpb.Index_NONE, Properties: []*dsadminpb.Index_IndexedProperty{
			{Name: "Tags", Direction: dsadminpb.Index_ASCENDING}, {Name: "N", Direction: dsadminpb.Index_ASCENDING}}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	ix, err := op.Wait(actx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ac.DeleteIndex(actx, &dsadminpb.DeleteIndexRequest{ProjectId: project, IndexId: ix.GetIndexId()}); err == nil {
			_, _ = op.Wait(actx)
		}
	})
	v := url.Values{"project": {project}, "database": {id}, "name": {"__indexes__"}}
	var page struct {
		Sections []struct {
			Listing struct {
				Columns []string
				Items   []struct {
					Name   string
					Fields map[string]string
				}
			}
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+v.Encode(), "", &page)
	found := false
	for _, s := range page.Sections {
		for _, it := range s.Listing.Items {
			if it.Name != ix.GetIndexId() {
				continue
			}
			found = true
			// Each entry: the key (Task 5 + tN 3 + 16, and tenant
			// 7 for x1), the tag (red 4, blue 5, green 6), N 8, and
			// 32: 68 + 69 + 77 bytes.
			if it.Fields["Entries"] != "3" || it.Fields["Size"] != "214 B" {
				t.Errorf("the index's Entries, Size = %q, %q; want 3, 214 B", it.Fields["Entries"], it.Fields["Size"])
			}
		}
	}
	if !found {
		t.Errorf("the Indexes page does not list %s: %+v", ix.GetIndexId(), page)
	}
}
