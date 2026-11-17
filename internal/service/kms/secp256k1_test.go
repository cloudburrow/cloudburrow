package kms

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSecp256k1 is EC_SIGN_SECP256K1_SHA256 (#1719) as the algorithms page
// documents it: HSM and HSM_SINGLE_TENANT only, a secp256k1
// SubjectPublicKeyInfo, and lower-S DER signatures of a SHA-256 digest.
func TestSecp256k1(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "k1"})
	alg := kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256
	tmpl := func(pl kmspb.ProtectionLevel) *kmspb.CryptoKey {
		return &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg, ProtectionLevel: pl}}
	}

	// The levels the algorithms page does not list it under are refused.
	for _, pl := range []kmspb.ProtectionLevel{kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED, kmspb.ProtectionLevel_SOFTWARE, kmspb.ProtectionLevel_EXTERNAL, kmspb.ProtectionLevel_EXTERNAL_VPC} {
		_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sw", CryptoKey: tmpl(pl)})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "HSM") {
			t.Errorf("a %s key = %v, want INVALID_ARGUMENT naming HSM", pl, err)
		}
	}
	// Nor can a SOFTWARE signing key's template switch to it.
	newKey(t, c, ring.GetName(), "p256", kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version_template.algorithm"}},
		CryptoKey: &kmspb.CryptoKey{Name: ring.GetName() + "/cryptoKeys/p256", VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a SOFTWARE key's template to secp256k1 = %v", err)
	}

	k, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "hsm", CryptoKey: tmpl(kmspb.ProtectionLevel_HSM)})
	if err != nil || k.GetVersionTemplate().GetAlgorithm() != alg || k.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Fatalf("CreateCryptoKey HSM = %v, %v", k, err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: k.GetName()})
	if err != nil || v2.GetAlgorithm() != alg || v2.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Fatalf("CreateCryptoKeyVersion = %v, %v", v2, err)
	}

	for _, v := range []string{k.GetName() + "/cryptoKeyVersions/1", v2.GetName()} {
		pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_DER})
		if err != nil {
			t.Fatal(err)
		}
		if pk.GetAlgorithm() != alg || pk.GetProtectionLevel() != kmspb.ProtectionLevel_HSM || pk.GetPemCrc32C().GetValue() != crc32c([]byte(pk.GetPem())) ||
			pk.GetPublicKey().GetCrc32CChecksum().GetValue() != crc32c(pk.GetPublicKey().GetData()) || !strings.HasPrefix(pk.GetPem(), "-----BEGIN PUBLIC KEY-----\n") {
			t.Fatalf("GetPublicKey = %v", pk)
		}
		b, _ := pem.Decode([]byte(pk.GetPem()))
		if string(b.Bytes) != string(pk.GetPublicKey().GetData()) {
			t.Error("the DER format is not the PEM's body")
		}
		// id-ecPublicKey with the namedCurve secp256k1, and an uncompressed point.
		var spki struct {
			Algo struct {
				Algorithm, Curve asn1.ObjectIdentifier
			}
			Key asn1.BitString
		}
		if _, err := asn1.Unmarshal(b.Bytes, &spki); err != nil || spki.Algo.Algorithm.String() != "1.2.840.10045.2.1" ||
			spki.Algo.Curve.String() != "1.3.132.0.10" || len(spki.Key.Bytes) != 65 || spki.Key.Bytes[0] != 4 {
			t.Fatalf("SubjectPublicKeyInfo = %+v, %v", spki, err)
		}
		pub, err := secp256k1.ParsePubKey(spki.Key.Bytes)
		if err != nil {
			t.Fatal(err)
		}

		msg := []byte("cloudburrow")
		d := sha256.Sum256(msg)
		keccakLike := sha512.Sum512_256(msg) // any 32 bytes, as a Keccak digest
		for what, req := range map[string]*kmspb.AsymmetricSignRequest{
			"data":           {Name: v, Data: msg, DataCrc32C: crc(msg)},
			"digest":         {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:]}}, DigestCrc32C: crc(d[:])},
			"another digest": {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: keccakLike[:]}}},
		} {
			resp, err := c.AsymmetricSign(ctx, req)
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if resp.GetSignatureCrc32C().GetValue() != crc32c(resp.GetSignature()) || resp.GetName() != v ||
				resp.GetProtectionLevel() != kmspb.ProtectionLevel_HSM ||
				resp.GetVerifiedDataCrc32C() != (req.DataCrc32C != nil) || resp.GetVerifiedDigestCrc32C() != (req.DigestCrc32C != nil) {
				t.Errorf("%s: integrity fields = %v", what, resp)
			}
			digest := d[:]
			if what == "another digest" {
				digest = keccakLike[:]
			}
			if !verifySecp256k1(pub, digest, resp.GetSignature()) {
				t.Errorf("%s: the signature does not verify with the public key", what)
			}
			if ok, err := VerifySignature(pk.GetPem(), alg, msg, resp.GetSignature()); what != "another digest" && (!ok || err != nil) {
				t.Errorf("%s: VerifySignature = %v, %v", what, ok, err)
			}
			// DER ECDSA-Sig-Value, lower-S.
			var rs struct{ R, S *big.Int }
			if rest, err := asn1.Unmarshal(resp.GetSignature(), &rs); err != nil || len(rest) > 0 {
				t.Fatalf("%s: the signature is not DER: %v", what, err)
			}
			halfN := new(big.Int).Rsh(secp256k1.S256().N, 1)
			if rs.S.Cmp(halfN) > 0 {
				t.Errorf("%s: s is not lower-S", what)
			}
		}
		if ok, _ := VerifySignature(pk.GetPem(), alg, []byte("other"), mustSign(t, c, v, msg)); ok {
			t.Error("VerifySignature accepted a signature of other data")
		}
	}

	v := v2.GetName()
	d := sha256.Sum256([]byte("x"))
	d384 := sha512.Sum384([]byte("x"))
	for what, req := range map[string]*kmspb.AsymmetricSignRequest{
		"a 31-byte digest":     {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:31]}}},
		"a SHA-384 digest":     {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha384{Sha384: d384[:]}}},
		"data and digest":      {Name: v, Data: []byte("x"), Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:]}}},
		"neither":              {Name: v},
		"a wrong digest crc":   {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d[:]}}, DigestCrc32C: crc([]byte("y"))},
		"a wrong data crc":     {Name: v, Data: []byte("x"), DataCrc32C: crc([]byte("y"))},
		"data over the limit":  {Name: v, Data: make([]byte, maxSoftwareBytes+1)},
		"an unknown public fm": nil,
	} {
		if req == nil {
			if _, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_NIST_PQC}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("GetPublicKey NIST_PQC = %v", err)
			}
			continue
		}
		if _, err := c.AsymmetricSign(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v, want INVALID_ARGUMENT", what, err)
		}
	}
}

func mustSign(t *testing.T, c *kms.KeyManagementClient, v string, msg []byte) []byte {
	t.Helper()
	r, err := c.AsymmetricSign(context.Background(), &kmspb.AsymmetricSignRequest{Name: v, Data: msg})
	if err != nil {
		t.Fatal(err)
	}
	return r.GetSignature()
}

// opensslSecp256k1 is a PKCS #8 secp256k1 key as OpenSSL writes it: the
// curve in the AlgorithmIdentifier, and the public key in the ECPrivateKey.
func opensslSecp256k1(t *testing.T, k *secp256k1.PrivateKey) []byte {
	t.Helper()
	pub := k.PubKey().SerializeUncompressed()
	inner, err := asn1.Marshal(ecPrivateKey{Version: 1, PrivateKey: k.Serialize(), PublicKey: asn1.BitString{Bytes: pub, BitLength: 8 * len(pub)}})
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(pkcs8{Algo: algorithmIdentifier{Algorithm: oidECPublicKey, Parameters: secp256k1Params()}, PrivateKey: inner})
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestSecp256k1Material(t *testing.T) {
	k, _ := secp256k1.GeneratePrivateKey()
	der := opensslSecp256k1(t, k)
	got, err := parseSecp256k1(der)
	if err != nil || !got.PubKey().IsEqual(k.PubKey()) {
		t.Fatalf("parse = %v", err)
	}
	// The curve named in the ECPrivateKey only.
	inner, _ := asn1.Marshal(ecPrivateKey{Version: 1, PrivateKey: k.Serialize(), NamedCurveOID: oidSecp256k1})
	only, _ := asn1.Marshal(pkcs8{Algo: algorithmIdentifier{Algorithm: oidECPublicKey}, PrivateKey: inner})
	if _, err := parseSecp256k1(only); err != nil {
		t.Errorf("curve in the ECPrivateKey only = %v", err)
	}
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p256der, _ := x509.MarshalPKCS8PrivateKey(p256)
	other, _ := secp256k1.GeneratePrivateKey()
	badPub, _ := asn1.Marshal(ecPrivateKey{Version: 1, PrivateKey: k.Serialize(), NamedCurveOID: oidSecp256k1,
		PublicKey: asn1.BitString{Bytes: other.PubKey().SerializeUncompressed(), BitLength: 520}})
	mismatch, _ := asn1.Marshal(pkcs8{Algo: algorithmIdentifier{Algorithm: oidECPublicKey}, PrivateKey: badPub})
	unnamedInner, _ := asn1.Marshal(ecPrivateKey{Version: 1, PrivateKey: k.Serialize()})
	unnamed, _ := asn1.Marshal(pkcs8{Algo: algorithmIdentifier{Algorithm: oidECPublicKey}, PrivateKey: unnamedInner})
	for what, b := range map[string][]byte{"P-256": p256der, "garbage": []byte("x"), "a mismatched public key": mismatch, "no curve": unnamed} {
		if _, err := parseSecp256k1(b); err == nil {
			t.Errorf("%s parsed", what)
		}
	}
	if _, err := importedMaterial(kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256, p256der); status.Code(err) != codes.InvalidArgument {
		t.Errorf("importing a P-256 key as secp256k1 = %v", err)
	}
	if _, err := importedMaterial(kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, der); status.Code(err) != codes.InvalidArgument {
		t.Errorf("importing a secp256k1 key as P-256 = %v", err)
	}
}

func TestSecp256k1Import(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "k1imp"})
	job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_HSM}})
	if err != nil {
		t.Fatal(err)
	}
	k, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
			Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256, ProtectionLevel: kmspb.ProtectionLevel_HSM}}})
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := secp256k1.GeneratePrivateKey()
	v, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: k.GetName(), ImportJob: job.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256, WrappedKey: wrapForImport(t, job.GetPublicKey().GetPem(), false, opensslSecp256k1(t, priv))})
	if err != nil {
		t.Fatal(err)
	}
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(pk.GetPem()))
	pub, err := parseSecp256k1Public(b.Bytes)
	if err != nil || !pub.IsEqual(priv.PubKey()) {
		t.Fatalf("the public key is not the imported key's: %v", err)
	}
	msg := []byte("imported")
	d := sha256.Sum256(msg)
	if !verifySecp256k1(priv.PubKey(), d[:], mustSign(t, c, v.GetName(), msg)) {
		t.Error("the imported key's signature does not verify")
	}
}

// TestSecp256k1MatchesOpenSSL reads a throwaway test key OpenSSL 3.5
// generated (genpkey ec_paramgen_curve:secp256k1, then pkcs8 -topk8 DER) and
// checks that the SubjectPublicKeyInfo served for it is byte-for-byte what
// `openssl ec -pubout -outform DER` wrote.
func TestSecp256k1MatchesOpenSSL(t *testing.T) {
	const (
		priv = "308184020100301006072a8648ce3d020106052b8104000a046d306b020101042088c40c20714d2bb957255e18d5e5d3e77c97753d2a247124639aa7e873f250a0a14403420004ab1b8e4294e1e7f29c6076ba09ecee35ec94f063b0844f4eb4cb9dcac790ff48a0ddabcb060f510a0b34f757d26908562519df20de5cbe9dac0781eb15c8cfce"
		pub  = "3056301006072a8648ce3d020106052b8104000a03420004ab1b8e4294e1e7f29c6076ba09ecee35ec94f063b0844f4eb4cb9dcac790ff48a0ddabcb060f510a0b34f757d26908562519df20de5cbe9dac0781eb15c8cfce"
	)
	der, _ := hex.DecodeString(priv)
	k, err := parseSecp256k1(der)
	if err != nil {
		t.Fatal(err)
	}
	got, err := secp256k1PublicDER(k.PubKey())
	if err != nil || hex.EncodeToString(got) != pub {
		t.Errorf("public key = %x, %v; OpenSSL wrote %s", got, err, pub)
	}
}
