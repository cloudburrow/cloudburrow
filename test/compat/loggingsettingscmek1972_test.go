//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.logging.v2.ConfigServiceV2/GetSettings
// covers: google.logging.v2.ConfigServiceV2/UpdateSettings
// covers: google.logging.v2.ConfigServiceV2/GetCmekSettings
// covers: google.logging.v2.ConfigServiceV2/UpdateCmekSettings
// covers: google.logging.v2.ConfigServiceV2/CreateBucket
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestLoggingSettingsCMEK (#1972), with the official ConfigClient and
// Client, as docs.cloud.google.com/logging/docs/routing/managed-encryption
// describes the default key: UpdateSettings with a kms_key_name outside the
// settings' storage_location is INVALID_ARGUMENT; with the key's location
// as storage_location it is set, and GetSettings and GetCmekSettings report
// it (GetCmekSettings with its version and service account); a new custom
// bucket without cmek_settings gets the key, and one in global is refused;
// an entry an exclusion keeps out of _Default and a sink routes to that
// bucket reads back, and its read is FAILED_PRECONDITION while the key
// version is disabled; UpdateCmekSettings of a project with a key is
// FAILED_PRECONDITION (organizations only), and an empty key clears it.
// The settings are a throwaway project's, so no other test's buckets are
// affected.
func TestLoggingSettingsCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvKMS)
	ctx := h.Context()
	c := loggingConfigClient(t, h)
	lc := loggingAPIClient(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	key := loggingCMEKKey(t, h, c, "us-central1", "logging-settings-"+suffix, "default")
	project := "projects/lcmek-" + suffix
	// The throwaway project's Logging service account may use the key.
	cs, err := c.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: project + "/cmekSettings"})
	if err != nil || cs.GetKmsKeyName() != "" || cs.GetServiceAccountId() == "" {
		t.Fatalf("GetCmekSettings before = %v, %v", cs, err)
	}
	k := kmsClients(t, h)["grpc"]
	ih := k.ResourceIAM(key)
	p, err := ih.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Add("serviceAccount:"+cs.GetServiceAccountId(), iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	if err := ih.SetPolicy(ctx, p); err != nil {
		t.Fatalf("grant the Logging service account: %v", err)
	}

	update := func(st *loggingpb.Settings, paths ...string) (*loggingpb.Settings, error) {
		return c.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: project + "/settings", Settings: st,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	if _, err := update(&loggingpb.Settings{KmsKeyName: key}, "kms_key_name"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSettings with a key outside the storage location = %v; want INVALID_ARGUMENT", err)
	}
	st, err := update(&loggingpb.Settings{KmsKeyName: key, StorageLocation: "us-central1"}, "kms_key_name", "storage_location")
	if err != nil || st.GetKmsKeyName() != key || st.GetStorageLocation() != "us-central1" || st.GetKmsServiceAccountId() != cs.GetServiceAccountId() {
		t.Fatalf("UpdateSettings with the key = %v, %v", st, err)
	}
	t.Cleanup(func() {
		_, _ = c.UpdateSettings(h.Context(), &loggingpb.UpdateSettingsRequest{Name: project + "/settings", Settings: &loggingpb.Settings{},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name", "storage_location"}}})
	})
	if got, err := c.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: project + "/settings"}); err != nil || got.GetKmsKeyName() != key {
		t.Errorf("GetSettings = %v, %v; want the key", got, err)
	}
	if got, err := c.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: project + "/cmekSettings"}); err != nil ||
		got.GetKmsKeyName() != key || got.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Errorf("GetCmekSettings = %v, %v; want the key and its version", got, err)
	}

	if _, err := c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: project + "/locations/global", BucketId: "g-" + suffix}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a global bucket under a default key = %v; want INVALID_ARGUMENT", err)
	}
	b, err := c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: project + "/locations/us-central1", BucketId: "inherits-" + suffix})
	if err != nil || b.GetCmekSettings().GetKmsKeyName() != key || b.GetCmekSettings().GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Fatalf("a new bucket under a default key = %v, %v; want the default key", b, err)
	}
	bucket := b.GetName()
	t.Cleanup(func() { _ = c.DeleteBucket(h.Context(), &loggingpb.DeleteBucketRequest{Name: bucket}) })
	logID := "secret-" + suffix
	filter := fmt.Sprintf(`logName="%s/logs/%s"`, project, logID)
	if _, err := c.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: project, Sink: &loggingpb.LogSink{Name: "to-cmek",
		Destination: "logging.googleapis.com/" + bucket, Filter: filter}}); err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	if _, err := c.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: project, Exclusion: &loggingpb.LogExclusion{
		Name: "not-default", Filter: filter}}); err != nil {
		t.Fatalf("CreateExclusion: %v", err)
	}
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
		LogName: project + "/logs/" + logID, Resource: &monitoredres.MonitoredResource{Type: "global"},
		Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "only in the keyed bucket"}}}}); err != nil {
		t.Fatalf("WriteLogEntries: %v", err)
	}
	read := func() ([]string, error) {
		var out []string
		it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{bucket + "/views/_AllLogs"}})
		for {
			e, err := it.Next()
			if err == iterator.Done {
				return out, nil
			}
			if err != nil {
				return out, err
			}
			out = append(out, e.GetTextPayload())
		}
	}
	if got, err := read(); err != nil || len(got) != 1 || got[0] != "only in the keyed bucket" {
		t.Fatalf("read the bucket = %q, %v", got, err)
	}
	setVersionState(t, h, k, key+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_DISABLED)
	t.Cleanup(func() { setVersionState(t, h, k, key+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_ENABLED) })
	if _, err := read(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("read with the key version disabled = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := update(&loggingpb.Settings{KmsKeyName: key, StorageLocation: "us-central1"}, "kms_key_name", "storage_location"); err != nil {
		t.Errorf("UpdateSettings keeping the key while it is disabled = %v; the key is not changed", err)
	}
	setVersionState(t, h, k, key+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_ENABLED)
	if got, err := read(); err != nil || len(got) != 1 {
		t.Errorf("read once enabled again = %q, %v", got, err)
	}

	if _, err := c.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: project + "/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UpdateCmekSettings of a project with a key = %v; want FAILED_PRECONDITION", err)
	}
	cleared, err := c.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: project + "/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}})
	if err != nil || cleared.GetKmsKeyName() != "" {
		t.Errorf("UpdateCmekSettings clearing the key = %v, %v", cleared, err)
	}
	if got, err := c.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: project + "/settings"}); err != nil || got.GetKmsKeyName() != "" {
		t.Errorf("GetSettings after clearing = %v, %v", got, err)
	}
}
