//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey, google.cloud.kms.v1.KeyManagementService/CreateImportJob, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestOrgPolicyCustomConstraintsKMS (#1895), with the official KMS
// client over gRPC and REST: the KMS page's examples, a maximum rotation
// period on cloudkms.googleapis.com/CryptoKey (ALLOW
// resource.rotationPeriod <= duration('7776000s'), CREATE and UPDATE)
// and HSM import jobs (ALLOW resource.protectionLevel == 'HSM' on
// cloudkms.googleapis.com/ImportJob), refuse a 200-day key, raising a
// key's period to a year and a SOFTWARE import job, FAILED_PRECONDITION
// "Operation denied by custom org policies: [...]"; a DENY on
// CryptoKeyVersion state DISABLED refuses disabling a version. A project
// without the policies is unaffected.
func TestOrgPolicyCustomConstraintsKMS(t *testing.T) {
	h := New(t)
	kcs := kmsClients(t, h)
	ctx := h.Context()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ock-")
	rotation := customName(h, "kmsrotation")
	hsm := customName(h, "kmshsmimport")
	noDisable := customName(h, "kmsnodisable")
	rdesc, hdesc, ddesc := "Keys must rotate within 90 days.", "Import jobs must be HSM.", "Versions cannot be disabled."
	createCustom(t, h, c, rotation, "cloudkms.googleapis.com/CryptoKey", "resource.rotationPeriod <= duration('7776000s')", ccAllow,
		rdesc, ccCreate, ccUpdate)
	createCustom(t, h, c, hsm, "cloudkms.googleapis.com/ImportJob", "resource.protectionLevel == 'HSM'", ccAllow, hdesc, ccCreate)
	createCustom(t, h, c, noDisable, "cloudkms.googleapis.com/CryptoKeyVersion", "resource.state == 'DISABLED'", ccDeny, ddesc, ccUpdate)
	for _, id := range []string{rotation, hsm, noDisable} {
		setOrgPolicy(t, h, c, "projects/"+project, id, opEnforce())
	}
	key := func(period time.Duration) *kmspb.CryptoKey {
		return &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
			RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(period)},
			NextRotationTime: timestamppb.New(time.Now().Add(24 * time.Hour))}
	}
	for variant, kc := range kcs {
		t.Run(variant, func(t *testing.T) {
			want := func(what string, err error, msg string) {
				t.Helper()
				if variant == "grpc" {
					wantFailedPrecondition(t, what, err, msg)
				} else if kmsCode(variant, err) != codes.FailedPrecondition {
					t.Errorf("%s = %v, want FAILED_PRECONDITION", what, err)
				}
			}
			ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us-east1", KeyRingId: "cc-" + variant})
			if err != nil {
				t.Fatal(err)
			}
			_, err = kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "long", CryptoKey: key(200 * 24 * time.Hour)})
			want("a 200-day rotation", err, customMessage(rotation, rdesc))
			k, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "ok", CryptoKey: key(30 * 24 * time.Hour)})
			if err != nil {
				t.Fatalf("a 30-day rotation: %v", err)
			}
			upd := key(365 * 24 * time.Hour)
			upd.Name = k.GetName()
			_, err = kc.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period"}}})
			want("raising the period to a year", err, customMessage(rotation, rdesc))
			upd = key(60 * 24 * time.Hour)
			upd.Name = k.GetName()
			if _, err := kc.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period"}}}); err != nil {
				t.Errorf("a 60-day period: %v", err)
			}
			_, err = kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
				CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: k.GetName() + "/cryptoKeyVersions/1", State: kmspb.CryptoKeyVersion_DISABLED},
				UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
			want("disabling a version", err, customMessage(noDisable, ddesc))
			_, err = kc.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "soft", ImportJob: &kmspb.ImportJob{
				ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
			want("a SOFTWARE import job", err, customMessage(hsm, hdesc))
			if _, err := kc.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "hsm", ImportJob: &kmspb.ImportJob{
				ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_HSM}}); err != nil {
				t.Errorf("an HSM import job: %v", err)
			}
		})
	}
	free := orgPolicyProject(t, h, pc, localOrg, "ockf-")
	kc := kcs["grpc"]
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + free + "/locations/us-east1", KeyRingId: "cc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "long", CryptoKey: key(200 * 24 * time.Hour)}); err != nil {
		t.Errorf("a 200-day key in a project without the policy: %v", err)
	}
}
