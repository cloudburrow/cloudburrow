package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The console's reach into what the Bigtable front keeps (#1499): SQL in
// the table's query form; Snapshot table and Create schema bundle on a
// table's page; an instance's Backups, Logical views and Materialized views
// tabs, with Create logical view, Create materialized view and Undelete
// table (#1511), and Restore
// and Delete on each row; and a cluster's Edit memory layer.

const (
	bigtableBackupsSegment      = "backups"
	bigtableLogicalSegment      = "logicalViews"
	bigtableMaterializedSegment = "materializedViews"
)

// --- SQL ---

// bigtableSQLField is the query form's SQL editor.
func bigtableSQLField() console.Field {
	return console.Field{Name: "sql", Label: "SQL", Type: "textarea",
		Help: "Optional: GoogleSQL for Bigtable, such as SELECT _key, CAST(cf['col'] AS STRING) AS col FROM t WHERE STARTS_WITH(_key, 'a') LIMIT 10, " +
			"with functions, GROUP BY and aggregates, t(with_history => TRUE), or FROM a logical or materialized view. " +
			"When given, it runs instead of the row range."}
}

// bigtableSQL runs a query with the data client and lists its rows.
func (p bigtableProvider) bigtableSQL(ctx context.Context, project, sql string) (console.Listing, error) {
	c, err := p.newData(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: "cannot reach Bigtable: " + err.Error()}, nil
	}
	defer c.Close()
	ps, err := c.PrepareStatement(ctx, sql, nil)
	if err != nil {
		return console.Listing{}, err
	}
	bs, err := ps.Bind(nil)
	if err != nil {
		return console.Listing{}, err
	}
	out := console.Listing{Noun: "rows"}
	n := 0
	err = bs.Execute(ctx, func(r bigtable.ResultRow) bool {
		if out.Columns == nil {
			for i, col := range r.Metadata.Columns {
				if i == 0 {
					out.NameColumn = col.Name
					continue
				}
				out.Columns = append(out.Columns, col.Name)
			}
		}
		res := console.Resource{Fields: map[string]string{}}
		for i, col := range r.Metadata.Columns {
			var v any
			text := "NULL"
			if err := r.GetByIndex(i, &v); err == nil {
				text = sqlText(v)
			}
			if i == 0 {
				res.Name = text
			} else {
				res.Fields[col.Name] = text
			}
		}
		out.Items = append(out.Items, res)
		n++
		return n < detailLimit
	})
	if err != nil {
		return console.Listing{}, err
	}
	out.Total = len(out.Items)
	return out, nil
}

// sqlText is how a SQL value reads on the console.
func sqlText(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return bigtableCellValue(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case map[string][]byte:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			name := k
			if b, err := base64.StdEncoding.DecodeString(k); err == nil {
				name = string(b)
			}
			parts = append(parts, name+": "+summarise(bigtableCellValue(x[k])))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

// --- table actions ---

func bigtableTableExtraActions() []console.Action {
	return []console.Action{
		{ID: "snapshot", Label: "Snapshot table", Fields: []console.Field{
			{Name: "cluster", Label: "Cluster ID", Type: "text", Required: true, Default: bigtableBackupCluster},
			{Name: "snapshot", Label: "Snapshot ID", Type: "text", Required: true}}},
		{ID: "schemabundle", Label: "Create schema bundle", Fields: []console.Field{
			{Name: "bundle", Label: "Schema bundle ID", Type: "text", Required: true},
			{Name: "descriptors", Label: "Descriptor set (base64)", Type: "textarea", Required: true,
				Help: "A serialized google.protobuf.FileDescriptorSet, base64-encoded, as protoc --descriptor_set_out writes it."}}},
	}
}

// actTableExtra performs a table page's extra actions; ok is false for
// any other action.
func (p bigtableProvider) actTableExtra(ctx context.Context, project, table, action string, values map[string]string) (bool, error) {
	switch action {
	case "snapshot":
		return true, p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.SnapshotTable(ctx, table, strings.TrimSpace(values["cluster"]), strings.TrimSpace(values["snapshot"]), bigtable.DefaultSnapshotDuration)
		})
	case "schemabundle":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(values["descriptors"]))
		if err != nil {
			return true, fmt.Errorf("the descriptor set is not base64: %v", err)
		}
		return true, p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: table, SchemaBundleID: strings.TrimSpace(values["bundle"]),
				ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}})
		})
	}
	return false, nil
}

// --- the instance page ---

func (p bigtableInstancesProvider) backupsListing(ctx context.Context, project, instance string) console.Listing {
	list := console.Listing{Columns: []string{"Cluster", "Source table", "Expires", "Size"}, NameColumn: "Backup ID", Noun: "backups"}
	admin, err := bigtable.NewAdminClient(ctx, project, instance, localOpts(p.endpoint)...)
	if err != nil {
		list.Unavailable = "cannot reach the Bigtable admin API: " + err.Error()
		return list
	}
	defer admin.Close()
	it := admin.TableAdminClientV2().ListBackups(ctx, &btapb.ListBackupsRequest{
		Parent: "projects/" + project + "/instances/" + instance + "/clusters/-"})
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			list.Unavailable = "listing backups: " + err.Error()
			return list
		}
		// projects/{p}/instances/{i}/clusters/{c}/backups/{b}
		parts := strings.Split(b.GetName(), "/")
		if len(parts) != 8 {
			continue
		}
		cluster, id := parts[5], parts[7]
		list.Items = append(list.Items, console.Resource{Name: id,
			Fields: map[string]string{"Cluster": cluster, "Source table": lastSegment(b.GetSourceTable()),
				"Expires": b.GetExpireTime().AsTime().UTC().Format("2006-01-02 15:04 MST"), "Size": strconv.FormatInt(b.GetSizeBytes(), 10) + " bytes"},
			Actions: backupActions(), ActsOn: []string{instance, bigtableBackupsSegment, cluster + ":" + id}})
	}
	list.Total = len(list.Items)
	list.Note = "A backup is a copy of its table's rows, kept by the Bigtable front; it outlives the table, and Restore writes it into a new table."
	return list
}

func backupActions() []console.Action {
	return []console.Action{
		{ID: "restorebackup", Label: "Restore", Fields: []console.Field{{Name: "table", Label: "New table ID", Type: "text", Required: true}}},
		{ID: "editexpiry", Label: "Edit expiry", Fields: []console.Field{{Name: "days", Label: "Expire after (days)", Type: "text", Required: true, Default: "7", Help: "From now; 1 to 90."}}},
		{ID: "copybackup", Label: "Copy", Fields: []console.Field{
			{Name: "cluster", Label: "Destination cluster ID", Type: "text", Required: true},
			{Name: "backup", Label: "New backup ID", Type: "text", Required: true},
			{Name: "days", Label: "Expire after (days)", Type: "text", Default: "7"}}},
		{ID: "deletebackup", Label: "Delete backup", Destructive: true},
	}
}

func sqlViewActions(materialized bool) []console.Action {
	edit := console.Action{ID: "editview", Label: "Edit query", Fields: []console.Field{{Name: "query", Label: "Query", Type: "textarea", Required: true}}}
	if materialized {
		// Google does not change a materialized view's query.
		edit = console.Action{ID: "editview", Label: "Edit deletion protection", Fields: []console.Field{{Name: "protected", Label: "Deletion protection", Type: "checkbox"}}}
	}
	return []console.Action{edit, {ID: "deleteview", Label: "Delete view", Destructive: true}}
}

func (p bigtableInstancesProvider) sqlViewsListing(ctx context.Context, c *bigtable.InstanceAdminClient, project, instance string, materialized bool) console.Listing {
	noun, segment := "logical views", bigtableLogicalSegment
	if materialized {
		noun, segment = "materialized views", bigtableMaterializedSegment
	}
	list := console.Listing{Columns: []string{"Query", "Rows", "Deletion protection"}, NameColumn: "View ID", Noun: noun}
	type view struct {
		id, query string
		protected bool
	}
	var views []view
	if materialized {
		mvs, err := c.MaterializedViews(ctx, instance)
		if err != nil {
			list.Unavailable = "listing materialized views: " + err.Error()
			return list
		}
		for _, v := range mvs {
			views = append(views, view{v.MaterializedViewID, v.Query, v.DeletionProtection == bigtable.Protected})
		}
	} else {
		lvs, err := c.LogicalViews(ctx, instance)
		if err != nil {
			list.Unavailable = "listing logical views: " + err.Error()
			return list
		}
		for _, v := range lvs {
			views = append(views, view{v.LogicalViewID, v.Query, v.DeletionProtection == bigtable.Protected})
		}
	}
	for _, v := range views {
		list.Items = append(list.Items, console.Resource{Name: lastSegment(v.id),
			Fields:  map[string]string{"Query": v.query, "Rows": p.viewRows(ctx, project, instance, lastSegment(v.id)), "Deletion protection": strconv.FormatBool(v.protected)},
			Actions: sqlViewActions(materialized), ActsOn: []string{instance, segment, lastSegment(v.id)}})
	}
	list.Total = len(list.Items)
	list.Note = "Each view runs its query (#1511): Rows is how many rows it returns now. Read them from any table's query form " +
		"in this instance, with SQL such as SELECT * FROM `view-id`."
	return list
}

// viewRows runs the view, counting the rows it returns now.
func (p bigtableInstancesProvider) viewRows(ctx context.Context, project, instance, view string) string {
	c, err := bigtable.NewClient(ctx, project, instance, localOpts(p.endpoint)...)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	defer c.Close()
	ps, err := c.PrepareStatement(ctx, "SELECT COUNT(*) AS n FROM `"+view+"`", nil)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	bs, err := ps.Bind(nil)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	var n int64
	err = bs.Execute(ctx, func(r bigtable.ResultRow) bool { _ = r.GetByIndex(0, &n); return false })
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return strconv.FormatInt(n, 10)
}

func sqlViewCreateFields() []console.Field {
	return []console.Field{
		{Name: "view", Label: "View ID", Type: "text", Required: true},
		{Name: "query", Label: "Query", Type: "textarea", Required: true},
	}
}

func instanceExtraActions() []console.Action {
	return []console.Action{
		{ID: "createlogical", Label: "Create logical view", Fields: sqlViewCreateFields()},
		{ID: "creatematerialized", Label: "Create materialized view", Fields: sqlViewCreateFields()},
		{ID: "undeletetable", Label: "Undelete table", Fields: []console.Field{{Name: "table", Label: "Table ID", Type: "text", Required: true,
			Help: "A table of this instance deleted in the last seven days (#1511)."}}},
	}
}

func memoryLayerAction(enabled bool) console.Action {
	return console.Action{ID: "memorylayer", Label: "Edit memory layer", Fields: []console.Field{{
		Name: "enabled", Label: "Memory layer", Type: "checkbox", Default: strconv.FormatBool(enabled),
		Help: "Stored as Google keeps it; the emulator serves every read from memory either way."}}}
}

// memoryLayerEnabled reads whether a cluster's memory layer is on.
func memoryLayerEnabled(ctx context.Context, c *bigtable.InstanceAdminClient, name string) bool {
	l, err := c.InstanceAdminClientV2().GetMemoryLayer(ctx, &btapb.GetMemoryLayerRequest{Name: name + "/memoryLayer"})
	return err == nil && l.GetMemoryConfig() != nil
}

// actInstanceExtra performs the extra instance page actions; ok is false
// for any other.
func (p bigtableInstancesProvider) actInstanceExtra(ctx context.Context, c *bigtable.InstanceAdminClient, project string, path []string, action string, values map[string]string) (bool, error) {
	if ok, err := p.actTableExtrasRow(ctx, project, path, action, values); ok {
		return true, err
	}
	inst := path[0]
	name := "projects/" + project + "/instances/" + inst
	switch {
	case len(path) == 1 && action == "createlogical":
		return true, c.CreateLogicalView(ctx, inst, &bigtable.LogicalViewInfo{LogicalViewID: strings.TrimSpace(values["view"]), Query: values["query"]})
	case len(path) == 1 && action == "undeletetable":
		admin, err := bigtable.NewAdminClient(ctx, project, inst, localOpts(p.endpoint)...)
		if err != nil {
			return true, err
		}
		defer admin.Close()
		op, err := admin.TableAdminClientV2().UndeleteTable(ctx, &btapb.UndeleteTableRequest{Name: name + "/tables/" + strings.TrimSpace(values["table"])})
		if err != nil {
			return true, err
		}
		_, err = op.Wait(ctx)
		return true, err
	case len(path) == 1 && action == "creatematerialized":
		return true, c.CreateMaterializedView(ctx, inst, &bigtable.MaterializedViewInfo{MaterializedViewID: strings.TrimSpace(values["view"]), Query: values["query"]})
	case len(path) == 3 && path[1] == bigtableLogicalSegment && action == "editview":
		return true, c.UpdateLogicalView(ctx, inst, bigtable.LogicalViewInfo{LogicalViewID: path[2], Query: values["query"]})
	case len(path) == 3 && path[1] == bigtableMaterializedSegment && action == "editview":
		protection := bigtable.Unprotected
		if values["protected"] == "true" {
			protection = bigtable.Protected
		}
		return true, c.UpdateMaterializedView(ctx, inst, bigtable.MaterializedViewInfo{MaterializedViewID: path[2], DeletionProtection: protection})
	case len(path) == 3 && path[1] == bigtableLogicalSegment && action == "deleteview":
		return true, c.DeleteLogicalView(ctx, inst, path[2])
	case len(path) == 3 && path[1] == bigtableMaterializedSegment && action == "deleteview":
		return true, c.DeleteMaterializedView(ctx, inst, path[2])
	case len(path) == 3 && path[1] == bigtableBackupsSegment && (action == "restorebackup" || action == "deletebackup"):
		cluster, backup, ok := strings.Cut(path[2], ":")
		if !ok {
			return true, fmt.Errorf("%q is not cluster:backup", path[2])
		}
		admin, err := bigtable.NewAdminClient(ctx, project, inst, localOpts(p.endpoint)...)
		if err != nil {
			return true, err
		}
		defer admin.Close()
		if action == "deletebackup" {
			return true, admin.DeleteBackup(ctx, cluster, backup)
		}
		table := strings.TrimSpace(values["table"])
		if table == "" {
			return true, errors.New("a new table ID is required")
		}
		return true, admin.RestoreTable(ctx, table, cluster, backup)
	case len(path) == 3 && path[1] == bigtableClustersSegment && action == "memorylayer":
		req := &btapb.UpdateMemoryLayerRequest{MemoryLayer: &btapb.MemoryLayer{Name: name + "/clusters/" + path[2] + "/memoryLayer"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"memory_config"}}}
		if values["enabled"] == "true" {
			req.MemoryLayer.MemoryConfig = &btapb.MemoryLayer_MemoryConfig{}
		}
		op, err := c.InstanceAdminClientV2().UpdateMemoryLayer(ctx, req)
		if err != nil {
			return true, err
		}
		_, err = op.Wait(ctx)
		return true, err
	}
	return false, nil
}

const (
	bigtableSnapshotsSegment = "snapshots"
	bigtableAuthViewsSegment = "authorizedViews"
	bigtableBundlesSegment   = "schemaBundles"
)

func snapshotActions() []console.Action {
	return []console.Action{
		{ID: "tablefromsnapshot", Label: "Create table", Fields: []console.Field{{Name: "table", Label: "New table ID", Type: "text", Required: true}}},
		{ID: "deletesnapshot", Label: "Delete snapshot", Destructive: true},
	}
}

func authViewRowActions() []console.Action {
	return []console.Action{
		{ID: "editauthview", Label: "Edit authorized view", Fields: []console.Field{
			{Name: "prefixes", Label: "Row key prefixes", Type: "text", Help: "Comma-separated; empty for every row."},
			{Name: "columns", Label: "Columns", Type: "text", Required: true, Help: "Comma-separated family:qualifier, family:prefix* or family:*."}}},
		{ID: "deleteauthview", Label: "Delete authorized view", Destructive: true},
	}
}

func bundleRowActions() []console.Action {
	return []console.Action{
		{ID: "editbundle", Label: "Edit schema bundle", Fields: []console.Field{
			{Name: "descriptors", Label: "Descriptor set (base64)", Type: "textarea", Required: true}}},
		{ID: "deletebundle", Label: "Delete schema bundle", Destructive: true},
	}
}

// subsetText is an authorized view's subset in the console's words.
func subsetText(sv *btapb.AuthorizedView_SubsetView) (prefixes, columns string) {
	var ps, cs []string
	for _, p := range sv.GetRowPrefixes() {
		ps = append(ps, string(p))
	}
	fams := make([]string, 0, len(sv.GetFamilySubsets()))
	for f := range sv.GetFamilySubsets() {
		fams = append(fams, f)
	}
	sort.Strings(fams)
	for _, f := range fams {
		fs := sv.GetFamilySubsets()[f]
		for _, q := range fs.GetQualifiers() {
			cs = append(cs, f+":"+string(q))
		}
		for _, q := range fs.GetQualifierPrefixes() {
			cs = append(cs, f+":"+string(q)+"*")
		}
	}
	return strings.Join(ps, ", "), strings.Join(cs, ", ")
}

// tableExtrasListings are an instance's Snapshots, Authorized views and
// Schema bundles tabs, read with the generated table admin client.
func (p bigtableInstancesProvider) tableExtrasListings(ctx context.Context, project, instance string) []console.Section {
	snaps := console.Listing{Columns: []string{"Cluster", "Source table", "Deletes"}, NameColumn: "Snapshot ID", Noun: "snapshots"}
	views := console.Listing{Columns: []string{"Table", "Row prefixes", "Columns"}, NameColumn: "Authorized view ID", Noun: "authorized views"}
	bundles := console.Listing{Columns: []string{"Table", "Size"}, NameColumn: "Schema bundle ID", Noun: "schema bundles"}
	sections := func() []console.Section {
		snaps.Total, views.Total, bundles.Total = len(snaps.Items), len(views.Items), len(bundles.Items)
		return []console.Section{
			{ID: "snapshots", Label: "Snapshots", Listing: snaps},
			{ID: "authorizedviews", Label: "Authorized views", Listing: views},
			{ID: "schemabundles", Label: "Schema bundles", Listing: bundles},
		}
	}
	admin, err := bigtable.NewAdminClient(ctx, project, instance, localOpts(p.endpoint)...)
	if err != nil {
		snaps.Unavailable, views.Unavailable, bundles.Unavailable = err.Error(), err.Error(), err.Error()
		return sections()
	}
	defer admin.Close()
	a := admin.TableAdminClientV2()
	parent := "projects/" + project + "/instances/" + instance
	sit := a.ListSnapshots(ctx, &btapb.ListSnapshotsRequest{Parent: parent + "/clusters/-"})
	for {
		s, err := sit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			snaps.Unavailable = "listing snapshots: " + err.Error()
			break
		}
		parts := strings.Split(s.GetName(), "/")
		if len(parts) != 8 {
			continue
		}
		snaps.Items = append(snaps.Items, console.Resource{Name: parts[7],
			Fields:  map[string]string{"Cluster": parts[5], "Source table": lastSegment(s.GetSourceTable().GetName()), "Deletes": s.GetDeleteTime().AsTime().UTC().Format("2006-01-02 15:04 MST")},
			Actions: snapshotActions(), ActsOn: []string{instance, bigtableSnapshotsSegment, parts[5] + ":" + parts[7]}})
	}
	tables, err := admin.Tables(ctx)
	if err != nil {
		views.Unavailable, bundles.Unavailable = "listing tables: "+err.Error(), "listing tables: "+err.Error()
		return sections()
	}
	sort.Strings(tables)
	for _, t := range tables {
		vit := a.ListAuthorizedViews(ctx, &btapb.ListAuthorizedViewsRequest{Parent: parent + "/tables/" + t, View: btapb.AuthorizedView_FULL})
		for {
			v, err := vit.Next()
			if err != nil {
				break
			}
			ps, cs := subsetText(v.GetSubsetView())
			views.Items = append(views.Items, console.Resource{Name: lastSegment(v.GetName()),
				Fields:  map[string]string{"Table": t, "Row prefixes": ps, "Columns": cs},
				Actions: authViewRowActions(), ActsOn: []string{instance, bigtableAuthViewsSegment, t + ":" + lastSegment(v.GetName())}})
		}
		bit := a.ListSchemaBundles(ctx, &btapb.ListSchemaBundlesRequest{Parent: parent + "/tables/" + t})
		for {
			b, err := bit.Next()
			if err != nil {
				break
			}
			bundles.Items = append(bundles.Items, console.Resource{Name: lastSegment(b.GetName()),
				Fields:  map[string]string{"Table": t, "Size": strconv.Itoa(len(b.GetProtoSchema().GetProtoDescriptors())) + " bytes"},
				Actions: bundleRowActions(), ActsOn: []string{instance, bigtableBundlesSegment, t + ":" + lastSegment(b.GetName())}})
		}
	}
	return sections()
}

// actTableExtrasRow performs the Snapshots, Authorized views and Schema
// bundles rows' actions, and the Backups rows' Edit expiry and Copy.
func (p bigtableInstancesProvider) actTableExtrasRow(ctx context.Context, project string, path []string, action string, values map[string]string) (bool, error) {
	if len(path) != 3 {
		return false, nil
	}
	owner, id, ok := strings.Cut(path[2], ":")
	switch path[1] {
	case bigtableSnapshotsSegment, bigtableAuthViewsSegment, bigtableBundlesSegment, bigtableBackupsSegment:
	default:
		return false, nil
	}
	if !ok {
		return true, fmt.Errorf("%q is not owner:id", path[2])
	}
	admin, err := bigtable.NewAdminClient(ctx, project, path[0], localOpts(p.endpoint)...)
	if err != nil {
		return true, err
	}
	defer admin.Close()
	switch {
	case path[1] == bigtableSnapshotsSegment && action == "tablefromsnapshot":
		return true, admin.CreateTableFromSnapshot(ctx, strings.TrimSpace(values["table"]), owner, id)
	case path[1] == bigtableSnapshotsSegment && action == "deletesnapshot":
		return true, admin.DeleteSnapshot(ctx, owner, id)
	case path[1] == bigtableAuthViewsSegment && action == "editauthview":
		sv, err := parseSubsetView(values["prefixes"], values["columns"])
		if err != nil {
			return true, err
		}
		return true, admin.UpdateAuthorizedView(ctx, bigtable.UpdateAuthorizedViewConf{AuthorizedViewConf: bigtable.AuthorizedViewConf{TableID: owner, AuthorizedViewID: id, AuthorizedView: sv}})
	case path[1] == bigtableAuthViewsSegment && action == "deleteauthview":
		return true, admin.DeleteAuthorizedView(ctx, owner, id)
	case path[1] == bigtableBundlesSegment && action == "editbundle":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(values["descriptors"]))
		if err != nil {
			return true, fmt.Errorf("the descriptor set is not base64: %v", err)
		}
		return true, admin.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{TableID: owner, SchemaBundleID: id,
			ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}}})
	case path[1] == bigtableBundlesSegment && action == "deletebundle":
		return true, admin.DeleteSchemaBundle(ctx, owner, id)
	case path[1] == bigtableBackupsSegment && action == "editexpiry":
		days, err := strconv.Atoi(strings.TrimSpace(values["days"]))
		if err != nil || days < 1 || days > 90 {
			return true, fmt.Errorf("expire after %q days: want a whole number of days from 1 to 90", values["days"])
		}
		return true, admin.UpdateBackup(ctx, owner, id, time.Now().Add(time.Duration(days)*24*time.Hour))
	case path[1] == bigtableBackupsSegment && action == "copybackup":
		days, err := strconv.Atoi(strings.TrimSpace(values["days"]))
		if err != nil || days < 1 || days > 90 {
			return true, fmt.Errorf("expire after %q days: want a whole number of days from 1 to 90", values["days"])
		}
		return true, admin.CopyBackup(ctx, owner, id, project, path[0], strings.TrimSpace(values["cluster"]), strings.TrimSpace(values["backup"]),
			time.Now().Add(time.Duration(days)*24*time.Hour))
	}
	return false, nil
}
