//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// TestConsoleDatastoreStudio (#1846): the Datastore page of a Datastore
// mode database as Google's console has it. A kind's query tab runs Query
// by GQL, entities and an aggregation, in the chosen database alone; the
// Indexes page lists the database's composite index, and Delete index
// deletes it; Export writes the database to Cloud Storage and Import reads
// the export back, each answering with what it did; the official clients
// read every result back.
func TestConsoleDatastoreStudio(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	dsAddr := h.Endpoint(EnvDatastore)
	sc := firestoreStorage(t, h)
	ac := datastoreAdminClient(t, h)
	ctx := h.Context()
	project := h.Project()
	id := uniqueDB("cstudio")
	if _, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	bh := bucket(t, h, sc)
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	seedDatastore(t, ctx, dc)
	q := "?project=" + url.QueryEscape(project) + "&database=" + id

	// Query by GQL, on Task's query tab.
	gql := func(statement string) datastoreRows {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{"Task"},
			"Values": map[string]string{"queryBy": "Query by GQL", "gql": statement}})
		var res struct{ Listing datastoreRows }
		consoleJSON(t, addr, http.MethodPost, "/api/query/datastore"+q, string(body), &res)
		return res.Listing
	}
	rows := gql("SELECT * FROM Task WHERE V = '2'")
	if len(rows.Items) != 1 || rows.Items[0].Name != "name=t2" || rows.Items[0].Fields["Kind"] != "Task" {
		t.Errorf("Query by GQL WHERE V = '2' = %+v, want name=t2", rows.Items)
	}
	if rows := gql("SELECT * FROM Note"); len(rows.Items) != 1 || rows.Items[0].Name != "name=n1" {
		t.Errorf("Query by GQL FROM Note = %+v, want name=n1", rows.Items)
	}
	if rows := gql("SELECT COUNT(*) AS n FROM Task"); len(rows.Items) != 1 || rows.Items[0].Name != "n" || rows.Items[0].Fields["Value"] != "2" {
		t.Errorf("Query by GQL COUNT(*) = %+v, want n 2 (the default namespace's t1, t2)", rows.Items)
	}

	// Create entity's Properties, a row each with its Indexed.
	body, _ := json.Marshal(map[string]string{"kind": "Made", "keyType": "Custom name", "key": "m1",
		"properties": `[{"name":"hours","type":"integer","value":"3","indexed":"false"},{"name":"when","type":"timestamp","value":"2026-11-19T10:00:00Z","indexed":"true"}]`})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create entity = %d: %s", code, out)
	}
	var made datastore.PropertyList
	if err := dc.Get(ctx, datastore.NameKey("Made", "m1", nil), &made); err != nil {
		t.Fatalf("the created entity: %v", err)
	}
	if len(made) != 2 || made[0].Name != "hours" || made[0].Value != int64(3) || !made[0].NoIndex || made[1].NoIndex {
		t.Errorf("Create entity wrote %+v", made)
	}
	// Edit entity: its page's form holds every property; Save writes the
	// rows back, a removed one deleted.
	var entity struct {
		Edit *struct {
			Label  string
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(consoleGetDetail(t, addr, project, id, "Made", "m1")), &entity); err != nil {
		t.Fatal(err)
	}
	if entity.Edit == nil || entity.Edit.Label != "Edit entity" || len(entity.Edit.Fields) != 1 ||
		!strings.Contains(entity.Edit.Fields[0].Default, `"name":"hours"`) {
		t.Fatalf("the entity's page offers %+v, want Edit entity with its properties", entity.Edit)
	}
	edit, _ := json.Marshal(map[string]any{"Path": []string{"Made", "m1"}, "Values": map[string]string{
		"properties": `[{"name":"hours","type":"float","value":"4.5","indexed":"true"}]`}})
	if code, out := consoleDo(t, addr, http.MethodPatch, "/api/resources/datastore"+q, string(edit)); code != http.StatusOK {
		t.Fatalf("Edit entity = %d: %s", code, out)
	}
	made = nil
	if err := dc.Get(ctx, datastore.NameKey("Made", "m1", nil), &made); err != nil {
		t.Fatal(err)
	}
	if len(made) != 1 || made[0].Value != 4.5 || made[0].NoIndex {
		t.Errorf("after Edit entity the entity holds %+v, want hours 4.5 indexed alone", made)
	}

	body, _ = json.Marshal(map[string]string{"kind": "Made", "keyType": "Numeric ID (auto-generated)", "key": "ignored"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create entity with a numeric ID = %d: %s", code, out)
	}
	if keys, err := dc.GetAll(ctx, datastore.NewQuery("Made").KeysOnly(), nil); err != nil || len(keys) != 2 {
		t.Errorf("Made after two creates = %v, %v", keys, err)
	} else {
		for _, k := range keys {
			if k.Name == "ignored" {
				t.Errorf("Numeric ID (auto-generated) made a named key %v", k)
			}
		}
	}

	// Indexes.
	actx := inDatabase(ctx, project, id)
	op, err := ac.CreateIndex(actx, &dsadminpb.CreateIndexRequest{ProjectId: project, Index: &dsadminpb.Index{Kind: "Task",
		Ancestor: dsadminpb.Index_NONE, Properties: []*dsadminpb.Index_IndexedProperty{
			{Name: "V", Direction: dsadminpb.Index_ASCENDING}, {Name: "N", Direction: dsadminpb.Index_DESCENDING}}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	ix, err := op.Wait(actx)
	if err != nil {
		t.Fatal(err)
	}
	detail := func(path ...string) string {
		t.Helper()
		v := url.Values{"project": {project}, "database": {id}, "name": path}
		code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/datastore?"+v.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("the Datastore page %v = %d: %s", path, code, out)
		}
		return out
	}
	page := detail("__indexes__")
	for _, want := range []string{`"` + ix.GetIndexId() + `"`, "V (Ascending), N (Descending)", `"Serving"`, `"Delete index"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Indexes page does not show %s: %s", want, page)
		}
	}
	act := func(path []string, action string, values map[string]string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/datastore"+q, string(body))
		if code != http.StatusOK {
			t.Fatalf("%s = %d: %s", action, code, out)
		}
		return out
	}
	act([]string{"__indexes__"}, "deleteindex", map[string]string{"index": ix.GetIndexId()})
	if page := detail("__indexes__"); strings.Contains(page, `"`+ix.GetIndexId()+`"`) {
		t.Errorf("the index is listed after Delete index: %s", page)
	}

	// Export, then Import into a cleared database.
	out := act(nil, "export", map[string]string{"namespace": "All Namespaces", "kind": "All Kinds",
		"destination": "gs://" + bh.BucketName() + "/console"})
	var res struct{ Result datastoreRows }
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, it := range res.Result.Items {
		fields[it.Name] = it.Fields["Value"]
	}
	file := fields["Output URL"]
	if !strings.HasPrefix(file, "gs://"+bh.BucketName()+"/console/") || !strings.HasSuffix(file, ".overall_export_metadata") || fields["Entities"] != "6" {
		t.Fatalf("Export answered %v", fields)
	}
	clearDatastore(t, ctx, dc)
	act(nil, "import", map[string]string{"file": file, "namespace": "(default)", "kind": "Task"})
	if got := strings.Join(entityNames(t, ctx, dc), ","); got != "/Task/t1,/Task/t2" {
		t.Errorf("after Import of (default)'s Task = %s, want /Task/t1,/Task/t2", got)
	}
	act(nil, "import", map[string]string{"file": file, "namespace": "All Namespaces", "kind": "All Kinds"})
	if got := strings.Join(entityNames(t, ctx, dc), ","); got != allEntities {
		t.Errorf("after Import of everything = %s, want %s", got, allEntities)
	}
}

// consoleGetDetail is the Datastore page at path in a database, as JSON.
func consoleGetDetail(t *testing.T, addr, project, database string, path ...string) string {
	t.Helper()
	v := url.Values{"project": {project}, "database": {database}, "name": path}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/datastore?"+v.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("the Datastore page %v = %d: %s", path, code, out)
	}
	return out
}
