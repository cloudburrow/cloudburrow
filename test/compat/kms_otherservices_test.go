//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
)

// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy, google.iam.v1.IAMPolicy/TestIamPermissions
//
// TestKMSImportJobIamPolicy (#1946): of the services cloudkms.googleapis.com
// serves beside KeyManagementService, EkmService, Autokey and AutokeyAdmin
// are served since #1452 (kms_hsm_ekm_autokey_test.go), HsmManagement since
// #1469 (kms_hsm_management_test.go), the Locations mixin since #1291, and
// IAM on import jobs since #1946 (importJobs.getIamPolicy, setIamPolicy and
// testIamPermissions): through the official client over gRPC and REST, a
// job's policy is set, read back with its etag and kept; an absent job is
// NOT_FOUND, and its TestIamPermissions the empty set, as for a ring.
func TestKMSImportJobIamPolicy(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "ijiam-"+variant)
			absent := ring + "/importJobs/absent"
			if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: absent}); kmsCode(variant, err) != codes.NotFound {
				t.Errorf("GetIamPolicy on a missing job = %v, want NOT_FOUND", err)
			}
			if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: absent, Policy: &iampb.Policy{}}); kmsCode(variant, err) != codes.NotFound {
				t.Errorf("SetIamPolicy on a missing job = %v, want NOT_FOUND", err)
			}
			if r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: absent, Permissions: []string{"cloudkms.importJobs.get"}}); err != nil || len(r.GetPermissions()) != 0 {
				t.Errorf("TestIamPermissions on a missing job = %v, %v; want an empty set", r, err)
			}
			job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring, ImportJobId: "j",
				ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
			if err != nil {
				t.Fatal(err)
			}
			cur, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()})
			if err != nil || len(cur.GetBindings()) != 0 {
				t.Fatalf("GetIamPolicy of a new job = %v, %v", cur, err)
			}
			const member = "user:importer@example.com"
			set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job.GetName(), Policy: &iampb.Policy{Etag: cur.GetEtag(),
				Bindings: []*iampb.Binding{{Role: "roles/cloudkms.importer", Members: []string{member}}}}})
			if err != nil || len(set.GetEtag()) == 0 {
				t.Fatalf("SetIamPolicy = %v, %v", set, err)
			}
			got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()})
			if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != member || string(got.GetEtag()) != string(set.GetEtag()) {
				t.Errorf("GetIamPolicy = %v, %v; want the importer binding", got, err)
			}
			if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job.GetName(), Policy: &iampb.Policy{Etag: cur.GetEtag()}}); kmsCode(variant, err) != codes.Aborted {
				t.Errorf("SetIamPolicy with a stale etag = %v, want ABORTED", err)
			}
			r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: job.GetName(), Permissions: []string{"cloudkms.importJobs.useToImport"}})
			if err != nil || len(r.GetPermissions()) != 1 {
				t.Errorf("TestIamPermissions = %v, %v", r, err)
			}
			if j, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job.GetName()}); err != nil || j.GetState() != kmspb.ImportJob_ACTIVE {
				t.Errorf("GetImportJob after SetIamPolicy = %v, %v", j, err)
			}
		})
	}
}
