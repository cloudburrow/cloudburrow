//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestKMSOtherServicesAreUnimplemented: of the services
// cloudkms.googleapis.com serves beside KeyManagementService, EkmService,
// Autokey and AutokeyAdmin are served since #1452
// (kms_hsm_ekm_autokey_test.go), and the Locations mixin since #1291. What
// stays UNIMPLEMENTED, through each official client: every HsmManagement
// method, since a single-tenant HSM instance is hardware administered by
// quorum-approved proposals; and IAM on import jobs, whose records keep no
// policy, never an empty policy. The IAMPolicy mixin stores policies on key
// rings and crypto keys (#428, TestKMSIamPolicyIsStoredNotEnforced), and on
// ekmConfig and EKM connections (#1469, TestKMSEkmIamPolicyIsStoredNotEnforced).
//
// covers: google.cloud.kms.v1.HsmManagement/ListSingleTenantHsmInstances (unimplemented), google.cloud.kms.v1.HsmManagement/GetSingleTenantHsmInstance (unimplemented), google.cloud.kms.v1.HsmManagement/CreateSingleTenantHsmInstance (unimplemented)
// covers: google.cloud.kms.v1.HsmManagement/CreateSingleTenantHsmInstanceProposal (unimplemented), google.cloud.kms.v1.HsmManagement/ApproveSingleTenantHsmInstanceProposal (unimplemented), google.cloud.kms.v1.HsmManagement/ExecuteSingleTenantHsmInstanceProposal (unimplemented)
// covers: google.cloud.kms.v1.HsmManagement/GetSingleTenantHsmInstanceProposal (unimplemented), google.cloud.kms.v1.HsmManagement/ListSingleTenantHsmInstanceProposals (unimplemented), google.cloud.kms.v1.HsmManagement/DeleteSingleTenantHsmInstanceProposal (unimplemented)
func TestKMSOtherServicesAreUnimplemented(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := kmsClients(t, h)["grpc"]
	loc := "projects/" + h.Project() + "/locations/global"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "otherservices"})
	if err != nil {
		t.Fatal(err)
	}
	hsm, err := kms.NewHsmManagementClient(ctx, kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	defer hsm.Close()

	calls := map[string]func(context.Context) error{}
	for _, res := range []string{ring.GetName() + "/importJobs/j"} {
		calls["GetIamPolicy on "+res] = func(ctx context.Context) error {
			_, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
			return err
		}
		calls["SetIamPolicy on "+res] = func(ctx context.Context) error {
			_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{}})
			return err
		}
		calls["TestIamPermissions on "+res] = func(ctx context.Context) error {
			_, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: []string{"cloudkms.importJobs.get"}})
			return err
		}
	}
	inst := loc + "/singleTenantHsmInstances/i"
	prop := inst + "/proposals/p"
	calls["ListSingleTenantHsmInstances"] = func(ctx context.Context) error {
		_, err := hsm.ListSingleTenantHsmInstances(ctx, &kmspb.ListSingleTenantHsmInstancesRequest{Parent: loc}).Next()
		return err
	}
	calls["GetSingleTenantHsmInstance"] = func(ctx context.Context) error {
		_, err := hsm.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst})
		return err
	}
	calls["CreateSingleTenantHsmInstance"] = func(ctx context.Context) error {
		_, err := hsm.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: loc, SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{}})
		return err
	}
	calls["CreateSingleTenantHsmInstanceProposal"] = func(ctx context.Context) error {
		_, err := hsm.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst,
			SingleTenantHsmInstanceProposal: &kmspb.SingleTenantHsmInstanceProposal{}})
		return err
	}
	calls["ApproveSingleTenantHsmInstanceProposal"] = func(ctx context.Context) error {
		_, err := hsm.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: prop})
		return err
	}
	calls["ExecuteSingleTenantHsmInstanceProposal"] = func(ctx context.Context) error {
		_, err := hsm.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: prop})
		return err
	}
	calls["GetSingleTenantHsmInstanceProposal"] = func(ctx context.Context) error {
		_, err := hsm.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: prop})
		return err
	}
	calls["ListSingleTenantHsmInstanceProposals"] = func(ctx context.Context) error {
		_, err := hsm.ListSingleTenantHsmInstanceProposals(ctx, &kmspb.ListSingleTenantHsmInstanceProposalsRequest{Parent: inst}).Next()
		return err
	}
	calls["DeleteSingleTenantHsmInstanceProposal"] = func(ctx context.Context) error {
		return hsm.DeleteSingleTenantHsmInstanceProposal(ctx, &kmspb.DeleteSingleTenantHsmInstanceProposalRequest{Name: prop})
	}
	for name, call := range calls {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := call(cctx)
		cancel()
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("%s = %v, want UNIMPLEMENTED", name, err)
		}
	}
}
