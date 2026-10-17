//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestKMSOtherServicesAreUnimplemented: of the services
// cloudkms.googleapis.com serves beside KeyManagementService, EkmService,
// Autokey and AutokeyAdmin are served since #1452
// (kms_hsm_ekm_autokey_test.go), HsmManagement since #1469
// (kms_hsm_management_test.go), and the Locations mixin since #1291. What
// stays UNIMPLEMENTED, through the official client: IAM on import jobs,
// whose records keep no policy, never an empty policy. The IAMPolicy mixin stores policies on key
// rings and crypto keys (#428, TestKMSIamPolicyIsStoredNotEnforced), and on
// ekmConfig and EKM connections (#1469, TestKMSEkmIamPolicyIsStoredNotEnforced).
func TestKMSOtherServicesAreUnimplemented(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := kmsClients(t, h)["grpc"]
	loc := "projects/" + h.Project() + "/locations/global"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "otherservices"})
	if err != nil {
		t.Fatal(err)
	}
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
	for name, call := range calls {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := call(cctx)
		cancel()
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("%s = %v, want UNIMPLEMENTED", name, err)
		}
	}
}
