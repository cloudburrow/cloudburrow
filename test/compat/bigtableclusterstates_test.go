//go:build compat

package compat

import (
	"slices"
	"sort"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/api/iterator"
)

// TestBigtableTableClusterStates (#2027): a table's cluster_states, which
// GetTable and ListTables return in the views table.proto documents them
// for ("Views: REPLICATION_VIEW, ENCRYPTION_VIEW, FULL"; ListTables: "Only
// NAME_ONLY view (default), REPLICATION_VIEW and ENCRYPTION_VIEW are
// supported"), read through the official admin client's generated table
// admin client. Each cluster of the table's instance is listed, READY (the
// emulator holds the one copy, written before the call returns); in
// ENCRYPTION_VIEW and FULL each has Google-managed encryption, in
// REPLICATION_VIEW none; SCHEMA_VIEW and NAME_ONLY carry no cluster states;
// a cluster added to the instance is listed from then on.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetTable, google.bigtable.admin.v2.BigtableTableAdmin/ListTables
func TestBigtableTableClusterStates(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id = "cb-inst-states"
	if err := iac.CreateInstanceWithClusters(ctx, &bigtable.InstanceWithClustersConfig{InstanceID: id, DisplayName: "CloudBurrow states",
		Clusters: []bigtable.ClusterConfig{
			{ClusterID: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD},
			{ClusterID: id + "-c2", Zone: "us-east1-b", NumNodes: 1, StorageType: bigtable.SSD},
		}}); err != nil {
		t.Fatalf("CreateInstanceWithClusters: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	for _, tbl := range []string{"orders", "users"} {
		if err := admin.CreateTable(ctx, tbl); err != nil {
			t.Fatalf("CreateTable %s: %v", tbl, err)
		}
	}
	v2 := admin.TableAdminClientV2()
	parent := "projects/" + h.Project() + "/instances/" + id
	clusterIDs := func(states map[string]*adminpb.Table_ClusterState) []string {
		ids := make([]string, 0, len(states))
		for k := range states {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		return ids
	}
	check := func(what string, states map[string]*adminpb.Table_ClusterState, want []string, encryption bool) {
		t.Helper()
		if got := clusterIDs(states); !slices.Equal(got, want) {
			t.Errorf("%s: cluster_states for %v, want %v", what, got, want)
			return
		}
		for cl, cs := range states {
			if cs.GetReplicationState() != adminpb.Table_ClusterState_READY {
				t.Errorf("%s: cluster %s is %v, want READY", what, cl, cs.GetReplicationState())
			}
			enc := cs.GetEncryptionInfo()
			if encryption && (len(enc) != 1 || enc[0].GetEncryptionType() != adminpb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION) {
				t.Errorf("%s: cluster %s encryption_info = %v, want GOOGLE_DEFAULT_ENCRYPTION", what, cl, enc)
			}
			if !encryption && len(enc) != 0 {
				t.Errorf("%s: cluster %s has encryption_info %v in a view without it", what, cl, enc)
			}
		}
	}
	two := []string{id + "-c1", id + "-c2"}
	get := func(view adminpb.Table_View) *adminpb.Table {
		t.Helper()
		tb, err := v2.GetTable(ctx, &adminpb.GetTableRequest{Name: parent + "/tables/orders", View: view})
		if err != nil {
			t.Fatalf("GetTable %v: %v", view, err)
		}
		return tb
	}
	check("GetTable REPLICATION_VIEW", get(adminpb.Table_REPLICATION_VIEW).GetClusterStates(), two, false)
	check("GetTable ENCRYPTION_VIEW", get(adminpb.Table_ENCRYPTION_VIEW).GetClusterStates(), two, true)
	check("GetTable FULL", get(adminpb.Table_FULL).GetClusterStates(), two, true)
	if cs := get(adminpb.Table_SCHEMA_VIEW).GetClusterStates(); len(cs) != 0 {
		t.Errorf("GetTable SCHEMA_VIEW has cluster_states %v, want none", clusterIDs(cs))
	}

	list := func(view adminpb.Table_View) map[string]*adminpb.Table {
		t.Helper()
		out := map[string]*adminpb.Table{}
		it := v2.ListTables(ctx, &adminpb.ListTablesRequest{Parent: parent, View: view})
		for {
			tb, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListTables %v: %v", view, err)
			}
			out[tb.GetName()] = tb
		}
		if len(out) != 2 {
			t.Fatalf("ListTables %v listed %d tables, want 2", view, len(out))
		}
		return out
	}
	for name, tb := range list(adminpb.Table_REPLICATION_VIEW) {
		check("ListTables REPLICATION_VIEW "+name, tb.GetClusterStates(), two, false)
	}
	for name, tb := range list(adminpb.Table_ENCRYPTION_VIEW) {
		check("ListTables ENCRYPTION_VIEW "+name, tb.GetClusterStates(), two, true)
	}
	for name, tb := range list(adminpb.Table_NAME_ONLY) {
		if len(tb.GetClusterStates()) != 0 {
			t.Errorf("ListTables NAME_ONLY %s has cluster_states %v, want none", name, clusterIDs(tb.GetClusterStates()))
		}
	}

	// A cluster added later holds the table too.
	if err := iac.CreateCluster(ctx, &bigtable.ClusterConfig{InstanceID: id, ClusterID: id + "-c3", Zone: "europe-west1-b",
		NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	check("GetTable REPLICATION_VIEW after CreateCluster", get(adminpb.Table_REPLICATION_VIEW).GetClusterStates(),
		[]string{id + "-c1", id + "-c2", id + "-c3"}, false)
}
