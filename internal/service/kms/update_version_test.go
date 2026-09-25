package kms

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func stateMask() *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: []string{"state"}} }

// A version past DISABLED cannot be updated: it must be restored first
// (#400). The state is set through the store, since DestroyCryptoKeyVersion
// is a later issue.
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion FAILED_PRECONDITION: a DESTROY_SCHEDULED or DESTROYED version
func TestUpdateVersionRefusesADestroyScheduledVersion(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "u"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(db)
	name := key.GetPrimary().GetName()
	for _, st := range []kmspb.CryptoKeyVersion_CryptoKeyVersionState{kmspb.CryptoKeyVersion_DESTROY_SCHEDULED, kmspb.CryptoKeyVersion_DESTROYED} {
		var v keyVersion
		if found, err := s.get(dbKey(versionPrefix, name), &v); err != nil || !found {
			t.Fatal(found, err)
		}
		v.State = st.String()
		if err := s.put(dbKey(versionPrefix, name), v); err != nil {
			t.Fatal(err)
		}
		_, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: stateMask(),
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: kmspb.CryptoKeyVersion_ENABLED}})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("updating a %s version = %v, want FAILED_PRECONDITION", st, err)
		}
	}
}

// Disabling the primary keeps it as the primary, now DISABLED.
func TestDisablingThePrimaryKeepsIt(t *testing.T) {
	ctx := context.Background()
	c := client(t)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "p"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: stateMask(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetPrimary().GetName(), State: kmspb.CryptoKeyVersion_DISABLED}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	if err != nil || got.GetPrimary().GetName() != key.GetPrimary().GetName() || got.GetPrimary().GetState() != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("after disabling the primary, the key's primary is %v (%v); want the same version, DISABLED", got.GetPrimary(), err)
	}
}

// Names are validated like every other RPC's.
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion INVALID_ARGUMENT: a malformed version name
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion NOT_FOUND: a well-formed version that does not exist
func TestUpdateVersionNames(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	for name, want := range map[string]codes.Code{okKey + "/cryptoKeyVersions/01": codes.InvalidArgument, okKey + "/cryptoKeyVersions/9": codes.NotFound} {
		_, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: stateMask(),
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: kmspb.CryptoKeyVersion_DISABLED}})
		if status.Code(err) != want {
			t.Errorf("UpdateCryptoKeyVersion(%q) = %v, want %s", name, err, want)
		}
	}
	_, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"external_protection_level_options"}},
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: okVersion}})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("an EKM mask path = %v, want UNIMPLEMENTED", err)
	}
}
