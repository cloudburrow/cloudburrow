package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The console's reach into what the Bigtable front keeps (#1499): SQL in
// the table's query form; Snapshot table and Create schema bundle on a
// table's page; an instance's Backups, Logical views and Materialized views
// tabs, with Create logical view and Create materialized view, and Restore
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
		Help: "Optional: GoogleSQL for Bigtable, such as SELECT _key, CAST(cf['col'] AS STRING) AS col FROM t WHERE STARTS_WITH(_key, 'a') LIMIT 10. " +
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
		{ID: "deletebackup", Label: "Delete backup", Destructive: true},
	}
}

func sqlViewActions() []console.Action {
	return []console.Action{{ID: "deleteview", Label: "Delete view", Destructive: true}}
}

func (p bigtableInstancesProvider) sqlViewsListing(ctx context.Context, c *bigtable.InstanceAdminClient, instance string, materialized bool) console.Listing {
	noun, segment := "logical views", bigtableLogicalSegment
	if materialized {
		noun, segment = "materialized views", bigtableMaterializedSegment
	}
	list := console.Listing{Columns: []string{"Query", "Deletion protection"}, NameColumn: "View ID", Noun: noun}
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
			Fields:  map[string]string{"Query": v.query, "Deletion protection": strconv.FormatBool(v.protected)},
			Actions: sqlViewActions(), ActsOn: []string{instance, segment, lastSegment(v.id)}})
	}
	list.Total = len(list.Items)
	list.Note = "The query is kept as written; the emulator does not run views, so a view is never read."
	return list
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
	inst := path[0]
	name := "projects/" + project + "/instances/" + inst
	switch {
	case len(path) == 1 && action == "createlogical":
		return true, c.CreateLogicalView(ctx, inst, &bigtable.LogicalViewInfo{LogicalViewID: strings.TrimSpace(values["view"]), Query: values["query"]})
	case len(path) == 1 && action == "creatematerialized":
		return true, c.CreateMaterializedView(ctx, inst, &bigtable.MaterializedViewInfo{MaterializedViewID: strings.TrimSpace(values["view"]), Query: values["query"]})
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
