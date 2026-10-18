//go:build compat

package compat

import (
	"bytes"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestKMSEkmIamPolicyIsStoredNotEnforced (#1469; ADR-0006): IAM on a
// location's ekmConfig and on an EkmConnection, through the official
// EkmClient's IAMPolicy methods over gRPC and REST. The ekmConfig singleton
// starts with an empty policy, as a new connection does; a policy round-trips
// with etags and a stale etag is ABORTED; TestIamPermissions returns
// everything asked, since nothing is enforced. Setting a policy leaves the
// connection's own etag alone, and UpdateEkmConfig keeps the config's policy.
//
// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy, google.iam.v1.IAMPolicy/TestIamPermissions
// covers: google.cloud.kms.v1.EkmService/CreateEkmConnection, google.cloud.kms.v1.EkmService/GetEkmConnection, google.cloud.kms.v1.EkmService/UpdateEkmConfig
//
// unverified: google.iam.v1.IAMPolicy/GetIamPolicy NOT_FOUND: an EkmConnection that does not exist
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/iam/v1/iam_policy.proto
// unverified: google.iam.v1.IAMPolicy/SetIamPolicy ABORTED: a stale etag on an EkmConnection or ekmConfig
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/iam/v1/policy.proto
func TestKMSEkmIamPolicyIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	// Asked here, not first inside a subtest: an unserved KMS skips this
	// test, where the harness's Skip from a subtest failed it (#1362).
	h.Endpoint(EnvKMS)
	ctx := h.Context()
	der := ekmCertificate(t)
	const member = "user:ekm-admin@example.com"
	for _, variant := range []string{"grpc", "rest"} {
		t.Run(variant, func(t *testing.T) {
			h := h.For(t)
			newClient := kms.NewEkmClient
			if variant == "rest" {
				newClient = kms.NewEkmRESTClient
			}
			c, err := newClient(ctx, kmsOptions(h, variant)...)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			loc := "projects/" + h.Project() + "/locations/europe-west1"
			if variant == "rest" {
				loc = "projects/" + h.Project() + "/locations/europe-west4"
			}
			conn, err := c.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "ekm-iam",
				EkmConnection: &kmspb.EkmConnection{ServiceResolvers: []*kmspb.EkmConnection_ServiceResolver{{
					ServiceDirectoryService: "projects/" + h.Project() + "/locations/europe-west1/namespaces/ekm/services/replica",
					Hostname:                "ekm.example.com",
					ServerCertificates:      []*kmspb.Certificate{{RawDer: der}},
				}}}})
			if err != nil {
				t.Fatal(err)
			}
			cfgName := loc + "/ekmConfig"
			for kind, res := range map[string]string{"ekmConfig": cfgName, "ekmConnection": conn.GetName()} {
				t.Run(kind, func(t *testing.T) {
					empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
					if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
						t.Fatalf("GetIamPolicy on a new %s = %v, %v; want the empty policy with an etag", kind, empty, err)
					}
					set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag(),
						Bindings: []*iampb.Binding{{Role: "roles/cloudkms.admin", Members: []string{member}}}}})
					if err != nil || bytes.Equal(set.GetEtag(), empty.GetEtag()) {
						t.Fatalf("SetIamPolicy = %v, %v; want a new etag", set, err)
					}
					got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
					if err != nil || !bytes.Equal(got.GetEtag(), set.GetEtag()) || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != member {
						t.Errorf("GetIamPolicy after Set = %v, %v", got, err)
					}
					if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); kmsCode(variant, err) != codes.Aborted {
						t.Errorf("a stale etag = %v, want ABORTED", err)
					}
					perms := []string{"cloudkms.ekmConnections.use", "cloudkms.ekmConfigs.get"}
					if r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: perms}); err != nil || len(r.GetPermissions()) != len(perms) {
						t.Errorf("TestIamPermissions = %v, %v; want all of %v, since nothing is enforced", r, err, perms)
					}
				})
			}
			if got, err := c.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: conn.GetName()}); err != nil || got.GetEtag() != conn.GetEtag() {
				t.Errorf("the connection after SetIamPolicy = %v, %v; want its etag %q unchanged", got, err, conn.GetEtag())
			}
			if _, err := c.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{EkmConfig: &kmspb.EkmConfig{Name: cfgName, DefaultEkmConnection: conn.GetName()},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}}); err != nil {
				t.Fatalf("UpdateEkmConfig = %v", err)
			}
			if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: cfgName}); err != nil || len(got.GetBindings()) != 1 {
				t.Errorf("the ekmConfig policy after UpdateEkmConfig = %v, %v; want it kept", got, err)
			}
			if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: loc + "/ekmConnections/absent"}); kmsCode(variant, err) != codes.NotFound {
				t.Errorf("GetIamPolicy on an absent connection = %v, want NOT_FOUND", err)
			}
		})
	}
}
