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
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
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
// covers: google.cloud.kms.v1.KeyManagementService/AsymmetricDecrypt (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/AsymmetricSign (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/CreateImportJob (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/Decapsulate (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/DeleteCryptoKey (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/DeleteCryptoKeyVersion (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/ExportTrustedKeyWrappedCryptoKeyVersion (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/GenerateRandomBytes (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/GetImportJob (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/GetPublicKey (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/GetRetiredResource (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/ImportCryptoKeyVersion (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/ImportTrustedKeyWrappedCryptoKeyVersion (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/ListImportJobs (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/ListRetiredResources (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/MacSign (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/MacVerify (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/RawDecrypt (unimplemented)
// covers: google.cloud.kms.v1.KeyManagementService/RawEncrypt (unimplemented)
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
	job := ring.GetName() + "/importJobs/j"
	data := []byte("cloudburrow")
	calls := map[string]func(context.Context) error{
		"AsymmetricDecrypt": func(ctx context.Context) error {
			_, err := c.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{Name: v, Ciphertext: data})
			return err
		},
		"AsymmetricSign": func(ctx context.Context) error {
			_, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: data})
			return err
		},
		"CreateImportJob": func(ctx context.Context) error {
			_, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j",
				ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
			return err
		},
		"Decapsulate": func(ctx context.Context) error {
			_, err := c.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: v, Ciphertext: data})
			return err
		},
		"DeleteCryptoKey": func(ctx context.Context) error {
			_, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: k})
			return err
		},
		"DeleteCryptoKeyVersion": func(ctx context.Context) error {
			_, err := c.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: v})
			return err
		},
		"ExportTrustedKeyWrappedCryptoKeyVersion": func(ctx context.Context) error {
			_, err := c.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: v})
			return err
		},
		"GenerateRandomBytes": func(ctx context.Context) error {
			_, err := c.GenerateRandomBytes(ctx, &kmspb.GenerateRandomBytesRequest{Location: loc, LengthBytes: 8, ProtectionLevel: kmspb.ProtectionLevel_HSM})
			return err
		},
		"GetImportJob": func(ctx context.Context) error {
			_, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job})
			return err
		},
		"GetPublicKey": func(ctx context.Context) error {
			_, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v})
			return err
		},
		"GetRetiredResource": func(ctx context.Context) error {
			_, err := c.GetRetiredResource(ctx, &kmspb.GetRetiredResourceRequest{Name: loc + "/retiredResources/r"})
			return err
		},
		"ImportCryptoKeyVersion": func(ctx context.Context) error {
			_, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: k, ImportJob: job,
				Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION})
			return err
		},
		"ImportTrustedKeyWrappedCryptoKeyVersion": func(ctx context.Context) error {
			_, err := c.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: k,
				Algorithm: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, WrappedKey: data})
			return err
		},
		"ListImportJobs": func(ctx context.Context) error {
			_, err := c.ListImportJobs(ctx, &kmspb.ListImportJobsRequest{Parent: ring.GetName()}).Next()
			return err
		},
		"ListRetiredResources": func(ctx context.Context) error {
			_, err := c.ListRetiredResources(ctx, &kmspb.ListRetiredResourcesRequest{Parent: loc}).Next()
			return err
		},
		"MacSign": func(ctx context.Context) error {
			_, err := c.MacSign(ctx, &kmspb.MacSignRequest{Name: v, Data: data})
			return err
		},
		"MacVerify": func(ctx context.Context) error {
			_, err := c.MacVerify(ctx, &kmspb.MacVerifyRequest{Name: v, Data: data, Mac: data})
			return err
		},
		"RawDecrypt": func(ctx context.Context) error {
			_, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: v, Ciphertext: data, InitializationVector: make([]byte, 12)})
			return err
		},
		"RawEncrypt": func(ctx context.Context) error {
			_, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: v, Plaintext: data})
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
// field, through the official client (#396).
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
		"ASYMMETRIC_SIGN":     {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN}, "purpose"},
		"ASYMMETRIC_DECRYPT":  {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT}, "purpose"},
		"MAC":                 {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_MAC}, "purpose"},
		"RAW_ENCRYPT_DECRYPT": {&kmspb.CryptoKey{Purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT}, "purpose"},
		"HSM":                 {&kmspb.CryptoKey{Purpose: sym, VersionTemplate: level(kmspb.ProtectionLevel_HSM)}, "protection_level"},
		"EXTERNAL":            {&kmspb.CryptoKey{Purpose: sym, VersionTemplate: level(kmspb.ProtectionLevel_EXTERNAL)}, "protection_level"},
		"EXTERNAL_VPC":        {&kmspb.CryptoKey{Purpose: sym, VersionTemplate: level(kmspb.ProtectionLevel_EXTERNAL_VPC)}, "protection_level"},
		"rotation_period": {&kmspb.CryptoKey{Purpose: sym,
			RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(30 * 24 * time.Hour)}}, "rotation_period"},
		"next_rotation_time": {&kmspb.CryptoKey{Purpose: sym, NextRotationTime: timestamppb.New(time.Now().Add(24 * time.Hour))}, "next_rotation_time"},
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
