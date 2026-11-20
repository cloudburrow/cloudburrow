//go:build compat

package compat

import (
	"bytes"
	"crypto"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/cloudflare/circl/kem/xwing"
	circlslh "github.com/cloudflare/circl/sign/slhdsa"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/signature"
	tinkmldsa "github.com/tink-crypto/tink-go/v2/signature/mldsa"
	tinkslh "github.com/tink-crypto/tink-go/v2/signature/slhdsa"
	"github.com/tink-crypto/tink-go/v2/tink"
	"google.golang.org/grpc/codes"
)

// The post-quantum algorithms (#1946), through the official Go client over
// gRPC and REST. Every signature is checked with an implementation
// independent of the server's: Tink's own ML-DSA and SLH-DSA (Google's, not
// Go's crypto/mldsa or CIRCL), and CIRCL upstream for the pre-hash SLH-DSA,
// which Tink lacks. X-Wing is encapsulated with CIRCL's kem/xwing, the
// server's being Go's crypto/mlkem, crypto/ecdh and crypto/sha3.

// tinkVerify verifies sig over data with Tink and pub, a raw (no output
// prefix) Tink ML-DSA or SLH-DSA public key.
func tinkVerify(t *testing.T, pub any, sig, data []byte) error {
	t.Helper()
	km := keyset.NewManager()
	var id uint32
	var err error
	switch k := pub.(type) {
	case *tinkmldsa.PublicKey:
		id, err = km.AddKey(k)
	case *tinkslh.PublicKey:
		id, err = km.AddKey(k)
	}
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if err := km.SetPrimary(id); err != nil {
		t.Fatal(err)
	}
	h, err := km.Handle()
	if err != nil {
		t.Fatal(err)
	}
	var v tink.Verifier
	if v, err = signature.NewVerifier(h); err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v.Verify(sig, data)
}

// covers: google.cloud.kms.v1.KeyManagementService/AsymmetricSign, google.cloud.kms.v1.KeyManagementService/GetPublicKey
//
// TestKMSPostQuantumSignatures (#1946): a key of each ML-DSA and SLH-DSA
// algorithm signs, its NIST_PQC public key is the FIPS 204/205 encoding
// (and an ML-DSA key's PEM is its SubjectPublicKeyInfo), and the signature
// verifies with Tink (pure ML-DSA and SLH-DSA) or CIRCL (HashSLH-DSA over
// the SHA-256 digest sent).
func TestKMSPostQuantumSignatures(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	data := []byte("signed by a post-quantum key")
	sum := sha256.Sum256(data)
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "pq-"+variant)
			for a, inst := range map[kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm]tinkmldsa.Instance{
				kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_44: tinkmldsa.MLDSA44,
				kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_65: tinkmldsa.MLDSA65,
				kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_87: tinkmldsa.MLDSA87,
			} {
				v := kmsPurposeKey(t, h, c, ring, "mldsa-"+a.String()[15:], kmspb.CryptoKey_ASYMMETRIC_SIGN, a)
				raw := pqPublicKey(t, h, c, v, kmspb.PublicKey_NIST_PQC)
				if p := pqPublicKey(t, h, c, v, kmspb.PublicKey_PEM); !bytes.Contains(p, []byte("-----BEGIN PUBLIC KEY-----")) {
					t.Errorf("%s PEM = %q", a, p)
				}
				resp, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: data, DataCrc32C: kmsCRC(data)})
				if err != nil || !resp.GetVerifiedDataCrc32C() || resp.GetSignatureCrc32C().GetValue() != crc32c(resp.GetSignature()) {
					t.Fatalf("%s AsymmetricSign = %v, %v", a, resp, err)
				}
				params, err := tinkmldsa.NewParameters(inst, tinkmldsa.VariantNoPrefix)
				if err != nil {
					t.Fatal(err)
				}
				pub, err := tinkmldsa.NewPublicKey(raw, 0, params)
				if err != nil {
					t.Fatalf("%s: Tink refuses the NIST_PQC key: %v", a, err)
				}
				if err := tinkVerify(t, pub, resp.GetSignature(), data); err != nil {
					t.Errorf("%s: Tink does not verify the signature: %v", a, err)
				}
				if err := tinkVerify(t, pub, resp.GetSignature(), []byte("other data")); err == nil {
					t.Errorf("%s: Tink verifies the signature over other data", a)
				}
			}

			v := kmsPurposeKey(t, h, c, ring, "slh", kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_PQ_SIGN_SLH_DSA_SHA2_128S)
			raw := pqPublicKey(t, h, c, v, kmspb.PublicKey_NIST_PQC)
			resp, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: data})
			if err != nil {
				t.Fatalf("SLH-DSA AsymmetricSign: %v", err)
			}
			params, err := tinkslh.NewParameters(tinkslh.SHA2, 64, tinkslh.SmallSignature, tinkslh.VariantNoPrefix)
			if err != nil {
				t.Fatal(err)
			}
			pub, err := tinkslh.NewPublicKey(raw, 0, params)
			if err != nil {
				t.Fatalf("Tink refuses the SLH-DSA NIST_PQC key: %v", err)
			}
			if err := tinkVerify(t, pub, resp.GetSignature(), data); err != nil {
				t.Errorf("SLH-DSA: Tink does not verify the signature: %v", err)
			}
			_, err = c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: sum[:]}}})
			if kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("pure SLH-DSA with a digest = %v, want INVALID_ARGUMENT", err)
			}

			v = kmsPurposeKey(t, h, c, ring, "hslh", kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256)
			raw = pqPublicKey(t, h, c, v, kmspb.PublicKey_NIST_PQC)
			resp, err = c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v,
				Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: sum[:]}}, DigestCrc32C: kmsCRC(sum[:])})
			if err != nil || !resp.GetVerifiedDigestCrc32C() {
				t.Fatalf("HashSLH-DSA AsymmetricSign = %v, %v", resp, err)
			}
			cpub := circlslh.PublicKey{ID: circlslh.SHA2_128s}
			if err := cpub.UnmarshalBinary(raw); err != nil {
				t.Fatal(err)
			}
			ph, err := circlslh.NewPreHashWithHash(crypto.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			ph.Write(data)
			msg, err := ph.BuildMessage()
			if err != nil {
				t.Fatal(err)
			}
			if !circlslh.Verify(&cpub, msg, resp.GetSignature(), nil) {
				t.Error("HashSLH-DSA: CIRCL does not verify the signature of the SHA-256 digest")
			}
		})
	}
}

// pqPublicKey is GetPublicKey's public_key in format f, its CRC32C checked.
func pqPublicKey(t *testing.T, h *Harness, c *kms.KeyManagementClient, v string, f kmspb.PublicKey_PublicKeyFormat) []byte {
	t.Helper()
	pk, err := c.GetPublicKey(h.Context(), &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: f})
	if err != nil {
		t.Fatalf("GetPublicKey %s: %v", f, err)
	}
	if pk.GetPublicKeyFormat() != f || pk.GetPublicKey().GetCrc32CChecksum().GetValue() != crc32c(pk.GetPublicKey().GetData()) {
		t.Fatalf("GetPublicKey %s = %v", f, pk)
	}
	return pk.GetPublicKey().GetData()
}

// covers: google.cloud.kms.v1.KeyManagementService/Decapsulate, google.cloud.kms.v1.KeyManagementService/GetPublicKey
//
// TestKMSDecapsulateXWing (#1946): CIRCL's X-Wing encapsulates under the
// XWING_RAW_BYTES public key, and Decapsulate returns the same secret;
// ML-KEM-1024 does the same with Go's crypto/mlkem under NIST_PQC. A
// post-quantum key has no default format.
func TestKMSDecapsulateXWing(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "xwing-"+variant)
			v := kmsPurposeKey(t, h, c, ring, "x", kmspb.CryptoKey_KEY_ENCAPSULATION, kmspb.CryptoKeyVersion_KEM_XWING)
			if _, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("GetPublicKey with no format = %v, want INVALID_ARGUMENT", err)
			}
			raw := pqPublicKey(t, h, c, v, kmspb.PublicKey_XWING_RAW_BYTES)
			pk, err := xwing.Scheme().UnmarshalBinaryPublicKey(raw)
			if err != nil {
				t.Fatalf("CIRCL refuses the X-Wing public key: %v", err)
			}
			ct, ss, err := xwing.Scheme().Encapsulate(pk)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: ct, CiphertextCrc32C: kmsCRC(ct)})
			if err != nil || !bytes.Equal(resp.GetSharedSecret(), ss) || !resp.GetVerifiedCiphertextCrc32C() ||
				resp.GetSharedSecretCrc32C() != crc32c(ss) {
				t.Fatalf("Decapsulate = %v, %v; want the secret CIRCL encapsulated", resp, err)
			}

			v = kmsPurposeKey(t, h, c, ring, "k1024", kmspb.CryptoKey_KEY_ENCAPSULATION, kmspb.CryptoKeyVersion_ML_KEM_1024)
			ek, err := mlkem.NewEncapsulationKey1024(pqPublicKey(t, h, c, v, kmspb.PublicKey_NIST_PQC))
			if err != nil {
				t.Fatal(err)
			}
			secret, kct := ek.Encapsulate()
			resp, err = c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: kct})
			if err != nil || !bytes.Equal(resp.GetSharedSecret(), secret) {
				t.Fatalf("ML-KEM-1024 Decapsulate = %v, %v", resp, err)
			}
		})
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/ImportCryptoKeyVersion
//
// TestKMSImportOnlyKeys (#1946): an import_only key is created with
// skip_initial_version_creation and reads back import_only; without it, or
// with a rotation period, it is refused, as "Keys created as --import-only
// must be rotated manually"; CreateCryptoKeyVersion is refused, as "new key
// versions for this key must be imported"; ImportCryptoKeyVersion makes
// version 1, which encrypts.
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: import_only without skip_initial_version_creation, or with a rotation schedule
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion FAILED_PRECONDITION: on an import_only key
// Docs re-read 2026-11-20, still unanswered: https://docs.cloud.google.com/kms/docs/importing-a-key
func TestKMSImportOnlyKeys(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "io-"+variant)
			create := func(id string, skip bool) (*kmspb.CryptoKey, error) {
				return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id, SkipInitialVersionCreation: skip,
					CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, ImportOnly: true}})
			}
			if _, err := create("generated", false); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("import_only with an initial version = %v, want INVALID_ARGUMENT", err)
			}
			k, err := create("k", true)
			if err != nil || !k.GetImportOnly() || k.GetPrimary() != nil {
				t.Fatalf("CreateCryptoKey = %v, %v", k, err)
			}
			if got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: k.GetName()}); err != nil || !got.GetImportOnly() {
				t.Errorf("GetCryptoKey = %v, %v; want import_only", got, err)
			}
			if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: k.GetName()}); kmsCode(variant, err) != codes.FailedPrecondition {
				t.Errorf("CreateCryptoKeyVersion on an import_only key = %v, want FAILED_PRECONDITION", err)
			}
			v := importAES(t, h, c, ring, k.GetName(), "j")
			if v.GetImportJob() == "" || v.GetName() != k.GetName()+"/cryptoKeyVersions/1" {
				t.Fatalf("ImportCryptoKeyVersion = %v", v)
			}
			if _, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: v.GetName(), Plaintext: []byte("hi")}); err != nil {
				t.Errorf("Encrypt with the imported version: %v", err)
			}
		})
	}
}

// importAES imports 32 zero bytes into key with a new RSA_OAEP_3072_SHA256
// job named id.
func importAES(t *testing.T, h *Harness, c *kms.KeyManagementClient, ring, key, id string) *kmspb.CryptoKeyVersion {
	t.Helper()
	job, err := c.CreateImportJob(h.Context(), &kmspb.CreateImportJobRequest{Parent: ring, ImportJobId: id,
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil {
		t.Fatalf("CreateImportJob: %v", err)
	}
	v, err := c.ImportCryptoKeyVersion(h.Context(), importRequest(t, job, key))
	if err != nil {
		t.Fatalf("ImportCryptoKeyVersion: %v", err)
	}
	return v
}

// importRequest wraps 32 zero bytes with RSA-OAEP (SHA-256) under job's
// public key, as the direct import methods take them.
func importRequest(t *testing.T, job *kmspb.ImportJob, key string) *kmspb.ImportCryptoKeyVersionRequest {
	t.Helper()
	b, _ := pem.Decode([]byte(job.GetPublicKey().GetPem()))
	if b == nil {
		t.Fatal("the import job's public key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &kmspb.ImportCryptoKeyVersionRequest{Parent: key, ImportJob: job.GetName(),
		Algorithm:          kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		WrappedKeyMaterial: &kmspb.ImportCryptoKeyVersionRequest_RsaAesWrappedKey{RsaAesWrappedKey: wrapped}}
}

// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy, google.cloud.kms.v1.KeyManagementService/ImportCryptoKeyVersion
//
// TestIAMEnforcementKMSImportJobs (#1946), with --iam-enforcement=on, the
// official client over gRPC and a minted token: an account with no role is
// refused ImportCryptoKeyVersion (cloudkms.importJobs.useToImport), and the
// job's getIamPolicy and setIamPolicy, PERMISSION_DENIED with Google's
// "denied on resource" message; Cloud KMS Importer granted on the import
// job's own policy, which the owner reads back, lets the import through,
// while getIamPolicy and setIamPolicy stay denied, as the Importer holds
// neither.
func TestIAMEnforcementKMSImportJobs(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvKMS)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iekij-")
	owner, err := kms.NewKeyManagementClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	ring, err := owner.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: "ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := owner.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "key", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, ImportOnly: true}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	job, err := owner.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "job",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil {
		t.Fatalf("CreateImportJob: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iekij")
	as, err := kms.NewKeyManagementClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	imp := func() error {
		_, err := as.ImportCryptoKeyVersion(ctx, importRequest(t, job, key.GetName()))
		return err
	}
	denied := func(perm string) string {
		return "Permission '" + perm + "' denied on resource '" + job.GetName() + "' (or it may not exist)."
	}
	wantDenied(t, "ImportCryptoKeyVersion without a role", imp(), denied("cloudkms.importJobs.useToImport"), "cloudkms.importJobs.useToImport", true)
	getPolicy := func() error {
		_, err := as.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()})
		return err
	}
	setPolicy := func() error {
		_, err := as.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job.GetName(), Policy: &iampb.Policy{}})
		return err
	}
	wantDenied(t, "the import job's getIamPolicy without a role", getPolicy(), denied("cloudkms.importJobs.getIamPolicy"), "cloudkms.importJobs.getIamPolicy", true)
	wantDenied(t, "the import job's setIamPolicy without a role", setPolicy(), denied("cloudkms.importJobs.setIamPolicy"), "cloudkms.importJobs.setIamPolicy", true)

	undo := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, job.GetName(),
		&iampb.Binding{Role: "roles/cloudkms.importer", Members: []string{"serviceAccount:" + p.email}})
	defer undo()
	pol, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()})
	if err != nil || len(pol.GetBindings()) != 1 || pol.GetBindings()[0].GetRole() != "roles/cloudkms.importer" {
		t.Fatalf("the import job's policy = %v, %v", pol, err)
	}
	waitGranted(t, "ImportCryptoKeyVersion as an importer on the job", imp)
	wantDenied(t, "the import job's getIamPolicy as an importer", getPolicy(), denied("cloudkms.importJobs.getIamPolicy"), "cloudkms.importJobs.getIamPolicy", true)
	wantDenied(t, "the import job's setIamPolicy as an importer", setPolicy(), denied("cloudkms.importJobs.setIamPolicy"), "cloudkms.importJobs.setIamPolicy", true)
}
