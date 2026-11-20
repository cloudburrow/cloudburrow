package bigtablefront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/bigtable/bttest"
	"google.golang.org/api/option"
)

// TestTableClusterStates (#1858): GetTable and ListTables carry a READY
// state for each cluster of the instance in the views table.proto names
// (REPLICATION_VIEW, ENCRYPTION_VIEW, FULL), Google-managed encryption in
// the last two, and none in SCHEMA_VIEW or NAME_ONLY.
func TestTableClusterStates(t *testing.T) {
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addr, _ := serveKept(t, emu.Addr, t.TempDir())
	opt := option.WithGRPCConn(dial(t, addr))
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateInstanceWithClusters(ctx, &bigtable.InstanceWithClustersConfig{InstanceID: "inst-1", DisplayName: "inst-1",
		Clusters: []bigtable.ClusterConfig{
			{ClusterID: "clus-1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD},
			{ClusterID: "clus-2", Zone: "us-central1-c", NumNodes: 1, StorageType: bigtable.SSD},
		}}); err != nil {
		t.Fatalf("CreateInstanceWithClusters: %v", err)
	}
	ac, err := bigtable.NewAdminClient(ctx, "p", "inst-1", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	raw := ac.TableAdminClientV2()
	name := "projects/p/instances/inst-1/tables/t"
	for view, want := range map[btapb.Table_View]struct{ states, encryption bool }{
		btapb.Table_REPLICATION_VIEW: {true, false}, btapb.Table_ENCRYPTION_VIEW: {true, true}, btapb.Table_FULL: {true, true},
		btapb.Table_SCHEMA_VIEW: {}, btapb.Table_NAME_ONLY: {},
	} {
		tb, err := raw.GetTable(ctx, &btapb.GetTableRequest{Name: name, View: view})
		if err != nil {
			t.Fatalf("GetTable(%v): %v", view, err)
		}
		cs := tb.GetClusterStates()
		if !want.states {
			if len(cs) != 0 {
				t.Errorf("GetTable(%v) cluster_states = %v, want none", view, cs)
			}
			continue
		}
		if len(cs) != 2 {
			t.Fatalf("GetTable(%v) cluster_states = %v, want clus-1 and clus-2", view, cs)
		}
		for id, s := range cs {
			if s.GetReplicationState() != btapb.Table_ClusterState_READY {
				t.Errorf("GetTable(%v) %s = %v, want READY", view, id, s.GetReplicationState())
			}
			enc := len(s.GetEncryptionInfo()) == 1 && s.GetEncryptionInfo()[0].GetEncryptionType() == btapb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION
			if enc != want.encryption {
				t.Errorf("GetTable(%v) %s encryption_info = %v", view, id, s.GetEncryptionInfo())
			}
		}
	}
	lt, err := raw.ListTables(ctx, &btapb.ListTablesRequest{Parent: "projects/p/instances/inst-1", View: btapb.Table_REPLICATION_VIEW}).Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.GetClusterStates()) != 2 {
		t.Errorf("ListTables(REPLICATION_VIEW) = %v", lt)
	}
}
