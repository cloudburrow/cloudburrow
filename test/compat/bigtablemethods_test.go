//go:build compat

package compat

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// The Bigtable RPCs TestBigtableTableAndRows does not reach (#1198).
//
// The methods the emulator (bttest) has no implementation of panic it,
// which restarts the pod and loses every table. The front before the
// emulator (internal/bigtablefront, #1206) serves backups itself since
// #1499 (TestBigtableBackups), and schema bundles
// (TestBigtableSchemaBundles), and answers the rest, UndeleteTable and the
// change stream reads, UNIMPLEMENTED;
// TestBigtableCrashingMethodsAreRefused calls each.

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

// TestBigtableRefusedDataMethods records the data methods the emulator
// answers UNIMPLEMENTED: the session data plane (GetClientConfiguration,
// OpenTable, OpenAuthorizedView, OpenMaterializedView), which no veneer
// call reaches with the emulator, so the module's generated stub sends
// them. SQL is served by the front since #1499 (TestBigtableSQL).
//
// covers: google.bigtable.v2.Bigtable/GetClientConfiguration (unimplemented)
// covers: google.bigtable.v2.Bigtable/OpenTable (unimplemented), google.bigtable.v2.Bigtable/OpenAuthorizedView (unimplemented), google.bigtable.v2.Bigtable/OpenMaterializedView (unimplemented)
func TestBigtableRefusedDataMethods(t *testing.T) {
	h := New(t)
	bigtableTable(t, h, "sessions")
	ctx := h.Context()
	inst := fmt.Sprintf("projects/%s/instances/%s", h.Project(), bigtableInstance)

	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d := bigtablepb.NewBigtableClient(conn)

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

// TestBigtableCrashingMethodsAreRefused calls each method the emulator would
// panic on (#1206): CloudBurrow's front answers UNIMPLEMENTED, a table
// created before is still there after all three, and, given the
// instance's kubeconfig, the Bigtable pod's containers restarted no more
// than before. Admin calls go through the generated admin client the
// veneer wraps; the change stream reads, which the veneer does not send,
// through the module's generated data stub.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/UndeleteTable (unimplemented)
// covers: google.bigtable.v2.Bigtable/GenerateInitialChangeStreamPartitions (unimplemented), google.bigtable.v2.Bigtable/ReadChangeStream (unimplemented)
func TestBigtableCrashingMethodsAreRefused(t *testing.T) {
	h := New(t)
	admin, _ := bigtableTable(t, h, "crashers")
	restarts := bigtableRestarts(t)
	before := restarts()
	a := admin.TableAdminClientV2()
	ctx := h.Context()
	inst := fmt.Sprintf("projects/%s/instances/%s", h.Project(), bigtableInstance)
	tbl := inst + "/tables/crashers"

	_, err := a.UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl})
	wantRefused(t, "UndeleteTable", err, codes.Unimplemented, "the emulator does not currently support undeleting tables")

	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d := bigtablepb.NewBigtableClient(conn)
	const streams = "the emulator does not currently support change streams"
	gp, err := d.GenerateInitialChangeStreamPartitions(ctx, &bigtablepb.GenerateInitialChangeStreamPartitionsRequest{TableName: tbl})
	if err == nil {
		_, err = gp.Recv()
	}
	wantRefused(t, "GenerateInitialChangeStreamPartitions", err, codes.Unimplemented, streams)
	rc, err := d.ReadChangeStream(ctx, &bigtablepb.ReadChangeStreamRequest{TableName: tbl})
	if err == nil {
		_, err = rc.Recv()
	}
	wantRefused(t, "ReadChangeStream", err, codes.Unimplemented, streams)

	// A crash would have lost the table with the emulator's memory.
	if _, err := admin.TableInfo(ctx, "crashers"); err != nil {
		t.Errorf("the table after the three calls: %v; did the emulator restart?", err)
	}
	if after := restarts(); after != before {
		t.Errorf("the Bigtable pod's restarts went from %s to %s", before, after)
	}
}

// bigtableRestarts returns a function that reads the restart counts of the
// Bigtable pod's containers, or one that always answers "unknown" without
// the instance's kubeconfig and kubectl.
func bigtableRestarts(t *testing.T) func() string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if _, err := exec.LookPath("kubectl"); kubeconfig == "" || err != nil {
		t.Logf("no %s or kubectl: the pod's restart count is not read", envKubeconfig)
		return func() string { return "unknown" }
	}
	return func() string {
		t.Helper()
		out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A",
			"-l", "app=bigtable,cloudburrow.dev/owned=true",
			"-o", "jsonpath={.items[*].metadata.name} {.items[*].status.containerStatuses[*].restartCount}").Output()
		if err != nil {
			t.Fatalf("kubectl get the Bigtable pod: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
}
