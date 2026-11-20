package kms

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/sha256"
	"crypto/sha3"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
	slhdsa "github.com/cloudburrow/cloudburrow/third_party/circl-slhdsa"
)

// The SLH-DSA keys (#1946): pure over data, the pre-hash variant over a
// SHA-256 digest or data; each signature verifies with the NIST_PQC public
// key, and a digest is refused by the pure variant.
func TestSLHDSASignatures(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "slh"})
	data := []byte("post-quantum")
	sum := sha256.Sum256(data)
	for _, a := range []kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{kmspb.CryptoKeyVersion_PQ_SIGN_SLH_DSA_SHA2_128S, kmspb.CryptoKeyVersion_PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256} {
		v := newKey(t, c, ring.GetName(), "k"+a.String()[8:12]+map[bool]string{true: "h"}[algorithms[a].hash != 0], kmspb.CryptoKey_ASYMMETRIC_SIGN, a)
		if _, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s GetPublicKey without a format = %v, want INVALID_ARGUMENT", a, err)
		}
		if _, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_PEM}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s GetPublicKey PEM = %v, want INVALID_ARGUMENT", a, err)
		}
		pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_NIST_PQC})
		if err != nil || len(pk.GetPublicKey().GetData()) != 32 || pk.GetPublicKeyFormat() != kmspb.PublicKey_NIST_PQC {
			t.Fatalf("%s GetPublicKey = %v, %v", a, pk, err)
		}
		pub := slhdsa.PublicKey{ID: slhdsa.SHA2_128s}
		if err := pub.UnmarshalBinary(pk.GetPublicKey().GetData()); err != nil {
			t.Fatal(err)
		}
		if algorithms[a].hash == 0 {
			sig, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: data})
			if err != nil || len(sig.GetSignature()) != 7856 || !slhdsa.Verify(&pub, slhdsa.NewMessage(data), sig.GetSignature(), nil) {
				t.Errorf("%s AsymmetricSign = %d bytes, %v", a, len(sig.GetSignature()), err)
			}
			_, err = c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: sum[:]}}})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s with a digest = %v, want INVALID_ARGUMENT", a, err)
			}
			continue
		}
		ph, _ := slhdsa.NewPreHashWithHash(crypto.SHA256)
		ph.Write(data)
		msg, _ := ph.BuildMessage()
		for name, req := range map[string]*kmspb.AsymmetricSignRequest{
			"digest": {Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: sum[:]}}},
			"data":   {Name: v, Data: data},
		} {
			sig, err := c.AsymmetricSign(ctx, req)
			if err != nil || !slhdsa.Verify(&pub, msg, sig.GetSignature(), nil) {
				t.Errorf("%s AsymmetricSign of the %s = %v", a, name, err)
			}
			if slhdsa.Verify(&pub, slhdsa.NewMessage(data), sig.GetSignature(), nil) {
				t.Errorf("%s: a pre-hash signature verifies as a pure one", a)
			}
		}
		_, err = c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha384{Sha384: make([]byte, 48)}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s with a SHA-384 digest = %v, want INVALID_ARGUMENT", a, err)
		}
	}
}

// NewPreHashedDigest, the constructor CloudBurrow adds to the vendored
// CIRCL, builds the message PreHash.BuildMessage does for the same data.
func TestNewPreHashedDigestMatchesPreHash(t *testing.T) {
	_, priv, err := slhdsa.GenerateKey(nil, slhdsa.SHA2_128s)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("abc")
	sum := sha256.Sum256(data)
	m, err := slhdsa.NewPreHashedDigest(crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig, err := slhdsa.SignDeterministic(&priv, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	ph, _ := slhdsa.NewPreHashWithHash(crypto.SHA256)
	ph.Write(data)
	want, _ := ph.BuildMessage()
	pub := priv.PublicKey()
	if !slhdsa.Verify(&pub, want, sig, nil) {
		t.Error("a signature of NewPreHashedDigest does not verify against PreHash.BuildMessage")
	}
	if _, err := slhdsa.NewPreHashedDigest(crypto.SHA256, sum[:31]); err == nil {
		t.Error("a short digest was accepted")
	}
}

// X-Wing (#1946): XWING_RAW_BYTES is pk_M || pk_X, and Decapsulate returns
// the secret of draft-connolly-cfrg-xwing-kem-06's Encapsulate, computed
// here from the public key alone.
func TestXWingDecapsulate(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "xwing"})
	v := newKey(t, c, ring.GetName(), "x", kmspb.CryptoKey_KEY_ENCAPSULATION, kmspb.CryptoKeyVersion_KEM_XWING)
	for _, f := range []kmspb.PublicKey_PublicKeyFormat{kmspb.PublicKey_PUBLIC_KEY_FORMAT_UNSPECIFIED, kmspb.PublicKey_NIST_PQC, kmspb.PublicKey_PEM} {
		if _, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: f}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("GetPublicKey %s = %v, want INVALID_ARGUMENT", f, err)
		}
	}
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_XWING_RAW_BYTES})
	if err != nil || len(pk.GetPublicKey().GetData()) != 1216 {
		t.Fatalf("GetPublicKey = %v, %v", pk, err)
	}
	raw := pk.GetPublicKey().GetData()
	ekM, err := mlkem.NewEncapsulationKey768(raw[:1184])
	if err != nil {
		t.Fatal(err)
	}
	pkX, err := ecdh.X25519().NewPublicKey(raw[1184:])
	if err != nil {
		t.Fatal(err)
	}
	ssM, ctM := ekM.Encapsulate()
	eph, _ := ecdh.X25519().GenerateKey(nil)
	ssX, _ := eph.ECDH(pkX)
	ctX := eph.PublicKey().Bytes()
	h := sha3.New256()
	for _, b := range [][]byte{ssM, ssX, ctX, raw[1184:], []byte(`\.//^\`)} {
		h.Write(b)
	}
	want := h.Sum(nil)
	resp, err := c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: append(ctM, ctX...)})
	if err != nil || !bytes.Equal(resp.GetSharedSecret(), want) {
		t.Fatalf("Decapsulate = %x, %v; want %x", resp.GetSharedSecret(), err, want)
	}
	if _, err := c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: ctM}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Decapsulate of a short ciphertext = %v, want INVALID_ARGUMENT", err)
	}
}

// The PQ algorithms are SOFTWARE only (the algorithms page).
func TestPQAlgorithmsAreSoftwareOnly(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "pqhsm"})
	_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_KEY_ENCAPSULATION, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
			Algorithm: kmspb.CryptoKeyVersion_KEM_XWING, ProtectionLevel: kmspb.ProtectionLevel_HSM}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an HSM X-Wing key = %v, want INVALID_ARGUMENT", err)
	}
}

// import_only (#1946): the key is made with no version, echoes import_only,
// refuses CreateCryptoKeyVersion and a rotation schedule, and takes an
// imported version.
func TestImportOnlyKey(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "io"})
	mk := func(id string, skip bool, period *durationpb.Duration) (*kmspb.CryptoKey, error) {
		k := &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, ImportOnly: true}
		if period != nil {
			k.RotationSchedule = &kmspb.CryptoKey_RotationPeriod{RotationPeriod: period}
			k.NextRotationTime = nil
		}
		return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: id, CryptoKey: k, SkipInitialVersionCreation: skip})
	}
	if _, err := mk("a", false, nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("import_only without skip_initial_version_creation = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := mk("b", true, durationpb.New(48*3600e9)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("import_only with a rotation period = %v, want INVALID_ARGUMENT", err)
	}
	k, err := mk("k", true, nil)
	if err != nil || !k.GetImportOnly() {
		t.Fatalf("CreateCryptoKey = %v, %v", k, err)
	}
	if got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: k.GetName()}); err != nil || !got.GetImportOnly() {
		t.Errorf("GetCryptoKey = %v, %v", got, err)
	}
	if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: k.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateCryptoKeyVersion = %v, want FAILED_PRECONDITION", err)
	}
	_, err = c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: &kmspb.CryptoKey{Name: k.GetName(),
		RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(48 * 3600e9)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateCryptoKey rotation_period = %v, want INVALID_ARGUMENT", err)
	}
	job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := WrapForImport(job.GetPublicKey().GetPem(), job.GetImportMethod(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: k.GetName(), ImportJob: job.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKeyMaterial: &kmspb.ImportCryptoKeyVersionRequest_RsaAesWrappedKey{RsaAesWrappedKey: wrapped}})
	if err != nil || v.GetImportJob() != job.GetName() {
		t.Errorf("ImportCryptoKeyVersion = %v, %v", v, err)
	}
}

// IAM on import jobs (#1946): a policy is set, read back and kept.
func TestIamOnImportJobs(t *testing.T) {
	srv, iam, ring, _ := iamFixture(t, store.NewMemory())
	ctx := context.Background()
	job, err := srv.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring, ImportJobId: "j",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := iam.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()}); err != nil || len(p.GetBindings()) != 0 {
		t.Fatalf("GetIamPolicy of a new job = %v, %v", p, err)
	}
	member := "user:importer@example.com"
	pol := &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/cloudkms.importer", Members: []string{member}}}}
	if _, err := iam.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job.GetName(), Policy: pol}); err != nil {
		t.Fatal(err)
	}
	p, err := iam.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()})
	if err != nil || len(p.GetBindings()) != 1 || p.GetBindings()[0].GetMembers()[0] != member {
		t.Errorf("GetIamPolicy = %v, %v", p, err)
	}
	if got, err := srv.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job.GetName()}); err != nil || got.GetState() != kmspb.ImportJob_ACTIVE {
		t.Errorf("GetImportJob after SetIamPolicy = %v, %v", got, err)
	}
	r, err := iam.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: job.GetName(), Permissions: []string{"cloudkms.importJobs.useToImport"}})
	if err != nil || len(r.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", r, err)
	}
}
