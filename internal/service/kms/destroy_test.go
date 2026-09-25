package kms

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// destroy_time is exactly the clock's now plus the key's duration (#402).
func TestDestroyTimeIsNowPlusTheKeysDuration(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	clock := sched.NewFakeClock(start)
	c := clientOf(t, NewServerWithClock(store.NewMemory(), clock))
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "dt"})
	for id, d := range map[string]*durationpb.Duration{"default": nil, "day": durationpb.New(24 * time.Hour)} {
		key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: id,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: d}})
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Hour)
		v, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: key.GetPrimary().GetName()})
		if err != nil {
			t.Fatal(err)
		}
		want := clock.Now().Add(key.GetDestroyScheduledDuration().AsDuration())
		if v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED || !v.GetDestroyTime().AsTime().Equal(want) {
			t.Errorf("%s: %s destroy_time %v, want DESTROY_SCHEDULED at %v", id, v.GetState(), v.GetDestroyTime().AsTime(), want)
		}
		got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
		if err != nil || got.GetPrimary().GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
			t.Errorf("%s: the destroyed primary reads as %v (%v); want DESTROY_SCHEDULED", id, got.GetPrimary().GetState(), err)
		}
	}
}

// A DISABLED version can be destroyed; a DESTROYED one cannot, and a
// DESTROY_SCHEDULED one cannot be updated.
//
// unverified: google.cloud.kms.v1.KeyManagementService/DestroyCryptoKeyVersion FAILED_PRECONDITION: a DESTROYED version
func TestDestroyFromEachState(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "ds2"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	dis, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: kmspb.CryptoKeyVersion_DISABLED}})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: dis.GetName()}); err != nil || v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
		t.Errorf("destroying a DISABLED version = %v, %v", v.GetState(), err)
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: stateMask(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: dis.GetName(), State: kmspb.CryptoKeyVersion_ENABLED}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("updating a DESTROY_SCHEDULED version = %v, want FAILED_PRECONDITION", err)
	}
	s := NewServer(db)
	var v keyVersion
	if found, err := s.get(dbKey(versionPrefix, dis.GetName()), &v); err != nil || !found {
		t.Fatal(found, err)
	}
	v.State = kmspb.CryptoKeyVersion_DESTROYED.String()
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: dis.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("destroying a DESTROYED version = %v, want FAILED_PRECONDITION", err)
	}
}
