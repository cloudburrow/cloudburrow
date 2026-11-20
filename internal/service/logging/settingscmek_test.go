package logging

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestSettingsCMEK (#1972): a parent's default key is set through
// UpdateSettings and reported by GetSettings and GetCmekSettings with its
// version; a key outside the storage location, an invalid name or a
// disabled key is refused; a new custom bucket gets the default key, a
// global one is refused; UpdateCmekSettings sets a key for organizations
// only; an empty name clears it.
func TestSettingsCMEK(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	a := NewAdmin(store.NewMemory())
	a.UseKeys(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	cs := a.ConfigServer()
	key := newLoggingKey(t, k, "us-central1", "default")
	const name = "projects/proj-1/settings"
	upd := func(st *loggingpb.Settings, paths ...string) (*loggingpb.Settings, error) {
		return cs.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: name, Settings: st, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	if _, err := upd(&loggingpb.Settings{KmsKeyName: key}, "kms_key_name"); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "must match the storage location") {
		t.Fatalf("a key outside the storage location: %v", err)
	}
	if _, err := upd(&loggingpb.Settings{KmsKeyName: "nope", StorageLocation: "us-central1"}, "kms_key_name", "storage_location"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an invalid key name: %v", err)
	}
	setLoggingKeyState(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := upd(&loggingpb.Settings{KmsKeyName: key, StorageLocation: "us-central1"}, "kms_key_name", "storage_location"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a disabled key: %v", err)
	}
	setLoggingKeyState(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	st, err := upd(&loggingpb.Settings{KmsKeyName: key, StorageLocation: "us-central1"}, "kms_key_name", "storage_location")
	if err != nil || st.GetKmsKeyName() != key || st.GetStorageLocation() != "us-central1" {
		t.Fatalf("UpdateSettings = %v, %v", st, err)
	}
	if got, err := cs.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: name}); err != nil || got.GetKmsKeyName() != key {
		t.Fatalf("GetSettings = %v, %v", got, err)
	}
	c, err := cs.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: "projects/proj-1/cmekSettings"})
	if err != nil || c.GetKmsKeyName() != key || c.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" || c.GetServiceAccountId() == "" {
		t.Fatalf("GetCmekSettings = %v, %v", c, err)
	}
	b, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/us-central1", BucketId: "inherits"})
	if err != nil || b.GetCmekSettings().GetKmsKeyName() != key || b.GetCmekSettings().GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Fatalf("a new bucket under a default key = %v, %v", b, err)
	}
	if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/global", BucketId: "g"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a global bucket under a default key: %v", err)
	}
	if _, err := cs.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: "projects/proj-1/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("UpdateCmekSettings of a project: %v", err)
	}
	if c, err := cs.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: "projects/proj-1/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}}); err != nil || c.GetKmsKeyName() != "" || c.GetKmsKeyVersionName() != "" {
		t.Fatalf("clearing the key = %v, %v", c, err)
	}
	if b, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/global", BucketId: "g"}); err != nil || b.GetCmekSettings() != nil {
		t.Fatalf("a global bucket once the key is cleared = %v, %v", b, err)
	}
}

// TestCMEKEntriesAtRest (#1972): an entry only a customer-managed bucket
// holds is kept in the store as a stub, its payload sealed; the bucket's
// read opens it, and fails while the key is disabled; an entry also in
// _Default is kept as written.
func TestCMEKEntriesAtRest(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	a.UseKeys(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	cs := a.ConfigServer()
	key := newLoggingKey(t, k, "us-central1", "rest")
	bucket := "projects/proj-1/locations/us-central1/buckets/enc"
	if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/us-central1", BucketId: "enc",
		Bucket: &loggingpb.LogBucket{CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{
		Name: "to-enc", Destination: "logging.googleapis.com/" + bucket}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: "projects/proj-1", Exclusion: &loggingpb.LogExclusion{
		Name: "secret-out", Filter: `logName="projects/proj-1/logs/secret"`}}); err != nil {
		t.Fatal(err)
	}
	write := func(log, text string) {
		t.Helper()
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
			LogName: "projects/proj-1/logs/" + log, Resource: &monitoredres.MonitoredResource{Type: "global"},
			Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	write("secret", "only-in-the-cmek-bucket")
	write("app", "also-in-default")
	srv.store.mu.Lock()
	var clear []string
	for _, e := range srv.store.entries {
		raw, _ := proto.Marshal(e)
		if bytes.Contains(raw, []byte("only-in-the-cmek-bucket")) {
			t.Errorf("the store holds the sealed-only entry in the clear: %v", e)
		}
		clear = append(clear, e.GetTextPayload())
	}
	srv.store.mu.Unlock()
	if len(clear) != 2 || clear[1] != "also-in-default" {
		t.Errorf("the store's payloads = %q", clear)
	}
	read := func() ([]string, error) {
		resp, err := srv.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{bucket + "/views/_AllLogs"}})
		var out []string
		for _, e := range resp.GetEntries() {
			out = append(out, e.GetTextPayload())
		}
		return out, err
	}
	if got, err := read(); err != nil || len(got) != 2 || got[0] != "only-in-the-cmek-bucket" {
		t.Fatalf("the bucket's read = %q, %v", got, err)
	}
	setLoggingKeyState(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := read(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("the bucket's read with the key disabled: %v", err)
	}
	// Written while the key cannot encrypt: kept until a read seals it.
	write("secret", "written-while-disabled")
	setLoggingKeyState(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	if got, err := read(); err != nil || len(got) != 3 {
		t.Fatalf("the bucket's read once enabled = %q, %v", got, err)
	}
	srv.store.mu.Lock()
	for _, e := range srv.store.entries {
		if e.GetTextPayload() == "written-while-disabled" {
			t.Error("the entry written while the key was disabled is still in the clear after a read sealed it")
		}
	}
	srv.store.mu.Unlock()
	if _, err := srv.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: "projects/proj-1/logs/secret"}); err != nil {
		t.Fatalf("DeleteLog of the sealed-only log: %v", err)
	}
	if got, err := read(); err != nil || len(got) != 1 || got[0] != "also-in-default" {
		t.Fatalf("after DeleteLog = %q, %v", got, err)
	}
}

// TestOrganizationCMEKInherited (#1972): UpdateCmekSettings sets an
// organization's key, and a child project's new bucket gets it.
func TestOrganizationCMEKInherited(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	a := NewAdmin(store.NewMemory())
	a.UseHierarchy(fakeHierarchy{})
	a.UseKeys(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	cs := a.ConfigServer()
	key := newLoggingKey(t, k, "europe-west1", "org")
	c, err := cs.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: "organizations/1/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}})
	if err != nil || c.GetKmsKeyName() != key || c.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Fatalf("UpdateCmekSettings(organization) = %v, %v", c, err)
	}
	if st, err := cs.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: "organizations/1/settings"}); err != nil || st.GetKmsKeyName() != key {
		t.Fatalf("GetSettings(organization) = %v, %v", st, err)
	}
	b, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/child-proj/locations/europe-west1", BucketId: "b"})
	if err != nil || b.GetCmekSettings().GetKmsKeyName() != key {
		t.Fatalf("a child project's new bucket = %v, %v", b, err)
	}
	if b, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/unrelated/locations/global", BucketId: "b"}); err != nil || b.GetCmekSettings() != nil {
		t.Fatalf("an unrelated project's bucket = %v, %v", b, err)
	}
	if _, err := cs.UpdateCmekSettings(ctx, &loggingpb.UpdateCmekSettingsRequest{Name: "organizations/1/cmekSettings",
		CmekSettings: &loggingpb.CmekSettings{KmsKeyName: "projects/p/keyRings/bad"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an invalid key: %v", err)
	}
}
