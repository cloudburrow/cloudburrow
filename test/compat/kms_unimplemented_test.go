//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// kmsService prefixes every KeyManagementService method in the registry.
const kmsService = "google.cloud.kms.v1.KeyManagementService/"

// TestKMSUnimplementedContract calls every KeyManagementService method the
// coverage registry lists as unimplemented, through the official client
// against the running instance, and requires UNIMPLEMENTED (#396). The
// registry drives it: an unimplemented method with no request here, or a
// request here for a method that is no longer unimplemented, fails the test,
// so a feature issue that implements a method must update the table.
//
// covers: google.cloud.kms.v1.KeyManagementService/ExportTrustedKeyWrappedCryptoKeyVersion (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/ImportTrustedKeyWrappedCryptoKeyVersion (unimplemented)
func TestKMSUnimplementedContract(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := kmsClients(t, h)["grpc"]
	loc := "projects/" + h.Project() + "/locations/global"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "unimplemented"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	k, v := key.GetName(), key.GetName()+"/cryptoKeyVersions/1"
	data := []byte("cloudburrow")
	calls := map[string]func(context.Context) error{
		"ExportTrustedKeyWrappedCryptoKeyVersion": func(ctx context.Context) error {
			_, err := c.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: v})
			return err
		},
		"ImportTrustedKeyWrappedCryptoKeyVersion": func(ctx context.Context) error {
			_, err := c.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: k,
				Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: data})
			return err
		},
	}

	unimplemented := registryUnimplemented(t, kmsService)
	if len(unimplemented) == 0 {
		t.Fatal("the registry lists no unimplemented KMS methods; is it being read?")
	}
	for _, m := range unimplemented {
		call, ok := calls[m]
		if !ok {
			t.Errorf("%s%s is unimplemented in the registry but has no request here; add one", kmsService, m)
			continue
		}
		delete(calls, m)
		// The official client retries some codes; a short deadline keeps a
		// wiring fault from hanging the test.
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := call(cctx)
		cancel()
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("%s = %v, want UNIMPLEMENTED", m, err)
		}
	}
	for m := range calls {
		t.Errorf("%s%s has a request here but the registry no longer lists it as unimplemented; move its test", kmsService, m)
	}
}

// registryUnimplemented lists the methods under prefix that
// tools/coverage/registry.json marks unimplemented, without the prefix.
func registryUnimplemented(t *testing.T, prefix string) []string {
	t.Helper()
	b, err := os.ReadFile("../../tools/coverage/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]string
	if err := json.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	var out []string
	for name, st := range reg {
		if strings.HasPrefix(name, prefix) && st == "unimplemented" {
			out = append(out, strings.TrimPrefix(name, prefix))
		}
	}
	sort.Strings(out)
	return out
}

// TestKMSOutOfScopeKeyOptionsAreUnimplemented: each CreateCryptoKey option
// CloudBurrow does not implement is refused with UNIMPLEMENTED naming the
// field, through the official client (#396). Rotation schedules, once here,
// are implemented (#816; TestKMSRotationSchedule), and so are the asymmetric,
// MAC, raw and ML-KEM purposes (#1212), and HSM protection (#1452); the
// algorithms Go's standard library does not compute, and the external
// protection levels, are not.
func TestKMSOutOfScopeKeyOptionsAreUnimplemented(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := kmsClients(t, h)["grpc"]
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "options"})
	if err != nil {
		t.Fatal(err)
	}
	sym := kmspb.CryptoKey_ENCRYPT_DECRYPT
	level := func(pl kmspb.ProtectionLevel) *kmspb.CryptoKeyVersionTemplate {
		return &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: pl, Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION}
	}
	for name, c2 := range map[string]struct {
		key   *kmspb.CryptoKey
		field string
	}{
		"AES_WRAPPING": {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_AES_WRAPPING}, "purpose"},
		"secp256k1": {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256}}, "algorithm"},
		"ML-DSA": {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_65}}, "algorithm"},
		"X-Wing": {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_KEY_ENCAPSULATION,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_KEM_XWING}}, "algorithm"},
		"EXTERNAL":           {&kmspb.CryptoKey{Purpose: sym, VersionTemplate: level(kmspb.ProtectionLevel_EXTERNAL)}, "protection_level"},
		"EXTERNAL_VPC":       {&kmspb.CryptoKey{Purpose: sym, VersionTemplate: level(kmspb.ProtectionLevel_EXTERNAL_VPC)}, "protection_level"},
		"import_only":        {&kmspb.CryptoKey{Purpose: sym, ImportOnly: true}, "import_only"},
		"crypto_key_backend": {&kmspb.CryptoKey{Purpose: sym, CryptoKeyBackend: ring.GetName()}, "crypto_key_backend"},
	} {
		_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(),
			CryptoKeyId: strings.ToLower(strings.ReplaceAll(name, "_", "-")), CryptoKey: c2.key})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), c2.field) {
			t.Errorf("%s = %v, want UNIMPLEMENTED naming %s", name, err, c2.field)
		}
	}
}
