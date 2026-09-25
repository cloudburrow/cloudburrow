package kms

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// A DESTROY_SCHEDULED version cannot become the primary (#401); its state is
// set through the store, since DestroyCryptoKeyVersion is a later issue.
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion FAILED_PRECONDITION: a DESTROY_SCHEDULED target version
func TestPrimaryRefusesADestroyScheduledVersion(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "pr"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(db)
	var v keyVersion
	if found, err := s.get(dbKey(versionPrefix, v2.GetName()), &v); err != nil || !found {
		t.Fatal(found, err)
	}
	v.State = kmspb.CryptoKeyVersion_DESTROY_SCHEDULED.String()
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("making a DESTROY_SCHEDULED version primary = %v, want FAILED_PRECONDITION", err)
	}
	for id, want := range map[string]codes.Code{"abc": codes.InvalidArgument, "0": codes.InvalidArgument, "9": codes.NotFound} {
		_, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: id})
		if status.Code(err) != want {
			t.Errorf("crypto_key_version_id %q = %v, want %s", id, err, want)
		}
	}
}
