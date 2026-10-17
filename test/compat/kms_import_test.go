//go:build compat

package compat

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	kwp "github.com/tink-crypto/tink-go/v2/kwp/subtle"
	"google.golang.org/grpc/codes"
)

// covers: google.cloud.kms.v1.KeyManagementService/CreateImportJob, google.cloud.kms.v1.KeyManagementService/GetImportJob, google.cloud.kms.v1.KeyManagementService/ListImportJobs, google.cloud.kms.v1.KeyManagementService/ImportCryptoKeyVersion
//
// TestKMSImportManuallyWrappedKeys (#1212) imports as Google's
// import-manually-wrapped-key sample does: an ephemeral AES-256 key wrapped
// with RSA-OAEP (SHA-1) under the import job's public key, then the key
// wrapped with AES-KWP (Tink's, an independent implementation). A
// symmetric key encrypts and decrypts with the imported version, and an EC
// key's public key is the one imported.
//
// unverified: google.cloud.kms.v1.KeyManagementService/ImportCryptoKeyVersion INVALID_ARGUMENT: a wrapped key that does not unwrap, or a key of the wrong length
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/kms/docs/importing-a-key
func TestKMSImportManuallyWrappedKeys(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "import-"+variant)
			job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring, ImportJobId: "j",
				ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
			if err != nil {
				t.Fatalf("CreateImportJob: %v", err)
			}
			if job.GetState() != kmspb.ImportJob_ACTIVE || job.GetPublicKey().GetPem() == "" || job.GetExpireTime() == nil {
				t.Fatalf("CreateImportJob = %v", job)
			}
			if got, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job.GetName()}); err != nil || got.GetPublicKey().GetPem() != job.GetPublicKey().GetPem() {
				t.Errorf("GetImportJob = %v, %v", got, err)
			}
			if got, err := c.ListImportJobs(ctx, &kmspb.ListImportJobsRequest{Parent: ring}).Next(); err != nil || got.GetName() != job.GetName() {
				t.Errorf("ListImportJobs = %v, %v", got, err)
			}
			b, _ := pem.Decode([]byte(job.GetPublicKey().GetPem()))
			pub, err := x509.ParsePKIXPublicKey(b.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			wrap := func(key []byte) []byte {
				kek := make([]byte, 32)
				_, _ = rand.Read(kek)
				wrappedKEK, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub.(*rsa.PublicKey), kek, nil)
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

			sym, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "sym", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
			if err != nil {
				t.Fatal(err)
			}
			v, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sym.GetName(), ImportJob: job.GetName(),
				Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrap(bytes.Repeat([]byte{5}, 32))})
			if err != nil {
				t.Fatalf("ImportCryptoKeyVersion: %v", err)
			}
			if v.GetState() != kmspb.CryptoKeyVersion_ENABLED || v.GetImportJob() != job.GetName() || v.GetImportTime() == nil {
				t.Errorf("imported version = %v", v)
			}
			enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: v.GetName(), Plaintext: []byte("imported")})
			if err != nil {
				t.Fatal(err)
			}
			if dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: sym.GetName(), Ciphertext: enc.GetCiphertext()}); err != nil || string(dec.GetPlaintext()) != "imported" {
				t.Errorf("Decrypt = %v, %v", dec, err)
			}

			priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			der, _ := x509.MarshalPKCS8PrivateKey(priv)
			sign, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "sign", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}})
			if err != nil {
				t.Fatal(err)
			}
			ev, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sign.GetName(), ImportJob: job.GetName(),
				Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, WrappedKey: wrap(der)})
			if err != nil {
				t.Fatal(err)
			}
			if got := kmsPublicKey(t, h, c, ev.GetName()).(*ecdsa.PublicKey); !got.Equal(&priv.PublicKey) {
				t.Error("GetPublicKey is not the imported key's")
			}
			if _, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sym.GetName(), ImportJob: job.GetName(),
				Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: wrap([]byte("sixteen byte key"))}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("a 16-byte key for a 32-byte algorithm = %v, want INVALID_ARGUMENT", err)
			}
			if _, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sym.GetName(), ImportJob: job.GetName(),
				Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: []byte("garbage")}); kmsCode(variant, err) != codes.InvalidArgument {
				t.Errorf("garbage = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/Decapsulate, google.cloud.kms.v1.KeyManagementService/GetPublicKey
//
// TestKMSDecapsulateMLKEM (#1212): the secret encapsulated with Go's
// crypto/mlkem under the NIST_PQC public key is the one Decapsulate returns.
func TestKMSDecapsulateMLKEM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			ring := kmsRing(t, h, c, "kem-"+variant)
			v := kmsPurposeKey(t, h, c, ring, "k", kmspb.CryptoKey_KEY_ENCAPSULATION, kmspb.CryptoKeyVersion_ML_KEM_768)
			pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_NIST_PQC})
			if err != nil || pk.GetPublicKey().GetCrc32CChecksum().GetValue() != crc32c(pk.GetPublicKey().GetData()) {
				t.Fatalf("GetPublicKey = %v, %v", pk, err)
			}
			ek, err := mlkem.NewEncapsulationKey768(pk.GetPublicKey().GetData())
			if err != nil {
				t.Fatal(err)
			}
			secret, ct := ek.Encapsulate()
			resp, err := c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: ct, CiphertextCrc32C: kmsCRC(ct)})
			if err != nil || !bytes.Equal(resp.GetSharedSecret(), secret) || !resp.GetVerifiedCiphertextCrc32C() {
				t.Fatalf("Decapsulate = %v, %v", resp, err)
			}
		})
	}
}
