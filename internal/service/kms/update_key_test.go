package kms

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// UpdateCryptoKey's mask handling (#405).
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey INVALID_ARGUMENT: an empty mask, an immutable, output-only or unknown path, or an invalid label
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey NOT_FOUND: a well-formed key that does not exist
func TestUpdateCryptoKeyMask(t *testing.T) {
	ctx := context.Background()
	c := client(t)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "uk"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	upd := func(paths []string, k *kmspb.CryptoKey) error {
		if k.Name == "" {
			k.Name = key.GetName()
		}
		_, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: k, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		return err
	}
	for what, c2 := range map[string]struct {
		paths []string
		key   *kmspb.CryptoKey
		want  codes.Code
	}{
		"empty mask":             {nil, &kmspb.CryptoKey{}, codes.InvalidArgument},
		"purpose":                {[]string{"purpose"}, &kmspb.CryptoKey{}, codes.InvalidArgument},
		"primary":                {[]string{"primary"}, &kmspb.CryptoKey{}, codes.InvalidArgument},
		"nonsense":               {[]string{"nonsense"}, &kmspb.CryptoKey{}, codes.InvalidArgument},
		"rotation_period":        {[]string{"rotation_period"}, &kmspb.CryptoKey{}, codes.Unimplemented},
		"an asymmetric template": {[]string{"version_template.algorithm"}, &kmspb.CryptoKey{VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}, codes.Unimplemented},
		"an invalid label key":   {[]string{"labels"}, &kmspb.CryptoKey{Labels: map[string]string{"Bad Key": "v"}}, codes.InvalidArgument},
		"a malformed name":       {[]string{"labels"}, &kmspb.CryptoKey{Name: ring.GetName() + "/cryptoKey/k"}, codes.InvalidArgument},
		"a missing key":          {[]string{"labels"}, &kmspb.CryptoKey{Name: ring.GetName() + "/cryptoKeys/absent"}, codes.NotFound},
		"the same template":      {[]string{"version_template"}, &kmspb.CryptoKey{VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}}, codes.OK},
	} {
		if err := upd(c2.paths, c2.key); status.Code(err) != c2.want {
			t.Errorf("%s = %v, want %s", what, err, c2.want)
		}
	}
}
