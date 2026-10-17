package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// firestoreAdminFront is the screen against the call front's own Admin API
// (internal/firestoreadmin), in front of a stand-in for the emulator.
func firestoreAdminFront(t *testing.T) firestoreDatabasesProvider {
	t.Helper()
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(emu.Close)
	up := strings.TrimPrefix(emu.URL, "http://")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := callfront.Server(callfront.HandlerWith(up, &calllog.Ring{}, t.Logf, firestoreadmin.New(up)), t.Logf)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return firestoreDatabasesProvider{endpoint: l.Addr().String()}
}

// TestConsoleFirestoreDatabasesIndexesAndFields (#1331): the Firestore
// databases screen lists (default) and a database Create database made, a
// database's page shows its settings, its composite indexes and its field
// overrides as the Admin API returns them, and Delete database removes it.
// The API is the call front's own (internal/firestoreadmin), in front of a
// stand-in for the emulator.
func TestConsoleFirestoreDatabasesIndexesAndFields(t *testing.T) {
	p := firestoreAdminFront(t)
	ctx := context.Background()

	name, err := p.Create(ctx, "p", map[string]string{"database": "orders", "location": "eur3", "concurrency": "OPTIMISTIC"})
	if err != nil || name != "orders" {
		t.Fatalf("Create = %q, %v", name, err)
	}
	list, err := p.List(ctx, "p")
	if err != nil || list.Unavailable != "" || len(list.Items) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if got := list.Items[1]; got.Name != "orders" || got.Fields["Location"] != "eur3" || got.Fields["Concurrency"] != "Optimistic" || got.Fields["Type"] != "Native" {
		t.Errorf("the orders row = %+v", got)
	}

	c, err := admin.NewFirestoreAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := "projects/p/databases/orders"
	if _, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: db + "/collectionGroups/lines", Index: &adminpb.Index{
		QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{
			{FieldPath: "sku", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "qty", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_DESCENDING}},
		}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateField(ctx, &adminpb.UpdateFieldRequest{
		Field:      &adminpb.Field{Name: db + "/collectionGroups/sessions/fields/expireAt", TtlConfig: &adminpb.Field_TtlConfig{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl_config"}}}); err != nil {
		t.Fatal(err)
	}

	d, err := p.Detail(ctx, "p", []string{"orders"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 4 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if props["Location"] != "eur3" || props["Concurrency mode"] != "Optimistic" || props["Delete protection"] != "Disabled" {
		t.Errorf("the summary = %v", props)
	}
	idx := d.Sections[0].Listing
	if len(idx.Items) != 1 || idx.Items[0].Fields["Collection group"] != "lines" ||
		idx.Items[0].Fields["Fields"] != "sku Ascending, qty Descending, __name__ Descending" || idx.Items[0].Fields["State"] != "Ready" {
		t.Errorf("the Indexes tab = %+v", idx.Items)
	}
	fields := d.Sections[1].Listing
	if len(fields.Items) != 1 || fields.Items[0].Name != "expireAt" || fields.Items[0].Fields["TTL"] != "Active" ||
		fields.Items[0].Fields["Indexes"] != "Inherited" {
		t.Errorf("the Field overrides tab = %+v", fields.Items)
	}

	if err := p.Delete(ctx, "p", "orders"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if list, _ := p.List(ctx, "p"); len(list.Items) != 1 || list.Items[0].Name != "(default)" {
		t.Errorf("after Delete = %+v", list.Items)
	}
}

func fsAdminAction(actions []console.Action, id string) (console.Action, bool) {
	for _, a := range actions {
		if a.ID == id {
			return a, true
		}
	}
	return console.Action{}, false
}

func fsAdminDefaults(a console.Action) map[string]string {
	out := map[string]string{}
	for _, f := range a.Fields {
		out[f.Name] = f.Default
	}
	return out
}

// TestConsoleFirestoreDatabaseEditsIndexesAndFieldOverrides (#1343): Edit
// database, prefilled from the database, changes its four settings as the
// admin client's GetDatabase reads them, and an edit from a form drawn
// before is refused ABORTED. Create index makes the composite index
// ListIndexes returns, and its row's Delete index removes it. Add field
// override sets a TTL policy and an exemption that GetField returns; Edit
// field override, prefilled from it, changes them, and saved unchanged
// leaves them; Delete field override returns the field to its inherited
// settings.
func TestConsoleFirestoreDatabaseEditsIndexesAndFieldOverrides(t *testing.T) {
	p := firestoreAdminFront(t)
	ctx := context.Background()
	c, err := admin.NewFirestoreAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := "projects/p/databases/(default)"

	// Edit database.
	edit, ok := fsAdminAction(p.DetailActions(ctx, "p", []string{"(default)"}), "editdatabase")
	if !ok {
		t.Fatal("a database's page offers no Edit database")
	}
	values := fsAdminDefaults(edit)
	if values["concurrency"] != "PESSIMISTIC" || values["deleteProtection"] != "false" || values["etag"] == "" {
		t.Fatalf("Edit database is prefilled %v", values)
	}
	stale := fsAdminDefaults(edit)
	values["concurrency"], values["pitr"], values["appEngine"], values["deleteProtection"] = "OPTIMISTIC", "true", "true", "true"
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "editdatabase", values); err != nil {
		t.Fatalf("Edit database: %v", err)
	}
	got, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db})
	if err != nil {
		t.Fatal(err)
	}
	if got.ConcurrencyMode != adminpb.Database_OPTIMISTIC || got.PointInTimeRecoveryEnablement != adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED ||
		got.AppEngineIntegrationMode != adminpb.Database_ENABLED || got.DeleteProtectionState != adminpb.Database_DELETE_PROTECTION_ENABLED {
		t.Errorf("after Edit database, GetDatabase reads %v", got)
	}
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "editdatabase", stale); status.Code(err) != codes.Aborted {
		t.Errorf("Edit database from a stale form: %v, want ABORTED", err)
	}
	if err := p.Delete(ctx, "p", "(default)"); err == nil || !strings.Contains(err.Error(), "delete protection") {
		t.Errorf("Delete database with delete protection on: %v", err)
	}

	// Create index, and Delete index on its row.
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "createindex", map[string]string{"collectionGroup": "orders",
		"fields": "city ascending\ntags array-contains\n", "queryScope": "COLLECTION_GROUP"}); err != nil {
		t.Fatalf("Create index: %v", err)
	}
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "createindex", map[string]string{"collectionGroup": "orders",
		"fields": "city sideways", "queryScope": "COLLECTION"}); err == nil {
		t.Error("Create index with a mode that is not one accepted it")
	}
	resp, err := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/orders"}).Next()
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if resp.QueryScope != adminpb.Index_COLLECTION_GROUP || len(resp.Fields) != 3 || resp.Fields[0].FieldPath != "city" ||
		resp.Fields[0].GetOrder() != adminpb.Index_IndexField_ASCENDING || resp.Fields[1].GetArrayConfig() != adminpb.Index_IndexField_CONTAINS {
		t.Errorf("the created index = %v", resp)
	}
	d, err := p.Detail(ctx, "p", []string{"(default)"})
	if err != nil || len(d.Sections) != 4 || len(d.Sections[0].Listing.Items) != 1 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	row := d.Sections[0].Listing.Items[0]
	if _, ok := fsAdminAction(row.Actions, "deleteindex"); !ok || len(row.ActsOn) != 4 {
		t.Fatalf("the index row offers %+v at %v", row.Actions, row.ActsOn)
	}
	if _, ok := fsAdminAction(p.DetailActions(ctx, "p", row.ActsOn), "deleteindex"); !ok {
		t.Fatal("DetailActions at the index row's path offers no Delete index")
	}
	if err := p.ActAt(ctx, "p", row.ActsOn, "deleteindex", nil); err != nil {
		t.Fatalf("Delete index: %v", err)
	}
	if _, err := c.GetIndex(ctx, &adminpb.GetIndexRequest{Name: resp.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIndex after Delete index: %v, want NOT_FOUND", err)
	}

	// Add, edit and delete a field override.
	fieldName := db + "/collectionGroups/sessions/fields/expireAt"
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "addfield", map[string]string{"collectionGroup": "sessions", "field": "expireAt",
		"ttl": "true", "override": "true", "descCollection": "true"}); err != nil {
		t.Fatalf("Add field override: %v", err)
	}
	f, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName})
	if err != nil {
		t.Fatal(err)
	}
	if f.GetTtlConfig().GetState() != adminpb.Field_TtlConfig_ACTIVE || f.GetIndexConfig().GetUsesAncestorConfig() ||
		len(f.GetIndexConfig().GetIndexes()) != 1 || f.GetIndexConfig().GetIndexes()[0].Fields[0].GetOrder() != adminpb.Index_IndexField_DESCENDING {
		t.Errorf("after Add field override, GetField reads %v", f)
	}
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "addfield", map[string]string{"collectionGroup": "sessions", "field": "expireAt"}); err == nil {
		t.Error("Add field override on a field that has one was accepted")
	}
	path := []string{"(default)", "fields", "sessions", "expireAt"}
	editField, ok := fsAdminAction(p.DetailActions(ctx, "p", path), "editfield")
	if !ok {
		t.Fatal("the field override offers no Edit field override")
	}
	values = fsAdminDefaults(editField)
	if values["ttl"] != "true" || values["override"] != "true" || values["descCollection"] != "true" || values["ascCollection"] != "false" {
		t.Errorf("Edit field override is prefilled %v", values)
	}
	if err := p.ActAt(ctx, "p", path, "editfield", values); err != nil {
		t.Fatalf("Edit field override saved unchanged: %v", err)
	}
	if again, _ := c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName}); !proto.Equal(again, f) {
		t.Errorf("saved unchanged, GetField reads %v, want %v", again, f)
	}
	values["ttl"], values["descCollection"], values["arrayGroup"] = "false", "false", "true"
	if err := p.ActAt(ctx, "p", path, "editfield", values); err != nil {
		t.Fatalf("Edit field override: %v", err)
	}
	f, _ = c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName})
	if x := f.GetIndexConfig().GetIndexes(); f.GetTtlConfig() != nil || len(x) != 1 || x[0].QueryScope != adminpb.Index_COLLECTION_GROUP ||
		x[0].Fields[0].GetArrayConfig() != adminpb.Index_IndexField_CONTAINS {
		t.Errorf("after Edit field override, GetField reads %v", f)
	}
	if _, ok := fsAdminAction(p.DetailActions(ctx, "p", path), "deletefield"); !ok {
		t.Fatal("the field override offers no Delete field override")
	}
	if err := p.ActAt(ctx, "p", path, "deletefield", nil); err != nil {
		t.Fatalf("Delete field override: %v", err)
	}
	f, _ = c.GetField(ctx, &adminpb.GetFieldRequest{Name: fieldName})
	if f.GetTtlConfig() != nil || !f.GetIndexConfig().GetUsesAncestorConfig() {
		t.Errorf("after Delete field override, GetField reads %v", f)
	}
	if acts := p.DetailActions(ctx, "p", path); len(acts) != 0 {
		t.Errorf("a field with no override is offered %+v", acts)
	}
}

// TestFirestoreIndexFieldsParse: Create index's Fields lines, and a field
// path with a space in it.
func TestFirestoreIndexFieldsParse(t *testing.T) {
	fields, err := parseIndexFields("a asc\n\n`odd name` DESCENDING\ntags array\n")
	if err != nil || len(fields) != 3 || fields[1].FieldPath != "`odd name`" ||
		fields[1].GetOrder() != adminpb.Index_IndexField_DESCENDING || fields[2].GetArrayConfig() != adminpb.Index_IndexField_CONTAINS {
		t.Errorf("parseIndexFields = %v, %v", fields, err)
	}
	for _, bad := range []string{"", "a", "a up"} {
		if _, err := parseIndexFields(bad); err == nil {
			t.Errorf("parseIndexFields(%q) accepted it", bad)
		}
	}
}

// TestConsoleFirestoreBackupSchedules (#1461): Create backup schedule on a
// database's page, the Backup schedules tab, Edit retention and Delete
// backup schedule on a schedule's row, each read back through the official
// Firestore Admin client.
func TestConsoleFirestoreBackupSchedules(t *testing.T) {
	p := firestoreAdminFront(t)
	ctx := context.Background()
	c, err := p.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := "projects/p/databases/(default)"
	found := false
	for _, a := range p.DetailActions(ctx, "p", []string{"(default)"}) {
		found = found || a.ID == "createschedule"
	}
	if !found {
		t.Fatal("a database's page offers no Create backup schedule")
	}
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "createschedule", map[string]string{"recurrence": "weekly", "day": "SUNDAY", "retentionDays": "14"}); err != nil {
		t.Fatalf("Create backup schedule: %v", err)
	}
	if err := p.ActAt(ctx, "p", []string{"(default)"}, "createschedule", map[string]string{"recurrence": "daily", "retentionDays": "8"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a daily schedule keeping 8 days: %v, want the API's INVALID_ARGUMENT", err)
	}
	d, err := p.Detail(ctx, "p", []string{"(default)"})
	if err != nil || len(d.Sections) != 4 || len(d.Sections[2].Listing.Items) != 1 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	row := d.Sections[2].Listing.Items[0]
	if row.Fields["Recurrence"] != "Weekly, Sunday" || row.Fields["Retention"] != "336h0m0s" {
		t.Errorf("the schedule row = %+v", row.Fields)
	}
	if err := p.ActAt(ctx, "p", row.ActsOn, "editschedule", map[string]string{"retentionDays": "21"}); err != nil {
		t.Fatalf("Edit retention: %v", err)
	}
	s, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: db + "/backupSchedules/" + row.Name})
	if err != nil || s.GetRetention().AsDuration() != 21*24*time.Hour {
		t.Errorf("after Edit retention = %v, %v", s, err)
	}
	if acts := p.DetailActions(ctx, "p", row.ActsOn); len(acts) != 2 {
		t.Errorf("a schedule's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "p", row.ActsOn, "deleteschedule", nil); err != nil {
		t.Fatalf("Delete backup schedule: %v", err)
	}
	if _, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: db + "/backupSchedules/" + row.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete backup schedule: %v, want NotFound", err)
	}
}
