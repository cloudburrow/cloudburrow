package kms

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A view outside the defined set is refused (#406).
//
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions INVALID_ARGUMENT: a view that is not a CryptoKeyVersionView
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeys INVALID_ARGUMENT: a version_view that is not a CryptoKeyVersionView
func TestAnUndefinedViewIsRefused(t *testing.T) {
	ctx := context.Background()
	c := client(t)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "v"})
	key, _ := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if _, err := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName(), View: 99}).Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("view 99 = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring.GetName(), VersionView: 99}).Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("version_view 99 = %v, want INVALID_ARGUMENT", err)
	}
}
