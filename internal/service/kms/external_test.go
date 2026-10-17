package kms

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const extLoc = "projects/demo-project/locations/europe-west1"

// EXTERNAL keys (#1469): created empty, each version names its key URI, and
// the instance acting as the EKM encrypts, decrypts and signs.
func TestExternalKeysAreServedByTheLocalEKM(t *testing.T) {
	cs := extras(t)
	c := cs.kms
	ctx := context.Background()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: extLoc, KeyRingId: "ext"})
	if err != nil {
		t.Fatal(err)
	}
	ext := &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: ext}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an initial version = %v, want INVALID_ARGUMENT", err)
	}
	for what, k := range map[string]*kmspb.CryptoKey{
		"MAC":             {Purpose: kmspb.CryptoKey_MAC, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL, Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256}},
		"Ed25519":         {Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_ED25519}},
		"GOOGLE_SYMM":     {Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL, Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION}},
		"EXTERNAL_SYMM@S": {Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION}},
		"a backend":       {Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: ext, CryptoKeyBackend: extLoc + "/ekmConnections/c"},
		"a rotation": {Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: ext, RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(48 * 3600e9)},
			NextRotationTime: timestamppb.Now()},
	} {
		_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "bad", CryptoKey: k, SkipInitialVersionCreation: true})
		if code := status.Code(err); code != codes.InvalidArgument && code != codes.Unimplemented {
			t.Errorf("%s = %v, want a refusal", what, err)
		}
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: ext}})
	if err != nil || key.GetVersionTemplate().GetAlgorithm() != kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION || key.GetPrimary() != nil {
		t.Fatalf("CreateCryptoKey = %v, %v", key, err)
	}
	for what, opts := range map[string]*kmspb.ExternalProtectionLevelOptions{
		"no options":   nil,
		"an http URI":  {ExternalKeyUri: "http://ekm.example/v0/k"},
		"no host":      {ExternalKeyUri: "https:///v0/k"},
		"a key path":   {ExternalKeyUri: "https://ekm.example/v0/k", EkmConnectionKeyPath: "v0/k"},
		"not absolute": {ExternalKeyUri: "ekm.example/v0/k"},
	} {
		if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: opts}}); err == nil {
			t.Errorf("%s: a version was created", what)
		}
	}
	const uri = "https://ekm.example/v0/keys/a"
	v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: uri}}})
	if err != nil || v.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL || v.GetExternalProtectionLevelOptions().GetExternalKeyUri() != uri {
		t.Fatalf("CreateCryptoKeyVersion = %v, %v", v, err)
	}
	if _, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "1"}); err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("hello")})
	if err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
		t.Fatalf("Encrypt = %v, %v", enc, err)
	}
	// The path moves; the key stays the same.
	const moved = "https://ekm2.example/v0/keys/a"
	upd, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"external_protection_level_options"}},
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v.GetName(), ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: moved}}})
	if err != nil || upd.GetExternalProtectionLevelOptions().GetExternalKeyUri() != moved || upd.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Fatalf("UpdateCryptoKeyVersion = %v, %v", upd, err)
	}
	dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()})
	if err != nil || string(dec.GetPlaintext()) != "hello" || dec.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
		t.Errorf("Decrypt = %v, %v", dec, err)
	}
	if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period", "next_rotation_time"}},
		CryptoKey: &kmspb.CryptoKey{Name: key.GetName(), RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(48 * 3600e9)}, NextRotationTime: timestamppb.Now()}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a rotation schedule = %v, want UNIMPLEMENTED", err)
	}

	sk, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sign", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}})
	if err != nil {
		t.Fatal(err)
	}
	sv, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: sk.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: uri}}})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: sv.GetName()})
	if err != nil || pub.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
		t.Fatalf("GetPublicKey = %v, %v", pub, err)
	}
	sum := [32]byte{1}
	sig, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: sv.GetName(), Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: sum[:]}}})
	if err != nil || sig.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
		t.Fatalf("AsymmetricSign = %v, %v", sig, err)
	}
}

// EXTERNAL_VPC keys (#1469) on a MANUAL connection, named or the location's
// default, and on a CLOUD_KMS one, whose versions the local EKM creates.
func TestExternalVPCKeysUseTheirEkmConnection(t *testing.T) {
	cs := extras(t)
	c, e := cs.kms, cs.ekm
	ctx := context.Background()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: extLoc, KeyRingId: "vpc"})
	if err != nil {
		t.Fatal(err)
	}
	resolver := []*kmspb.EkmConnection_ServiceResolver{{ServiceDirectoryService: extLoc + "/namespaces/ns/services/ekm",
		Hostname: "ekm.example", ServerCertificates: []*kmspb.Certificate{{RawDer: selfSigned(t)}}}}
	manual, err := e.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: extLoc, EkmConnectionId: "manual",
		EkmConnection: &kmspb.EkmConnection{ServiceResolvers: resolver}})
	if err != nil {
		t.Fatal(err)
	}
	coord, err := e.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: extLoc, EkmConnectionId: "coord",
		EkmConnection: &kmspb.EkmConnection{ServiceResolvers: resolver, KeyManagementMode: kmspb.EkmConnection_CLOUD_KMS, CryptoSpacePath: "v0/longlived/space"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyConnectivity(ctx, &kmspb.VerifyConnectivityRequest{Name: manual.GetName()}); err != nil {
		t.Errorf("VerifyConnectivity = %v", err)
	}
	vpc := &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL_VPC}
	create := func(id, backend string, skip bool) (*kmspb.CryptoKey, error) {
		return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: id, SkipInitialVersionCreation: skip,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: vpc, CryptoKeyBackend: backend}})
	}
	if _, err := create("nodefault", "", true); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no backend and no default = %v", err)
	}
	if _, err := create("absent", extLoc+"/ekmConnections/absent", true); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an absent backend = %v", err)
	}
	if _, err := create("elsewhere", "projects/demo-project/locations/us-east1/ekmConnections/manual", true); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a backend in another location = %v", err)
	}
	if _, err := create("noskip", manual.GetName(), false); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an initial version on a MANUAL connection = %v", err)
	}
	if _, err := e.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{EkmConfig: &kmspb.EkmConfig{Name: extLoc + "/ekmConfig", DefaultEkmConnection: manual.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}}); err != nil {
		t.Fatal(err)
	}
	k, err := create("manual", "", true)
	if err != nil || k.GetCryptoKeyBackend() != manual.GetName() {
		t.Fatalf("a key on the default connection = %v, %v", k, err)
	}
	if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: k.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: "https://ekm.example/k"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key URI on EXTERNAL_VPC = %v", err)
	}
	v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: k.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{EkmConnectionKeyPath: "v0/my/key"}}})
	if err != nil || v.GetExternalProtectionLevelOptions().GetEkmConnectionKeyPath() != "v0/my/key" || v.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL_VPC {
		t.Fatalf("a version by key path = %v, %v", v, err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: v.GetName(), Plaintext: []byte("x")})
	if err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL_VPC {
		t.Errorf("Encrypt = %v, %v", enc, err)
	}

	ck, err := create("coord", coord.GetName(), false)
	if err != nil || ck.GetPrimary().GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Fatalf("a coordinated key = %v, %v", ck, err)
	}
	if p := ck.GetPrimary().GetExternalProtectionLevelOptions().GetEkmConnectionKeyPath(); len(p) <= len("v0/longlived/space/") || p[:len("v0/longlived/space/")] != "v0/longlived/space/" {
		t.Errorf("a coordinated key path = %q", p)
	}
	if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: ck.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{EkmConnectionKeyPath: "v0/mine"}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a path on a coordinated key = %v", err)
	}
	if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period", "next_rotation_time"}},
		CryptoKey: &kmspb.CryptoKey{Name: ck.GetName(), RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(48 * 3600e9)}, NextRotationTime: timestamppb.Now()}}); err != nil {
		t.Errorf("rotating a symmetric coordinated key = %v", err)
	}
}
