package kms

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	okLoc     = "projects/demo-project/locations/global"
	okRing    = okLoc + "/keyRings/r"
	okKey     = okRing + "/cryptoKeys/k"
	okVersion = okKey + "/cryptoKeyVersions/1"
)

// badNames returns malformed names of one kind, each derived from a valid one.
func badNames(kind string) []string {
	long := strings.Repeat("a", 64)
	loc := []string{"", "projects/demo-project", "projects/demo-project/locations", "projects/demo-project/locations/1abc",
		"projects/demo-project/locations/us--east1", "projects/Bad_Project/locations/global", "projects/123456/locations/global",
		okLoc + "/extra"}
	ring := []string{"", okLoc + "/keyRing/r", okRing + "/extra", "projects/Bad_Project/locations/global/keyRings/r",
		"projects/demo-project/locations/1abc/keyRings/r", "projects/demo-project/locations/us--east1/keyRings/r",
		okLoc + "/keyRings/" + long, okLoc + "/keyRings/..", okLoc + "/keyRings/a%2fb"}
	key := []string{"", okRing + "/cryptoKey/k", okKey + "/extra", okRing + "/cryptoKeys/" + long,
		okRing + "/cryptoKeys/..", okRing + "/cryptoKeys/a%2fb", "projects/Bad_Project/locations/global/keyRings/r/cryptoKeys/k",
		"projects/demo-project/locations/1abc/keyRings/r/cryptoKeys/k"}
	version := []string{"", okKey + "/cryptoKeyVersion/1", okVersion + "/extra", okKey + "/cryptoKeyVersions/0",
		okKey + "/cryptoKeyVersions/-1", okKey + "/cryptoKeyVersions/01", okKey + "/cryptoKeyVersions/abc",
		okRing + "/cryptoKeys/" + long + "/cryptoKeyVersions/1", "projects/demo-project/locations/us--east1/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"}
	return map[string][]string{"location": loc, "ring": ring, "key": key, "version": version}[kind]
}

// TestMalformedNamesAreInvalidArgument (#394): every implemented RPC parses its
// name or parent before looking anything up, so a malformed one is
// INVALID_ARGUMENT naming the field, and a well-formed one that does not exist
// is NOT_FOUND. Google documents neither code for KMS.
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateKeyRing INVALID_ARGUMENT: a malformed parent
// unverified: google.cloud.kms.v1.KeyManagementService/GetKeyRing INVALID_ARGUMENT: a malformed name
// unverified: google.cloud.kms.v1.KeyManagementService/ListKeyRings INVALID_ARGUMENT: a malformed parent
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: a malformed parent
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKey INVALID_ARGUMENT: a malformed name
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeys INVALID_ARGUMENT: a malformed parent
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion INVALID_ARGUMENT: a malformed parent
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKeyVersion INVALID_ARGUMENT: a malformed name, including version IDs 0, -1, 01 and abc
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions INVALID_ARGUMENT: a malformed parent
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion INVALID_ARGUMENT: a malformed name or crypto_key_version_id
// unverified: google.cloud.kms.v1.KeyManagementService/GetKeyRing NOT_FOUND: a well-formed name that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKey NOT_FOUND: a well-formed name that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKeyVersion NOT_FOUND: a well-formed name that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeys NOT_FOUND: a well-formed parent that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions NOT_FOUND: a well-formed parent that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey NOT_FOUND: a well-formed parent that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion NOT_FOUND: a well-formed parent that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion NOT_FOUND: a well-formed name that does not exist
func TestMalformedNamesAreInvalidArgument(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	sym := &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}
	calls := []struct {
		rpc, kind, field string
		call             func(string) error
	}{
		{"CreateKeyRing", "location", "parent", func(n string) error {
			_, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: n, KeyRingId: "r"})
			return err
		}},
		{"GetKeyRing", "ring", "name", func(n string) error {
			_, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: n})
			return err
		}},
		{"ListKeyRings", "location", "parent", func(n string) error {
			_, err := c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: n}).Next()
			return err
		}},
		{"CreateCryptoKey", "ring", "parent", func(n string) error {
			_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: n, CryptoKeyId: "k", CryptoKey: sym})
			return err
		}},
		{"GetCryptoKey", "key", "name", func(n string) error {
			_, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: n})
			return err
		}},
		{"ListCryptoKeys", "ring", "parent", func(n string) error {
			_, err := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: n}).Next()
			return err
		}},
		{"CreateCryptoKeyVersion", "key", "parent", func(n string) error {
			_, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: n})
			return err
		}},
		{"GetCryptoKeyVersion", "version", "name", func(n string) error {
			_, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: n})
			return err
		}},
		{"ListCryptoKeyVersions", "key", "parent", func(n string) error {
			_, err := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: n}).Next()
			return err
		}},
		{"UpdateCryptoKeyPrimaryVersion", "key", "name", func(n string) error {
			_, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: n, CryptoKeyVersionId: "1"})
			return err
		}},
	}
	for _, rc := range calls {
		for _, bad := range badNames(rc.kind) {
			err := rc.call(bad)
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), rc.field) {
				t.Errorf("%s(%s=%q) = %v, want INVALID_ARGUMENT naming %s", rc.rpc, rc.field, bad, err, rc.field)
			}
		}
	}

	// Well formed but absent: NOT_FOUND. The location kind has no existence
	// to check, so CreateKeyRing and ListKeyRings are not in this half.
	missing := map[string]string{"ring": okLoc + "/keyRings/absent", "key": okRing + "/cryptoKeys/absent",
		"version": okKey + "/cryptoKeyVersions/7"}
	for _, rc := range calls {
		n, ok := missing[rc.kind]
		if !ok {
			continue
		}
		if err := rc.call(n); status.Code(err) != codes.NotFound {
			t.Errorf("%s(%s=%q) = %v, want NOT_FOUND", rc.rpc, rc.field, n, err)
		}
	}

	// UpdateCryptoKeyPrimaryVersion checks its version ID too.
	for _, id := range []string{"0", "-1", "01", "abc", ""} {
		_, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: okKey, CryptoKeyVersionId: id})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateCryptoKeyPrimaryVersion(crypto_key_version_id=%q) = %v, want INVALID_ARGUMENT", id, err)
		}
	}
}

// A bad crypto_key_id under a ring that does not exist is INVALID_ARGUMENT:
// the request is validated before the parent is looked up.
func TestCreateCryptoKeyValidatesBeforeLookingUpTheRing(t *testing.T) {
	c := client(t)
	_, err := c.CreateCryptoKey(context.Background(), &kmspb.CreateCryptoKeyRequest{Parent: okLoc + "/keyRings/absent",
		CryptoKeyId: "not/valid", CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateCryptoKey with a bad ID under a missing ring = %v, want INVALID_ARGUMENT", err)
	}
}
