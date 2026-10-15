//go:build compat

package compat

import (
	"fmt"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	vlogging "cloud.google.com/go/logging/apiv2"
)

// TestTerraformLoggingBucketsAndLinks (#1332): google_logging_project_bucket_config
// on the built-in _Default bucket and on a user bucket with Log Analytics on,
// a regional user bucket whose Log Analytics is turned on by an update
// (the provider's updateAsync), and a google_logging_linked_dataset on the
// first, apply with the official
// hashicorp/google provider through `cloudburrow terraform` (egress blocked),
// are read back with the official ConfigClient, plan clean, take a retention
// and description change in place, plan clean again, and are destroyed: the
// link and the user bucket are gone (the bucket DELETE_REQUESTED, as Google
// keeps a deleted bucket for 7 days) and _Default stays, as the provider
// documents for a built-in bucket.
// covers: google.logging.v2.ConfigServiceV2/GetBucket, google.logging.v2.ConfigServiceV2/CreateBucket, google.logging.v2.ConfigServiceV2/UpdateBucket, google.logging.v2.ConfigServiceV2/DeleteBucket, google.logging.v2.ConfigServiceV2/CreateLink, google.logging.v2.ConfigServiceV2/GetLink, google.logging.v2.ConfigServiceV2/DeleteLink
func TestTerraformLoggingBucketsAndLinks(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	m := newTFModule(t)
	ctx := h.Context()
	config, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()

	project := h.Project()
	module := func(retention int, description string) string {
		return fmt.Sprintf(`
resource "google_logging_project_bucket_config" "default" {
  project        = %[1]q
  location       = "global"
  bucket_id      = "_Default"
  retention_days = %[2]d
  description    = %[3]q
}
resource "google_logging_project_bucket_config" "analytics" {
  project          = %[1]q
  location         = "global"
  bucket_id        = "tf-analytics"
  retention_days   = %[2]d
  description      = %[3]q
  enable_analytics = true
}
resource "google_logging_project_bucket_config" "toggle" {
  project          = %[1]q
  location         = "us-central1"
  bucket_id        = "tf-toggle"
  retention_days   = %[2]d
  enable_analytics = %[4]t
}
resource "google_logging_linked_dataset" "link" {
  link_id     = "tf_link"
  bucket      = google_logging_project_bucket_config.analytics.id
  description = "linked"
}
`, project, retention, description, retention > 45)
	}

	parent := "projects/" + project + "/locations/global/buckets/"
	check := func(when string, retention int32, description string) {
		t.Helper()
		for _, id := range []string{"_Default", "tf-analytics"} {
			b, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + id})
			if err != nil {
				t.Fatalf("%s: GetBucket %s: %v", when, id, err)
			}
			if b.GetRetentionDays() != retention || b.GetDescription() != description {
				t.Errorf("%s: bucket %s = %v; want retention %d and description %q", when, id, b, retention, description)
			}
			if id == "tf-analytics" && !b.GetAnalyticsEnabled() {
				t.Errorf("%s: bucket %s has analytics off", when, id)
			}
		}
		tb, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "projects/" + project + "/locations/us-central1/buckets/tf-toggle"})
		if err != nil || tb.GetRetentionDays() != retention || tb.GetAnalyticsEnabled() != (retention > 45) {
			t.Errorf("%s: bucket tf-toggle = %v, %v; want retention %d and analytics %v", when, tb, err, retention, retention > 45)
		}
		l, err := config.GetLink(ctx, &loggingpb.GetLinkRequest{Name: parent + "tf-analytics/links/tf_link"})
		if err != nil {
			t.Fatalf("%s: GetLink: %v", when, err)
		}
		if l.GetDescription() != "linked" || l.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE ||
			l.GetBigqueryDataset().GetDatasetId() != "bigquery.googleapis.com/projects/"+project+"/datasets/tf_link" {
			t.Errorf("%s: the link is %v", when, l)
		}
	}

	m.write(module(45, "first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 45, "first")
	m.planClean()

	m.write(module(60, "second"))
	m.apply()
	check("after the update", 60, "second")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := config.GetLink(ctx, &loggingpb.GetLinkRequest{Name: parent + "tf-analytics/links/tf_link"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLink after destroy: %v; want NotFound", err)
	}
	if b, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + "tf-analytics"}); err != nil ||
		b.GetLifecycleState() != loggingpb.LifecycleState_DELETE_REQUESTED {
		t.Errorf("GetBucket tf-analytics after destroy = %v, %v; want DELETE_REQUESTED", b, err)
	}
	if _, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + "_Default"}); err != nil {
		t.Errorf("GetBucket _Default after destroy: %v; want it kept", err)
	}
}
