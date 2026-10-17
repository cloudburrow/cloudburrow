//go:build compat

package compat

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/pem"
	"hash/crc32"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The asymmetric, MAC and raw purposes, random bytes and key deletion
// (#1212), through the official client over gRPC and REST. Signatures are
// verified and ciphertexts made with Go's standard crypto, as Google's
// samples do: a signature against the GetPublicKey PEM, and
// AsymmetricDecrypt of what was encrypted with that public key.

func kmsCRC(b []byte) *wrapperspb.Int64Value {
	return wrapperspb.Int64(int64(crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))))
}

func kmsPurposeKey(t *testing.T, h *Harness, c *kms.KeyManagementClient, ring, id string, p kmspb.CryptoKey_CryptoKeyPurpose, a kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) string {
	t.Helper()
	k, err := c.CreateCryptoKey(h.Context(), &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
		CryptoKey: &kmspb.CryptoKey{Purpose: p, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: a}}})
	if err != nil {
		t.Fatalf("CreateCryptoKey %s: %v", a, err)
	}
	if k.GetPurpose() != p || k.GetVersionTemplate().GetAlgorithm() != a {
		t.Fatalf("CreateCryptoKey = %v", k)
	}
	return k.GetName() + "/cryptoKeyVersions/1"
}

func kmsPublicKey(t *testing.T, h *Harness, c *kms.KeyManagementClient, v string) any {
	t.Helper()
	pk, err := c.GetPublicKey(h.Context(), &kmspb.GetPublicKeyRequest{Name: v})
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	// The integrity check Google's samples make.
	if pk.GetPemCrc32C().GetValue() != crc32c([]byte(pk.GetPem())) {
		t.Fatalf("pem_crc32c does not match the PEM")
	}
	b, _ := pem.Decode([]byte(pk.GetPem()))
	if b == nil {
		t.Fatalf("not PEM: %q", pk.GetPem())
	}
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func kmsRing(t *testing.T, h *Harness, c *kms.KeyManagementClient, id string) string {
	t.Helper()
	ring, err := c.CreateKeyRing(h.Context(), &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: id})
	if err != nil {
		t.Fatal(err)
	}
	return ring.GetName()
}

// covers: google.cloud.kms.v1.KeyManagementService/GetPublicKey, google.cloud.kms.v1.KeyManagementService/AsymmetricSign, google.cloud.kms.v1.KeyManagementService/CreateCryptoKey
//
// unverified: google.cloud.kms.v1.KeyManagementService/AsymmetricSign INVALID_ARGUMENT: a digest of the wrong hash, or a data_crc32c that does not match
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
// unverified: google.cloud.kms.v1.KeyManagementService/AsymmetricSign FAILED_PRECONDITION: a key of another purpose
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
func TestKMSAsymmetricSignVerifiesWithThePublicKeyPEM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	msg := []byte("cloudburrow")
	d256 := sha256.Sum256(msg)
	d384 := sha512.Sum384(msg)
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "sign-"+variant)
			for id, a := range map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
				"p256":  kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256,
				"p384":  kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384,
				"pss":   kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256,
				"pkcs1": kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256,
			} {
				v := kmsPurposeKey(t, h, c, ring, id, kmspb.CryptoKey_ASYMMETRIC_SIGN, a)
				pub := kmsPublicKey(t, h, c, v)
				digest := &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d256[:]}}
				d := d256[:]
				if a == kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384 {
					digest, d = &kmspb.Digest{Digest: &kmspb.Digest_Sha384{Sha384: d384[:]}}, d384[:]
				}
				resp, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: digest, DigestCrc32C: kmsCRC(d)})
				if err != nil {
					t.Fatalf("%s AsymmetricSign: %v", a, err)
				}
				if !resp.GetVerifiedDigestCrc32C() || resp.GetName() != v || resp.GetSignatureCrc32C().GetValue() != crc32c(resp.GetSignature()) {
					t.Errorf("%s integrity fields = %v", a, resp)
				}
				sig := resp.GetSignature()
				var ok bool
				switch k := pub.(type) {
				case *ecdsa.PublicKey:
					ok = ecdsa.VerifyASN1(k, d, sig)
				case *rsa.PublicKey:
					if a == kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256 {
						ok = rsa.VerifyPSS(k, crypto.SHA256, d, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
					} else {
						ok = rsa.VerifyPKCS1v15(k, crypto.SHA256, d, sig) == nil
					}
				}
				if !ok {
					t.Errorf("%s: the signature does not verify with the GetPublicKey PEM", a)
				}
				// data instead of digest: the server hashes it.
				byData, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: msg, DataCrc32C: kmsCRC(msg)})
				if err != nil || !byData.GetVerifiedDataCrc32C() {
					t.Fatalf("%s AsymmetricSign with data = %v, %v", a, byData, err)
				}
				if p, ok := pub.(*ecdsa.PublicKey); ok && !ecdsa.VerifyASN1(p, d, byData.GetSignature()) {
					t.Errorf("%s: the signature of data does not verify", a)
				}
			}
			v := ring + "/cryptoKeys/pkcs1/cryptoKeyVersions/1"
			d512 := sha512.Sum512(msg)
			if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha512{Sha512: d512[:]}}}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("a SHA-512 digest for a SHA-256 key = %v, want INVALID_ARGUMENT", err)
			}
			if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: msg, DataCrc32C: kmsCRC([]byte("other"))}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("a wrong data_crc32c = %v, want INVALID_ARGUMENT", err)
			}
			sym := kmsPurposeKey(t, h, c, ring, "sym", kmspb.CryptoKey_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION)
			if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: sym, Data: msg}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("sign with an ENCRYPT_DECRYPT key = %v, want FAILED_PRECONDITION", err)
			}
		})
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/GetPublicKey, google.cloud.kms.v1.KeyManagementService/AsymmetricDecrypt
//
// unverified: google.cloud.kms.v1.KeyManagementService/AsymmetricDecrypt INVALID_ARGUMENT: a ciphertext that does not decrypt
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
func TestKMSAsymmetricDecryptOpensWhatThePublicKeyEncrypted(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "decrypt-"+variant)
			v := kmsPurposeKey(t, h, c, ring, "k", kmspb.CryptoKey_ASYMMETRIC_DECRYPT, kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_2048_SHA256)
			pub := kmsPublicKey(t, h, c, v).(*rsa.PublicKey)
			ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("secret"), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{Name: v, Ciphertext: ct, CiphertextCrc32C: kmsCRC(ct)})
			if err != nil || string(resp.GetPlaintext()) != "secret" || !resp.GetVerifiedCiphertextCrc32C() ||
				resp.GetPlaintextCrc32C().GetValue() != crc32c(resp.GetPlaintext()) {
				t.Fatalf("AsymmetricDecrypt = %v, %v", resp, err)
			}
			if _, err := c.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{Name: v, Ciphertext: []byte("junk")}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("junk ciphertext = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/MacSign, google.cloud.kms.v1.KeyManagementService/MacVerify
func TestKMSMacSignAndVerify(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	data := []byte("cloudburrow")
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "mac-"+variant)
			for id, a := range map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
				"sha256": kmspb.CryptoKeyVersion_HMAC_SHA256, "sha1": kmspb.CryptoKeyVersion_HMAC_SHA1, "sha512": kmspb.CryptoKeyVersion_HMAC_SHA512,
			} {
				v := kmsPurposeKey(t, h, c, ring, id, kmspb.CryptoKey_MAC, a)
				m, err := c.MacSign(ctx, &kmspb.MacSignRequest{Name: v, Data: data, DataCrc32C: kmsCRC(data)})
				if err != nil || !m.GetVerifiedDataCrc32C() || m.GetMacCrc32C().GetValue() != crc32c(m.GetMac()) {
					t.Fatalf("%s MacSign = %v, %v", a, m, err)
				}
				ok, err := c.MacVerify(ctx, &kmspb.MacVerifyRequest{Name: v, Data: data, Mac: m.GetMac(), MacCrc32C: kmsCRC(m.GetMac())})
				if err != nil || !ok.GetSuccess() || !ok.GetVerifiedSuccessIntegrity() || !ok.GetVerifiedMacCrc32C() {
					t.Errorf("%s MacVerify = %v, %v", a, ok, err)
				}
				bad, err := c.MacVerify(ctx, &kmspb.MacVerifyRequest{Name: v, Data: []byte("other"), Mac: m.GetMac()})
				if err != nil || bad.GetSuccess() || bad.GetVerifiedSuccessIntegrity() {
					t.Errorf("%s MacVerify of other data = %v, %v; want success false", a, bad, err)
				}
			}
		})
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/RawEncrypt, google.cloud.kms.v1.KeyManagementService/RawDecrypt
//
// unverified: google.cloud.kms.v1.KeyManagementService/RawDecrypt INVALID_ARGUMENT: AES-GCM with the wrong additional authenticated data
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
func TestKMSRawEncryptAndDecrypt(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	pt := []byte("sixteen byte msg")
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "raw-"+variant)
			for id, a := range map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
				"gcm": kmspb.CryptoKeyVersion_AES_256_GCM, "cbc": kmspb.CryptoKeyVersion_AES_128_CBC, "ctr": kmspb.CryptoKeyVersion_AES_256_CTR,
			} {
				v := kmsPurposeKey(t, h, c, ring, id, kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, a)
				var aad, iv []byte
				if id == "gcm" {
					aad = []byte("context")
				} else {
					iv = bytes.Repeat([]byte{9}, 16)
				}
				enc, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: v, Plaintext: pt, AdditionalAuthenticatedData: aad, InitializationVector: iv,
					PlaintextCrc32C: kmsCRC(pt), AdditionalAuthenticatedDataCrc32C: kmsCRC(aad)})
				if err != nil {
					t.Fatalf("%s RawEncrypt: %v", a, err)
				}
				if !enc.GetVerifiedPlaintextCrc32C() || !enc.GetVerifiedAdditionalAuthenticatedDataCrc32C() ||
					enc.GetCiphertextCrc32C().GetValue() != crc32c(enc.GetCiphertext()) || len(enc.GetInitializationVector()) == 0 {
					t.Errorf("%s RawEncrypt fields = %v", a, enc)
				}
				if iv != nil && !bytes.Equal(enc.GetInitializationVector(), iv) {
					t.Errorf("%s did not use the caller's IV", a)
				}
				dec, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: v, Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad,
					InitializationVector: enc.GetInitializationVector(), TagLength: enc.GetTagLength(), CiphertextCrc32C: kmsCRC(enc.GetCiphertext())})
				if err != nil || !bytes.Equal(dec.GetPlaintext(), pt) || !dec.GetVerifiedCiphertextCrc32C() {
					t.Errorf("%s RawDecrypt = %v, %v", a, dec, err)
				}
				if id == "gcm" {
					if _, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: v, Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: []byte("wrong"),
						InitializationVector: enc.GetInitializationVector()}); kmsCode(variant, err) != codes.InvalidArgument {
						t.Errorf("wrong AAD = %v, want INVALID_ARGUMENT", err)
					}
				}
			}
		})
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/GenerateRandomBytes
//
// unverified: google.cloud.kms.v1.KeyManagementService/GenerateRandomBytes INVALID_ARGUMENT: a length outside 8 to 1024, or a protection level other than HSM
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
func TestKMSGenerateRandomBytes(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	loc := "projects/" + h.Project() + "/locations/global"
	for variant, c := range kmsClients(t, h) {
		r, err := c.GenerateRandomBytes(ctx, &kmspb.GenerateRandomBytesRequest{Location: loc, LengthBytes: 64, ProtectionLevel: kmspb.ProtectionLevel_HSM})
		if err != nil || len(r.GetData()) != 64 || r.GetDataCrc32C().GetValue() != crc32c(r.GetData()) {
			t.Fatalf("%s GenerateRandomBytes = %v, %v", variant, r, err)
		}
		if _, err := c.GenerateRandomBytes(ctx, &kmspb.GenerateRandomBytesRequest{Location: loc, LengthBytes: 4, ProtectionLevel: kmspb.ProtectionLevel_HSM}); kmsCode(variant, err) != codes.InvalidArgument {
			t.Errorf("%s 4 bytes = %v, want INVALID_ARGUMENT", variant, err)
		}
		if _, err := c.GenerateRandomBytes(ctx, &kmspb.GenerateRandomBytesRequest{Location: loc, LengthBytes: 8, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}); kmsCode(variant, err) != codes.InvalidArgument {
			t.Errorf("%s SOFTWARE = %v, want INVALID_ARGUMENT", variant, err)
		}
	}
}

// Deleting a version needs it DESTROYED, which takes at least 24 hours, so
// the successful DeleteCryptoKeyVersion is the in-process
// TestDeleteKeyAfterEveryVersionIsDestroyedAndDeleted; here a key created
// with no versions is deleted.
//
// covers: google.cloud.kms.v1.KeyManagementService/DeleteCryptoKey, google.cloud.kms.v1.KeyManagementService/DeleteCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/GetRetiredResource, google.cloud.kms.v1.KeyManagementService/ListRetiredResources
//
// unverified: google.cloud.kms.v1.KeyManagementService/DeleteCryptoKeyVersion FAILED_PRECONDITION: a version that is not DESTROYED
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
// unverified: google.cloud.kms.v1.KeyManagementService/DeleteCryptoKey FAILED_PRECONDITION: a key that still has versions, or has automatic rotation scheduled (the rules are delete-kms-resources', the code is not)
// Docs re-read 2026-10-17, still unanswered: https://cloud.google.com/kms/docs/delete-kms-resources
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey ALREADY_EXISTS: the name of a deleted key
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/resources.proto
func TestKMSDeleteCryptoKeyLeavesARetiredResource(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	loc := "projects/" + h.Project() + "/locations/global"
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "delete-"+variant)
			v := kmsPurposeKey(t, h, c, ring, "has-versions", kmspb.CryptoKey_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION)
			if _, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("delete an ENABLED version = %v, want FAILED_PRECONDITION", err)
			}
			if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: v}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("delete a DESTROY_SCHEDULED version = %v, want FAILED_PRECONDITION", err)
			}
			if _, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: ring + "/cryptoKeys/has-versions"}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("delete a key with a version = %v, want FAILED_PRECONDITION", err)
			}
			// A key with automatic rotation scheduled cannot be deleted, as
			// delete-kms-resources documents (read 2026-10-17).
			rotating, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "rotating", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
					RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(30 * 24 * time.Hour)},
					NextRotationTime: timestamppb.New(time.Now().Add(24 * time.Hour))}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: rotating.GetName()}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("delete a key with rotation scheduled = %v, want FAILED_PRECONDITION", err)
			}
			empty, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "empty", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
			if err != nil {
				t.Fatal(err)
			}
			op, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: empty.GetName()})
			if err != nil {
				t.Fatalf("DeleteCryptoKey: %v", err)
			}
			if err := op.Wait(ctx); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			md, err := op.Metadata()
			if err != nil || md.GetRetiredResource() == "" {
				t.Fatalf("metadata = %v, %v", md, err)
			}
			if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: empty.GetName()}); kmsCode(variant, err) != codes.NotFound {
				t.Errorf("a deleted key = %v, want NOT_FOUND", err)
			}
			r, err := c.GetRetiredResource(ctx, &kmspb.GetRetiredResourceRequest{Name: md.GetRetiredResource()})
			if err != nil || r.GetOriginalResource() != empty.GetName() || r.GetResourceType() != "cloudkms.googleapis.com/CryptoKey" || r.GetDeleteTime() == nil {
				t.Fatalf("GetRetiredResource = %v, %v", r, err)
			}
			found := false
			it := c.ListRetiredResources(ctx, &kmspb.ListRetiredResourcesRequest{Parent: loc})
			for {
				got, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("ListRetiredResources: %v", err)
				}
				found = found || got.GetName() == r.GetName()
			}
			if !found {
				t.Errorf("ListRetiredResources does not list %s", r.GetName())
			}
			if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "empty",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); kmsCode(variant, err) != codes.AlreadyExists {
				t.Errorf("reuse a retired name = %v, want ALREADY_EXISTS", err)
			}
		})
	}
}
