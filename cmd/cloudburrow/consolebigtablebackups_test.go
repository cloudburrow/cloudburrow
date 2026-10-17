package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestConsoleBigtableBackupsAndViews (#1499): a table page's Create backup,
// Restore backup to a new table and Create authorized view, over the
// Bigtable front, read back through the official clients.
func TestConsoleBigtableBackupsAndViews(t *testing.T) {
	srv, addr := bigtablePickerConsole(t)
	ctx := context.Background()
	admin, err := bigtable.NewAdminClient(ctx, "demo", bigtableInstance, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, "demo", bigtableInstance, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := admin.CreateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "orders", "cf"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"eu#1", "us#1"} {
		m := bigtable.NewMutation()
		m.Set("cf", "total", bigtable.Now(), []byte("9"))
		if err := client.Open("orders").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	act := func(action string, values map[string]string) (int, string) {
		return pickerDo(t, srv, http.MethodPost, "/api/actions/bigtable?project=demo",
			map[string]any{"Path": []string{"orders"}, "Action": action, "Values": values})
	}
	if code, out := act("backup", map[string]string{"cluster": "c1", "backup": "nightly", "days": "7"}); code != http.StatusOK {
		t.Fatalf("Create backup = %d: %s", code, out)
	}
	if info, err := admin.BackupInfo(ctx, "c1", "nightly"); err != nil || info.SourceTable != "orders" {
		t.Errorf("BackupInfo = %+v, %v", info, err)
	}
	if code, out := act("backup", map[string]string{"cluster": "c1", "backup": "x", "days": "400"}); code != http.StatusBadRequest {
		t.Errorf("a 400-day backup = %d: %s", code, out)
	}
	if code, out := act("restore", map[string]string{"cluster": "c1", "backup": "nightly", "table": "orders-restored"}); code != http.StatusOK {
		t.Fatalf("Restore backup = %d: %s", code, out)
	}
	n := 0
	if err := client.Open("orders-restored").ReadRows(ctx, bigtable.InfiniteRange(""), func(bigtable.Row) bool { n++; return true }); err != nil || n != 2 {
		t.Errorf("the restored table has %d rows, %v; want 2", n, err)
	}
	if code, out := act("authview", map[string]string{"view": "eu", "prefixes": "eu#", "columns": "cf:*"}); code != http.StatusOK {
		t.Fatalf("Create authorized view = %d: %s", code, out)
	}
	var keys []string
	if err := client.OpenAuthorizedView("orders", "eu").ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil || len(keys) != 1 || keys[0] != "eu#1" {
		t.Errorf("rows through the view = %v, %v; want [eu#1]", keys, err)
	}
	if code, out := act("authview", map[string]string{"view": "bad", "columns": "nofamily"}); code != http.StatusBadRequest {
		t.Errorf("a column with no family = %d: %s", code, out)
	}
}

// TestConsoleBigtableSQLSnapshotsBundlesAndViews (#1499): the query form's
// SQL; a table page's Snapshot table and Create schema bundle; an
// instance's Backups tab with Restore and Delete, Create logical and
// materialized view with their tabs and Delete; and a cluster's Edit memory
// layer, each read back through the official clients.
func TestConsoleBigtableSQLSnapshotsBundlesAndViews(t *testing.T) {
	srv, addr := bigtablePickerConsole(t)
	ctx := context.Background()
	iac, err := bigtable.NewInstanceAdminClient(ctx, "demo", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "shop-inst", DisplayName: "Shop",
		ClusterId: "shop-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	admin, err := bigtable.NewAdminClient(ctx, "demo", "shop-inst", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, "demo", "shop-inst", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := admin.CreateTable(ctx, "items"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "items", "cf"); err != nil {
		t.Fatal(err)
	}
	m := bigtable.NewMutation()
	m.Set("cf", "name", bigtable.Now(), []byte("lamp"))
	if err := client.Open("items").Apply(ctx, "i#1", m); err != nil {
		t.Fatal(err)
	}
	const scoped = "project=demo&instance=shop-inst"
	act := func(path []string, action string, values map[string]string) (int, string) {
		return pickerDo(t, srv, http.MethodPost, "/api/actions/bigtable?"+scoped, map[string]any{"Path": path, "Action": action, "Values": values})
	}
	if code, out := pickerDo(t, srv, http.MethodPost, "/api/query/bigtable?"+scoped, map[string]any{"Path": []string{"items"},
		"Values": map[string]string{"sql": "SELECT _key, CAST(cf['name'] AS STRING) AS name FROM items"}}); code != http.StatusOK || !strings.Contains(out, "lamp") || !strings.Contains(out, "i#1") {
		t.Errorf("SQL in the query form = %d: %s", code, out)
	}
	if code, out := act([]string{"items"}, "snapshot", map[string]string{"cluster": "shop-c1", "snapshot": "s1"}); code != http.StatusOK {
		t.Errorf("Snapshot table = %d: %s", code, out)
	}
	if s, err := admin.SnapshotInfo(ctx, "shop-c1", "s1"); err != nil || s.SourceTable != "items" {
		t.Errorf("SnapshotInfo = %+v, %v", s, err)
	}
	fd := protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)
	set, _ := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
	if code, out := act([]string{"items"}, "schemabundle", map[string]string{"bundle": "b1", "descriptors": base64.StdEncoding.EncodeToString(set)}); code != http.StatusOK {
		t.Errorf("Create schema bundle = %d: %s", code, out)
	}
	if names, err := admin.SchemaBundles(ctx, "items"); err != nil || len(names) != 1 {
		t.Errorf("SchemaBundles = %v, %v", names, err)
	}
	if err := admin.CreateBackup(ctx, "items", "shop-c1", "bk", time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	iact := func(path []string, action string, values map[string]string) (int, string) {
		return pickerDo(t, srv, http.MethodPost, "/api/actions/bigtable-instances?project=demo", map[string]any{"Path": path, "Action": action, "Values": values})
	}
	detail := func() string {
		_, out := pickerDo(t, srv, http.MethodGet, "/api/detail/bigtable-instances?project=demo&name=shop-inst", nil)
		return out
	}
	if out := detail(); !strings.Contains(out, `"bk"`) {
		t.Errorf("the Backups tab lists no bk: %s", out)
	}
	if code, out := iact([]string{"shop-inst", "backups", "shop-c1:bk"}, "restorebackup", map[string]string{"table": "items2"}); code != http.StatusOK {
		t.Errorf("Restore = %d: %s", code, out)
	}
	if _, err := admin.TableInfo(ctx, "items2"); err != nil {
		t.Errorf("the restored table: %v", err)
	}
	if code, out := iact([]string{"shop-inst", "backups", "shop-c1:bk"}, "editexpiry", map[string]string{"days": "30"}); code != http.StatusOK {
		t.Errorf("Edit expiry = %d: %s", code, out)
	}
	if b, err := admin.BackupInfo(ctx, "shop-c1", "bk"); err != nil || time.Until(b.ExpireTime) < 29*24*time.Hour {
		t.Errorf("BackupInfo after Edit expiry = %+v, %v", b, err)
	}
	if code, out := iact([]string{"shop-inst", "backups", "shop-c1:bk"}, "copybackup", map[string]string{"cluster": "shop-c1", "backup": "bk2", "days": "7"}); code != http.StatusOK {
		t.Errorf("Copy = %d: %s", code, out)
	}
	if _, err := admin.BackupInfo(ctx, "shop-c1", "bk2"); err != nil {
		t.Errorf("the copy: %v", err)
	}
	// Snapshots, authorized views and schema bundles are tabs of the
	// instance, each row with its actions.
	if err := admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{TableID: "items", AuthorizedViewID: "av",
		AuthorizedView: &bigtable.SubsetViewConf{RowPrefixes: [][]byte{[]byte("i#")}, FamilySubsets: map[string]bigtable.FamilySubset{"cf": {QualifierPrefixes: [][]byte{[]byte("")}}}}}); err != nil {
		t.Fatal(err)
	}
	if out := detail(); !strings.Contains(out, `"s1"`) || !strings.Contains(out, `"av"`) || !strings.Contains(out, `"b1"`) {
		t.Errorf("the Snapshots, Authorized views and Schema bundles tabs: %s", out)
	}
	if code, out := iact([]string{"shop-inst", "snapshots", "shop-c1:s1"}, "tablefromsnapshot", map[string]string{"table": "items3"}); code != http.StatusOK {
		t.Errorf("Create table from the snapshot = %d: %s", code, out)
	}
	if _, err := admin.TableInfo(ctx, "items3"); err != nil {
		t.Errorf("the table from the snapshot: %v", err)
	}
	if code, out := iact([]string{"shop-inst", "snapshots", "shop-c1:s1"}, "deletesnapshot", nil); code != http.StatusOK {
		t.Errorf("Delete snapshot = %d: %s", code, out)
	}
	if code, out := iact([]string{"shop-inst", "authorizedViews", "items:av"}, "editauthview", map[string]string{"prefixes": "j#", "columns": "cf:*"}); code != http.StatusOK {
		t.Errorf("Edit authorized view = %d: %s", code, out)
	}
	if info, err := admin.AuthorizedViewInfo(ctx, "items", "av"); err != nil || string(info.AuthorizedView.(*bigtable.SubsetViewInfo).RowPrefixes[0]) != "j#" {
		t.Errorf("AuthorizedViewInfo after Edit = %+v, %v", info, err)
	}
	if code, out := iact([]string{"shop-inst", "authorizedViews", "items:av"}, "deleteauthview", nil); code != http.StatusOK {
		t.Errorf("Delete authorized view = %d: %s", code, out)
	}
	if code, out := iact([]string{"shop-inst", "schemaBundles", "items:b1"}, "editbundle", map[string]string{"descriptors": base64.StdEncoding.EncodeToString(set)}); code != http.StatusOK {
		t.Errorf("Edit schema bundle = %d: %s", code, out)
	}
	if code, out := iact([]string{"shop-inst", "schemaBundles", "items:b1"}, "deletebundle", nil); code != http.StatusOK {
		t.Errorf("Delete schema bundle = %d: %s", code, out)
	}
	if names, err := admin.SchemaBundles(ctx, "items"); err != nil || len(names) != 0 {
		t.Errorf("SchemaBundles after Delete = %v, %v", names, err)
	}
	if code, out := iact([]string{"shop-inst", "backups", "shop-c1:bk"}, "deletebackup", nil); code != http.StatusOK {
		t.Errorf("Delete backup = %d: %s", code, out)
	}
	for _, kind := range []string{"logical", "materialized"} {
		if code, out := iact([]string{"shop-inst"}, "create"+kind, map[string]string{"view": kind + "-v", "query": "SELECT _key FROM items"}); code != http.StatusOK {
			t.Errorf("Create %s view = %d: %s", kind, code, out)
		}
	}
	if lv, err := iac.LogicalViewInfo(ctx, "shop-inst", "logical-v"); err != nil || lv.Query != "SELECT _key FROM items" {
		t.Errorf("LogicalViewInfo = %+v, %v", lv, err)
	}
	if code, out := iact([]string{"shop-inst", "logicalViews", "logical-v"}, "editview", map[string]string{"query": "SELECT _key FROM items2"}); code != http.StatusOK {
		t.Errorf("Edit query = %d: %s", code, out)
	}
	if lv, err := iac.LogicalViewInfo(ctx, "shop-inst", "logical-v"); err != nil || lv.Query != "SELECT _key FROM items2" {
		t.Errorf("LogicalViewInfo after Edit query = %+v, %v", lv, err)
	}
	if code, out := iact([]string{"shop-inst", "materializedViews", "materialized-v"}, "editview", map[string]string{"protected": "false"}); code != http.StatusOK {
		t.Errorf("Edit deletion protection = %d: %s", code, out)
	}
	if out := detail(); !strings.Contains(out, "materialized-v") {
		t.Errorf("the Materialized views tab lists no view: %s", out)
	}
	if code, out := iact([]string{"shop-inst", "materializedViews", "materialized-v"}, "deleteview", nil); code != http.StatusOK {
		t.Errorf("Delete view = %d: %s", code, out)
	}
	if _, err := iac.MaterializedViewInfo(ctx, "shop-inst", "materialized-v"); err == nil {
		t.Error("the materialized view is still there")
	}
	if code, out := iact([]string{"shop-inst", "clusters", "shop-c1"}, "memorylayer", map[string]string{"enabled": "true"}); code != http.StatusOK {
		t.Errorf("Edit memory layer = %d: %s", code, out)
	}
	l, err := iac.InstanceAdminClientV2().GetMemoryLayer(ctx, &btapb.GetMemoryLayerRequest{Name: "projects/demo/instances/shop-inst/clusters/shop-c1/memoryLayer"})
	if err != nil || l.GetMemoryConfig() == nil {
		t.Errorf("the memory layer = %v, %v; want enabled", l, err)
	}
}
