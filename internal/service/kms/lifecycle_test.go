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

// Once its version is DESTROYED a ciphertext never decrypts again, and the
// version cannot be restored, even after the Server is rebuilt over the same
// store (#413).
//
// unverified: google.cloud.kms.v1.KeyManagementService/Decrypt FAILED_PRECONDITION: a DESTROYED version
func TestDecryptIsRefusedOnceDestroyed(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	db := store.NewMemory()
	c := clientOf(t, NewServerWithClock(db, clock))
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "gone"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	v1 := key.GetPrimary().GetName()
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: v1}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(24*time.Hour + time.Second)
	check := func(what string, srv *Server) {
		t.Helper()
		cc := clientOf(t, srv)
		if _, err := cc.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: Decrypt with v1 DESTROYED = %v, want FAILED_PRECONDITION", what, err)
		}
		if _, err := cc.RestoreCryptoKeyVersion(ctx, &kmspb.RestoreCryptoKeyVersionRequest{Name: v1}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: Restore of a DESTROYED version = %v, want FAILED_PRECONDITION", what, err)
		}
	}
	check("before the sweep", NewServerWithClock(db, clock))
	rebuilt := NewServerWithClock(db, clock)
	if _, err := rebuilt.Sweep(); err != nil {
		t.Fatal(err)
	}
	check("after a rebuild and sweep", rebuilt)
}
