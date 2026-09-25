package oracle

import (
	"context"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// The resource RPCs both servers implement (#419): create, get and list of
// key rings, crypto keys and versions, and their error cases.
func TestResourceRPCsAgreeWithFakeKMS(t *testing.T) {
	ring := loc + "/keyRings/oracle"
	key := ring + "/cryptoKeys/k"
	sym := &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}
	type C = *kms.KeyManagementClient
	type X = context.Context
	compare(t, []step{
		{"CreateKeyRing", func(ctx X, c C) (any, error) {
			return c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "oracle"})
		}},
		{"CreateKeyRing duplicate", func(ctx X, c C) (any, error) {
			return c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "oracle"})
		}},
		{"CreateKeyRing bad ID", func(ctx X, c C) (any, error) {
			return c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "bad id!"})
		}},
		{"GetKeyRing", func(ctx X, c C) (any, error) { return c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}) }},
		{"GetKeyRing missing", func(ctx X, c C) (any, error) {
			return c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: loc + "/keyRings/absent"})
		}},
		{"GetKeyRing malformed", func(ctx X, c C) (any, error) {
			return c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: loc + "/keyRing/oracle"})
		}},
		{"ListKeyRings", func(ctx X, c C) (any, error) {
			return drain(c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc}).Next)
		}},
		{"CreateCryptoKey", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "k", CryptoKey: sym})
		}},
		{"CreateCryptoKey duplicate", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "k", CryptoKey: sym})
		}},
		{"CreateCryptoKey skip initial version", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "bare",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}, SkipInitialVersionCreation: true})
		}},
		{"CreateCryptoKey missing ring", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: loc + "/keyRings/absent", CryptoKeyId: "k", CryptoKey: sym})
		}},
		{"CreateCryptoKey no purpose", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "nopurpose", CryptoKey: &kmspb.CryptoKey{}})
		}},
		{"GetCryptoKey", func(ctx X, c C) (any, error) { return c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}) }},
		{"GetCryptoKey missing", func(ctx X, c C) (any, error) {
			return c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/absent"})
		}},
		{"ListCryptoKeys", func(ctx X, c C) (any, error) {
			return drain(c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring}).Next)
		}},
		{"CreateCryptoKeyVersion", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key})
		}},
		{"CreateCryptoKeyVersion missing key", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: ring + "/cryptoKeys/absent"})
		}},
		{"GetCryptoKeyVersion", func(ctx X, c C) (any, error) {
			return c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/2"})
		}},
		{"GetCryptoKeyVersion missing", func(ctx X, c C) (any, error) {
			return c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/9"})
		}},
		{"GetCryptoKeyVersion malformed", func(ctx X, c C) (any, error) {
			return c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/01"})
		}},
		{"ListCryptoKeyVersions", func(ctx X, c C) (any, error) {
			return drain(c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key}).Next)
		}},
		// Last, so fakekms's refusal cannot change what the steps above see.
		{"CreateCryptoKey with labels", func(ctx X, c C) (any, error) {
			return c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "labelled",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Labels: map[string]string{"env": "oracle"}}})
		}},
	})
}
