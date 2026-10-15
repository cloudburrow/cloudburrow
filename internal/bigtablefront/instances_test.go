package bigtablefront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInstanceAdmin drives instances, clusters, app profiles and an
// instance's IAM policy through the official InstanceAdminClient, and checks
// that deleting an instance deletes its tables.
func TestInstanceAdmin(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dial(t, addr)
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iac.InstanceInfo(ctx, "missing-inst"); status.Code(err) != codes.NotFound {
		t.Fatalf("InstanceInfo of a missing instance: %v, want NotFound", err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "my-inst", DisplayName: "My instance",
		ClusterId: "my-inst-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD,
		Labels: map[string]string{"env": "dev"}}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "my-inst", DisplayName: "My instance",
		ClusterId: "my-inst-c1", Zone: "us-central1-b", NumNodes: 1}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("second CreateInstance: %v, want AlreadyExists", err)
	}
	info, err := iac.InstanceInfo(ctx, "my-inst")
	if err != nil {
		t.Fatal(err)
	}
	if info.DisplayName != "My instance" || info.InstanceState != bigtable.Ready || info.InstanceType != bigtable.PRODUCTION || info.Labels["env"] != "dev" {
		t.Errorf("InstanceInfo = %+v", info)
	}
	if err := iac.UpdateInstanceWithClusters(ctx, &bigtable.InstanceWithClustersConfig{InstanceID: "my-inst",
		DisplayName: "Renamed", Clusters: []bigtable.ClusterConfig{{ClusterID: "my-inst-c1", NumNodes: 3}}}); err != nil {
		t.Fatalf("UpdateInstanceWithClusters: %v", err)
	}
	if err := iac.CreateCluster(ctx, &bigtable.ClusterConfig{InstanceID: "my-inst", ClusterID: "my-inst-c2",
		Zone: "us-east1-c", AutoscalingConfig: &bigtable.AutoscalingConfig{MinNodes: 1, MaxNodes: 4, CPUTargetPercent: 60}}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	cs, err := iac.Clusters(ctx, "my-inst")
	if err != nil || len(cs) != 2 {
		t.Fatalf("Clusters = %v, %v", cs, err)
	}
	if cs[0].ServeNodes != 3 || cs[0].Zone != "us-central1-b" || cs[0].State != "READY" {
		t.Errorf("cluster 1 = %+v", cs[0])
	}
	if cs[1].AutoscalingConfig == nil || cs[1].AutoscalingConfig.MaxNodes != 4 {
		t.Errorf("cluster 2 = %+v", cs[1])
	}
	if err := iac.SetAutoscaling(ctx, "my-inst", "my-inst-c2", bigtable.AutoscalingConfig{MinNodes: 2, MaxNodes: 5, CPUTargetPercent: 50}); err != nil {
		t.Fatalf("SetAutoscaling: %v", err)
	}
	if c, err := iac.GetCluster(ctx, "my-inst", "my-inst-c2"); err != nil || c.AutoscalingConfig.MinNodes != 2 || c.ServeNodes != 2 {
		t.Fatalf("GetCluster = %+v, %v", c, err)
	}

	// App profiles: the default one, and one routed to a cluster.
	if _, err := iac.CreateAppProfile(ctx, bigtable.ProfileConf{InstanceID: "my-inst", ProfileID: "batch",
		Description: "batch jobs", RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "my-inst-c2"}}); err != nil {
		t.Fatalf("CreateAppProfile: %v", err)
	}
	if err := iac.DeleteCluster(ctx, "my-inst", "my-inst-c2"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteCluster of a routed-to cluster: %v, want FailedPrecondition", err)
	}
	if err := iac.UpdateAppProfile(ctx, "my-inst", "batch", bigtable.ProfileAttrsToUpdate{Description: "nightly",
		RoutingConfig: &bigtable.MultiClusterRoutingUseAnyConfig{}}); err != nil {
		t.Fatalf("UpdateAppProfile: %v", err)
	}
	p, err := iac.GetAppProfile(ctx, "my-inst", "batch")
	if err != nil || p.Description != "nightly" || p.GetMultiClusterRoutingUseAny() == nil {
		t.Fatalf("GetAppProfile = %v, %v", p, err)
	}
	var ids []string
	it := iac.ListAppProfiles(ctx, "my-inst")
	for {
		p, err := it.Next()
		if err != nil {
			break
		}
		ids = append(ids, p.Name)
	}
	if len(ids) != 2 {
		t.Errorf("ListAppProfiles = %v, want batch and default", ids)
	}
	if err := iac.DeleteAppProfile(ctx, "my-inst", "batch"); err != nil {
		t.Fatalf("DeleteAppProfile: %v", err)
	}
	if err := iac.DeleteCluster(ctx, "my-inst", "my-inst-c2"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if err := iac.DeleteCluster(ctx, "my-inst", "my-inst-c1"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteCluster of the last cluster: %v, want FailedPrecondition", err)
	}

	// IAM, stored and not enforced.
	h := iac.InstanceIAM("my-inst")
	pol, err := h.Policy(ctx)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	pol.Add("user:a@example.com", iam.Viewer)
	if err := h.SetPolicy(ctx, pol); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if pol, err = h.Policy(ctx); err != nil || !pol.HasRole("user:a@example.com", iam.Viewer) {
		t.Fatalf("Policy after SetPolicy = %v, %v", pol, err)
	}

	// A table in the instance is deleted with it.
	ac, err := bigtable.NewAdminClient(ctx, "p", "my-inst", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	other, err := bigtable.NewAdminClient(ctx, "p", "other-inst", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.CreateTable(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	insts, err := iac.Instances(ctx)
	if err != nil || len(insts) != 1 || insts[0].DisplayName != "Renamed" {
		t.Fatalf("Instances = %v, %v", insts, err)
	}
	if err := iac.DeleteInstance(ctx, "my-inst"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if tables, err := ac.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("tables after DeleteInstance = %v, %v", tables, err)
	}
	if tables, err := other.Tables(ctx); err != nil || len(tables) != 1 {
		t.Errorf("another instance's tables = %v, %v", tables, err)
	}
	if _, err := iac.InstanceInfo(ctx, "my-inst"); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after DeleteInstance: %v, want NotFound", err)
	}
	if err := iac.DeleteInstance(ctx, "my-inst"); status.Code(err) != codes.NotFound {
		t.Errorf("second DeleteInstance: %v, want NotFound", err)
	}
	// The methods neither the emulator nor the front implement are refused,
	// not forwarded to a panic.
	if _, err := iac.LogicalViews(ctx, "my-inst"); status.Code(err) != codes.Unimplemented {
		t.Errorf("LogicalViews: %v, want Unimplemented", err)
	}
}
