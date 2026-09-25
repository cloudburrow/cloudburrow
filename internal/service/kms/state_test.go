package kms

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// A version can only start ENABLED or DISABLED (#398).
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion INVALID_ARGUMENT: an initial state other than ENABLED or DISABLED
func TestCreateVersionRefusesOtherInitialStates(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "s"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []kmspb.CryptoKeyVersion_CryptoKeyVersionState{kmspb.CryptoKeyVersion_DESTROYED,
		kmspb.CryptoKeyVersion_DESTROY_SCHEDULED, kmspb.CryptoKeyVersion_PENDING_GENERATION} {
		_, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: st}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateCryptoKeyVersion(state=%s) = %v, want INVALID_ARGUMENT", st, err)
		}
	}
	v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: kmspb.CryptoKeyVersion_DISABLED}})
	if err != nil || v.GetState() != kmspb.CryptoKeyVersion_DISABLED || v.GetDestroyTime() != nil || v.GetDestroyEventTime() != nil {
		t.Errorf("a DISABLED create = %v, %v", v, err)
	}
}

// A version's state is stored: a second Server over the same durable store
// reads what the first wrote.
func TestVersionStateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := store.OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := clientFor(t, db)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "d"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: kmspb.CryptoKeyVersion_DISABLED}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := store.OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	got, err := clientFor(t, db2).GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v.GetName()})
	if err != nil || got.GetState() != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("after a restart the version is %v, %v; want DISABLED", got.GetState(), err)
	}
}

// A record written before states were stored reads as ENABLED.
func TestAVersionRecordWithoutStateIsEnabled(t *testing.T) {
	db := store.NewMemory()
	s := NewServer(db)
	name := okKey + "/cryptoKeyVersions/1"
	if err := s.put(dbKey(versionPrefix, name), map[string]any{"name": name, "created": time.Now().UTC(), "material": make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCryptoKeyVersion(context.Background(), &kmspb.GetCryptoKeyVersionRequest{Name: name})
	if err != nil || got.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("a record without a state = %v, %v; want ENABLED", got.GetState(), err)
	}
}
