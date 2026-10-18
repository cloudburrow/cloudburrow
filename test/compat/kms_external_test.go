//go:build compat

package compat

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestKMSExternalKeys (#1469): EXTERNAL and EXTERNAL_VPC keys through the
// official KeyManagementClient and EkmClient, over gRPC and REST, with the
// instance acting as the external key manager. An EXTERNAL key is created
// with skip_initial_version_creation, as create-external-key does; a version
// names its key URI, an invalid URI is FAILED_PRECONDITION and creates no
// version; Encrypt and Decrypt report EXTERNAL, and the URI is updated
// without changing the key. An EXTERNAL signing key returns a public key and
// a signature that verifies. An EXTERNAL_VPC key takes the location's default
// EkmConnection, its versions name a key path, and on a CLOUD_KMS-mode
// connection the initial version is created with a path under the crypto
// space. VerifyConnectivity succeeds.
//
// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion
// covers: google.cloud.kms.v1.KeyManagementService/Encrypt, google.cloud.kms.v1.KeyManagementService/Decrypt, google.cloud.kms.v1.KeyManagementService/AsymmetricSign, google.cloud.kms.v1.KeyManagementService/GetPublicKey
// covers: google.cloud.kms.v1.EkmService/CreateEkmConnection, google.cloud.kms.v1.EkmService/UpdateEkmConfig, google.cloud.kms.v1.EkmService/VerifyConnectivity
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion FAILED_PRECONDITION: an invalid external key URI
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/resources.proto
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: an EXTERNAL key without skip_initial_version_creation
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/resources.proto
func TestKMSExternalKeys(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	der := ekmCertificate(t)
	clients := kmsClients(t, h)
	for _, variant := range []string{"grpc", "rest"} {
		t.Run(variant, func(t *testing.T) {
			h := h.For(t)
			c := clients[variant]
			newEkm := kms.NewEkmClient
			if variant == "rest" {
				newEkm = kms.NewEkmRESTClient
			}
			e, err := newEkm(ctx, kmsOptions(h, variant)...)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			loc := "projects/" + h.Project() + "/locations/asia-east1"
			if variant == "rest" {
				loc = "projects/" + h.Project() + "/locations/asia-east2"
			}
			ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "external"})
			if err != nil {
				t.Fatal(err)
			}

			// EXTERNAL, symmetric.
			ext := &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL, Algorithm: kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION}
			if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sym",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: ext}}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("an EXTERNAL key with an initial version = %v, want INVALID_ARGUMENT", err)
			}
			key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sym", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: ext}})
			if err != nil || key.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL || key.GetPrimary() != nil {
				t.Fatalf("CreateCryptoKey = %v, %v", key, err)
			}
			if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(), CryptoKeyVersion: &kmspb.CryptoKeyVersion{
				ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: "not a uri"}}}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("an invalid key URI = %v, want FAILED_PRECONDITION", err)
			}
			const uri = "https://ekm.example.com/v0/keys/app"
			v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(), CryptoKeyVersion: &kmspb.CryptoKeyVersion{
				ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: uri}}})
			if err != nil || !strings.HasSuffix(v.GetName(), "/cryptoKeyVersions/1") || v.GetExternalProtectionLevelOptions().GetExternalKeyUri() != uri ||
				v.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL || v.GetAlgorithm() != kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION {
				t.Fatalf("CreateCryptoKeyVersion = %v, %v", v, err)
			}
			if _, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "1"}); err != nil {
				t.Fatal(err)
			}
			enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("external")})
			if err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
				t.Fatalf("Encrypt = %v, %v", enc, err)
			}
			const moved = "https://ekm2.example.com/v0/keys/app"
			upd, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
				CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v.GetName(), ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: moved}},
				UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"external_protection_level_options"}}})
			if err != nil || upd.GetExternalProtectionLevelOptions().GetExternalKeyUri() != moved {
				t.Fatalf("UpdateCryptoKeyVersion = %v, %v", upd, err)
			}
			dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()})
			if err != nil || string(dec.GetPlaintext()) != "external" || dec.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
				t.Errorf("Decrypt after the URI moved = %v, %v", dec, err)
			}

			// EXTERNAL, asymmetric signing.
			sk, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sign", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
					VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}})
			if err != nil {
				t.Fatal(err)
			}
			sv, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: sk.GetName(), CryptoKeyVersion: &kmspb.CryptoKeyVersion{
				ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: "https://ekm.example.com/v0/keys/signer"}}})
			if err != nil {
				t.Fatal(err)
			}
			pub, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: sv.GetName()})
			if err != nil || pub.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
				t.Fatalf("GetPublicKey = %v, %v", pub, err)
			}
			data := []byte("signed by the EKM")
			sum := sha256.Sum256(data)
			sig, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: sv.GetName(), Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: sum[:]}}})
			if err != nil || sig.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL {
				t.Fatalf("AsymmetricSign = %v, %v", sig, err)
			}
			if !verifyECDSA(t, pub.GetPem(), sum[:], sig.GetSignature()) {
				t.Error("the signature does not verify under the public key")
			}

			// EXTERNAL_VPC on the location's default (MANUAL) connection.
			resolver := []*kmspb.EkmConnection_ServiceResolver{{ServiceDirectoryService: loc + "/namespaces/ekm/services/replica",
				Hostname: "ekm.example.com", ServerCertificates: []*kmspb.Certificate{{RawDer: der}}}}
			manual, err := e.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "manual",
				EkmConnection: &kmspb.EkmConnection{ServiceResolvers: resolver}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.VerifyConnectivity(ctx, &kmspb.VerifyConnectivityRequest{Name: manual.GetName()}); err != nil {
				t.Errorf("VerifyConnectivity = %v", err)
			}
			if _, err := e.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{EkmConfig: &kmspb.EkmConfig{Name: loc + "/ekmConfig", DefaultEkmConnection: manual.GetName()},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}}); err != nil {
				t.Fatal(err)
			}
			vpc := &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL_VPC}
			vk, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "vpc", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: vpc}})
			if err != nil || vk.GetCryptoKeyBackend() != manual.GetName() {
				t.Fatalf("an EXTERNAL_VPC key on the default connection = %v, %v", vk, err)
			}
			vv, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: vk.GetName(), CryptoKeyVersion: &kmspb.CryptoKeyVersion{
				ExternalProtectionLevelOptions: &kmspb.ExternalProtectionLevelOptions{EkmConnectionKeyPath: "v0/my/key"}}})
			if err != nil || vv.GetExternalProtectionLevelOptions().GetEkmConnectionKeyPath() != "v0/my/key" {
				t.Fatalf("an EXTERNAL_VPC version = %v, %v", vv, err)
			}
			if enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: vv.GetName(), Plaintext: []byte("vpc")}); err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL_VPC {
				t.Errorf("Encrypt with an EXTERNAL_VPC version = %v, %v", enc, err)
			}

			// A coordinated external key.
			coord, err := e.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: loc, EkmConnectionId: "coordinated",
				EkmConnection: &kmspb.EkmConnection{ServiceResolvers: resolver, KeyManagementMode: kmspb.EkmConnection_CLOUD_KMS, CryptoSpacePath: "v0/longlived/space"}})
			if err != nil {
				t.Fatal(err)
			}
			ck, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "coordinated",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: vpc, CryptoKeyBackend: coord.GetName()}})
			if err != nil || ck.GetCryptoKeyBackend() != coord.GetName() || ck.GetPrimary().GetState() != kmspb.CryptoKeyVersion_ENABLED ||
				!strings.HasPrefix(ck.GetPrimary().GetExternalProtectionLevelOptions().GetEkmConnectionKeyPath(), "v0/longlived/space/") {
				t.Fatalf("a coordinated key = %v, %v", ck, err)
			}
			if enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: ck.GetName(), Plaintext: []byte("coordinated")}); err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL_VPC {
				t.Errorf("Encrypt with a coordinated key = %v, %v", enc, err)
			}
		})
	}
}

// verifyECDSA checks an ECDSA signature of digest under a PEM public key.
func verifyECDSA(t *testing.T, publicPEM string, digest, sig []byte) bool {
	t.Helper()
	b, _ := pem.Decode([]byte(publicPEM))
	if b == nil {
		t.Fatalf("not a PEM public key: %q", publicPEM)
	}
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := pub.(*ecdsa.PublicKey)
	return ok && ecdsa.VerifyASN1(k, digest, sig)
}
