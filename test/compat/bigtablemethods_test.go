//go:build compat

package compat

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// The Bigtable RPCs TestBigtableTableAndRows does not reach (#1198).
//
// Fifteen more methods are left Unknown on purpose: the emulator (bttest)
// embeds a nil server interface for them, so any call panics and restarts
// the pod, losing every table. They are the backup methods (CreateBackup,
// CopyBackup, GetBackup, ListBackups, UpdateBackup, DeleteBackup,
// RestoreTable), the schema bundle methods, UndeleteTable, and the change
// stream reads (GenerateInitialChangeStreamPartitions, ReadChangeStream). A
// compat test cannot call them without breaking every other test sharing
// the emulator (#1206).

const bigtableInstance = "cb-instance"

// bigtableTable creates a table with an "info" family through the admin
// client and returns both clients; the table is deleted at cleanup.
func bigtableTable(t *testing.T, h *Harness, table string) (*bigtable.AdminClient, *bigtable.Client) {
	t.Helper()
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	ctx := h.Context()
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateTable(ctx, table); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), table) })
	if err := admin.CreateColumnFamily(ctx, table, "info"); err != nil {
		t.Fatalf("CreateColumnFamily: %v", err)
	}
	c, err := bigtable.NewClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return admin, c
}

func bigtableRowKeys(t *testing.T, h *Harness, tbl *bigtable.Table) []string {
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

// TestBigtableBulkAndConditionalWrites writes rows with ApplyBulk
// (MutateRows), applies a conditional mutation (CheckAndMutateRow) whose
// branch depends on the row, increments a counter (ReadModifyWriteRow), and
// samples the row keys (SampleRowKeys).
//
// covers: google.bigtable.v2.Bigtable/MutateRows, google.bigtable.v2.Bigtable/CheckAndMutateRow, google.bigtable.v2.Bigtable/ReadModifyWriteRow, google.bigtable.v2.Bigtable/SampleRowKeys
func TestBigtableBulkAndConditionalWrites(t *testing.T) {
	h := New(t)
	_, c := bigtableTable(t, h, "writes")
	ctx := h.Context()
	tbl := c.Open("writes")

	var keys []string
	var muts []*bigtable.Mutation
	for i := 0; i < 3; i++ {
		m := bigtable.NewMutation()
		m.Set("info", "n", bigtable.Now(), []byte(fmt.Sprint(i)))
		keys, muts = append(keys, fmt.Sprintf("row-%d", i)), append(muts, m)
	}
	errs, err := tbl.ApplyBulk(ctx, keys, muts)
	if err != nil || errs != nil {
		t.Fatalf("ApplyBulk = %v, %v", errs, err)
	}
	if got := bigtableRowKeys(t, h, tbl); fmt.Sprint(got) != fmt.Sprint(keys) {
		t.Errorf("rows after ApplyBulk = %v, want %v", got, keys)
	}

	// row-0 has info:n, so the true branch runs; row-9 does not exist.
	for _, tc := range []struct {
		row         string
		wantMatched bool
		wantValue   string
	}{{"row-0", true, "yes"}, {"row-9", false, "no"}} {
		yes, no := bigtable.NewMutation(), bigtable.NewMutation()
		yes.Set("info", "matched", bigtable.Now(), []byte("yes"))
		no.Set("info", "matched", bigtable.Now(), []byte("no"))
		var matched bool
		cond := bigtable.NewCondMutation(bigtable.ColumnFilter("n"), yes, no)
		if err := tbl.Apply(ctx, tc.row, cond, bigtable.GetCondMutationResult(&matched)); err != nil {
			t.Fatalf("conditional Apply(%s): %v", tc.row, err)
		}
		if matched != tc.wantMatched {
			t.Errorf("%s matched = %v, want %v", tc.row, matched, tc.wantMatched)
		}
		r, err := tbl.ReadRow(ctx, tc.row, bigtable.RowFilter(bigtable.ColumnFilter("matched")))
		if err != nil {
			t.Fatalf("ReadRow(%s): %v", tc.row, err)
		}
		if items := r["info"]; len(items) != 1 || string(items[0].Value) != tc.wantValue {
			t.Errorf("%s info:matched = %v, want %q", tc.row, items, tc.wantValue)
		}
	}

	for _, step := range []struct{ delta, want int64 }{{5, 5}, {7, 12}} {
		want := step.want
		rmw := bigtable.NewReadModifyWrite()
		rmw.Increment("info", "count", step.delta)
		r, err := tbl.ApplyReadModifyWrite(ctx, "counter", rmw)
		if err != nil {
			t.Fatalf("ApplyReadModifyWrite: %v", err)
		}
		items := r["info"]
		if len(items) != 1 || len(items[0].Value) != 8 || int64(binary.BigEndian.Uint64(items[0].Value)) != want {
			t.Errorf("counter = %v, want %d", items, want)
		}
	}

	samples, err := tbl.SampleRowKeys(ctx)
	if err != nil {
		t.Fatalf("SampleRowKeys: %v", err)
	}
	if !sort.StringsAreSorted(samples) {
		t.Errorf("SampleRowKeys = %q, want sorted keys", samples)
	}
}

// TestBigtableTableMaintenance drops rows by prefix and then all rows
// (DropRowRange), waits for replication (GenerateConsistencyToken, then
// CheckConsistency), turns deletion protection on and off (UpdateTable),
// and deletes the table (DeleteTable), checking each effect.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/DropRowRange, google.bigtable.admin.v2.BigtableTableAdmin/GenerateConsistencyToken, google.bigtable.admin.v2.BigtableTableAdmin/CheckConsistency, google.bigtable.admin.v2.BigtableTableAdmin/UpdateTable, google.bigtable.admin.v2.BigtableTableAdmin/DeleteTable
func TestBigtableTableMaintenance(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "upkeep")
	ctx := h.Context()
	tbl := c.Open("upkeep")

	for _, k := range []string{"a-1", "a-2", "b-1"} {
		m := bigtable.NewMutation()
		m.Set("info", "v", bigtable.Now(), []byte(k))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatalf("Apply(%s): %v", k, err)
		}
	}
	if err := admin.DropRowRange(ctx, "upkeep", "a-"); err != nil {
		t.Fatalf("DropRowRange: %v", err)
	}
	if got := bigtableRowKeys(t, h, tbl); fmt.Sprint(got) != "[b-1]" {
		t.Errorf("rows after dropping a- = %v, want [b-1]", got)
	}
	if err := admin.DropAllRows(ctx, "upkeep"); err != nil {
		t.Fatalf("DropAllRows: %v", err)
	}
	if got := bigtableRowKeys(t, h, tbl); len(got) != 0 {
		t.Errorf("rows after DropAllRows = %v, want none", got)
	}

	if err := admin.WaitForReplication(ctx, "upkeep"); err != nil {
		t.Fatalf("WaitForReplication: %v", err)
	}

	for _, p := range []bigtable.DeletionProtection{bigtable.Protected, bigtable.Unprotected} {
		if err := admin.UpdateTableWithDeletionProtection(ctx, "upkeep", p); err != nil {
			t.Fatalf("UpdateTableWithDeletionProtection(%v): %v", p, err)
		}
		info, err := admin.TableInfo(ctx, "upkeep")
		if err != nil {
			t.Fatalf("TableInfo: %v", err)
		}
		if info.DeletionProtection != p {
			t.Errorf("DeletionProtection = %v, want %v", info.DeletionProtection, p)
		}
	}

	if err := admin.DeleteTable(ctx, "upkeep"); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}
	tables, err := admin.Tables(ctx)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	for _, name := range tables {
		if name == "upkeep" {
			t.Errorf("Tables after DeleteTable = %v, still lists upkeep", tables)
		}
	}
}

// TestBigtablePingAndWarm warms the client's connection.
//
// covers: google.bigtable.v2.Bigtable/PingAndWarm
func TestBigtablePingAndWarm(t *testing.T) {
	h := New(t)
	_, c := bigtableTable(t, h, "warm")
	if err := c.PingAndWarm(h.Context()); err != nil {
		t.Fatalf("PingAndWarm: %v", err)
	}
}

// TestBigtableRefusedAdminMethods records the table admin methods the
// emulator answers UNIMPLEMENTED, called through the generated admin client
// the veneer wraps (TableAdminClientV2).
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateAuthorizedView (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/GetAuthorizedView (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/ListAuthorizedViews (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/UpdateAuthorizedView (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/DeleteAuthorizedView (unimplemented)
// covers: google.bigtable.admin.v2.BigtableTableAdmin/SnapshotTable (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/GetSnapshot (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/ListSnapshots (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/DeleteSnapshot (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/CreateTableFromSnapshot (unimplemented)
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/SetIamPolicy (unimplemented), google.bigtable.admin.v2.BigtableTableAdmin/TestIamPermissions (unimplemented)
func TestBigtableRefusedAdminMethods(t *testing.T) {
	h := New(t)
	admin, _ := bigtableTable(t, h, "refusals")
	a := admin.TableAdminClientV2()
	ctx := h.Context()
	inst := fmt.Sprintf("projects/%s/instances/%s", h.Project(), bigtableInstance)
	tbl, cl := inst+"/tables/refusals", inst+"/clusters/c"
	view, snap := tbl+"/authorizedViews/v", cl+"/snapshots/s"

	const views = "the emulator does not currently support authorized views"
	_, err := a.CreateAuthorizedView(ctx, &adminpb.CreateAuthorizedViewRequest{Parent: tbl, AuthorizedViewId: "v", AuthorizedView: &adminpb.AuthorizedView{
		AuthorizedView: &adminpb.AuthorizedView_SubsetView_{SubsetView: &adminpb.AuthorizedView_SubsetView{RowPrefixes: [][]byte{[]byte("a")}}},
	}})
	wantRefused(t, "CreateAuthorizedView", err, codes.Unimplemented, views)
	_, err = a.GetAuthorizedView(ctx, &adminpb.GetAuthorizedViewRequest{Name: view})
	wantRefused(t, "GetAuthorizedView", err, codes.Unimplemented, views)
	_, err = a.ListAuthorizedViews(ctx, &adminpb.ListAuthorizedViewsRequest{Parent: tbl}).Next()
	wantRefused(t, "ListAuthorizedViews", err, codes.Unimplemented, views)
	_, err = a.UpdateAuthorizedView(ctx, &adminpb.UpdateAuthorizedViewRequest{AuthorizedView: &adminpb.AuthorizedView{Name: view}})
	wantRefused(t, "UpdateAuthorizedView", err, codes.Unimplemented, views)
	err = a.DeleteAuthorizedView(ctx, &adminpb.DeleteAuthorizedViewRequest{Name: view})
	wantRefused(t, "DeleteAuthorizedView", err, codes.Unimplemented, views)

	const snaps = "the emulator does not currently support snapshots"
	_, err = a.SnapshotTable(ctx, &adminpb.SnapshotTableRequest{Name: tbl, Cluster: cl, SnapshotId: "s"})
	wantRefused(t, "SnapshotTable", err, codes.Unimplemented, snaps)
	_, err = a.GetSnapshot(ctx, &adminpb.GetSnapshotRequest{Name: snap})
	wantRefused(t, "GetSnapshot", err, codes.Unimplemented, snaps)
	_, err = a.ListSnapshots(ctx, &adminpb.ListSnapshotsRequest{Parent: cl}).Next()
	wantRefused(t, "ListSnapshots", err, codes.Unimplemented, snaps)
	err = a.DeleteSnapshot(ctx, &adminpb.DeleteSnapshotRequest{Name: snap})
	wantRefused(t, "DeleteSnapshot", err, codes.Unimplemented, snaps)
	_, err = a.CreateTableFromSnapshot(ctx, &adminpb.CreateTableFromSnapshotRequest{Parent: inst, TableId: "fs", SourceSnapshot: snap})
	wantRefused(t, "CreateTableFromSnapshot", err, codes.Unimplemented, snaps)

	const iam = "unimplemented feature"
	_, err = a.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: tbl})
	wantRefused(t, "GetIamPolicy", err, codes.Unimplemented, iam)
	_, err = a.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: tbl, Policy: &iampb.Policy{}})
	wantRefused(t, "SetIamPolicy", err, codes.Unimplemented, iam)
	_, err = a.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: tbl, Permissions: []string{"bigtable.tables.get"}})
	wantRefused(t, "TestIamPermissions", err, codes.Unimplemented, iam)
}

// TestBigtableRefusedDataMethods records the data methods the emulator
// answers UNIMPLEMENTED: SQL (PrepareQuery, ExecuteQuery) and the session
// data plane (GetClientConfiguration, OpenTable, OpenAuthorizedView,
// OpenMaterializedView). The veneer's PrepareStatement sends PrepareQuery;
// the others have no veneer call that reaches the emulator, so the module's
// generated stub sends them.
//
// covers: google.bigtable.v2.Bigtable/PrepareQuery (unimplemented), google.bigtable.v2.Bigtable/ExecuteQuery (unimplemented), google.bigtable.v2.Bigtable/GetClientConfiguration (unimplemented)
// covers: google.bigtable.v2.Bigtable/OpenTable (unimplemented), google.bigtable.v2.Bigtable/OpenAuthorizedView (unimplemented), google.bigtable.v2.Bigtable/OpenMaterializedView (unimplemented)
func TestBigtableRefusedDataMethods(t *testing.T) {
	h := New(t)
	_, c := bigtableTable(t, h, "sql")
	ctx := h.Context()
	inst := fmt.Sprintf("projects/%s/instances/%s", h.Project(), bigtableInstance)

	_, err := c.PrepareStatement(ctx, "SELECT _key FROM sql", nil)
	wantRefused(t, "PrepareStatement", err, codes.Unimplemented, "the emulator does not currently support PrepareQuery")

	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d := bigtablepb.NewBigtableClient(conn)

	eq, err := d.ExecuteQuery(ctx, &bigtablepb.ExecuteQueryRequest{InstanceName: inst, Query: "SELECT _key FROM sql"})
	if err == nil {
		_, err = eq.Recv()
	}
	wantRefused(t, "ExecuteQuery", err, codes.Unimplemented, "the emulator does not currently support ExecuteQuery")

	_, err = d.GetClientConfiguration(ctx, &bigtablepb.GetClientConfigurationRequest{InstanceName: inst})
	wantRefused(t, "GetClientConfiguration", err, codes.Unimplemented, "the emulator does not currently support GetClientConfiguration")

	type session interface {
		Send(*bigtablepb.SessionRequest) error
		Recv() (*bigtablepb.SessionResponse, error)
	}
	for _, tc := range []struct {
		name string
		open func() (session, error)
	}{
		{"OpenTable", func() (session, error) { return d.OpenTable(ctx) }},
		{"OpenAuthorizedView", func() (session, error) { return d.OpenAuthorizedView(ctx) }},
		{"OpenMaterializedView", func() (session, error) { return d.OpenMaterializedView(ctx) }},
	} {
		s, err := tc.open()
		if err == nil {
			_ = s.Send(&bigtablepb.SessionRequest{})
			_, err = s.Recv()
		}
		if err == io.EOF {
			err = nil
		}
		wantRefused(t, tc.name, err, codes.Unimplemented, "the emulator does not currently support "+tc.name)
	}
}
