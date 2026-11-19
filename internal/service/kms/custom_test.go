package kms

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

type kmsCustoms struct{ list []orgpolicy.CustomConstraint }

func (kmsCustoms) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (f kmsCustoms) Customs(_ context.Context, _, resourceType, method string) ([]orgpolicy.CustomConstraint, error) {
	var out []orgpolicy.CustomConstraint
	for _, c := range f.list {
		if strings.HasPrefix(c.DisplayName, resourceType+" ") && strings.Contains(c.DisplayName, method) {
			out = append(out, c)
		}
	}
	return out, nil
}

// TestCustomConstraints (#1895): the KMS page's examples, a maximum
// rotation period on CryptoKey (CREATE, UPDATE) and HSM import jobs,
// refuse what they do not allow, FAILED_PRECONDITION "Operation denied
// by custom org policies: [...]"; an unset importOnly reads false; a
// field the page does not list (labels) cannot be evaluated.
func TestCustomConstraints(t *testing.T) {
	enforced := orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})
	orgpolicy.Use(kmsCustoms{list: []orgpolicy.CustomConstraint{
		{Name: "custom.rotation", Condition: "resource.rotationPeriod <= duration('7776000s')", ActionType: "ALLOW",
			DisplayName: orgpolicy.TypeKMSCryptoKey + " CREATE UPDATE", Description: "Rotate within 90 days.", Effective: enforced},
		{Name: "custom.notimport", Condition: "resource.importOnly == false", ActionType: "ALLOW",
			DisplayName: orgpolicy.TypeKMSCryptoKey + " CREATE", Description: "importOnly", Effective: enforced},
		{Name: "custom.hsm", Condition: "resource.protectionLevel == 'HSM'", ActionType: "ALLOW",
			DisplayName: orgpolicy.TypeKMSImportJob + " CREATE", Description: "HSM only.", Effective: enforced},
		{Name: "custom.byname", Condition: "resource.name.endsWith('/x')", ActionType: "DENY",
			DisplayName: orgpolicy.TypeKMSEkmConnection + " CREATE", Description: "name", Effective: enforced},
	}})
	defer orgpolicy.Use(nil)
	c := client(t)
	ctx := context.Background()
	if _, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "cc"}); err != nil {
		t.Fatal(err)
	}
	ring := loc + "/keyRings/cc"
	key := func(period time.Duration) *kmspb.CryptoKey {
		return &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
			RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(period)},
			NextRotationTime: timestamppb.New(time.Now().Add(24 * time.Hour))}
	}
	want := func(what string, err error, msg string) {
		t.Helper()
		if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != msg {
			t.Errorf("%s = %v, want FAILED_PRECONDITION %s", what, err, msg)
		}
	}
	_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "long", CryptoKey: key(200 * 24 * time.Hour)})
	want("a 200-day rotation", err, `Operation denied by custom org policies: ["customConstraints/custom.rotation":"Rotate within 90 days."]`)
	_, err = c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "none", CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	want("no rotation period (not evaluable)", err, `Operation denied by custom org policies: ["customConstraints/custom.rotation":"Rotate within 90 days."]`)
	k, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "ok", CryptoKey: key(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("a 30-day rotation: %v", err)
	}
	upd := key(365 * 24 * time.Hour)
	upd.Name = k.GetName()
	_, err = c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period"}}})
	want("raising the period to a year", err, `Operation denied by custom org policies: ["customConstraints/custom.rotation":"Rotate within 90 days."]`)
	upd = key(60 * 24 * time.Hour)
	upd.Name = k.GetName()
	if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period"}}}); err != nil {
		t.Errorf("a 60-day period: %v", err)
	}
	_, err = c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring, ImportJobId: "soft", ImportJob: &kmspb.ImportJob{
		ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	want("a SOFTWARE import job", err, `Operation denied by custom org policies: ["customConstraints/custom.hsm":"HSM only."]`)
	if res := customResource(&kmspb.EkmConnection{Name: loc + "/ekmConnections/y"}); res["cryptoSpacePath"] != "" || res["keyManagementMode"] != "KEY_MANAGEMENT_MODE_UNSPECIFIED" {
		t.Errorf("customResource left out unset scalars: %v", res)
	}
	// resource.name is not a documented EkmConnection field: the DENY
	// cannot be evaluated, which refuses.
	if err := checkCustom(loc+"/ekmConnections/x", orgpolicy.TypeKMSEkmConnection, orgpolicy.MethodCreate,
		&kmspb.EkmConnection{Name: loc + "/ekmConnections/y"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a condition on an unlisted field = %v, want FAILED_PRECONDITION (not evaluable)", err)
	}
	if got := constraintParent("folders/123/autokeyConfig"); got != "folders/123" {
		t.Errorf("constraintParent(folder) = %q", got)
	}
}
