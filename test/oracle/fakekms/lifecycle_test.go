package oracle

import (
	"context"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The state machine both servers implement (#420): UpdateCryptoKeyVersion
// and DestroyCryptoKeyVersion. fakekms does not implement Encrypt, Decrypt,
// UpdateCryptoKeyPrimaryVersion or RestoreCryptoKeyVersion
// (fakekms/interceptor.go:42-51), so those are not compared.
func TestVersionLifecycleAgreesWithFakeKMS(t *testing.T) {
	ring := loc + "/keyRings/lifecycle"
	key := ring + "/cryptoKeys/k"
	v := func(n string) string { return key + "/cryptoKeyVersions/" + n }
	type C = *kms.KeyManagementClient
	type X = context.Context
	state := func(name string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState, paths ...string) func(X, C) (any, error) {
		return func(ctx X, c C) (any, error) {
			return c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
				CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: st},
				UpdateMask:       &fieldmaskpb.FieldMask{Paths: paths}})
		}
	}
	destroy := func(name string) func(X, C) (any, error) {
		return func(ctx X, c C) (any, error) {
			return c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: name})
		}
	}
	compare(t, []step{
		{"CreateKeyRing", func(ctx X, c C) (any, error) {
			return c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "lifecycle"})
		}},
		{"CreateCryptoKey", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "k",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		}},
		{"CreateCryptoKeyVersion", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key})
		}},
		{"disable v2", state(v("2"), kmspb.CryptoKeyVersion_DISABLED, "state")},
		{"enable v2", state(v("2"), kmspb.CryptoKeyVersion_ENABLED, "state")},
		{"update without state in the mask", state(v("2"), kmspb.CryptoKeyVersion_DISABLED, "labels")},
		{"update with no mask", state(v("2"), kmspb.CryptoKeyVersion_DISABLED)},
		{"update to DESTROYED", state(v("2"), kmspb.CryptoKeyVersion_DESTROYED, "state")},
		{"update to DESTROY_SCHEDULED", state(v("2"), kmspb.CryptoKeyVersion_DESTROY_SCHEDULED, "state")},
		{"destroy v2 from ENABLED", destroy(v("2"))},
		{"destroy v2 again", destroy(v("2"))},
		{"update a DESTROY_SCHEDULED version", state(v("2"), kmspb.CryptoKeyVersion_ENABLED, "state")},
		{"get v2", func(ctx X, c C) (any, error) {
			return c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v("2")})
		}},
		{"destroy a missing version", destroy(v("9"))},
		{"disable v1 and destroy it", func(ctx X, c C) (any, error) {
			if _, err := state(v("1"), kmspb.CryptoKeyVersion_DISABLED, "state")(ctx, c); err != nil {
				return nil, err
			}
			return destroy(v("1"))(ctx, c)
		}},
		// Last: a key with a 24h schedule. fakekms hard-codes 30 days.
		{"CreateCryptoKey with a 24h schedule", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "day",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
		}},
		{"destroy on the 24h key", destroy(ring + "/cryptoKeys/day/cryptoKeyVersions/1")},
	})
}
