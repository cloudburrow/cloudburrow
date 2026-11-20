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
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newLoggingKey(t *testing.T, k *kms.Server, loc, id string) string {
	t.Helper()
	ctx := context.Background()
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/proj-1/locations/" + loc, KeyRingId: "ring-" + id})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: id,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return key.GetName()
}

func setLoggingKeyState(t *testing.T, k *kms.Server, key string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := k.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
}

// TestBucketCMEK (#1950): a bucket with cmek_settings reports its key, key
// version and service account; the entries routed to it are kept sealed;
// reading it fails FAILED_PRECONDITION while the version is disabled, while
// writes keep landing, and works again once it is enabled.
func TestBucketCMEK(t *testing.T) {
	ctx := context.Background()
	k := kms.NewServer(store.NewMemory())
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	a.UseKeys(cmek.Keys{KMS: func() cmek.KMS { return cmek.InProcess(k) }})
	cs := a.ConfigServer()
	key := newLoggingKey(t, k, "us-central1", "logs")
	other := newLoggingKey(t, k, "europe-west1", "eu")
	const parent = "projects/proj-1/locations/us-central1"
	create := func(id, loc, key string) (*loggingpb.LogBucket, error) {
		return cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/" + loc, BucketId: id,
			Bucket: &loggingpb.LogBucket{CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}}})
	}
	if _, err := create("g", "global", key); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a global bucket with a key: %v", err)
	}
	if _, err := create("wrong", "us-central1", other); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "The KMS key location must match the storage location") {
		t.Errorf("a key in another location: %v", err)
	}
	if _, err := create("none", "us-central1", "projects/proj-1/locations/us-central1/keyRings/ring-logs/cryptoKeys/nope"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a missing key: %v", err)
	}
	b, err := create("enc", "us-central1", key)
	if err != nil {
		t.Fatal(err)
	}
	c := b.GetCmekSettings()
	if c.GetKmsKeyName() != key || c.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" || c.GetServiceAccountId() != serviceAccount(projectParent("proj-1")) {
		t.Fatalf("cmek_settings %v", c)
	}
	bucket := parent + "/buckets/enc"
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{
		Name: "to-enc", Destination: "logging.googleapis.com/" + bucket}}); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
			LogName: "projects/proj-1/logs/app", Resource: &monitoredres.MonitoredResource{Type: "global"}, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() ([]string, error) {
		resp, err := srv.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{bucket + "/views/_AllLogs"}})
		var out []string
		for _, e := range resp.GetEntries() {
			out = append(out, e.GetTextPayload())
		}
		return out, err
	}
	write("secret-one")
	if got, err := read(); err != nil || len(got) != 1 || got[0] != "secret-one" {
		t.Fatalf("read %v, %v", got, err)
	}
	srv.store.mu.Lock()
	for _, r := range srv.store.routed {
		s, ok := r.sealed[bucket]
		if !ok || s.KeyVersion != key+"/cryptoKeyVersions/1" || bytes.Contains(s.Ciphertext, []byte("secret-one")) {
			t.Errorf("the bucket's copy %+v", s)
		}
	}
	srv.store.mu.Unlock()

	setLoggingKeyState(t, k, key, kmspb.CryptoKeyVersion_DISABLED)
	write("secret-two")
	if _, err := read(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Cryptographic key is disabled") {
		t.Errorf("read with the version disabled: %v", err)
	}
	tail := srv.readableSources(ctx, []source{{bucket: bucketRef{projectParent("proj-1"), "us-central1", "enc"}}})
	if status.Code(apierror.Wrap(tail)) != codes.FailedPrecondition {
		t.Errorf("tail with the version disabled: %v", tail)
	}
	if got, err := srv.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/proj-1"}}); err != nil || len(got.GetEntries()) != 2 {
		t.Errorf("_Default with the key disabled: %v, %v", got, err)
	}
	setLoggingKeyState(t, k, key, kmspb.CryptoKeyVersion_ENABLED)
	if got, err := read(); err != nil || len(got) != 2 {
		t.Errorf("read once enabled again: %v, %v", got, err)
	}

	// Removing the key is refused; switching it records the new version.
	upd := func(key string) (*loggingpb.LogBucket, error) {
		return cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: bucket, Bucket: &loggingpb.LogBucket{
			CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cmek_settings"}}})
	}
	if _, err := upd(""); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("removing the key: %v", err)
	}
	second := newLoggingKey(t, k, "us-central1", "second")
	if b, err := upd(second); err != nil || b.GetCmekSettings().GetKmsKeyVersionName() != second+"/cryptoKeyVersions/1" {
		t.Errorf("switching the key: %v, %v", b, err)
	}
	if got, err := read(); err != nil || len(got) != 2 {
		t.Errorf("read after the switch: %v, %v", got, err)
	}
}
