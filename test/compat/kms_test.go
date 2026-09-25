//go:build compat

package compat

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvKMS is the Cloud KMS endpoint.
const EnvKMS = "CLOUDBURROW_TEST_KMS"

// covers: google.cloud.kms.v1.KeyManagementService/CreateKeyRing, google.cloud.kms.v1.KeyManagementService/GetKeyRing, google.cloud.kms.v1.KeyManagementService/ListKeyRings, google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/GetCryptoKey, google.cloud.kms.v1.KeyManagementService/ListCryptoKeys, google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion
//
// TestKMSResources (#309), with cloud.google.com/go/kms/apiv1 against the CI
// instance: key rings, keys and versions created, read and listed, the
// primary version moved; asymmetric keys and import jobs UNIMPLEMENTED; the
// key material kept in owned Kubernetes Secrets; /admin/reset clearing it.
//
// unverified: google.cloud.kms.v1.KeyManagementService/GetKeyRing NOT_FOUND: a key ring removed by /admin/reset
func TestKMSResources(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	loc := "projects/" + h.Project() + "/locations/global"

	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "compat"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	if g, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); err != nil || g.GetName() != ring.GetName() {
		t.Errorf("GetKeyRing = %v, %v", g, err)
	}
	if r, err := c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc}).Next(); err != nil || r.GetName() != ring.GetName() {
		t.Errorf("ListKeyRings = %v, %v", r, err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "data",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	if k, err := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring.GetName()}).Next(); err != nil || k.GetName() != key.GetName() {
		t.Errorf("ListCryptoKeys = %v, %v", k, err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatalf("CreateCryptoKeyVersion: %v", err)
	}
	it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()})
	n := 0
	for {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Errorf("ListCryptoKeyVersions returned %d versions, want 2", n)
	}
	if u, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"}); err != nil ||
		u.GetPrimary().GetName() != v2.GetName() {
		t.Errorf("UpdateCryptoKeyPrimaryVersion = %v, %v", u, err)
	}
	if g, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); err != nil || g.GetPrimary().GetName() != v2.GetName() {
		t.Errorf("GetCryptoKey after the primary moved = %v, %v", g, err)
	}

	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sign",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an asymmetric key = %v, want Unimplemented", err)
	}
	if _, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "imp",
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateImportJob = %v, want Unimplemented", err)
	}

	// The material is in owned Secrets in the cluster.
	kc := filepath.Join(instanceDirFrom(t, strings.Fields(os.Getenv(EnvCLIArgs))), "kubeconfig")
	count := func() int {
		out, _ := exec.Command("kubectl", "--kubeconfig", kc, "-n", "cloudburrow", "get", "secrets",
			"-l", "cloudburrow.dev/service=kms,cloudburrow.dev/owned=true", "-o", "name").Output()
		return len(strings.Fields(string(out)))
	}
	if n := count(); n < 4 {
		t.Errorf("%d KMS Secrets in the cluster, want at least ring, key and two versions", n)
	}

	if code, body := adminReset(t, h.Endpoint(EnvControl), "service=kms"); code != http.StatusOK {
		t.Fatalf("reset = %d: %s", code, body)
	}
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetKeyRing after reset = %v, want NotFound", err)
	}
	if n := count(); n != 0 {
		t.Errorf("%d KMS Secrets remain after reset", n)
	}
}

// TestKMSNamesAreValidatedBeforeLookup (#394): a malformed CryptoKey name is
// INVALID_ARGUMENT, and a well-formed one that does not exist is NOT_FOUND.
// covers: google.cloud.kms.v1.KeyManagementService/GetCryptoKey
//
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKey INVALID_ARGUMENT: a malformed name (wrong collection)
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKey NOT_FOUND: a well-formed name that does not exist
func TestKMSNamesAreValidatedBeforeLookup(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	loc := "projects/" + h.Project() + "/locations/global"
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: loc + "/keyRing/r/cryptoKeys/k"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetCryptoKey with a malformed name = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: loc + "/keyRings/absent/cryptoKeys/absent"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCryptoKey of a missing key = %v, want NOT_FOUND", err)
	}
}

// TestKMSVersionCanStartDisabled (#398): a version created DISABLED reads back
// DISABLED, with its create_time and no destroy_time, and does not become the
// key's primary.
// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/GetCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions, google.cloud.kms.v1.KeyManagementService/GetCryptoKey
func TestKMSVersionCanStartDisabled(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	loc := "projects/" + h.Project() + "/locations/global"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "state-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	v, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: kmspb.CryptoKeyVersion_DISABLED}})
	if err != nil {
		t.Fatalf("CreateCryptoKeyVersion(DISABLED): %v", err)
	}
	got, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v.GetName()})
	if err != nil || got.GetState() != kmspb.CryptoKeyVersion_DISABLED || got.GetCreateTime() == nil || got.GetDestroyTime() != nil {
		t.Errorf("GetCryptoKeyVersion = %v, %v; want DISABLED with create_time and no destroy_time", got, err)
	}
	it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()})
	states := map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionState{}
	for {
		lv, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListCryptoKeyVersions: %v", err)
		}
		states[lv.GetName()] = lv.GetState()
	}
	if states[v.GetName()] != kmspb.CryptoKeyVersion_DISABLED || states[key.GetPrimary().GetName()] != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("listed states %v; want the new version DISABLED and version 1 ENABLED", states)
	}
	k2, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	if err != nil || k2.GetPrimary().GetName() != key.GetPrimary().GetName() {
		t.Errorf("the primary moved to %v (%v); a new version never becomes primary", k2.GetPrimary().GetName(), err)
	}
}

// TestKMSUpdateCryptoKeyVersion (#400): a version moves ENABLED to DISABLED
// and back, read after each step; a mask without state and a target outside
// ENABLED/DISABLED are refused.
// covers: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion INVALID_ARGUMENT: an update_mask without state
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion INVALID_ARGUMENT: a target state of DESTROYED
func TestKMSUpdateCryptoKeyVersion(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "update-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	name := key.GetPrimary().GetName()
	mask := &fieldmaskpb.FieldMask{Paths: []string{"state"}}
	for _, want := range []kmspb.CryptoKeyVersion_CryptoKeyVersionState{kmspb.CryptoKeyVersion_DISABLED, kmspb.CryptoKeyVersion_ENABLED} {
		upd, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: mask,
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: want}})
		if err != nil || upd.GetState() != want {
			t.Fatalf("UpdateCryptoKeyVersion to %s = %v, %v", want, upd.GetState(), err)
		}
		got, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name})
		if err != nil || got.GetState() != want {
			t.Errorf("after moving to %s, GetCryptoKeyVersion = %v, %v", want, got.GetState(), err)
		}
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: kmspb.CryptoKeyVersion_DISABLED}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a mask without state = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: mask,
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: kmspb.CryptoKeyVersion_DESTROYED}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a target of DESTROYED = %v, want INVALID_ARGUMENT", err)
	}
}

// TestKMSPrimaryMustBeEnabled (#401): a DISABLED version cannot become the
// primary, and once re-enabled it can.
// covers: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyPrimaryVersion FAILED_PRECONDITION: a DISABLED target version
func TestKMSPrimaryMustBeEnabled(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "primary-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: kmspb.CryptoKeyVersion_DISABLED}})
	if err != nil {
		t.Fatalf("CreateCryptoKeyVersion: %v", err)
	}
	if _, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("making a DISABLED version primary = %v, want FAILED_PRECONDITION", err)
	}
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); err != nil || k.GetPrimary().GetName() != key.GetPrimary().GetName() {
		t.Errorf("the refused call moved the primary to %v (%v)", k.GetPrimary().GetName(), err)
	}
	if _, err := c.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}},
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v2.GetName(), State: kmspb.CryptoKeyVersion_ENABLED}}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	k, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"})
	if err != nil || k.GetPrimary().GetName() != v2.GetName() || k.GetPrimary().GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("after re-enabling, UpdateCryptoKeyPrimaryVersion = %v, %v; want version 2 ENABLED", k.GetPrimary(), err)
	}
}

// TestKMSCreateCryptoKeyFields (#399): destroy_scheduled_duration defaults to
// 30 days and is echoed, 24h reads back as 24h, and purpose is required.
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: purpose CRYPTO_KEY_PURPOSE_UNSPECIFIED
func TestKMSCreateCryptoKeyFields(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "fields-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	const month = 30 * 24 * time.Hour
	def, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "default",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil || def.GetDestroyScheduledDuration().AsDuration() != month {
		t.Fatalf("CreateCryptoKey without the field = %v, %v; want 30d", def.GetDestroyScheduledDuration(), err)
	}
	if got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: def.GetName()}); err != nil || got.GetDestroyScheduledDuration().AsDuration() != month {
		t.Errorf("GetCryptoKey = %v, %v; want 30d", got.GetDestroyScheduledDuration(), err)
	}
	listed, err := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring.GetName()}).Next()
	if err != nil || listed.GetDestroyScheduledDuration().AsDuration() != month {
		t.Errorf("ListCryptoKeys = %v, %v; want 30d", listed.GetDestroyScheduledDuration(), err)
	}
	day, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "day",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil || day.GetDestroyScheduledDuration().AsDuration() != 24*time.Hour {
		t.Errorf("CreateCryptoKey with 24h = %v, %v", day.GetDestroyScheduledDuration(), err)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "nopurpose",
		CryptoKey: &kmspb.CryptoKey{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("purpose UNSPECIFIED = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "asym",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("purpose ASYMMETRIC_SIGN = %v, want UNIMPLEMENTED", err)
	}
}

// TestKMSDestroyCryptoKeyVersion (#402): a version of a key with a 24h
// destroy_scheduled_duration is DESTROY_SCHEDULED with destroy_time 24h out;
// destroying it again is refused.
// covers: google.cloud.kms.v1.KeyManagementService/DestroyCryptoKeyVersion
//
// unverified: google.cloud.kms.v1.KeyManagementService/DestroyCryptoKeyVersion FAILED_PRECONDITION: a version already DESTROY_SCHEDULED
func TestKMSDestroyCryptoKeyVersion(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "destroy-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	called := time.Now()
	v, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: key.GetPrimary().GetName()})
	if err != nil {
		t.Fatalf("DestroyCryptoKeyVersion: %v", err)
	}
	want := called.Add(24 * time.Hour)
	if v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED || v.GetDestroyTime().AsTime().Sub(want).Abs() > 5*time.Second {
		t.Errorf("destroyed: %s at %v, want DESTROY_SCHEDULED within 5s of %v", v.GetState(), v.GetDestroyTime().AsTime(), want)
	}
	if _, err := c.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: v.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("destroying it again = %v, want FAILED_PRECONDITION", err)
	}
}
