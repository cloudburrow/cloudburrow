//go:build compat

package compat

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	k1ecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"google.golang.org/grpc/codes"
)

// EC_SIGN_SECP256K1_SHA256 (#1719) through the official client over gRPC
// and REST. The algorithms page (cloud.google.com/kms/docs/algorithms) lists
// it under the HSM and HSM_SINGLE_TENANT protection levels only, and says
// secp256k1 signatures come "in the "normalized" form only (also known as
// the "lower-S form")", DER-encoded. Go's standard library has no
// secp256k1, so the GetPublicKey PEM is read with encoding/asn1 and the
// signature checked with github.com/decred/dcrd/dcrec/secp256k1/v4's
// verifier, whose ParseDERSignature refuses a signature that is not lower-S.

// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/GetPublicKey, google.cloud.kms.v1.KeyManagementService/AsymmetricSign
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: EC_SIGN_SECP256K1_SHA256 at a protection level other than HSM or HSM_SINGLE_TENANT
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/kms/docs/algorithms
// unverified: google.cloud.kms.v1.KeyManagementService/AsymmetricSign INVALID_ARGUMENT: a secp256k1 digest that is not 32 bytes of sha256
// Docs re-read 2026-11-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
func TestKMSSecp256k1SignsAndVerifiesWithThePublicKey(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	alg := kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256
	msg := []byte("cloudburrow secp256k1")
	d := sha256.Sum256(msg)
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "k1-"+variant)
			tmpl := func(pl kmspb.ProtectionLevel) *kmspb.CryptoKey {
				return &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
					VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg, ProtectionLevel: pl}}
			}
			// SOFTWARE (the default) is refused: Google offers it in an HSM only.
			for _, pl := range []kmspb.ProtectionLevel{kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED, kmspb.ProtectionLevel_SOFTWARE} {
				if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "sw", CryptoKey: tmpl(pl)}); kmsCode(variant, err) != codes.InvalidArgument {
					t.Errorf("a %s secp256k1 key = %v, want INVALID_ARGUMENT", pl, err)
				}
			}
			k, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "hsm", CryptoKey: tmpl(kmspb.ProtectionLevel_HSM)})
			if err != nil {
				t.Fatalf("CreateCryptoKey HSM: %v", err)
			}
			if k.GetVersionTemplate().GetAlgorithm() != alg || k.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
				t.Fatalf("CreateCryptoKey = %v", k)
			}
			v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: k.GetName()})
			if err != nil || v2.GetAlgorithm() != alg || v2.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
				t.Fatalf("CreateCryptoKeyVersion = %v, %v", v2, err)
			}
			for _, v := range []string{k.GetName() + "/cryptoKeyVersions/1", v2.GetName()} {
				pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_PEM})
				if err != nil {
					t.Fatalf("GetPublicKey: %v", err)
				}
				if pk.GetPemCrc32C().GetValue() != crc32c([]byte(pk.GetPem())) || pk.GetAlgorithm() != alg ||
					pk.GetProtectionLevel() != kmspb.ProtectionLevel_HSM || string(pk.GetPublicKey().GetData()) != pk.GetPem() ||
					pk.GetPublicKey().GetCrc32CChecksum().GetValue() != crc32c(pk.GetPublicKey().GetData()) {
					t.Fatalf("GetPublicKey = %v", pk)
				}
				b, _ := pem.Decode([]byte(pk.GetPem()))
				if b == nil || b.Type != "PUBLIC KEY" {
					t.Fatalf("not a PEM public key: %q", pk.GetPem())
				}
				// SubjectPublicKeyInfo: id-ecPublicKey, namedCurve secp256k1 (RFC 5480, SEC 2).
				var spki struct {
					Algo struct{ Algorithm, Curve asn1.ObjectIdentifier }
					Key  asn1.BitString
				}
				if _, err := asn1.Unmarshal(b.Bytes, &spki); err != nil || spki.Algo.Algorithm.String() != "1.2.840.10045.2.1" || spki.Algo.Curve.String() != "1.3.132.0.10" {
					t.Fatalf("SubjectPublicKeyInfo = %+v, %v", spki.Algo, err)
				}
				pub, err := secp256k1.ParsePubKey(spki.Key.RightAlign())
				if err != nil {
					t.Fatal(err)
				}

				for what, req := range map[string]*kmspb.AsymmetricSignRequest{
					"digest": {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:]}}, DigestCrc32C: kmsCRC(d[:])},
					"data":   {Name: v, Data: msg, DataCrc32C: kmsCRC(msg)},
				} {
					resp, err := c.AsymmetricSign(ctx, req)
					if err != nil {
						t.Fatalf("AsymmetricSign %s: %v", what, err)
					}
					if resp.GetName() != v || resp.GetSignatureCrc32C().GetValue() != crc32c(resp.GetSignature()) ||
						resp.GetVerifiedDigestCrc32C() != (what == "digest") || resp.GetVerifiedDataCrc32C() != (what == "data") ||
						resp.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
						t.Errorf("%s integrity fields = %v", what, resp)
					}
					sig, err := k1ecdsa.ParseDERSignature(resp.GetSignature())
					if err != nil {
						t.Fatalf("%s: the signature is not a lower-S DER signature: %v", what, err)
					}
					if !sig.Verify(d[:], pub) {
						t.Errorf("%s: the signature does not verify with the GetPublicKey PEM", what)
					}
					var rs struct{ R, S *big.Int }
					if _, err := asn1.Unmarshal(resp.GetSignature(), &rs); err != nil || rs.S.Cmp(new(big.Int).Rsh(secp256k1.S256().N, 1)) > 0 {
						t.Errorf("%s: s is not lower-S (%v)", what, err)
					}
				}
			}

			v := v2.GetName()
			d384 := sha512.Sum384(msg)
			for what, req := range map[string]*kmspb.AsymmetricSignRequest{
				"a 31-byte digest":    {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:31]}}},
				"a SHA-384 digest":    {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha384{Sha384: d384[:]}}},
				"a wrong digest crc":  {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:]}}, DigestCrc32C: kmsCRC([]byte("x"))},
				"a wrong data crc":    {Name: v, Data: msg, DataCrc32C: kmsCRC([]byte("x"))},
				"digest and data":     {Name: v, Data: msg, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:]}}},
				"neither digest/data": {Name: v},
			} {
				if _, err := c.AsymmetricSign(ctx, req); kmsCode(variant, err) != codes.InvalidArgument {
					t.Errorf("AsymmetricSign with %s = %v, want INVALID_ARGUMENT", what, err)
				}
			}
		})
	}
}
