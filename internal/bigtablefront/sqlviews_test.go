package bigtablefront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestLogicalAndMaterializedViews manages both kinds through the official
// instance admin client; a materialized view's query is not changed, and a
// protected view is not deleted.
func TestLogicalAndMaterializedViews(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateLogicalView(ctx, "nowhere", &bigtable.LogicalViewInfo{LogicalViewID: "lv", Query: "SELECT 1"}); status.Code(err) != codes.NotFound {
		t.Errorf("CreateLogicalView in a missing instance = %v, want NotFound", err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "views-inst", DisplayName: "views",
		ClusterId: "views-inst-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	// A view's query is compiled against the instance's tables (#1511).
	ac, err := bigtable.NewAdminClient(ctx, "p", "views-inst", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "users"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "users", "info"); err != nil {
		t.Fatal(err)
	}
	const q = "SELECT _key, info['name'] AS name FROM users"
	if err := iac.CreateLogicalView(ctx, "views-inst", &bigtable.LogicalViewInfo{LogicalViewID: "lv", Query: q}); err != nil {
		t.Fatalf("CreateLogicalView: %v", err)
	}
	if err := iac.CreateLogicalView(ctx, "views-inst", &bigtable.LogicalViewInfo{LogicalViewID: "empty"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a view with no query = %v, want InvalidArgument", err)
	}
	if lv, err := iac.LogicalViewInfo(ctx, "views-inst", "lv"); err != nil || lv.Query != q {
		t.Errorf("LogicalViewInfo = %+v, %v", lv, err)
	}
	if err := iac.UpdateLogicalView(ctx, "views-inst", bigtable.LogicalViewInfo{LogicalViewID: "lv", Query: "SELECT 2", DeletionProtection: bigtable.Protected}); err != nil {
		t.Fatalf("UpdateLogicalView: %v", err)
	}
	if lvs, err := iac.LogicalViews(ctx, "views-inst"); err != nil || len(lvs) != 1 || lvs[0].Query != "SELECT 2" || lvs[0].DeletionProtection != bigtable.Protected {
		t.Errorf("LogicalViews = %+v, %v", lvs, err)
	}
	if err := iac.DeleteLogicalView(ctx, "views-inst", "lv"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a protected view = %v, want FailedPrecondition", err)
	}
	if err := iac.UpdateLogicalView(ctx, "views-inst", bigtable.LogicalViewInfo{LogicalViewID: "lv", DeletionProtection: bigtable.Unprotected}); err != nil {
		t.Fatal(err)
	}
	if err := iac.DeleteLogicalView(ctx, "views-inst", "lv"); err != nil {
		t.Errorf("DeleteLogicalView: %v", err)
	}

	if err := iac.CreateMaterializedView(ctx, "views-inst", &bigtable.MaterializedViewInfo{MaterializedViewID: "mv", Query: q}); err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	if err := iac.UpdateMaterializedView(ctx, "views-inst", bigtable.MaterializedViewInfo{MaterializedViewID: "mv", Query: "SELECT 2"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("changing a materialized view's query = %v, want InvalidArgument", err)
	}
	if err := iac.UpdateMaterializedView(ctx, "views-inst", bigtable.MaterializedViewInfo{MaterializedViewID: "mv", DeletionProtection: bigtable.Protected}); err != nil {
		t.Errorf("UpdateMaterializedView: %v", err)
	}
	if mv, err := iac.MaterializedViewInfo(ctx, "views-inst", "mv"); err != nil || mv.Query != q || mv.DeletionProtection != bigtable.Protected {
		t.Errorf("MaterializedViewInfo = %+v, %v", mv, err)
	}
	if mvs, err := iac.MaterializedViews(ctx, "views-inst"); err != nil || len(mvs) != 1 {
		t.Errorf("MaterializedViews = %+v, %v", mvs, err)
	}
	if err := iac.DeleteInstance(ctx, "views-inst"); err != nil {
		t.Fatal(err)
	}
	if _, err := iac.MaterializedViewInfo(ctx, "views-inst", "mv"); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted instance's view = %v, want NotFound", err)
	}
}

// descriptorSet is a FileDescriptorSet holding timestamp.proto.
func descriptorSet(t *testing.T) []byte {
	t.Helper()
	fd := protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSchemaBundles manages a table's schema bundles through the official
// admin client; descriptors that are not a FileDescriptorSet are refused,
// and deleting the table deletes its bundles.
func TestSchemaBundles(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	set := descriptorSet(t)
	if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "t", SchemaBundleID: "sb",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}}); err != nil {
		t.Fatalf("CreateSchemaBundle: %v", err)
	}
	if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "t", SchemaBundleID: "bad",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: []byte("not a descriptor set")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad descriptors = %v, want InvalidArgument", err)
	}
	info, err := ac.GetSchemaBundle(ctx, "t", "sb")
	if err != nil || string(info.SchemaBundle) != string(set) || info.Etag == "" {
		t.Errorf("GetSchemaBundle = %+v, %v", info, err)
	}
	if err := ac.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{TableID: "t", SchemaBundleID: "sb",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}, Etag: "stale"}}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateSchemaBundle with a stale etag = %v, want Aborted", err)
	}
	if err := ac.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{TableID: "t", SchemaBundleID: "sb",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}, Etag: info.Etag}}); err != nil {
		t.Errorf("UpdateSchemaBundle: %v", err)
	}
	if names, err := ac.SchemaBundles(ctx, "t"); err != nil || len(names) != 1 || names[0] != "sb" {
		t.Errorf("SchemaBundles = %v, %v", names, err)
	}
	if err := ac.DeleteTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if names, err := ac.SchemaBundles(ctx, "t"); err != nil || len(names) != 0 {
		t.Errorf("a table made again has bundles %v, %v", names, err)
	}
	if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "t", SchemaBundleID: "sb2",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: set}}); err != nil {
		t.Fatal(err)
	}
	if err := ac.DeleteSchemaBundle(ctx, "t", "sb2"); err != nil {
		t.Errorf("DeleteSchemaBundle: %v", err)
	}
	if _, err := ac.GetSchemaBundle(ctx, "t", "sb2"); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchemaBundle after delete = %v, want NotFound", err)
	}
}

// TestMemoryLayersAndHotTablets: a cluster's memory layer is disabled until
// UpdateMemoryLayer enables it, and a cluster has no hot tablets.
func TestMemoryLayersAndHotTablets(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dial(t, addr)
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "mem-inst", DisplayName: "memory",
		ClusterId: "mem-inst-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	c := btapb.NewBigtableInstanceAdminClient(conn)
	cluster := "projects/p/instances/mem-inst/clusters/mem-inst-c1"
	l, err := c.GetMemoryLayer(ctx, &btapb.GetMemoryLayerRequest{Name: cluster + "/memoryLayer"})
	if err != nil || l.GetMemoryConfig() != nil || l.GetEtag() == "" {
		t.Fatalf("GetMemoryLayer = %v, %v", l, err)
	}
	if _, err := c.UpdateMemoryLayer(ctx, &btapb.UpdateMemoryLayerRequest{MemoryLayer: &btapb.MemoryLayer{Name: l.GetName(), MemoryConfig: &btapb.MemoryLayer_MemoryConfig{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"memory_config"}}}); err != nil {
		t.Fatalf("UpdateMemoryLayer: %v", err)
	}
	ls, err := c.ListMemoryLayers(ctx, &btapb.ListMemoryLayersRequest{Parent: "projects/p/instances/mem-inst/clusters/-"})
	if err != nil || len(ls.GetMemoryLayers()) != 1 || ls.GetMemoryLayers()[0].GetMemoryConfig() == nil {
		t.Errorf("ListMemoryLayers = %v, %v", ls, err)
	}
	if _, err := c.GetMemoryLayer(ctx, &btapb.GetMemoryLayerRequest{Name: "projects/p/instances/mem-inst/clusters/nope/memoryLayer"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing cluster's memory layer = %v, want NotFound", err)
	}
	if ht, err := c.ListHotTablets(ctx, &btapb.ListHotTabletsRequest{Parent: cluster}); err != nil || len(ht.GetHotTablets()) != 0 {
		t.Errorf("ListHotTablets = %v, %v", ht, err)
	}
}
