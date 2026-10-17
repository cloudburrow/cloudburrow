//go:build compat

package compat

import (
	"encoding/base64"
	"encoding/binary"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Backups, snapshots, authorized views and schema bundles (#1499), which CloudBurrow's
// Bigtable front keeps beside the emulator's tables
// (internal/bigtablefront).

// TestBigtableBackups backs a table up with the official admin client,
// updates, lists, copies and deletes backups, and restores a backup and its
// copy into new tables, whose rows the data client reads back.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateBackup, google.bigtable.admin.v2.BigtableTableAdmin/GetBackup, google.bigtable.admin.v2.BigtableTableAdmin/ListBackups, google.bigtable.admin.v2.BigtableTableAdmin/UpdateBackup, google.bigtable.admin.v2.BigtableTableAdmin/DeleteBackup, google.bigtable.admin.v2.BigtableTableAdmin/CopyBackup, google.bigtable.admin.v2.BigtableTableAdmin/RestoreTable
func TestBigtableBackups(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "bk-src")
	ctx := h.Context()
	tbl := c.Open("bk-src")
	for _, k := range []string{"r1", "r2", "r3"} {
		m := bigtable.NewMutation()
		m.Set("info", "v", bigtable.Timestamp(5000), []byte("val-"+k))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	expire := time.Now().Add(24 * time.Hour)
	if err := admin.CreateBackup(ctx, "bk-src", "c1", "bk1", expire); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteBackup(h.Context(), "c1", "bk1") })
	if err := admin.CreateBackup(ctx, "bk-src", "c1", "bk-short", time.Now().Add(time.Hour)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateBackup expiring in an hour = %v, want InvalidArgument", err)
	}
	info, err := admin.BackupInfo(ctx, "c1", "bk1")
	if err != nil || info.SourceTable != "bk-src" || info.State != "READY" {
		t.Fatalf("BackupInfo = %+v, %v", info, err)
	}
	later := expire.Add(48 * time.Hour)
	if err := admin.UpdateBackup(ctx, "c1", "bk1", later); err != nil {
		t.Fatalf("UpdateBackup: %v", err)
	}
	if info, err := admin.BackupInfo(ctx, "c1", "bk1"); err != nil || info.ExpireTime.Unix() != later.Unix() {
		t.Errorf("BackupInfo after UpdateBackup = %+v, %v", info, err)
	}

	if err := admin.RestoreTable(ctx, "bk-restored", "c1", "bk1"); err != nil {
		t.Fatalf("RestoreTable: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), "bk-restored") })
	if keys := bigtableRowKeys(t, h, c.Open("bk-restored")); strings.Join(keys, ",") != "r1,r2,r3" {
		t.Errorf("the restored table reads %v, want r1,r2,r3", keys)
	}
	row, err := c.Open("bk-restored").ReadRow(ctx, "r2")
	if err != nil || len(row["info"]) != 1 || string(row["info"][0].Value) != "val-r2" || row["info"][0].Timestamp != 5000 {
		t.Errorf("the restored row r2 = %+v, %v", row, err)
	}

	if err := admin.CopyBackup(ctx, "c1", "bk1", h.Project(), bigtableInstance, "c2", "bk1-copy", expire); err != nil {
		t.Fatalf("CopyBackup: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteBackup(h.Context(), "c2", "bk1-copy") })
	var names []string
	it := admin.Backups(ctx, "-")
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("Backups: %v", err)
		}
		names = append(names, b.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "bk1,bk1-copy" {
		t.Errorf("Backups(-) = %v", names)
	}
	if err := admin.RestoreTable(ctx, "bk-from-copy", "c2", "bk1-copy"); err != nil {
		t.Fatalf("RestoreTable from the copy: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), "bk-from-copy") })
	if keys := bigtableRowKeys(t, h, c.Open("bk-from-copy")); len(keys) != 3 {
		t.Errorf("the table restored from the copy reads %v", keys)
	}
	if err := admin.DeleteBackup(ctx, "c2", "bk1-copy"); err != nil {
		t.Errorf("DeleteBackup: %v", err)
	}
	if _, err := admin.BackupInfo(ctx, "c2", "bk1-copy"); status.Code(err) != codes.NotFound {
		t.Errorf("BackupInfo after DeleteBackup = %v, want NotFound", err)
	}
}

// TestBigtableSnapshots snapshots a table, reads, lists and deletes the
// snapshot, and creates a table from it holding the table's rows.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/SnapshotTable, google.bigtable.admin.v2.BigtableTableAdmin/GetSnapshot, google.bigtable.admin.v2.BigtableTableAdmin/ListSnapshots, google.bigtable.admin.v2.BigtableTableAdmin/DeleteSnapshot, google.bigtable.admin.v2.BigtableTableAdmin/CreateTableFromSnapshot
func TestBigtableSnapshots(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "snap-src")
	ctx := h.Context()
	m := bigtable.NewMutation()
	m.Set("info", "v", bigtable.Now(), []byte("x"))
	if err := c.Open("snap-src").Apply(ctx, "k1", m); err != nil {
		t.Fatal(err)
	}
	if err := admin.SnapshotTable(ctx, "snap-src", "c1", "s1", 48*time.Hour); err != nil {
		t.Fatalf("SnapshotTable: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteSnapshot(h.Context(), "c1", "s1") })
	if s, err := admin.SnapshotInfo(ctx, "c1", "s1"); err != nil || s.SourceTable != "snap-src" {
		t.Errorf("SnapshotInfo = %+v, %v", s, err)
	}
	if s, err := admin.Snapshots(ctx, "c1").Next(); err != nil || s.Name != "s1" {
		t.Errorf("Snapshots = %+v, %v", s, err)
	}
	if err := admin.CreateTableFromSnapshot(ctx, "snap-new", "c1", "s1"); err != nil {
		t.Fatalf("CreateTableFromSnapshot: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), "snap-new") })
	if keys := bigtableRowKeys(t, h, c.Open("snap-new")); strings.Join(keys, ",") != "k1" {
		t.Errorf("the table from the snapshot reads %v, want k1", keys)
	}
	if err := admin.DeleteSnapshot(ctx, "c1", "s1"); err != nil {
		t.Errorf("DeleteSnapshot: %v", err)
	}
	if _, err := admin.SnapshotInfo(ctx, "c1", "s1"); status.Code(err) != codes.NotFound {
		t.Errorf("SnapshotInfo after DeleteSnapshot = %v, want NotFound", err)
	}
}

// TestBigtableAuthorizedViews creates, reads, lists, updates and deletes an
// authorized view with the official admin client, and reads and writes
// through it with the data client's OpenAuthorizedView: only the view's
// rows and columns are read, and a write outside it is refused
// PERMISSION_DENIED.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateAuthorizedView, google.bigtable.admin.v2.BigtableTableAdmin/GetAuthorizedView, google.bigtable.admin.v2.BigtableTableAdmin/ListAuthorizedViews, google.bigtable.admin.v2.BigtableTableAdmin/UpdateAuthorizedView, google.bigtable.admin.v2.BigtableTableAdmin/DeleteAuthorizedView
func TestBigtableAuthorizedViews(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "av-src")
	ctx := h.Context()
	if err := admin.CreateColumnFamily(ctx, "av-src", "secret"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"eu#1", "eu#2", "us#1"} {
		m := bigtable.NewMutation()
		m.Set("info", "name", bigtable.Now(), []byte(k))
		m.Set("secret", "pin", bigtable.Now(), []byte("1234"))
		if err := c.Open("av-src").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	sv := &bigtable.SubsetViewConf{}
	sv.AddRowPrefix([]byte("eu#"))
	sv.AddFamilySubsetQualifierPrefix("info", []byte(""))
	if err := admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{TableID: "av-src", AuthorizedViewID: "eu", AuthorizedView: sv}); err != nil {
		t.Fatalf("CreateAuthorizedView: %v", err)
	}
	if names, err := admin.AuthorizedViews(ctx, "av-src"); err != nil || strings.Join(names, ",") != "eu" {
		t.Errorf("AuthorizedViews = %v, %v", names, err)
	}
	if info, err := admin.AuthorizedViewInfo(ctx, "av-src", "eu"); err != nil || len(info.AuthorizedView.(*bigtable.SubsetViewInfo).RowPrefixes) != 1 {
		t.Errorf("AuthorizedViewInfo = %+v, %v", info, err)
	}
	view := c.OpenAuthorizedView("av-src", "eu")
	var seen []string
	if err := view.ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		for fam := range r {
			seen = append(seen, r.Key()+"/"+fam)
		}
		return true
	}); err != nil {
		t.Fatalf("ReadRows through the view: %v", err)
	}
	sort.Strings(seen)
	if strings.Join(seen, ",") != "eu#1/info,eu#2/info" {
		t.Errorf("through the view ReadRows reads %v, want eu#1/info,eu#2/info", seen)
	}
	m := bigtable.NewMutation()
	m.Set("info", "name", bigtable.Now(), []byte("new"))
	if err := view.Apply(ctx, "eu#3", m); err != nil {
		t.Errorf("a write inside the view: %v", err)
	}
	if err := view.Apply(ctx, "us#2", m); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a write outside the view = %v, want PermissionDenied", err)
	}

	sv2 := &bigtable.SubsetViewConf{}
	sv2.AddRowPrefix([]byte("us#"))
	sv2.AddFamilySubsetQualifier("info", []byte("name"))
	if err := admin.UpdateAuthorizedView(ctx, bigtable.UpdateAuthorizedViewConf{AuthorizedViewConf: bigtable.AuthorizedViewConf{
		TableID: "av-src", AuthorizedViewID: "eu", AuthorizedView: sv2}}); err != nil {
		t.Fatalf("UpdateAuthorizedView: %v", err)
	}
	if keys := bigtableRowKeysIn(t, h, view); strings.Join(keys, ",") != "us#1" {
		t.Errorf("through the updated view ReadRows reads %v, want us#1", keys)
	}
	if err := admin.DeleteAuthorizedView(ctx, "av-src", "eu"); err != nil {
		t.Fatalf("DeleteAuthorizedView: %v", err)
	}
	if _, err := admin.AuthorizedViewInfo(ctx, "av-src", "eu"); status.Code(err) != codes.NotFound {
		t.Errorf("AuthorizedViewInfo after DeleteAuthorizedView = %v, want NotFound", err)
	}
}

func bigtableRowKeysIn(t *testing.T, h *Harness, tbl bigtable.TableAPI) []string {
	t.Helper()
	var keys []string
	if err := tbl.ReadRows(h.Context(), bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	return keys
}

// TestBigtableSchemaBundles creates, reads, lists, updates and deletes a
// table's schema bundle with the official admin client: descriptors that
// are not a FileDescriptorSet are refused, and a stale etag is ABORTED.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/GetSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/ListSchemaBundles, google.bigtable.admin.v2.BigtableTableAdmin/UpdateSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/DeleteSchemaBundle
func TestBigtableSchemaBundles(t *testing.T) {
	h := New(t)
	admin, _ := bigtableTable(t, h, "sb-src")
	ctx := h.Context()
	fd := protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "sb-src", SchemaBundleID: "sb",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}}); err != nil {
		t.Fatalf("CreateSchemaBundle: %v", err)
	}
	if err := admin.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "sb-src", SchemaBundleID: "bad",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: []byte("not descriptors")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateSchemaBundle with bad descriptors = %v, want InvalidArgument", err)
	}
	info, err := admin.GetSchemaBundle(ctx, "sb-src", "sb")
	if err != nil || string(info.SchemaBundle) != string(set) {
		t.Fatalf("GetSchemaBundle = %+v, %v", info, err)
	}
	if err := admin.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{TableID: "sb-src", SchemaBundleID: "sb",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}, Etag: "stale"}}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateSchemaBundle with a stale etag = %v, want Aborted", err)
	}
	if err := admin.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{TableID: "sb-src", SchemaBundleID: "sb",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}, Etag: info.Etag}}); err != nil {
		t.Errorf("UpdateSchemaBundle: %v", err)
	}
	if names, err := admin.SchemaBundles(ctx, "sb-src"); err != nil || strings.Join(names, ",") != "sb" {
		t.Errorf("SchemaBundles = %v, %v", names, err)
	}
	if err := admin.DeleteSchemaBundle(ctx, "sb-src", "sb"); err != nil {
		t.Errorf("DeleteSchemaBundle: %v", err)
	}
	if _, err := admin.GetSchemaBundle(ctx, "sb-src", "sb"); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchemaBundle after DeleteSchemaBundle = %v, want NotFound", err)
	}
}

// TestBigtableSQL runs GoogleSQL for Bigtable with the official client's
// PrepareStatement, Bind and Execute over rows written with Apply: a
// column's latest value, CAST AS STRING, TO_INT64, a family as a map, a
// bound STARTS_WITH, and LIMIT; what the emulator's SQL does not support is
// refused at PrepareQuery.
//
// covers: google.bigtable.v2.Bigtable/PrepareQuery, google.bigtable.v2.Bigtable/ExecuteQuery
func TestBigtableSQL(t *testing.T) {
	h := New(t)
	_, c := bigtableTable(t, h, "sql-users")
	ctx := h.Context()
	for i, k := range []string{"u#1", "u#2", "x#1"} {
		m := bigtable.NewMutation()
		m.Set("info", "name", bigtable.Timestamp(1000), []byte("stale"))
		m.Set("info", "name", bigtable.Timestamp(2000), []byte("name-"+k))
		n := make([]byte, 8)
		binary.BigEndian.PutUint64(n, uint64(10*(i+1)))
		m.Set("info", "n", bigtable.Timestamp(1000), n)
		if err := c.Open("sql-users").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	run := func(sql string, params map[string]bigtable.SQLType, bind map[string]any) []bigtable.ResultRow {
		t.Helper()
		ps, err := c.PrepareStatement(ctx, sql, params)
		if err != nil {
			t.Fatalf("PrepareStatement(%s): %v", sql, err)
		}
		bs, err := ps.Bind(bind)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var out []bigtable.ResultRow
		if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool { out = append(out, r); return true }); err != nil {
			t.Fatalf("Execute(%s): %v", sql, err)
		}
		return out
	}
	rows := run("SELECT _key, CAST(info['name'] AS STRING) AS name, TO_INT64(info['n']) AS n FROM `sql-users` WHERE STARTS_WITH(_key, @prefix)",
		map[string]bigtable.SQLType{"prefix": bigtable.BytesSQLType{}}, map[string]any{"prefix": []byte("u#")})
	if len(rows) != 2 {
		t.Fatalf("STARTS_WITH u# read %d rows, want 2", len(rows))
	}
	var key []byte
	var name string
	var n int64
	if err := rows[1].GetByName("_key", &key); err != nil || string(key) != "u#2" {
		t.Errorf("_key = %q, %v", key, err)
	}
	if err := rows[1].GetByName("name", &name); err != nil || name != "name-u#2" {
		t.Errorf("name = %q, %v; want the latest value", name, err)
	}
	if err := rows[1].GetByName("n", &n); err != nil || n != 20 {
		t.Errorf("n = %d, %v", n, err)
	}
	all := run("SELECT * FROM `sql-users` LIMIT 2", nil, nil)
	if len(all) != 2 {
		t.Fatalf("SELECT * LIMIT 2 read %d rows", len(all))
	}
	var info map[string][]byte // keyed by the qualifiers' base64
	if err := all[0].GetByName("info", &info); err != nil || len(info) != 2 || string(info[base64.StdEncoding.EncodeToString([]byte("name"))]) != "name-u#1" {
		t.Errorf("info = %v, %v", info, err)
	}
	if got := run("SELECT _key FROM `sql-users` WHERE _key >= 'u#2' AND _key < 'x#2'", nil, nil); len(got) != 2 {
		t.Errorf("a key range read %d rows, want 2", len(got))
	}
	if _, err := c.PrepareStatement(ctx, "SELECT COUNT(*) FROM `sql-users`", nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an aggregate = %v, want InvalidArgument", err)
	}
	if _, err := c.PrepareStatement(ctx, "SELECT _key FROM missing", nil); status.Code(err) != codes.NotFound {
		t.Errorf("a missing table = %v, want NotFound", err)
	}
}
