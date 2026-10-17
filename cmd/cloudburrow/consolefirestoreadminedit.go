package main

// Firestore database, index and field override writes (#1343).
//
// The Firestore databases screen (#1331) read a database's settings, its
// composite indexes and its field overrides through the official admin
// client against the Admin API the Firestore pod's call front serves. Every
// change here goes through the same client:
//
//   - a database's page: Edit database (UpdateDatabase, carrying the etag
//     the form was drawn from), Create index (CreateIndex) and Add field
//     override (UpdateField);
//   - an index's row on the Indexes tab: Delete index (DeleteIndex),
//     addressed as [database, "indexes", collection group, index ID];
//   - a field's row on the Field overrides tab: Edit field override and
//     Delete field override (UpdateField), addressed as
//     [database, "fields", collection group, field path].
//
// A refusal is the Admin API's own message.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The second segment of an index's and a field override's action path.
const (
	firestoreIndexesSegment = "indexes"
	firestoreFieldsSegment  = "fields"
)

// firestoreCollectionPattern is a collection ID: no slash, not empty. The
// slash is escaped, as the browser compiles a pattern with the v flag.
const firestoreCollectionPattern = `^[^\/]+$`

// databaseEditFields are Edit database's inputs, prefilled from db.
func databaseEditFields(db *adminpb.Database) []console.Field {
	return []console.Field{
		{Name: "concurrency", Label: "Concurrency mode", Type: "select", Default: db.ConcurrencyMode.String(),
			Options: []string{"PESSIMISTIC", "OPTIMISTIC", "OPTIMISTIC_WITH_ENTITY_GROUPS"},
			Help:    "How transactions on this database are controlled. The emulator runs every transaction the same way."},
		{Name: "pitr", Label: "Point-in-time recovery", Type: "checkbox",
			Default: strconv.FormatBool(db.PointInTimeRecoveryEnablement == adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED),
			Help:    "Stored and returned; the emulator keeps no earlier versions to read."},
		{Name: "appEngine", Label: "App Engine integration", Type: "checkbox",
			Default: strconv.FormatBool(db.AppEngineIntegrationMode == adminpb.Database_ENABLED)},
		{Name: "deleteProtection", Label: "Delete protection", Type: "checkbox",
			Default: strconv.FormatBool(db.DeleteProtectionState == adminpb.Database_DELETE_PROTECTION_ENABLED),
			Help:    "While it is on, Delete database is refused."},
		// The etag the form was drawn from: a database changed since is
		// refused ABORTED rather than overwritten.
		{Name: "etag", Type: "hidden", Default: db.Etag},
	}
}

// createIndexFields are Create index's inputs.
func createIndexFields() []console.Field {
	return []console.Field{
		{Name: "collectionGroup", Label: "Collection ID", Type: "text", Required: true, Pattern: firestoreCollectionPattern,
			Help: "The collection group the index is for."},
		{Name: "fields", Label: "Fields", Type: "textarea", Required: true,
			Help: "One field per line: its path, then ascending, descending or array-contains, such as \"city ascending\". " +
				"At least two fields; __name__ is appended as Firestore appends it."},
		{Name: "queryScope", Label: "Query scope", Type: "select", Default: "COLLECTION",
			Options: []string{"COLLECTION", "COLLECTION_GROUP"}},
	}
}

// parseIndexFields reads Create index's Fields lines.
func parseIndexFields(raw string) ([]*adminpb.Index_IndexField, error) {
	var out []*adminpb.Index_IndexField
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		i := strings.LastIndexAny(line, " \t")
		if i < 0 {
			return nil, fmt.Errorf("%q: give the field path, then ascending, descending or array-contains", line)
		}
		path, mode := strings.TrimSpace(line[:i]), strings.ToLower(strings.TrimSpace(line[i+1:]))
		f := &adminpb.Index_IndexField{FieldPath: path}
		switch mode {
		case "ascending", "asc":
			f.ValueMode = &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}
		case "descending", "desc":
			f.ValueMode = &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_DESCENDING}
		case "array-contains", "array":
			f.ValueMode = &adminpb.Index_IndexField_ArrayConfig_{ArrayConfig: adminpb.Index_IndexField_CONTAINS}
		default:
			return nil, fmt.Errorf("%q: %q is not ascending, descending or array-contains", line, mode)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("an index needs at least one field")
	}
	return out, nil
}

// singleFieldModes are the single-field indexes a field override's form
// offers, by checkbox name: each order and array-contains, at collection
// and collection group scope, as Google's console offers them.
var singleFieldModes = []struct {
	name, label string
	scope       adminpb.Index_QueryScope
	order       adminpb.Index_IndexField_Order
	array       bool
}{
	{"ascCollection", "Ascending, collection scope", adminpb.Index_COLLECTION, adminpb.Index_IndexField_ASCENDING, false},
	{"descCollection", "Descending, collection scope", adminpb.Index_COLLECTION, adminpb.Index_IndexField_DESCENDING, false},
	{"arrayCollection", "Array contains, collection scope", adminpb.Index_COLLECTION, 0, true},
	{"ascGroup", "Ascending, collection group scope", adminpb.Index_COLLECTION_GROUP, adminpb.Index_IndexField_ASCENDING, false},
	{"descGroup", "Descending, collection group scope", adminpb.Index_COLLECTION_GROUP, adminpb.Index_IndexField_DESCENDING, false},
	{"arrayGroup", "Array contains, collection group scope", adminpb.Index_COLLECTION_GROUP, 0, true},
}

// singleFieldMode is the checkbox an index of a field's own index settings
// is, or false when the form cannot express it.
func singleFieldMode(x *adminpb.Index) (string, bool) {
	if len(x.Fields) != 1 {
		return "", false
	}
	f := x.Fields[0]
	for _, m := range singleFieldModes {
		if m.scope == x.QueryScope && ((m.array && f.GetArrayConfig() == adminpb.Index_IndexField_CONTAINS) ||
			(!m.array && m.order != 0 && f.GetOrder() == m.order)) {
			return m.name, true
		}
	}
	return "", false
}

// fieldOverrideFields are a field override's inputs, prefilled from f (nil
// for none), or false when f holds index settings the form cannot express.
func fieldOverrideFields(f *adminpb.Field) ([]console.Field, bool) {
	checked := map[string]bool{}
	override := f != nil && f.GetIndexConfig() != nil && !f.GetIndexConfig().GetUsesAncestorConfig()
	if override {
		for _, x := range f.GetIndexConfig().GetIndexes() {
			name, ok := singleFieldMode(x)
			if !ok {
				return nil, false
			}
			checked[name] = true
		}
	}
	out := []console.Field{
		{Name: "ttl", Label: "TTL policy", Type: "checkbox", Default: strconv.FormatBool(f.GetTtlConfig() != nil),
			Section: "Time to live",
			Help:    "Firestore deletes a document once the timestamp in this field has passed. Stored and returned ACTIVE; the emulator deletes no expired document."},
		{Name: "override", Label: "Exempt from the automatic index settings", Type: "checkbox", Default: strconv.FormatBool(override),
			Section: "Single-field indexes",
			Help:    "Unchecked, the field keeps the database's automatic indexes and the boxes below are ignored. Checked, it has only the indexes checked below; none checked exempts it from indexing."},
	}
	for _, m := range singleFieldModes {
		out = append(out, console.Field{Name: m.name, Label: m.label, Type: "checkbox",
			Default: strconv.FormatBool(checked[m.name]), Section: "Single-field indexes"})
	}
	return out, true
}

// fieldOverrideUpdate is the UpdateField request a field override's form
// values make: both its index settings and its TTL policy, as the form
// shows both.
func fieldOverrideUpdate(name string, values map[string]string) *adminpb.UpdateFieldRequest {
	f := &adminpb.Field{Name: name}
	if values["ttl"] == "true" {
		f.TtlConfig = &adminpb.Field_TtlConfig{}
	}
	if values["override"] == "true" {
		f.IndexConfig = &adminpb.Field_IndexConfig{Indexes: []*adminpb.Index{}}
		for _, m := range singleFieldModes {
			if values[m.name] != "true" {
				continue
			}
			x := &adminpb.Index_IndexField{}
			if m.array {
				x.ValueMode = &adminpb.Index_IndexField_ArrayConfig_{ArrayConfig: adminpb.Index_IndexField_CONTAINS}
			} else {
				x.ValueMode = &adminpb.Index_IndexField_Order_{Order: m.order}
			}
			f.IndexConfig.Indexes = append(f.IndexConfig.Indexes, &adminpb.Index{QueryScope: m.scope, Fields: []*adminpb.Index_IndexField{x}})
		}
	}
	return &adminpb.UpdateFieldRequest{Field: f, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"index_config", "ttl_config"}}}
}

// hasOverride reports whether a field has settings of its own.
func hasOverride(f *adminpb.Field) bool {
	return f.GetTtlConfig() != nil || (f.GetIndexConfig() != nil && !f.GetIndexConfig().GetUsesAncestorConfig())
}

// indexActions are an index row's actions.
func indexActions() []console.Action {
	return []console.Action{{ID: "deleteindex", Label: "Delete index", Destructive: true}}
}

// fieldActions are a field override row's actions.
func fieldActions(f *adminpb.Field) []console.Action {
	var out []console.Action
	if fields, ok := fieldOverrideFields(f); ok {
		out = append(out, console.Action{ID: "editfield", Label: "Edit field override", Fields: fields})
	}
	return append(out, console.Action{ID: "deletefield", Label: "Delete field override", Destructive: true,
		Confirm: "The field's TTL policy and index settings are removed, and it takes the database's automatic index settings again."})
}

func (p firestoreDatabasesProvider) dbName(project, db string) string {
	return "projects/" + project + "/databases/" + db
}

// DetailActions implements console.PathActor.
func (p firestoreDatabasesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if project == "" || (len(path) != 1 && len(path) != 3 && len(path) != 4) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil
	}
	defer c.Close()
	db := p.dbName(project, path[0])
	if len(path) == 1 {
		d, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db})
		if err != nil {
			return nil
		}
		add, _ := fieldOverrideFields(nil)
		add = append([]console.Field{
			{Name: "collectionGroup", Label: "Collection ID", Type: "text", Required: true, Pattern: firestoreCollectionPattern},
			{Name: "field", Label: "Field path", Type: "text", Required: true},
		}, add...)
		return append([]console.Action{
			{ID: "editdatabase", Label: "Edit database", Fields: databaseEditFields(d)},
			{ID: "createindex", Label: "Create index", Fields: createIndexFields()},
			{ID: "addfield", Label: "Add field override", Fields: add},
			{ID: "createschedule", Label: "Create backup schedule", Fields: createScheduleFields()},
		}, firestoreTransferActions()...)
	}
	if len(path) == 3 {
		if path[1] == firestoreBackupsSegment {
			if _, err := backupNamed(ctx, c, db, path[2]); err != nil {
				return nil
			}
			return firestoreBackupActions()
		}
		if path[1] != firestoreSchedulesSegment {
			return nil
		}
		sch, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: db + "/backupSchedules/" + path[2]})
		if err != nil {
			return nil
		}
		return scheduleActions(sch)
	}
	switch path[1] {
	case firestoreIndexesSegment:
		if _, err := c.GetIndex(ctx, &adminpb.GetIndexRequest{Name: db + "/collectionGroups/" + path[2] + "/indexes/" + path[3]}); err != nil {
			return nil
		}
		return indexActions()
	case firestoreFieldsSegment:
		f, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: db + "/collectionGroups/" + path[2] + "/fields/" + path[3]})
		if err != nil || !hasOverride(f) {
			return nil
		}
		return fieldActions(f)
	}
	return nil
}

// ActAt implements console.PathActor.
func (p firestoreDatabasesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	db := p.dbName(project, path[0])
	if ok, err := actSchedule(ctx, c, db, path, action, values); ok {
		return err
	}
	if ok, err := actTransfer(ctx, c, db, path, action, values); ok {
		return err
	}
	switch {
	case action == "editdatabase" && len(path) == 1:
		mode, ok := adminpb.Database_ConcurrencyMode_value[values["concurrency"]]
		if !ok || mode == 0 {
			return fmt.Errorf("concurrency mode %q is not PESSIMISTIC, OPTIMISTIC or OPTIMISTIC_WITH_ENTITY_GROUPS", values["concurrency"])
		}
		d := &adminpb.Database{Name: db, Etag: values["etag"], ConcurrencyMode: adminpb.Database_ConcurrencyMode(mode),
			PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED,
			AppEngineIntegrationMode:      adminpb.Database_DISABLED,
			DeleteProtectionState:         adminpb.Database_DELETE_PROTECTION_DISABLED}
		if values["pitr"] == "true" {
			d.PointInTimeRecoveryEnablement = adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED
		}
		if values["appEngine"] == "true" {
			d.AppEngineIntegrationMode = adminpb.Database_ENABLED
		}
		if values["deleteProtection"] == "true" {
			d.DeleteProtectionState = adminpb.Database_DELETE_PROTECTION_ENABLED
		}
		op, err := c.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{Database: d, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{
			"concurrency_mode", "point_in_time_recovery_enablement", "app_engine_integration_mode", "delete_protection_state"}}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return err
	case action == "createindex" && len(path) == 1:
		cg := strings.TrimSpace(values["collectionGroup"])
		if cg == "" || strings.Contains(cg, "/") {
			return fmt.Errorf("collection ID %q is not a collection ID", cg)
		}
		fields, err := parseIndexFields(values["fields"])
		if err != nil {
			return err
		}
		scope := adminpb.Index_QueryScope(adminpb.Index_QueryScope_value[values["queryScope"]])
		op, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: db + "/collectionGroups/" + cg,
			Index: &adminpb.Index{QueryScope: scope, Fields: fields}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return err
	case action == "addfield" && len(path) == 1:
		cg, field := strings.TrimSpace(values["collectionGroup"]), strings.TrimSpace(values["field"])
		if cg == "" || strings.Contains(cg, "/") || field == "" {
			return errors.New("a collection ID and a field path are required")
		}
		name := db + "/collectionGroups/" + cg + "/fields/" + field
		cur, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: name})
		if err != nil {
			return err
		}
		if hasOverride(cur) {
			return fmt.Errorf("%s in %s already has an override: edit it on its row in Field overrides", field, cg)
		}
		return updateField(ctx, c, fieldOverrideUpdate(name, values))
	case len(path) == 4 && path[1] == firestoreIndexesSegment && action == "deleteindex":
		return c.DeleteIndex(ctx, &adminpb.DeleteIndexRequest{Name: db + "/collectionGroups/" + path[2] + "/indexes/" + path[3]})
	case len(path) == 4 && path[1] == firestoreFieldsSegment && action == "editfield":
		return updateField(ctx, c, fieldOverrideUpdate(db+"/collectionGroups/"+path[2]+"/fields/"+path[3], values))
	case len(path) == 4 && path[1] == firestoreFieldsSegment && action == "deletefield":
		return updateField(ctx, c, fieldOverrideUpdate(db+"/collectionGroups/"+path[2]+"/fields/"+path[3], nil))
	}
	return fmt.Errorf("%s is not available on this resource", action)
}

func updateField(ctx context.Context, c *admin.FirestoreAdminClient, req *adminpb.UpdateFieldRequest) error {
	op, err := c.UpdateField(ctx, req)
	if err == nil {
		_, err = op.Wait(ctx)
	}
	return err
}

var _ console.PathActor = firestoreDatabasesProvider{}
