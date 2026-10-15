package kms

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newKey(t *testing.T, c *kms.KeyManagementClient, ring, id string, p kmspb.CryptoKey_CryptoKeyPurpose, a kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) string {
	t.Helper()
	k, err := c.CreateCryptoKey(context.Background(), &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
		CryptoKey: &kmspb.CryptoKey{Purpose: p, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: a},
			DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatalf("CreateCryptoKey %s: %v", a, err)
	}
	if k.GetPurpose() != p || k.GetVersionTemplate().GetAlgorithm() != a {
		t.Fatalf("CreateCryptoKey = %v", k)
	}
	if (k.GetPrimary() != nil) != (p == kmspb.CryptoKey_ENCRYPT_DECRYPT) {
		t.Errorf("%s primary = %v: only ENCRYPT_DECRYPT has one", p, k.GetPrimary())
	}
	return k.GetName() + "/cryptoKeyVersions/1"
}

func publicKey(t *testing.T, c *kms.KeyManagementClient, v string) any {
	t.Helper()
	pk, err := c.GetPublicKey(context.Background(), &kmspb.GetPublicKeyRequest{Name: v})
	if err != nil {
		t.Fatal(err)
	}
	if pk.GetPemCrc32C().GetValue() != crc32c([]byte(pk.GetPem())) || pk.GetName() != v {
		t.Fatalf("GetPublicKey = %v", pk)
	}
	b, _ := pem.Decode([]byte(pk.GetPem()))
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestAsymmetricSignVerifiesWithThePublicKey(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "sign"})
	msg := []byte("cloudburrow")
	d256 := sha256.Sum256(msg)
	d512 := sha512.Sum512(msg)
	var first string
	for i, a := range []kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
		kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384,
		kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256, kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512, kmspb.CryptoKeyVersion_RSA_SIGN_RAW_PKCS1_2048,
		kmspb.CryptoKeyVersion_EC_SIGN_ED25519,
	} {
		v := newKey(t, c, ring.GetName(), "s"+string(rune('a'+i)), kmspb.CryptoKey_ASYMMETRIC_SIGN, a)
		if i == 0 {
			first = v
		}
		pub := publicKey(t, c, v)
		req := &kmspb.AsymmetricSignRequest{Name: v, Data: msg, DataCrc32C: crc(msg)}
		resp, err := c.AsymmetricSign(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		if !resp.GetVerifiedDataCrc32C() || resp.GetSignatureCrc32C().GetValue() != crc32c(resp.GetSignature()) {
			t.Errorf("%s: integrity fields = %v", a, resp)
		}
		sig := resp.GetSignature()
		var ok bool
		switch a {
		case kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:
			ok = ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), d256[:], sig)
		case kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:
			d := sha512.Sum384(msg)
			ok = ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), d[:], sig)
		case kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256:
			ok = rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA256, d256[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		case kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:
			ok = rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA512, d512[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		case kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256:
			ok = rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, d256[:], sig) == nil
			// The same signature from the digest.
			r2, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d256[:]}}, DigestCrc32C: crc(d256[:])})
			if err != nil || !r2.GetVerifiedDigestCrc32C() || !bytes.Equal(r2.GetSignature(), sig) {
				t.Errorf("sign the digest = %v, %v", r2, err)
			}
			if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha512{Sha512: d512[:]}}}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("a SHA-512 digest for a SHA-256 key = %v", err)
			}
			if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: msg, Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: d256[:]}}}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("data and digest = %v", err)
			}
		case kmspb.CryptoKeyVersion_RSA_SIGN_RAW_PKCS1_2048:
			ok = rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), 0, msg, sig) == nil
		case kmspb.CryptoKeyVersion_EC_SIGN_ED25519:
			ok = ed25519.Verify(pub.(ed25519.PublicKey), msg, sig)
		}
		if !ok {
			t.Errorf("%s: the signature does not verify with the PEM public key", a)
		}
	}
	if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: first, Data: msg, DataCrc32C: crc([]byte("other"))}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a wrong data_crc32c = %v", err)
	}
}

func TestAsymmetricDecryptOpensWhatThePublicKeyEncrypts(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "dec"})
	v := newKey(t, c, ring.GetName(), "k", kmspb.CryptoKey_ASYMMETRIC_DECRYPT, kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_2048_SHA256)
	pub := publicKey(t, c, v).(*rsa.PublicKey)
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{Name: v, Ciphertext: ct, CiphertextCrc32C: crc(ct)})
	if err != nil || string(resp.GetPlaintext()) != "secret" || !resp.GetVerifiedCiphertextCrc32C() {
		t.Fatalf("AsymmetricDecrypt = %v, %v", resp, err)
	}
	if _, err := c.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{Name: v, Ciphertext: []byte("junk")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("junk = %v", err)
	}
	if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: []byte("x")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("sign with a decrypt key = %v", err)
	}
	if _, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: v, Plaintext: []byte("x")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Encrypt with a decrypt key = %v", err)
	}
}

func TestMacSignAndVerify(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "mac"})
	v := newKey(t, c, ring.GetName(), "k", kmspb.CryptoKey_MAC, kmspb.CryptoKeyVersion_HMAC_SHA256)
	data := []byte("cloudburrow")
	m, err := c.MacSign(ctx, &kmspb.MacSignRequest{Name: v, Data: data, DataCrc32C: crc(data)})
	if err != nil || len(m.GetMac()) != 32 || !m.GetVerifiedDataCrc32C() {
		t.Fatalf("MacSign = %v, %v", m, err)
	}
	ok, err := c.MacVerify(ctx, &kmspb.MacVerifyRequest{Name: v, Data: data, Mac: m.GetMac(), MacCrc32C: crc(m.GetMac())})
	if err != nil || !ok.GetSuccess() || !ok.GetVerifiedSuccessIntegrity() || !ok.GetVerifiedMacCrc32C() {
		t.Fatalf("MacVerify = %v, %v", ok, err)
	}
	bad, err := c.MacVerify(ctx, &kmspb.MacVerifyRequest{Name: v, Data: []byte("other"), Mac: m.GetMac()})
	if err != nil || bad.GetSuccess() || bad.GetVerifiedSuccessIntegrity() {
		t.Fatalf("MacVerify of other data = %v, %v", bad, err)
	}
	if hmac.Equal(m.GetMac(), make([]byte, 32)) {
		t.Error("an all-zero MAC")
	}
}

func TestRawEncryptAndDecrypt(t *testing.T) {
	db := store.NewMemory()
	c := clientFor(t, db)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "raw"})
	pt16 := []byte("sixteen byte msg")
	for i, a := range []kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
		kmspb.CryptoKeyVersion_AES_256_GCM, kmspb.CryptoKeyVersion_AES_128_GCM,
		kmspb.CryptoKeyVersion_AES_256_CBC, kmspb.CryptoKeyVersion_AES_128_CTR,
	} {
		v := newKey(t, c, ring.GetName(), string(rune('a'+i)), kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, a)
		gcm := a == kmspb.CryptoKeyVersion_AES_256_GCM || a == kmspb.CryptoKeyVersion_AES_128_GCM
		var aad []byte
		if gcm {
			aad = []byte("context")
		}
		enc, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: v, Plaintext: pt16, AdditionalAuthenticatedData: aad,
			PlaintextCrc32C: crc(pt16), AdditionalAuthenticatedDataCrc32C: crc(aad)})
		if err != nil {
			t.Fatalf("%s RawEncrypt: %v", a, err)
		}
		if !enc.GetVerifiedPlaintextCrc32C() || !enc.GetVerifiedAdditionalAuthenticatedDataCrc32C() ||
			enc.GetCiphertextCrc32C().GetValue() != crc32c(enc.GetCiphertext()) || enc.GetInitializationVectorCrc32C().GetValue() != crc32c(enc.GetInitializationVector()) {
			t.Errorf("%s integrity = %v", a, enc)
		}
		// Standard AES opens it with the stored key.
		var kv keyVersion
		if _, err := NewServer(db).get(dbKey(versionPrefix, v), &kv); err != nil {
			t.Fatal(err)
		}
		block, _ := aes.NewCipher(kv.Material)
		got := make([]byte, len(pt16))
		switch {
		case gcm:
			g, _ := cipher.NewGCM(block)
			got, err = g.Open(nil, enc.GetInitializationVector(), enc.GetCiphertext(), aad)
			if err != nil || enc.GetTagLength() != 16 {
				t.Errorf("%s: GCM open = %v, tag %d", a, err, enc.GetTagLength())
			}
		case a == kmspb.CryptoKeyVersion_AES_256_CBC:
			cipher.NewCBCDecrypter(block, enc.GetInitializationVector()).CryptBlocks(got, enc.GetCiphertext())
		default:
			cipher.NewCTR(block, enc.GetInitializationVector()).XORKeyStream(got, enc.GetCiphertext())
		}
		if !bytes.Equal(got, pt16) {
			t.Errorf("%s: standard AES opened %q", a, got)
		}
		dec, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: v, Ciphertext: enc.GetCiphertext(), InitializationVector: enc.GetInitializationVector(),
			AdditionalAuthenticatedData: aad, CiphertextCrc32C: crc(enc.GetCiphertext())})
		if err != nil || !bytes.Equal(dec.GetPlaintext(), pt16) || !dec.GetVerifiedCiphertextCrc32C() {
			t.Errorf("%s RawDecrypt = %v, %v", a, dec, err)
		}
		if gcm {
			if _, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: v, Ciphertext: enc.GetCiphertext(), InitializationVector: enc.GetInitializationVector(),
				AdditionalAuthenticatedData: []byte("wrong")}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s wrong AAD = %v", a, err)
			}
			if _, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: v, Plaintext: pt16, InitializationVector: make([]byte, 12)}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s caller IV = %v", a, err)
			}
		} else {
			iv := bytes.Repeat([]byte{7}, 16)
			enc2, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: v, Plaintext: pt16, InitializationVector: iv, InitializationVectorCrc32C: crc(iv)})
			if err != nil || !bytes.Equal(enc2.GetInitializationVector(), iv) || !enc2.GetVerifiedInitializationVectorCrc32C() {
				t.Errorf("%s caller IV = %v, %v", a, enc2, err)
			}
			if _, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: v, Plaintext: pt16, AdditionalAuthenticatedData: []byte("x")}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s AAD = %v", a, err)
			}
		}
	}
}

func TestGenerateRandomBytes(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	r, err := c.GenerateRandomBytes(ctx, &kmspb.GenerateRandomBytesRequest{Location: loc, LengthBytes: 32, ProtectionLevel: kmspb.ProtectionLevel_HSM})
	if err != nil || len(r.GetData()) != 32 || r.GetDataCrc32C().GetValue() != crc32c(r.GetData()) {
		t.Fatalf("GenerateRandomBytes = %v, %v", r, err)
	}
	for what, req := range map[string]*kmspb.GenerateRandomBytesRequest{
		"7 bytes":    {Location: loc, LengthBytes: 7, ProtectionLevel: kmspb.ProtectionLevel_HSM},
		"1025 bytes": {Location: loc, LengthBytes: 1025, ProtectionLevel: kmspb.ProtectionLevel_HSM},
		"SOFTWARE":   {Location: loc, LengthBytes: 8, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE},
		"bad loc":    {Location: "projects/x", LengthBytes: 8, ProtectionLevel: kmspb.ProtectionLevel_HSM},
	} {
		if _, err := c.GenerateRandomBytes(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v", what, err)
		}
	}
}

func TestDeleteKeyAfterEveryVersionIsDestroyedAndDeleted(t *testing.T) {
	clock := sched.NewFakeClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	c := clientOf(t, NewServerWithClock(store.NewMemory(), clock))
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "del"})
	v := newKey(t, c, ring.GetName(), "k", kmspb.CryptoKey_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION)
	key := ring.GetName() + "/cryptoKeys/k"
	if _, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete an ENABLED version = %v", err)
	}
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: v}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete a DESTROY_SCHEDULED version = %v", err)
	}
	if _, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: key}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete a key with a version = %v", err)
	}
	clock.Advance(24 * time.Hour)
	op, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted version = %v", err)
	}
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil || k.GetPrimary() != nil {
		t.Errorf("the key after its primary is deleted = %v, %v", k, err)
	}
	kop, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := kop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	md, err := kop.Metadata()
	if err != nil || md.GetRetiredResource() == "" {
		t.Fatalf("metadata = %v, %v", md, err)
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted key = %v", err)
	}
	r, err := c.GetRetiredResource(ctx, &kmspb.GetRetiredResourceRequest{Name: md.GetRetiredResource()})
	if err != nil || r.GetOriginalResource() != key || r.GetResourceType() != "cloudkms.googleapis.com/CryptoKey" || r.GetDeleteTime() == nil {
		t.Fatalf("GetRetiredResource = %v, %v", r, err)
	}
	it := c.ListRetiredResources(ctx, &kmspb.ListRetiredResourcesRequest{Parent: loc})
	if got, err := it.Next(); err != nil || got.GetName() != r.GetName() {
		t.Errorf("ListRetiredResources = %v, %v", got, err)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("reuse a retired name = %v", err)
	}
}

func TestDecapsulateRecoversTheEncapsulatedSecret(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "kem"})
	v := newKey(t, c, ring.GetName(), "k", kmspb.CryptoKey_KEY_ENCAPSULATION, kmspb.CryptoKeyVersion_ML_KEM_768)
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_NIST_PQC})
	if err != nil || pk.GetPublicKey().GetCrc32CChecksum().GetValue() != crc32c(pk.GetPublicKey().GetData()) {
		t.Fatalf("GetPublicKey = %v, %v", pk, err)
	}
	if _, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no format for ML-KEM = %v", err)
	}
	ek, err := mlkem.NewEncapsulationKey768(pk.GetPublicKey().GetData())
	if err != nil {
		t.Fatal(err)
	}
	secret, ct := ek.Encapsulate()
	resp, err := c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: ct, CiphertextCrc32C: crc(ct)})
	if err != nil || !bytes.Equal(resp.GetSharedSecret(), secret) || !resp.GetVerifiedCiphertextCrc32C() || resp.GetSharedSecretCrc32C() != crc32c(secret) {
		t.Fatalf("Decapsulate = %v, %v", resp, err)
	}
}
