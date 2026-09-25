package kms

import (
	"context"
	"hash/crc32"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The compat test's cases, in-process, plus the ones only the store can set
// up (#411).
func TestEncryptInProcess(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "enc"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	tab := crc32.MakeTable(crc32.Castagnoli)
	pt, aad := []byte("PLAINMARKER"), []byte("AADMARKER")
	resp, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad,
		PlaintextCrc32C: wrapperspb.Int64(int64(crc32.Checksum(pt, tab))), AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(int64(crc32.Checksum(aad, tab)))})
	if err != nil || resp.GetName() != key.GetPrimary().GetName() || !resp.GetVerifiedPlaintextCrc32C() || !resp.GetVerifiedAdditionalAuthenticatedDataCrc32C() ||
		resp.GetCiphertextCrc32C().GetValue() != int64(crc32.Checksum(resp.GetCiphertext(), tab)) || resp.GetProtectionLevel() != kmspb.ProtectionLevel_SOFTWARE {
		t.Fatalf("Encrypt = %+v, %v", resp, err)
	}
	// The ciphertext opens with the primary's material.
	var v keyVersion
	if found, err := NewServer(db).get(dbKey(versionPrefix, key.GetPrimary().GetName()), &v); !found || err != nil {
		t.Fatal(found, err)
	}
	if got, err := open(v.Material, 1, resp.GetCiphertext(), aad); err != nil || string(got) != string(pt) {
		t.Errorf("the ciphertext does not open: %v", err)
	}

	// A DESTROY_SCHEDULED version, set through the store, is refused by name.
	v.State = kmspb.CryptoKeyVersion_DESTROY_SCHEDULED.String()
	v.DestroyTime = v.Created.AddDate(1, 0, 0)
	if err := NewServer(db).put(dbKey(versionPrefix, v.Name), v); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: v.Name, Plaintext: pt}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Encrypt with a DESTROY_SCHEDULED version = %v, want FAILED_PRECONDITION", err)
	}

	// No failing path echoes the plaintext or the AAD.
	for what, req := range map[string]*kmspb.EncryptRequest{
		"bad plaintext CRC":   {Name: key.GetName(), Plaintext: pt, PlaintextCrc32C: wrapperspb.Int64(1)},
		"bad AAD CRC":         {Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad, AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(1)},
		"refused version":     {Name: v.Name, Plaintext: pt, AdditionalAuthenticatedData: aad},
		"missing key":         {Name: ring.GetName() + "/cryptoKeys/none", Plaintext: pt, AdditionalAuthenticatedData: aad},
		"oversized AAD":       {Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: append([]byte("AADMARKER"), make([]byte, 64*1024)...)},
		"oversized plaintext": {Name: key.GetName(), Plaintext: append([]byte("PLAINMARKER"), make([]byte, 64*1024)...)},
	} {
		_, err := c.Encrypt(ctx, req)
		if err == nil {
			t.Errorf("%s: no error", what)
			continue
		}
		if strings.Contains(err.Error(), "PLAINMARKER") || strings.Contains(err.Error(), "AADMARKER") {
			t.Errorf("%s: the error echoes the plaintext or AAD: %v", what, err)
		}
	}
}
