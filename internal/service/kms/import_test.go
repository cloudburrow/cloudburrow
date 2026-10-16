package kms

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	kwp "github.com/tink-crypto/tink-go/v2/kwp/subtle"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// wrapForImport is CKM_RSA_AES_KEY_WRAP as Google's import sample builds it,
// with Tink's AES-KWP: an independent implementation of the wrapping.
func wrapForImport(t *testing.T, jobPEM string, sha256OAEP bool, key []byte) []byte {
	t.Helper()
	b, _ := pem.Decode([]byte(jobPEM))
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	h := sha1.New()
	if sha256OAEP {
		h = sha256.New()
	}
	wrappedKEK, err := rsa.EncryptOAEP(h, rand.Reader, pub.(*rsa.PublicKey), kek, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := kwp.NewKWP(kek)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := w.Wrap(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(wrappedKEK, wrapped...)
}

func TestImportJobAndImportCryptoKeyVersion(t *testing.T) {
	clock := sched.NewFakeClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	c := clientOf(t, NewServerWithClock(store.NewMemory(), clock))
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "imp"})
	job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil || job.GetState() != kmspb.ImportJob_ACTIVE || job.GetPublicKey().GetPem() == "" ||
		!job.GetExpireTime().AsTime().Equal(clock.Now().Add(72*time.Hour)) {
		t.Fatalf("CreateImportJob = %v, %v", job, err)
	}
	if got, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job.GetName()}); err != nil || got.GetPublicKey().GetPem() != job.GetPublicKey().GetPem() {
		t.Errorf("GetImportJob = %v, %v", got, err)
	}
	if got, err := c.ListImportJobs(ctx, &kmspb.ListImportJobsRequest{Parent: ring.GetName(), Filter: "state=ACTIVE"}).Next(); err != nil || got.GetName() != job.GetName() {
		t.Errorf("ListImportJobs = %v, %v", got, err)
	}

	// A symmetric key: Encrypt with the imported version, by name.
	sym, _ := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sym", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	material := bytes.Repeat([]byte{5}, 32)
	v, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sym.GetName(), ImportJob: job.GetName(),
		Algorithm:  kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		WrappedKey: wrapForImport(t, job.GetPublicKey().GetPem(), false, material)})
	if err != nil || v.GetState() != kmspb.CryptoKeyVersion_ENABLED || v.GetImportJob() != job.GetName() || v.GetImportTime() == nil {
		t.Fatalf("ImportCryptoKeyVersion = %v, %v", v, err)
	}
	if enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: v.GetName(), Plaintext: []byte("x")}); err != nil {
		t.Errorf("Encrypt with the imported version = %v", err)
	} else if dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: sym.GetName(), Ciphertext: enc.GetCiphertext()}); err != nil || string(dec.GetPlaintext()) != "x" {
		t.Errorf("Decrypt = %v, %v", dec, err)
	}

	// An EC signing key: the public key is the imported one's.
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	sign, _ := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sign", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}})
	ev, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sign.GetName(), ImportJob: job.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, WrappedKey: wrapForImport(t, job.GetPublicKey().GetPem(), false, der)})
	if err != nil {
		t.Fatal(err)
	}
	pub := publicKey(t, c, ev.GetName()).(*ecdsa.PublicKey)
	if !pub.Equal(&priv.PublicKey) {
		t.Error("the public key is not the imported key's")
	}

	for what, req := range map[string]*kmspb.ImportCryptoKeyVersionRequest{
		"a wrong-length key":              {Parent: sym.GetName(), ImportJob: job.GetName(), Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrapForImport(t, job.GetPublicKey().GetPem(), false, []byte("sixteen byte key"))},
		"another job's hash":              {Parent: sym.GetName(), ImportJob: job.GetName(), Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrapForImport(t, job.GetPublicKey().GetPem(), true, material)},
		"an algorithm of another purpose": {Parent: sym.GetName(), ImportJob: job.GetName(), Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256, WrappedKey: []byte("x")},
		"garbage":                         {Parent: sym.GetName(), ImportJob: job.GetName(), Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: []byte("garbage")},
	} {
		if _, err := c.ImportCryptoKeyVersion(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v, want INVALID_ARGUMENT", what, err)
		}
	}
	clock.Advance(72 * time.Hour)
	if got, _ := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job.GetName()}); got.GetState() != kmspb.ImportJob_EXPIRED || got.GetExpireEventTime() == nil {
		t.Errorf("after three days = %v", got)
	}
	if _, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sym.GetName(), ImportJob: job.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrapForImport(t, job.GetPublicKey().GetPem(), false, material)}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an expired job = %v", err)
	}
	if _, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "hsm",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256, ProtectionLevel: kmspb.ProtectionLevel_EXTERNAL}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an EXTERNAL job = %v", err)
	}
	// An HSM job is served (#1452), and imports only into an HSM key.
	hsmJob, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "hsm",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_HSM}})
	if err != nil || hsmJob.GetProtectionLevel() != kmspb.ProtectionLevel_HSM || hsmJob.GetAttestation() != nil {
		t.Fatalf("an HSM job = %v, %v", hsmJob, err)
	}
	hsmKey, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "hsm-import", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sym.GetName(), ImportJob: hsmJob.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrapForImport(t, hsmJob.GetPublicKey().GetPem(), false, material)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an HSM job into a SOFTWARE key = %v", err)
	}
	hv, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: hsmKey.GetName(), ImportJob: hsmJob.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrapForImport(t, hsmJob.GetPublicKey().GetPem(), false, material)})
	if err != nil || hv.GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Errorf("an HSM job into an HSM key = %v, %v", hv, err)
	}
}

// RSA_OAEP_*_SHA256 wraps the key directly with RSA-OAEP.
func TestImportWithDirectRSAOAEP(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "imp2"})
	job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(job.GetPublicKey().GetPem()))
	pub, _ := x509.ParsePKIXPublicKey(b.Bytes)
	key := bytes.Repeat([]byte{3}, 32)
	wrapped, _ := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), key, nil)
	mac, _ := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "mac", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_MAC, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256}}})
	v, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: mac.GetName(), ImportJob: job.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256, WrappedKey: wrapped})
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.MacSign(ctx, &kmspb.MacSignRequest{Name: v.GetName(), Data: []byte("d")})
	if err != nil {
		t.Fatal(err)
	}
	want := hmacSHA256(key, []byte("d"))
	if !bytes.Equal(m.GetMac(), want) {
		t.Error("the MAC is not HMAC-SHA256 under the imported key")
	}
	if _, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete an imported version = %v", err)
	}
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// WrapForImport's AES-KWP is opened by Tink's, an independent
// implementation, for every length class RFC 5649 distinguishes, and its
// output imports through ImportCryptoKeyVersion for both families of
// import method (#1219).
func TestWrapForImport(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, 32)
	w, err := kwp.NewKWP(kek)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{16, 20, 32, 33, 1217} {
		key := make([]byte, n)
		_, _ = rand.Read(key)
		c, err := wrapKWP(kek, key)
		if err != nil {
			t.Fatalf("wrapKWP %d bytes: %v", n, err)
		}
		got, err := w.Unwrap(c)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("Tink unwraps %d bytes wrapped here to %x, %v", n, got, err)
		}
		if back, err := unwrapKWP(kek, c); err != nil || !bytes.Equal(back, key) {
			t.Errorf("unwrapKWP of %d bytes = %v", n, err)
		}
	}
	if _, err := wrapKWP(kek, nil); err == nil {
		t.Error("wrapKWP wrapped nothing")
	}
	priv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pubDER, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	key := bytes.Repeat([]byte{9}, 32)
	for _, m := range []kmspb.ImportJob_ImportMethod{kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, kmspb.ImportJob_RSA_OAEP_3072_SHA256} {
		c, err := WrapForImport(pubPEM, m, key)
		if err != nil {
			t.Fatalf("WrapForImport %s: %v", m, err)
		}
		got, err := unwrap(importJob{Name: "j", Method: m.String(), Private: der}, c)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("%s: unwrap = %x, %v", m, got, err)
		}
	}
	if _, err := WrapForImport(pubPEM, kmspb.ImportJob_RSA_OAEP_3072_SHA256, make([]byte, 1000)); err == nil {
		t.Error("RSA-OAEP alone wrapped a key longer than the modulus allows")
	}
}
