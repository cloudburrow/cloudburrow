//go:build compat

package compat

import (
	"context"
	"testing"

	"cloud.google.com/go/bigtable"
)

// bigtableCustomMessage is the Bigtable page's wording, "Operation denied
// by custom org policy: [...]".
func bigtableCustomMessage(short, desc string) string {
	return `Operation denied by custom org policy: ["customConstraints/` + short + `":"` + desc + `"]`
}

// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateInstance, google.bigtable.admin.v2.BigtableInstanceAdmin/CreateCluster, google.bigtable.admin.v2.BigtableInstanceAdmin/CreateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateAppProfile, google.bigtable.admin.v2.BigtableTableAdmin/CreateTable
//
// TestOrgPolicyCustomConstraintsBigtable (#1895), with the official
// Bigtable admin clients and the Bigtable page's three examples: clusters
// in the US only (ALLOW resource.location.contains('/locations/us-'), on
// bigtableadmin.googleapis.com/Cluster) refuses an instance whose cluster
// is in Europe and a European cluster added to a US instance; tables with
// deletion protection only (ALLOW resource.deletionProtection == true)
// refuses an unprotected table and creates a protected one; app profiles
// must allow transactional writes (DENY
// resource.singleClusterRouting.allowTransactionalWrites != true, CREATE
// and UPDATE) refuses a profile without them and an update turning them
// off. Each refusal is FAILED_PRECONDITION "Operation denied by custom
// org policy: [...]".
func TestOrgPolicyCustomConstraintsBigtable(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envBigQueryResourceManager, EnvResourceManager)
	ctx := h.Context()
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocb-")
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })

	us := customName(h, "btclustersus")
	usDesc := "All new clusters must be in the US."
	createCustom(t, h, c, us, "bigtableadmin.googleapis.com/Cluster", "resource.location.contains('/locations/us-')", ccAllow, usDesc, ccCreate)
	protected := customName(h, "btprotected")
	pDesc := "All tables must have deletion_protection set to true."
	createCustom(t, h, c, protected, "bigtableadmin.googleapis.com/Table", "resource.deletionProtection == true", ccAllow, pDesc, ccCreate)
	txn := customName(h, "bttxnwrites")
	tDesc := "App profiles must allow transactional writes."
	createCustom(t, h, c, txn, "bigtableadmin.googleapis.com/AppProfile", "resource.singleClusterRouting.allowTransactionalWrites != true", ccDeny,
		tDesc, ccCreate, ccUpdate)
	for _, n := range []string{us, protected, txn} {
		setOrgPolicy(t, h, c, "projects/"+project, n, opEnforce())
	}

	err = iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "cc-eu-inst", DisplayName: "cc eu", ClusterId: "cc-eu-c1",
		Zone: "europe-west1-b", NumNodes: 1, StorageType: bigtable.SSD})
	wantFailedPrecondition(t, "an instance whose cluster is in Europe", err, bigtableCustomMessage(us, usDesc))
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "cc-us-inst", DisplayName: "cc us", ClusterId: "cc-us-c1",
		Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("an instance in us-central1-b: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), "cc-us-inst") })
	err = iac.CreateCluster(ctx, &bigtable.ClusterConfig{InstanceID: "cc-us-inst", ClusterID: "cc-us-eu", Zone: "europe-west1-b",
		NumNodes: 1, StorageType: bigtable.SSD})
	wantFailedPrecondition(t, "a cluster in Europe", err, bigtableCustomMessage(us, usDesc))

	ac, err := bigtable.NewAdminClient(ctx, project, "cc-us-inst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	err = ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "cc-open"})
	wantFailedPrecondition(t, "a table without deletion protection", err, bigtableCustomMessage(protected, pDesc))
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "cc-kept", DeletionProtection: bigtable.Protected}); err != nil {
		t.Errorf("a table with deletion protection: %v", err)
	} else {
		t.Cleanup(func() {
			_ = ac.UpdateTableWithDeletionProtection(context.Background(), "cc-kept", bigtable.Unprotected)
			_ = ac.DeleteTable(context.Background(), "cc-kept")
		})
	}

	_, err = iac.CreateAppProfile(ctx, bigtable.ProfileConf{InstanceID: "cc-us-inst", ProfileID: "cc-plain",
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "cc-us-c1"}})
	wantFailedPrecondition(t, "a profile without transactional writes", err, bigtableCustomMessage(txn, tDesc))
	if _, err := iac.CreateAppProfile(ctx, bigtable.ProfileConf{InstanceID: "cc-us-inst", ProfileID: "cc-txn",
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "cc-us-c1", AllowTransactionalWrites: true}}); err != nil {
		t.Fatalf("a profile with transactional writes: %v", err)
	}
	err = iac.UpdateAppProfile(ctx, "cc-us-inst", "cc-txn", bigtable.ProfileAttrsToUpdate{
		RoutingConfig: &bigtable.SingleClusterRoutingConfig{ClusterID: "cc-us-c1"}, IgnoreWarnings: true})
	wantFailedPrecondition(t, "turning transactional writes off", err, bigtableCustomMessage(txn, tDesc))
}
