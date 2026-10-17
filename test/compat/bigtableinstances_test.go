//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Bigtable instance admin API (#1331): the emulator answers every
// BigtableInstanceAdmin method UNIMPLEMENTED, so the Bigtable pod's front
// (internal/bigtablefront) serves instances, clusters, app profiles and
// instance IAM policies itself.

// bigtableInstanceAdmin returns the official instance admin client, at the
// instance through BIGTABLE_EMULATOR_HOST as every client of the emulator is.
func bigtableInstanceAdmin(t *testing.T, h *Harness) *bigtable.InstanceAdminClient {
	t.Helper()
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	iac, err := bigtable.NewInstanceAdminClient(h.Context(), h.Project())
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	return iac
}

// TestBigtableInstancesAndClusters creates an instance with one cluster,
// reads, lists and updates it, adds an autoscaled second cluster, resizes
// both and deletes them and the instance.
//
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateInstance, google.bigtable.admin.v2.BigtableInstanceAdmin/GetInstance, google.bigtable.admin.v2.BigtableInstanceAdmin/ListInstances, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateInstance, google.bigtable.admin.v2.BigtableInstanceAdmin/PartialUpdateInstance, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteInstance
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateCluster, google.bigtable.admin.v2.BigtableInstanceAdmin/GetCluster, google.bigtable.admin.v2.BigtableInstanceAdmin/ListClusters, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateCluster, google.bigtable.admin.v2.BigtableInstanceAdmin/PartialUpdateCluster, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteCluster
func TestBigtableInstancesAndClusters(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id = "cb-inst-a"
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Fatalf("InstanceInfo before CreateInstance: %v, want NotFound", err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "CloudBurrow A",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD,
		Labels: map[string]string{"team": "data"}}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "CloudBurrow A",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateInstance of a taken ID: %v, want AlreadyExists", err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "Bad_ID", DisplayName: "bad id",
		ClusterId: "bad-id-c1", Zone: "us-central1-b", NumNodes: 1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateInstance of a bad ID: %v, want InvalidArgument", err)
	}
	info, err := iac.InstanceInfo(ctx, id)
	if err != nil {
		t.Fatalf("InstanceInfo: %v", err)
	}
	if info.DisplayName != "CloudBurrow A" || info.InstanceState != bigtable.Ready || info.InstanceType != bigtable.PRODUCTION || info.Labels["team"] != "data" {
		t.Errorf("InstanceInfo = %+v", info)
	}
	list, err := iac.Instances(ctx)
	if err != nil || len(list) != 1 || list[0].Name != id {
		t.Errorf("Instances = %v, %v; want %s alone", list, err, id)
	}

	// PartialUpdateInstance and UpdateCluster, as the handwritten client
	// sends them, then UpdateInstance through the generated client.
	if err := iac.UpdateInstanceWithClusters(ctx, &bigtable.InstanceWithClustersConfig{InstanceID: id,
		DisplayName: "CloudBurrow A2", Clusters: []bigtable.ClusterConfig{{ClusterID: id + "-c1", NumNodes: 3}}}); err != nil {
		t.Fatalf("UpdateInstanceWithClusters: %v", err)
	}
	full := "projects/" + h.Project() + "/instances/" + id
	got, err := iac.InstanceAdminClientV2().UpdateInstance(ctx, &adminpb.Instance{Name: full, DisplayName: "CloudBurrow A3",
		Type: adminpb.Instance_PRODUCTION, Labels: map[string]string{"team": "platform"}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if got.GetDisplayName() != "CloudBurrow A3" || got.GetLabels()["team"] != "platform" {
		t.Errorf("UpdateInstance = %v", got)
	}

	if err := iac.CreateCluster(ctx, &bigtable.ClusterConfig{InstanceID: id, ClusterID: id + "-c2", Zone: "us-east1-c",
		StorageType: bigtable.HDD, AutoscalingConfig: &bigtable.AutoscalingConfig{MinNodes: 1, MaxNodes: 3, CPUTargetPercent: 60}}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if err := iac.SetAutoscaling(ctx, id, id+"-c2", bigtable.AutoscalingConfig{MinNodes: 2, MaxNodes: 6, CPUTargetPercent: 50}); err != nil {
		t.Fatalf("SetAutoscaling (PartialUpdateCluster): %v", err)
	}
	clusters, err := iac.Clusters(ctx, id)
	if err != nil || len(clusters) != 2 {
		t.Fatalf("Clusters = %v, %v", clusters, err)
	}
	if c := clusters[0]; c.ServeNodes != 3 || c.Zone != "us-central1-b" || c.State != "READY" || c.StorageType != bigtable.SSD {
		t.Errorf("first cluster = %+v", c)
	}
	c2, err := iac.GetCluster(ctx, id, id+"-c2")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if c2.AutoscalingConfig == nil || c2.AutoscalingConfig.MinNodes != 2 || c2.AutoscalingConfig.MaxNodes != 6 || c2.ServeNodes != 2 || c2.StorageType != bigtable.HDD {
		t.Errorf("GetCluster = %+v, autoscaling %+v", c2, c2.AutoscalingConfig)
	}
	if err := iac.DeleteCluster(ctx, id, id+"-c2"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if err := iac.DeleteCluster(ctx, id, id+"-c1"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteCluster of the last cluster: %v, want FailedPrecondition", err)
	}
	if _, err := iac.GetCluster(ctx, id, id+"-c2"); status.Code(err) != codes.NotFound {
		t.Errorf("GetCluster after DeleteCluster: %v, want NotFound", err)
	}
	if err := iac.DeleteInstance(ctx, id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after DeleteInstance: %v, want NotFound", err)
	}
}

// TestBigtableAppProfilesAndInstanceIAM manages an instance's app profiles,
// the default one among them, and sets and reads its IAM policy (stored,
// not enforced: ADR-0006).
//
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/GetAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/ListAppProfiles, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteAppProfile
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableInstanceAdmin/SetIamPolicy, google.bigtable.admin.v2.BigtableInstanceAdmin/TestIamPermissions
func TestBigtableAppProfilesAndInstanceIAM(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id = "cb-inst-b"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "CloudBurrow B",
		ClusterId: id + "-c1", Zone: "europe-west1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	if _, err := iac.CreateAppProfile(ctx, bigtable.ProfileConf{InstanceID: id, ProfileID: "batch", Description: "batch jobs",
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: id + "-c1", AllowTransactionalWrites: true}}); err != nil {
		t.Fatalf("CreateAppProfile: %v", err)
	}
	if _, err := iac.CreateAppProfile(ctx, bigtable.ProfileConf{InstanceID: id, ProfileID: "stray",
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "no-such-cluster"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateAppProfile routed to a missing cluster: %v, want InvalidArgument", err)
	}
	p, err := iac.GetAppProfile(ctx, id, "batch")
	if err != nil {
		t.Fatalf("GetAppProfile: %v", err)
	}
	if p.GetDescription() != "batch jobs" || !p.GetSingleClusterRouting().GetAllowTransactionalWrites() || p.GetEtag() == "" {
		t.Errorf("GetAppProfile = %v", p)
	}
	if err := iac.UpdateAppProfile(ctx, id, "batch", bigtable.ProfileAttrsToUpdate{Description: "nightly",
		RoutingConfig: &bigtable.MultiClusterRoutingUseAnyConfig{}}); err != nil {
		t.Fatalf("UpdateAppProfile: %v", err)
	}
	if p, err = iac.GetAppProfile(ctx, id, "batch"); err != nil || p.GetDescription() != "nightly" || p.GetMultiClusterRoutingUseAny() == nil {
		t.Errorf("GetAppProfile after UpdateAppProfile = %v, %v", p, err)
	}
	var names []string
	it := iac.ListAppProfiles(ctx, id)
	for {
		p, err := it.Next()
		if err != nil {
			break
		}
		names = append(names, p.GetName())
	}
	prefix := "projects/" + h.Project() + "/instances/" + id + "/appProfiles/"
	if len(names) != 2 || names[0] != prefix+"batch" || names[1] != prefix+"default" {
		t.Errorf("ListAppProfiles = %v, want batch and default", names)
	}
	if err := iac.DeleteAppProfile(ctx, id, "batch"); err != nil {
		t.Fatalf("DeleteAppProfile: %v", err)
	}
	if _, err := iac.GetAppProfile(ctx, id, "batch"); status.Code(err) != codes.NotFound {
		t.Errorf("GetAppProfile after DeleteAppProfile: %v, want NotFound", err)
	}

	handle := iac.InstanceIAM(id)
	pol, err := handle.Policy(ctx)
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	pol.Add("user:dev@example.com", "roles/bigtable.reader")
	if err := handle.SetPolicy(ctx, pol); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if pol, err = handle.Policy(ctx); err != nil || !pol.HasRole("user:dev@example.com", "roles/bigtable.reader") {
		t.Errorf("GetIamPolicy after SetIamPolicy = %v, %v", pol, err)
	}
	perms, err := handle.TestPermissions(ctx, []string{"bigtable.tables.list", "bigtable.instances.get"})
	if err != nil || len(perms) != 2 {
		t.Errorf("TestIamPermissions = %v, %v; want both (nothing is enforced)", perms, err)
	}
	if _, err := iac.InstanceIAM("cb-missing").Policy(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy of a missing instance: %v, want NotFound", err)
	}
}

// TestBigtableInstanceDeletesItsTables writes a table into a created
// instance and one into another instance name, deletes the created one, and
// finds its table gone and the other kept.
func TestBigtableInstanceDeletesItsTables(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id = "cb-inst-c"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "CloudBurrow C",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	in, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })
	other, err := bigtable.NewAdminClient(ctx, h.Project(), id+"-other")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if err := in.CreateTable(ctx, "inside"); err != nil {
		t.Fatalf("CreateTable in the instance: %v", err)
	}
	if err := other.CreateTable(ctx, "outside"); err != nil {
		t.Fatalf("CreateTable under an instance name never created: %v", err)
	}
	t.Cleanup(func() { _ = other.DeleteTable(h.Context(), "outside") })
	if err := iac.DeleteInstance(ctx, id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if tables, err := in.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("the deleted instance's tables = %v, %v; want none", tables, err)
	}
	if tables, err := other.Tables(ctx); err != nil || len(tables) != 1 {
		t.Errorf("the other instance's tables = %v, %v; want outside", tables, err)
	}
}

// TestBigtableLogicalAndMaterializedViews creates, reads, lists, updates
// and deletes a logical and a materialized view of an instance with the
// official instance admin client (#1499): the queries are kept as given, a
// materialized view's query cannot change, and a protected view is not
// deleted.
//
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateLogicalView, google.bigtable.admin.v2.BigtableInstanceAdmin/GetLogicalView, google.bigtable.admin.v2.BigtableInstanceAdmin/ListLogicalViews, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateLogicalView, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteLogicalView
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateMaterializedView, google.bigtable.admin.v2.BigtableInstanceAdmin/GetMaterializedView, google.bigtable.admin.v2.BigtableInstanceAdmin/ListMaterializedViews, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateMaterializedView, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteMaterializedView
func TestBigtableLogicalAndMaterializedViews(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id = "cb-inst-views"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "SQL views",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	const q = "SELECT _key, info['name'] AS name FROM users"
	if err := iac.CreateLogicalView(ctx, id, &bigtable.LogicalViewInfo{LogicalViewID: "lv", Query: q}); err != nil {
		t.Fatalf("CreateLogicalView: %v", err)
	}
	if lv, err := iac.LogicalViewInfo(ctx, id, "lv"); err != nil || lv.Query != q {
		t.Errorf("LogicalViewInfo = %+v, %v", lv, err)
	}
	if err := iac.UpdateLogicalView(ctx, id, bigtable.LogicalViewInfo{LogicalViewID: "lv", Query: "SELECT _key FROM users", DeletionProtection: bigtable.Protected}); err != nil {
		t.Fatalf("UpdateLogicalView: %v", err)
	}
	if lvs, err := iac.LogicalViews(ctx, id); err != nil || len(lvs) != 1 || lvs[0].Query != "SELECT _key FROM users" || lvs[0].DeletionProtection != bigtable.Protected {
		t.Errorf("LogicalViews = %+v, %v", lvs, err)
	}
	if err := iac.DeleteLogicalView(ctx, id, "lv"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteLogicalView of a protected view = %v, want FailedPrecondition", err)
	}
	if err := iac.UpdateLogicalView(ctx, id, bigtable.LogicalViewInfo{LogicalViewID: "lv", DeletionProtection: bigtable.Unprotected}); err != nil {
		t.Fatal(err)
	}
	if err := iac.DeleteLogicalView(ctx, id, "lv"); err != nil {
		t.Errorf("DeleteLogicalView: %v", err)
	}
	if _, err := iac.LogicalViewInfo(ctx, id, "lv"); status.Code(err) != codes.NotFound {
		t.Errorf("LogicalViewInfo after DeleteLogicalView = %v, want NotFound", err)
	}

	if err := iac.CreateMaterializedView(ctx, id, &bigtable.MaterializedViewInfo{MaterializedViewID: "mv", Query: q}); err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	if err := iac.UpdateMaterializedView(ctx, id, bigtable.MaterializedViewInfo{MaterializedViewID: "mv", Query: "SELECT 1"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("changing a materialized view's query = %v, want InvalidArgument", err)
	}
	if err := iac.UpdateMaterializedView(ctx, id, bigtable.MaterializedViewInfo{MaterializedViewID: "mv", DeletionProtection: bigtable.Protected}); err != nil {
		t.Errorf("UpdateMaterializedView: %v", err)
	}
	if mv, err := iac.MaterializedViewInfo(ctx, id, "mv"); err != nil || mv.Query != q || mv.DeletionProtection != bigtable.Protected {
		t.Errorf("MaterializedViewInfo = %+v, %v", mv, err)
	}
	if mvs, err := iac.MaterializedViews(ctx, id); err != nil || len(mvs) != 1 {
		t.Errorf("MaterializedViews = %+v, %v", mvs, err)
	}
	if err := iac.UpdateMaterializedView(ctx, id, bigtable.MaterializedViewInfo{MaterializedViewID: "mv", DeletionProtection: bigtable.Unprotected}); err != nil {
		t.Fatal(err)
	}
	if err := iac.DeleteMaterializedView(ctx, id, "mv"); err != nil {
		t.Errorf("DeleteMaterializedView: %v", err)
	}
}

// TestBigtableMemoryLayersAndHotTablets: a cluster's memory layer, through
// the module's generated instance admin stub (the veneer has no call for
// it), is disabled until UpdateMemoryLayer enables it; ListHotTablets
// answers none, since the emulator splits no table into tablets (#1499).
//
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/GetMemoryLayer, google.bigtable.admin.v2.BigtableInstanceAdmin/ListMemoryLayers, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateMemoryLayer, google.bigtable.admin.v2.BigtableInstanceAdmin/ListHotTablets
func TestBigtableMemoryLayersAndHotTablets(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id = "cb-inst-mem"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Memory layer",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := adminpb.NewBigtableInstanceAdminClient(conn)
	cluster := "projects/" + h.Project() + "/instances/" + id + "/clusters/" + id + "-c1"
	l, err := c.GetMemoryLayer(ctx, &adminpb.GetMemoryLayerRequest{Name: cluster + "/memoryLayer"})
	if err != nil || l.GetMemoryConfig() != nil {
		t.Fatalf("GetMemoryLayer = %v, %v; want disabled", l, err)
	}
	op, err := c.UpdateMemoryLayer(ctx, &adminpb.UpdateMemoryLayerRequest{
		MemoryLayer: &adminpb.MemoryLayer{Name: l.GetName(), MemoryConfig: &adminpb.MemoryLayer_MemoryConfig{}, Etag: l.GetEtag()},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"memory_config"}}})
	if err != nil || !op.GetDone() {
		t.Fatalf("UpdateMemoryLayer = %v, %v", op, err)
	}
	ls, err := c.ListMemoryLayers(ctx, &adminpb.ListMemoryLayersRequest{Parent: "projects/" + h.Project() + "/instances/" + id + "/clusters/-"})
	if err != nil || len(ls.GetMemoryLayers()) != 1 || ls.GetMemoryLayers()[0].GetMemoryConfig() == nil {
		t.Errorf("ListMemoryLayers = %v, %v; want one enabled", ls, err)
	}
	if ht, err := c.ListHotTablets(ctx, &adminpb.ListHotTabletsRequest{Parent: cluster}); err != nil || len(ht.GetHotTablets()) != 0 {
		t.Errorf("ListHotTablets = %v, %v; want none", ht, err)
	}
}

// TestConsoleBigtableListsInstances: the Bigtable screen's listing names the
// instances created through the instance admin API (#1331), with their
// display name, state and cluster count, and says so when there are none.
func TestConsoleBigtableListsInstances(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	note := func() string {
		t.Helper()
		code, out := consoleDo(t, addr, http.MethodGet, "/api/resources/bigtable?project="+h.Project(), "")
		if code != http.StatusOK {
			t.Fatalf("console listing = %d: %s", code, out)
		}
		var l struct {
			Note string `json:"note"`
		}
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			t.Fatalf("the listing is not JSON: %v: %s", err, out)
		}
		return l.Note
	}
	if n := note(); !strings.Contains(n, "No instance has been created") {
		t.Errorf("the note before CreateInstance = %q", n)
	}
	const id = "cb-console-inst"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Console instance",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	if n := note(); !strings.Contains(n, id+` ("Console instance", ready, 1 cluster(s))`) {
		t.Errorf("the note after CreateInstance = %q", n)
	}
}
