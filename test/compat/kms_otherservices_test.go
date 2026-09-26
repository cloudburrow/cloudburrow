//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestKMSOtherServicesAreUnimplemented: cloudkms.googleapis.com also serves
// the Locations mixin, EkmService, Autokey, AutokeyAdmin and HsmManagement.
// CloudBurrow serves none of them, and each answers UNIMPLEMENTED through its
// official client (#397). The IAMPolicy mixin stores policies on key rings and
// crypto keys (#428, TestKMSIamPolicyIsStoredNotEnforced); on an import job,
// which CloudBurrow does not serve, it is UNIMPLEMENTED, never an empty
// policy.
func TestKMSOtherServicesAreUnimplemented(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := h.Endpoint(EnvKMS)
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	c := kmsClients(t, h)["grpc"]
	loc := "projects/" + h.Project() + "/locations/global"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "otherservices"})
	if err != nil {
		t.Fatal(err)
	}
	ekm, err := kms.NewEkmClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ekm.Close()
	autokey, err := kms.NewAutokeyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer autokey.Close()
	admin, err := kms.NewAutokeyAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	hsm, err := kms.NewHsmManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer hsm.Close()

	calls := map[string]func(context.Context) error{}
	job := ring.GetName() + "/importJobs/j"
	calls["GetIamPolicy on an import job"] = func(ctx context.Context) error {
		_, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job})
		return err
	}
	calls["SetIamPolicy on an import job"] = func(ctx context.Context) error {
		_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job, Policy: &iampb.Policy{}})
		return err
	}
	calls["TestIamPermissions on an import job"] = func(ctx context.Context) error {
		_, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: job, Permissions: []string{"cloudkms.importJobs.get"}})
		return err
	}
	calls["ListLocations"] = func(ctx context.Context) error {
		_, err := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + h.Project()}).Next()
		return err
	}
	calls["GetLocation"] = func(ctx context.Context) error {
		_, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: loc})
		return err
	}
	calls["EkmService/ListEkmConnections"] = func(ctx context.Context) error {
		_, err := ekm.ListEkmConnections(ctx, &kmspb.ListEkmConnectionsRequest{Parent: loc}).Next()
		return err
	}
	calls["Autokey/ListKeyHandles"] = func(ctx context.Context) error {
		_, err := autokey.ListKeyHandles(ctx, &kmspb.ListKeyHandlesRequest{Parent: loc}).Next()
		return err
	}
	calls["AutokeyAdmin/GetAutokeyConfig"] = func(ctx context.Context) error {
		_, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: "folders/123/autokeyConfig"})
		return err
	}
	calls["HsmManagement/ListSingleTenantHsmInstances"] = func(ctx context.Context) error {
		_, err := hsm.ListSingleTenantHsmInstances(ctx, &kmspb.ListSingleTenantHsmInstancesRequest{Parent: loc}).Next()
		return err
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
