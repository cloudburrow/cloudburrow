package kms

import (
	"context"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The compat test's cases in-process, plus states only the store can set up
// (#412).
func TestDecryptInProcess(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "dec"})
	sym := &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", CryptoKey: sym})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "other", CryptoKey: sym})
	pt, aad := []byte("PLAINMARKER"), []byte("AADMARKER")
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad})
	if err != nil {
		t.Fatal(err)
	}
	tab := crc32.MakeTable(crc32.Castagnoli)
	dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad,
		CiphertextCrc32C: wrapperspb.Int64(int64(crc32.Checksum(enc.GetCiphertext(), tab))), AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(int64(crc32.Checksum(aad, tab)))})
	if err != nil || string(dec.GetPlaintext()) != string(pt) || !dec.GetUsedPrimary() || dec.GetProtectionLevel() != kmspb.ProtectionLevel_SOFTWARE ||
		dec.GetPlaintextCrc32C().GetValue() != int64(crc32.Checksum(pt, tab)) {
		t.Fatalf("Decrypt = %+v, %v", dec, err)
	}

	// After rotation the old ciphertext still decrypts, not with the primary.
	v2, _ := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if _, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"}); err != nil {
		t.Fatal(err)
	}
	if d, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad}); err != nil || d.GetUsedPrimary() {
		t.Errorf("after rotation: %v, used_primary %v; want a decrypt that did not use the primary", err, d.GetUsedPrimary())
	}
	if e, _ := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: pt}); e.GetName() != v2.GetName() {
		t.Errorf("after rotation Encrypt used %s, want %s", e.GetName(), v2.GetName())
	}

	flipped := append([]byte(nil), enc.GetCiphertext()...)
	flipped[len(flipped)-1] ^= 1
	fromOther, _ := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: other.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad})
	for what, req := range map[string]*kmspb.DecryptRequest{
		"wrong AAD":          {Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: []byte("WRONGAADMARKER")},
		"a flipped byte":     {Name: key.GetName(), Ciphertext: flipped, AdditionalAuthenticatedData: aad},
		"another key's":      {Name: key.GetName(), Ciphertext: fromOther.GetCiphertext(), AdditionalAuthenticatedData: aad},
		"garbage":            {Name: key.GetName(), Ciphertext: []byte("PLAINMARKER not a ciphertext at all")},
		"a version name":     {Name: key.GetPrimary().GetName(), Ciphertext: enc.GetCiphertext()},
		"empty ciphertext":   {Name: key.GetName()},
		"bad ciphertext CRC": {Name: key.GetName(), Ciphertext: enc.GetCiphertext(), CiphertextCrc32C: wrapperspb.Int64(1)},
		"bad AAD CRC":        {Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad, AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(1)},
	} {
		_, err := c.Decrypt(ctx, req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v, want INVALID_ARGUMENT", what, err)
		}
		if err != nil && (strings.Contains(err.Error(), "PLAINMARKER") || strings.Contains(err.Error(), "AADMARKER")) {
			t.Errorf("%s: the error echoes the plaintext or AAD: %v", what, err)
		}
	}
	for field, req := range map[string]*kmspb.DecryptRequest{
		"ciphertext_crc32c":                    {Name: key.GetName(), Ciphertext: enc.GetCiphertext(), CiphertextCrc32C: wrapperspb.Int64(1)},
		"additional_authenticated_data_crc32c": {Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad, AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(1)},
	} {
		if _, err := c.Decrypt(ctx, req); !strings.Contains(status.Convert(err).Message(), field) {
			t.Errorf("a wrong %s does not name it: %v", field, err)
		}
	}

	// Version 1, now not the primary, set to each refused state through the
	// store; DESTROYED has its material erased.
	s := NewServer(db)
	name := key.GetPrimary().GetName()
	for _, st := range []kmspb.CryptoKeyVersion_CryptoKeyVersionState{kmspb.CryptoKeyVersion_DISABLED,
		kmspb.CryptoKeyVersion_DESTROY_SCHEDULED, kmspb.CryptoKeyVersion_DESTROYED} {
		var v keyVersion
		if found, err := s.get(dbKey(versionPrefix, name), &v); !found || err != nil {
			t.Fatal(found, err)
		}
		v.State = st.String()
		v.DestroyTime = time.Now().Add(time.Hour)
		if st == kmspb.CryptoKeyVersion_DESTROYED {
			v.Material = nil
		}
		if err := s.put(dbKey(versionPrefix, name), v); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("Decrypt with a %s version = %v, want FAILED_PRECONDITION", st, err)
		}
	}
}
