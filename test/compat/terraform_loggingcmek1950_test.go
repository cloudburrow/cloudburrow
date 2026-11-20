//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
)

// TestTerraformLoggingBucketCMEK (#1950): google_logging_project_bucket_config
// with cmek_settings { kms_key_name }, a key of the instance's Cloud KMS
// the Logging service account was granted, applies with the official
// hashicorp/google provider through `cloudburrow terraform` (egress
// blocked), is read back with the official ConfigClient with the key, its
// version and the service account, plans clean (the provider reads
// cmek_settings' computed fields back), and is destroyed. Before #1950 log
// bucket CMEK was refused (docs/gaps.md).
// covers: google.logging.v2.ConfigServiceV2/CreateBucket, google.logging.v2.ConfigServiceV2/GetBucket, google.logging.v2.ConfigServiceV2/DeleteBucket
func TestTerraformLoggingBucketCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvKMS)
	c := loggingConfigClient(t, h)
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e9)
	key := loggingCMEKKey(t, h, c, "us-central1", "tf-logging-cmek-"+sfx, "logs")
	m := newTFModule(t)
	id := "tf-cmek-" + sfx
	m.write(fmt.Sprintf(`
resource "google_logging_project_bucket_config" "cmek" {
  project        = %[1]q
  location       = "us-central1"
  bucket_id      = %[2]q
  retention_days = 30
  cmek_settings {
    kms_key_name = %[3]q
  }
}
`, h.Project(), id, key))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + h.Project() + "/locations/us-central1/buckets/" + id
	b, err := c.GetBucket(h.Context(), &loggingpb.GetBucketRequest{Name: name})
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if cs := b.GetCmekSettings(); cs.GetKmsKeyName() != key || cs.GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" || cs.GetServiceAccountId() == "" {
		t.Errorf("cmek_settings = %v; want the key, its version 1 and the service account", cs)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if b, err := c.GetBucket(h.Context(), &loggingpb.GetBucketRequest{Name: name}); err != nil ||
		b.GetLifecycleState() != loggingpb.LifecycleState_DELETE_REQUESTED {
		t.Errorf("GetBucket after destroy = %v, %v; want DELETE_REQUESTED", b, err)
	}
}
