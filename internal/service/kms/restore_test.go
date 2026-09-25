package kms

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Restore works only before destroy_time; after it, the version stays
// DESTROYED. A restored version outlives its old destroy_time, DISABLED and
// with its material (#404).
//
// unverified: google.cloud.kms.v1.KeyManagementService/RestoreCryptoKeyVersion FAILED_PRECONDITION: a version whose destroy_time has passed
func TestRestoreBeforeAndAfterDestroyTime(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	db := store.NewMemory()
	srv := NewServerWithClock(db, clock)
	c := clientOf(t, srv)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "rs"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	late, kept := key.GetPrimary().GetName(), v2.GetName()
	for _, n := range []string{late, kept} {
		if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(12 * time.Hour)
	if v, err := c.RestoreCryptoKeyVersion(ctx, &kmspb.RestoreCryptoKeyVersionRequest{Name: kept}); err != nil ||
		v.GetState() != kmspb.CryptoKeyVersion_DISABLED || v.GetDestroyTime() != nil {
		t.Fatalf("restore before destroy_time = %v, %v; want DISABLED with no destroy_time", v, err)
	}
	clock.Advance(13 * time.Hour)
	if _, err := c.RestoreCryptoKeyVersion(ctx, &kmspb.RestoreCryptoKeyVersionRequest{Name: late}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("restore after destroy_time = %v, want FAILED_PRECONDITION", err)
	}
	if v, _ := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: late}); v.GetState() != kmspb.CryptoKeyVersion_DESTROYED {
		t.Errorf("the late version is %s, want DESTROYED", v.GetState())
	}
	if _, err := srv.Sweep(); err != nil {
		t.Fatal(err)
	}
	v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: kept})
	raw, _ := db.Get(dbKey(versionPrefix, kept))
	var rec keyVersion
	_ = json.Unmarshal(raw, &rec)
	if err != nil || v.GetState() != kmspb.CryptoKeyVersion_DISABLED || len(rec.Material) != 32 {
		t.Errorf("past its old destroy_time the restored version is %s with %d bytes of material (%v); want DISABLED with 32", v.GetState(), len(rec.Material), err)
	}
}
