package kms

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestCiphertextSurvivesReopeningADurableStore covers --mode persistent
// without a cluster (#418): ciphertext written before the store is closed
// decrypts after it is reopened, and the version states are unchanged.
func TestCiphertextSurvivesReopeningADurableStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kms")
	ctx := context.Background()
	db, err := store.OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := clientFor(t, db)
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "durable"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	destroyed, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: v2.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("before-close"), AdditionalAuthenticatedData: []byte("aad")})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	c = clientFor(t, reopened)
	dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: []byte("aad")})
	if err != nil || !bytes.Equal(dec.GetPlaintext(), []byte("before-close")) {
		t.Fatalf("Decrypt after reopening = %q, %v", dec.GetPlaintext(), err)
	}
	got, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v2.GetName()})
	if err != nil || got.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED || !got.GetDestroyTime().AsTime().Equal(destroyed.GetDestroyTime().AsTime()) {
		t.Errorf("v2 after reopening = %v at %v, %v; want DESTROY_SCHEDULED at %v", got.GetState(), got.GetDestroyTime().AsTime(), err, destroyed.GetDestroyTime().AsTime())
	}
	if v3, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()}); err != nil || v3.GetName() != key.GetName()+"/cryptoKeyVersions/3" {
		t.Errorf("next version after reopening = %v, %v; want 3", v3.GetName(), err)
	}
}
