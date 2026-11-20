//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/kms/apiv1/kmspb"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// loggingCMEKKey makes a key in location and grants the project's Logging
// service account (GetCmekSettings' service_account_id)
// roles/cloudkms.cryptoKeyEncrypterDecrypter on it, as
// managed-encryption-storage has it done.
func loggingCMEKKey(t *testing.T, h *Harness, c *vlogging.ConfigClient, location, ring, id string) string {
	t.Helper()
	ctx := h.Context()
	k := kmsClients(t, h)["grpc"]
	r, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/" + location, KeyRingId: ring})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: id,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	settings, err := c.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: "projects/" + h.Project() + "/cmekSettings"})
	if err != nil || settings.GetServiceAccountId() == "" {
		t.Fatalf("GetCmekSettings = %v, %v", settings, err)
	}
	ih := k.ResourceIAM(key.GetName())
	p, err := ih.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Add("serviceAccount:"+settings.GetServiceAccountId(), iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	if err := ih.SetPolicy(ctx, p); err != nil {
		t.Fatalf("grant the Logging service account: %v", err)
	}
	return key.GetName()
}

// covers: google.logging.v2.ConfigServiceV2/CreateBucket
// covers: google.logging.v2.ConfigServiceV2/UpdateBucket
// covers: google.logging.v2.ConfigServiceV2/GetBucket
// covers: google.logging.v2.ConfigServiceV2/GetCmekSettings
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestLoggingBucketCMEK (#1950): with the official ConfigClient, a log
// bucket created with cmek_settings.kms_key_name, a key of the instance's
// Cloud KMS in the bucket's region, reports the key, its version and the
// Logging service account; a global bucket and a key in another location
// are INVALID_ARGUMENT; an entry a sink routes to the bucket reads back
// through the bucket's view; with the key version disabled, the read is
// FAILED_PRECONDITION while writes keep landing, and it works again once
// the version is enabled; removing the key is refused. Measured first:
// cmek_settings was UNIMPLEMENTED.
func TestLoggingBucketCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvKMS)
	ctx := h.Context()
	c := loggingConfigClient(t, h)
	lc := loggingAPIClient(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	key := loggingCMEKKey(t, h, c, "us-central1", "logging-cmek-"+suffix, "logs")
	eu := loggingCMEKKey(t, h, c, "europe-west1", "logging-cmek-eu-"+suffix, "logs")
	project := "projects/" + h.Project()
	create := func(loc, id, key string) (*loggingpb.LogBucket, error) {
		return c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: project + "/locations/" + loc, BucketId: id,
			Bucket: &loggingpb.LogBucket{CmekSettings: &loggingpb.CmekSettings{KmsKeyName: key}}})
	}
	if _, err := create("global", "cmek-g-"+suffix, key); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a global bucket with a key = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := create("us-central1", "cmek-eu-"+suffix, eu); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "The KMS key location must match the storage location") {
		t.Errorf("a key in another location = %v; want INVALID_ARGUMENT", err)
	}
	id := "cmek-" + suffix
	b, err := create("us-central1", id, key)
	if err != nil {
		t.Fatalf("CreateBucket with cmek_settings: %v", err)
	}
	bucket := b.GetName()
	t.Cleanup(func() { _ = c.DeleteBucket(h.Context(), &loggingpb.DeleteBucketRequest{Name: bucket}) })
	got, err := c.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: bucket})
	if err != nil || got.GetCmekSettings().GetKmsKeyName() != key || got.GetCmekSettings().GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" ||
		!strings.HasSuffix(got.GetCmekSettings().GetServiceAccountId(), "@gcp-sa-logging.iam.gserviceaccount.com") {
		t.Fatalf("GetBucket = %v, %v; want the key, version 1 and the Logging service account", got.GetCmekSettings(), err)
	}
	logID := "cmek-" + suffix
	sink, err := c.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: project, Sink: &loggingpb.LogSink{Name: "cmek-" + suffix,
		Destination: "logging.googleapis.com/" + bucket, Filter: fmt.Sprintf(`logName="%s/logs/%s"`, project, logID)}})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteSink(h.Context(), &loggingpb.DeleteSinkRequest{SinkName: project + "/sinks/" + sink.GetName()})
	})
	write := func(text string) {
		t.Helper()
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
			LogName: project + "/logs/" + logID, Resource: &monitoredres.MonitoredResource{Type: "global"},
			Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
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
	write("sealed one")
	if got, err := read(); err != nil || len(got) != 1 || got[0] != "sealed one" {
		t.Fatalf("read the bucket = %v, %v", got, err)
	}
	kc := kmsClients(t, h)["grpc"]
	setVersionState(t, h, kc, key+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_DISABLED)
	t.Cleanup(func() { setVersionState(t, h, kc, key+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_ENABLED) })
	write("written while disabled")
	if _, err := read(); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("read with the key version disabled = %v; want FAILED_PRECONDITION", err)
	}
	setVersionState(t, h, kc, key+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_ENABLED)
	if got, err := read(); err != nil || len(got) != 2 {
		t.Errorf("read once enabled again = %v, %v; want both entries", got, err)
	}
	_, err = c.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: bucket, Bucket: &loggingpb.LogBucket{CmekSettings: &loggingpb.CmekSettings{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cmek_settings"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("removing the key = %v; want FAILED_PRECONDITION", err)
	}
}
