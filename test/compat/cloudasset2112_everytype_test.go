//go:build compat

package compat

import (
	"context"
	"slices"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigtable"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/protobuf/proto"
)

// TestCloudAssetAnalyzeIamPolicyEveryAssetType (#2112): on one instance
// serving them all, a binding for one principal on a resource of each
// asset type added in round 3, and of BigQuery, Spanner and Bigtable, is
// found by one analysis of the resource's project with no
// resourceSelector, attached to the resource by its Cloud Asset Inventory
// full resource name: BigQuery datasets (access entries), tables,
// routines and row access policies (their grantees); Spanner instances and
// databases; Bigtable instances, tables and backups; Cloud KMS key rings,
// keys and import jobs; a Compute Engine subnetwork; and a project's tag
// key and tag value. Each is set through the product's official client.
//
// covers: compute.subnetworks.getIamPolicy, compute.subnetworks.setIamPolicy, compute.subnetworks.testIamPermissions
func TestCloudAssetAnalyzeIamPolicyEveryAssetType(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	spAddr := h.Endpoint(EnvSpanner)
	btAddr := h.Endpoint(EnvBigtable)
	h.Endpoint(EnvKMS)
	copts := computeOpts(t, h)
	bqc, bqProject := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()
	// Every resource is in the project BigQuery's emulator serves, which
	// the instance's Resource Manager keeps, so one analysis of it finds
	// them all.
	project := bqProject
	suffix := v1ID(h, "")
	email := "every-" + suffix + "@example.com"
	member := "user:" + email
	// want are the full resource names each project's analysis must find
	// the member's bindings attached to.
	want := map[string][]string{}
	add := func(p, full string) { want[p] = append(want[p], full) }

	// BigQuery: a dataset's access entry, a table's and a routine's
	// policies, and a row access policy's grantee.
	dsID := "every_" + suffix
	ds := bqc.Dataset(dsID)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Access: []*bigquery.AccessEntry{
		{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: email}}}); err != nil {
		t.Fatalf("dataset create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	tbl := ds.Table("t")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("table create: %v", err)
	}
	tp, err := tbl.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("table policy: %v", err)
	}
	tp.Add(member, "roles/bigquery.dataViewer")
	if err := tbl.IAM().SetPolicy(ctx, tp); err != nil {
		t.Fatalf("table SetPolicy: %v", err)
	}
	if _, err := bqc.Query("CREATE FUNCTION `" + bqProject + "." + dsID + ".plus_one`(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	routine := "projects/" + bqProject + "/datasets/" + dsID + "/routines/plus_one"
	rp, err := svc.Routines.GetIamPolicy(routine, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("routine getIamPolicy: %v", err)
	}
	if _, err := svc.Routines.SetIamPolicy(routine, &bq.SetIamPolicyRequest{Policy: &bq.Policy{Etag: rp.Etag,
		Bindings: []*bq.Binding{{Role: "roles/bigquery.dataViewer", Members: []string{member}}}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("routine setIamPolicy: %v", err)
	}
	if _, err := bqc.Query("CREATE ROW ACCESS POLICY rap ON `" + bqProject + "." + dsID + ".t` GRANT TO ('" + member + "') FILTER USING (TRUE)").Read(ctx); err != nil {
		t.Fatalf("CREATE ROW ACCESS POLICY: %v", err)
	}
	bqBase := "//bigquery.googleapis.com/projects/" + bqProject + "/datasets/" + dsID
	add(bqProject, bqBase)
	add(bqProject, bqBase+"/tables/t")
	add(bqProject, bqBase+"/routines/plus_one")
	add(bqProject, bqBase+"/tables/t/rowAccessPolicies/rap")

	// Spanner: an instance's and a database's policies.
	spInst, spDB := "every-"+suffix, "everydb"
	dbName := spannerDatabase(t, ctx, spAddr, project, spInst, spDB, `CREATE TABLE T (Id INT64 NOT NULL) PRIMARY KEY (Id)`)
	ic, err := instance.NewInstanceAdminClient(ctx, spannerOpts(spAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.Close() })
	dc, err := database.NewDatabaseAdminClient(ctx, spannerOpts(spAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	instName := "projects/" + project + "/instances/" + spInst
	t.Cleanup(bindOn(t, ic.GetIamPolicy, ic.SetIamPolicy, instName, &iampb.Binding{Role: "roles/spanner.viewer", Members: []string{member}}))
	t.Cleanup(bindOn(t, dc.GetIamPolicy, dc.SetIamPolicy, dbName, &iampb.Binding{Role: "roles/spanner.databaseReader", Members: []string{member}}))
	add(project, "//spanner.googleapis.com/"+instName)
	add(project, "//spanner.googleapis.com/"+dbName)

	// Bigtable: an instance's, a table's and a backup's policies.
	t.Setenv("BIGTABLE_EMULATOR_HOST", btAddr)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	btInst, btCluster := "every-"+suffix, "every-"+suffix+"-c1"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: btInst, DisplayName: "every type",
		ClusterId: btCluster, Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("Bigtable CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), btInst) })
	bta, err := bigtable.NewAdminClient(ctx, project, btInst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bta.Close() })
	if err := bta.CreateTable(ctx, "bt"); err != nil {
		t.Fatalf("Bigtable CreateTable: %v", err)
	}
	if err := bta.CreateBackup(ctx, "bt", btCluster, "bk", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("Bigtable CreateBackup: %v", err)
	}
	t.Cleanup(func() { _ = bta.DeleteBackup(context.Background(), btCluster, "bk") })
	for _, x := range []struct {
		h    *iam.Handle
		role iam.RoleName
	}{
		{iac.InstanceIAM(btInst), "roles/bigtable.viewer"},
		{bta.TableIAM("bt"), "roles/bigtable.reader"},
		{bta.BackupIAM(btCluster, "bk"), "roles/bigtable.reader"},
	} {
		p, err := x.h.Policy(ctx)
		if err != nil {
			t.Fatalf("Bigtable policy: %v", err)
		}
		p.Add(member, x.role)
		if err := x.h.SetPolicy(ctx, p); err != nil {
			t.Fatalf("Bigtable SetPolicy: %v", err)
		}
	}
	btBase := "//bigtableadmin.googleapis.com/projects/" + project + "/instances/" + btInst
	add(project, btBase)
	add(project, btBase+"/tables/bt")
	add(project, btBase+"/clusters/"+btCluster+"/backups/bk")

	// Cloud KMS: a key ring's, a key's and an import job's policies.
	kc := kmsClients(t, h)["grpc"]
	ringParent := "projects/" + project + "/locations/global"
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: ringParent, KeyRingId: "every-" + suffix})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	job, err := kc.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil {
		t.Fatalf("CreateImportJob: %v", err)
	}
	for _, r := range []string{ring.GetName(), key.GetName(), job.GetName()} {
		p, err := kc.ResourceIAM(r).Policy(ctx)
		if err != nil {
			t.Fatalf("KMS policy of %s: %v", r, err)
		}
		p.Add(member, "roles/cloudkms.viewer")
		if err := kc.ResourceIAM(r).SetPolicy(ctx, p); err != nil {
			t.Fatalf("KMS SetPolicy of %s: %v", r, err)
		}
		add(project, "//cloudkms.googleapis.com/"+r)
	}

	// Compute Engine: the default network's subnetwork's policy.
	subs, err := computeapi.NewSubnetworksRESTClient(ctx, copts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subs.Close() })
	region, subID := "us-central1", "default"
	if _, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: project, Region: region, Subnetwork: subID}); err != nil {
		t.Fatalf("the default subnetwork: %v", err)
	}
	getSub := func() *computepb.Policy {
		t.Helper()
		p, err := subs.GetIamPolicy(ctx, &computepb.GetIamPolicySubnetworkRequest{Project: project, Region: region, Resource: subID})
		if err != nil {
			t.Fatalf("subnetworks.getIamPolicy: %v", err)
		}
		return p
	}
	setSub := func(p *computepb.Policy) (*computepb.Policy, error) {
		return subs.SetIamPolicy(ctx, &computepb.SetIamPolicySubnetworkRequest{Project: project, Region: region, Resource: subID,
			RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: p}})
	}
	sp := getSub()
	sp.Bindings = append(sp.Bindings, &computepb.Binding{Role: proto.String("roles/compute.networkUser"), Members: []string{member}})
	got, err := setSub(sp)
	if err != nil {
		t.Fatalf("subnetworks.setIamPolicy: %v", err)
	}
	if got.GetEtag() == sp.GetEtag() || len(got.GetBindings()) == 0 {
		t.Errorf("subnetworks.setIamPolicy = %v", got)
	}
	if _, err := setSub(sp); computeCode(err) != 409 {
		t.Errorf("a stale etag: %v, want 409", err)
	}
	perms, err := subs.TestIamPermissions(ctx, &computepb.TestIamPermissionsSubnetworkRequest{Project: project, Region: region, Resource: subID,
		TestPermissionsRequestResource: &computepb.TestPermissionsRequest{Permissions: []string{"compute.subnetworks.use"}}})
	if err != nil || len(perms.GetPermissions()) != 1 {
		t.Errorf("subnetworks.testIamPermissions = %v, %v", perms, err)
	}
	subnet := "projects/" + project + "/regions/" + region + "/subnetworks/" + subID
	t.Cleanup(func() {
		if cur, err := subs.GetIamPolicy(context.Background(), &computepb.GetIamPolicySubnetworkRequest{Project: project, Region: region, Resource: subID}); err == nil {
			_, _ = subs.SetIamPolicy(context.Background(), &computepb.SetIamPolicySubnetworkRequest{Project: project, Region: region, Resource: subID,
				RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: &computepb.Policy{Etag: cur.Etag}}})
		}
	})
	add(project, "//compute.googleapis.com/"+subnet)

	// Resource Manager: a project's tag key and its value.
	tc := newTagClients(t, h)
	kop, err := tc.keys.CreateTagKey(ctx, &resourcemanagerpb.CreateTagKeyRequest{TagKey: &resourcemanagerpb.TagKey{
		Parent: "projects/" + project, ShortName: "every-" + suffix}})
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	tk, err := kop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	vop, err := tc.values.CreateTagValue(ctx, &resourcemanagerpb.CreateTagValueRequest{TagValue: &resourcemanagerpb.TagValue{
		Parent: tk.GetName(), ShortName: "v"}})
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	tv, err := vop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	t.Cleanup(func() {
		if op, err := tc.values.DeleteTagValue(context.Background(), &resourcemanagerpb.DeleteTagValueRequest{Name: tv.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
		if op, err := tc.keys.DeleteTagKey(context.Background(), &resourcemanagerpb.DeleteTagKeyRequest{Name: tk.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	t.Cleanup(bindOn(t, tc.keys.GetIamPolicy, tc.keys.SetIamPolicy, tk.GetName(), &iampb.Binding{Role: "roles/resourcemanager.tagViewer", Members: []string{member}}))
	t.Cleanup(bindOn(t, tc.values.GetIamPolicy, tc.values.SetIamPolicy, tv.GetName(), &iampb.Binding{Role: "roles/resourcemanager.tagUser", Members: []string{member}}))
	add(project, "//cloudresourcemanager.googleapis.com/"+tk.GetName())
	add(project, "//cloudresourcemanager.googleapis.com/"+tv.GetName())

	// One analysis per project, of the member's access with no
	// resourceSelector.
	c := assetClient(t, h, rmEndpoint)
	projects := make([]string, 0, len(want))
	for p := range want {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		resp, err := c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{
			Scope: "projects/" + p, IdentitySelector: &assetpb.IamPolicyAnalysisQuery_IdentitySelector{Identity: member}}})
		if err != nil {
			t.Fatalf("AnalyzeIamPolicy of %s: %v", p, err)
		}
		var attached []string
		for _, r := range resp.GetMainAnalysis().GetAnalysisResults() {
			attached = append(attached, r.GetAttachedResourceFullName())
		}
		for _, w := range want[p] {
			if !slices.Contains(attached, w) {
				t.Errorf("no binding of %s attached to %s; attached: %v", member, w, attached)
			}
		}
	}
}
