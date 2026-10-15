//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// firestoreDatabasesPage is the part of a console Firestore database page
// these tests read.
type firestoreDatabasesPage struct {
	Unavailable string
	Actions     []bigtableAction
	Sections    []struct {
		ID      string
		Listing struct {
			Items []struct {
				Name    string
				ActsOn  []string
				Actions []bigtableAction
			}
		}
	}
}

func firestoreDatabasePage(t *testing.T, addr, project, db string) firestoreDatabasesPage {
	t.Helper()
	q := url.Values{"project": {project}, "name": {db}}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/firestore-databases?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %s = %d: %s", db, code, body)
	}
	var page firestoreDatabasesPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if page.Unavailable != "" {
		t.Fatalf("the database page is unavailable: %s", page.Unavailable)
	}
	return page
}

func firestoreDatabasesAct(t *testing.T, addr, project string, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	return consoleDo(t, addr, http.MethodPost, "/api/actions/firestore-databases?project="+url.QueryEscape(project), string(body))
}

func actionDefaults(actions []bigtableAction, id string) (map[string]string, bool) {
	for _, a := range actions {
		if a.ID == id {
			out := map[string]string{}
			for _, f := range a.Fields {
				out[f.Name] = f.Default
			}
			return out, true
		}
	}
	return nil, false
}

// TestConsoleFirestoreDatabaseEditsIndexesAndFieldOverrides (#1343).
//
// A database's settings, indexes and field overrides changed from the
// console's Firestore databases screen, each read back with the official
// admin client against the Admin API the Firestore pod's call front serves.
// Edit database, prefilled from the database, changes concurrency mode,
// point-in-time recovery, App Engine integration and delete protection as
// GetDatabase reads them, and the same form submitted again with its old
// etag is refused. Create index makes the composite index ListIndexes
// returns, with its fields' order and array config and its query scope, and
// Delete index on its row on the Indexes tab removes it. Add field override
// sets a TTL policy and a single-field exemption GetField returns; Edit
// field override on its row changes them; Delete field override returns the
// field to its inherited settings. The changes go through the console, so
// only the client's reads are claimed.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/GetDatabase, google.firestore.admin.v1.FirestoreAdmin/ListIndexes, google.firestore.admin.v1.FirestoreAdmin/GetField
func TestConsoleFirestoreDatabaseEditsIndexesAndFieldOverrides(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	project := h.Project()
	id := fmt.Sprintf("console-%d", time.Now().UnixNano()%1e9)
	db := "projects/" + project + "/databases/" + id

	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
			Database:   &adminpb.Database{Name: db, DeleteProtectionState: adminpb.Database_DELETE_PROTECTION_DISABLED},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"delete_protection_state"}}}); err == nil {
			_, _ = op.Wait(ctx)
		}
		if op, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: db}); err == nil {
			_, _ = op.Wait(ctx)
		}
	})

	// Edit database.
	page := firestoreDatabasePage(t, addr, project, id)
	values, ok := actionDefaults(page.Actions, "editdatabase")
	if !ok {
		t.Fatalf("the database page offers no Edit database: %+v", page.Actions)
	}
	stale := map[string]string{}
	for k, v := range values {
		stale[k] = v
	}
	values["concurrency"], values["pitr"], values["appEngine"], values["deleteProtection"] = "OPTIMISTIC", "true", "true", "true"
	if code, out := firestoreDatabasesAct(t, addr, project, []string{id}, "editdatabase", values); code != http.StatusOK {
		t.Fatalf("Edit database = %d: %s", code, out)
	}
	got, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db})
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if got.ConcurrencyMode != adminpb.Database_OPTIMISTIC || got.PointInTimeRecoveryEnablement != adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED ||
		got.AppEngineIntegrationMode != adminpb.Database_ENABLED || got.DeleteProtectionState != adminpb.Database_DELETE_PROTECTION_ENABLED {
		t.Errorf("after Edit database, GetDatabase reads %v", got)
	}
	if code, out := firestoreDatabasesAct(t, addr, project, []string{id}, "editdatabase", stale); code == http.StatusOK ||
		!strings.Contains(consoleError(t, out), "etag") {
		t.Errorf("Edit database from a form drawn before the change = %d: %s; want the etag refusal", code, out)
	}

	// Create index, then Delete index on its row.
	if code, out := firestoreDatabasesAct(t, addr, project, []string{id}, "createindex", map[string]string{
		"collectionGroup": "orders", "fields": "city ascending\ntags array-contains", "queryScope": "COLLECTION_GROUP"}); code != http.StatusOK {
		t.Fatalf("Create index = %d: %s", code, out)
	}
	idx, err := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/orders"}).Next()
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if idx.QueryScope != adminpb.Index_COLLECTION_GROUP || len(idx.Fields) < 2 || idx.Fields[0].FieldPath != "city" ||
		idx.Fields[0].GetOrder() != adminpb.Index_IndexField_ASCENDING || idx.Fields[1].FieldPath != "tags" ||
		idx.Fields[1].GetArrayConfig() != adminpb.Index_IndexField_CONTAINS {
		t.Errorf("ListIndexes returns %v", idx)
	}
	var indexTarget []string
	for _, s := range firestoreDatabasePage(t, addr, project, id).Sections {
		for _, item := range s.Listing.Items {
			if s.ID == "indexes" && strings.HasSuffix(idx.Name, "/"+item.Name) {
				if _, ok := confirmedDelete(item.Actions, "deleteindex"); ok {
					indexTarget = item.ActsOn
				}
			}
		}
	}
	if len(indexTarget) == 0 {
		t.Fatal("the Indexes tab offers no confirmed Delete index on the created index")
	}
	if code, out := firestoreDatabasesAct(t, addr, project, indexTarget, "deleteindex", nil); code != http.StatusOK {
		t.Fatalf("Delete index = %d: %s", code, out)
	}
	if _, err := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/orders"}).Next(); err != iterator.Done {
		t.Errorf("after Delete index, ListIndexes returns another index or fails: %v", err)
	}

	// Add, edit and delete a field override.
	fieldName := db + "/collectionGroups/sessions/fields/expireAt"
	if code, out := firestoreDatabasesAct(t, addr, project, []string{id}, "addfield", map[string]string{
		"collectionGroup": "sessions", "field": "expireAt", "ttl": "true", "override": "true", "ascCollection": "true"}); code != http.StatusOK {
		t.Fatalf("Add field override = %d: %s", code, out)
	}
	f, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName})
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if x := f.GetIndexConfig().GetIndexes(); f.GetTtlConfig().GetState() != adminpb.Field_TtlConfig_ACTIVE ||
		f.GetIndexConfig().GetUsesAncestorConfig() || len(x) != 1 || x[0].Fields[0].GetOrder() != adminpb.Index_IndexField_ASCENDING ||
		x[0].QueryScope != adminpb.Index_COLLECTION {
		t.Errorf("after Add field override, GetField reads %v", f)
	}
	var fieldTarget []string
	var edit map[string]string
	for _, s := range firestoreDatabasePage(t, addr, project, id).Sections {
		for _, item := range s.Listing.Items {
			if s.ID == "fields" && item.Name == "expireAt" {
				fieldTarget = item.ActsOn
				edit, _ = actionDefaults(item.Actions, "editfield")
			}
		}
	}
	if len(fieldTarget) == 0 || edit == nil {
		t.Fatal("the Field overrides tab offers no Edit field override on expireAt")
	}
	if edit["ttl"] != "true" || edit["override"] != "true" || edit["ascCollection"] != "true" {
		t.Errorf("Edit field override is prefilled %v", edit)
	}
	edit["ttl"], edit["ascCollection"] = "false", "false"
	if code, out := firestoreDatabasesAct(t, addr, project, fieldTarget, "editfield", edit); code != http.StatusOK {
		t.Fatalf("Edit field override = %d: %s", code, out)
	}
	f, err = c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName})
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if f.GetTtlConfig() != nil || f.GetIndexConfig().GetUsesAncestorConfig() || len(f.GetIndexConfig().GetIndexes()) != 0 {
		t.Errorf("after Edit field override to an exemption from every index and no TTL, GetField reads %v", f)
	}
	if code, out := firestoreDatabasesAct(t, addr, project, fieldTarget, "deletefield", nil); code != http.StatusOK {
		t.Fatalf("Delete field override = %d: %s", code, out)
	}
	f, err = c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName})
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if f.GetTtlConfig() != nil || !f.GetIndexConfig().GetUsesAncestorConfig() {
		t.Errorf("after Delete field override, GetField reads %v, want the inherited settings", f)
	}
}
