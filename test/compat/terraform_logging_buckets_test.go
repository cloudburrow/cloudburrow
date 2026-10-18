//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/googleapi"
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
	shardEndpoint(h, envSinkLogging, EnvLogging)
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	// The link makes a BigQuery dataset, so a shard without BigQuery skips
	// here, before terraform runs.
	bq, _ := bigqueryClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	config, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()

	project := linkProject(t) // the link's dataset is made in BigQuery's project (#1423)
	// That project is the same on every run, so the IDs are this run's.
	sfx := fmt.Sprint(time.Now().UnixNano())
	analyticsID, toggleID, linkID := "tf-analytics-"+sfx, "tf-toggle-"+sfx, "tf_link_"+sfx
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
  bucket_id        = %[5]q
  retention_days   = %[2]d
  description      = %[3]q
  enable_analytics = true
}
resource "google_logging_project_bucket_config" "toggle" {
  project          = %[1]q
  location         = "us-central1"
  bucket_id        = %[6]q
  retention_days   = %[2]d
  enable_analytics = %[4]t
}
resource "google_logging_linked_dataset" "link" {
  link_id     = %[7]q
  bucket      = google_logging_project_bucket_config.analytics.id
  description = "linked"
}
`, project, retention, description, retention > 45, analyticsID, toggleID, linkID)
	}

	parent := "projects/" + project + "/locations/global/buckets/"
	check := func(when string, retention int32, description string) {
		t.Helper()
		for _, id := range []string{"_Default", analyticsID} {
			b, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + id})
			if err != nil {
				t.Fatalf("%s: GetBucket %s: %v", when, id, err)
			}
			if b.GetRetentionDays() != retention || b.GetDescription() != description {
				t.Errorf("%s: bucket %s = %v; want retention %d and description %q", when, id, b, retention, description)
			}
			if id == analyticsID && !b.GetAnalyticsEnabled() {
				t.Errorf("%s: bucket %s has analytics off", when, id)
			}
		}
		tb, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "projects/" + project + "/locations/us-central1/buckets/" + toggleID})
		if err != nil || tb.GetRetentionDays() != retention || tb.GetAnalyticsEnabled() != (retention > 45) {
			t.Errorf("%s: bucket tf-toggle = %v, %v; want retention %d and analytics %v", when, tb, err, retention, retention > 45)
		}
		l, err := config.GetLink(ctx, &loggingpb.GetLinkRequest{Name: parent + analyticsID + "/links/" + linkID})
		if err != nil {
			t.Fatalf("%s: GetLink: %v", when, err)
		}
		if l.GetDescription() != "linked" || l.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE ||
			l.GetBigqueryDataset().GetDatasetId() != "bigquery.googleapis.com/projects/"+project+"/datasets/"+linkID {
			t.Errorf("%s: the link is %v", when, l)
		}
	}

	m.write(module(45, "first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 45, "first")
	// The link made its BigQuery dataset, with the _AllLogs view (#1423).
	if md, err := bq.Dataset(linkID).Table("_AllLogs").Metadata(ctx); err != nil || md.Type != bigquery.ViewTable {
		t.Errorf("_AllLogs of the Terraform link = %v, %v; want a view", md, err)
	}
	m.planClean()

	m.write(module(60, "second"))
	m.apply()
	check("after the update", 60, "second")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := config.GetLink(ctx, &loggingpb.GetLinkRequest{Name: parent + analyticsID + "/links/" + linkID}); status.Code(err) != codes.NotFound {
		t.Errorf("GetLink after destroy: %v; want NotFound", err)
	}
	var ge *googleapi.Error
	if _, err := bq.Dataset(linkID).Metadata(ctx); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("the link's dataset after destroy = %v; want 404", err)
	}
	if b, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + analyticsID}); err != nil ||
		b.GetLifecycleState() != loggingpb.LifecycleState_DELETE_REQUESTED {
		t.Errorf("GetBucket tf-analytics after destroy = %v, %v; want DELETE_REQUESTED", b, err)
	}
	if _, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + "_Default"}); err != nil {
		t.Errorf("GetBucket _Default after destroy: %v; want it kept", err)
	}
}

// TestTerraformLoggingLogView (#1304): a google_logging_log_view on a user
// bucket made by google_logging_project_bucket_config applies through
// `cloudburrow terraform` (egress blocked), is read back with the official
// ConfigClient, plans clean, takes a description and filter change in place,
// plans clean again, and is destroyed with its bucket.
// covers: google.logging.v2.ConfigServiceV2/CreateView, google.logging.v2.ConfigServiceV2/GetView, google.logging.v2.ConfigServiceV2/UpdateView, google.logging.v2.ConfigServiceV2/DeleteView
func TestTerraformLoggingLogView(t *testing.T) {
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
	module := func(description, filter string) string {
		return fmt.Sprintf(`
resource "google_logging_project_bucket_config" "b" {
  project        = %[1]q
  location       = "global"
  bucket_id      = "tf-views"
  retention_days = 30
}
resource "google_logging_log_view" "v" {
  name        = "tf-view"
  bucket      = google_logging_project_bucket_config.b.id
  description = %[2]q
  filter      = %[3]q
}
`, project, description, filter)
	}
	name := "projects/" + project + "/locations/global/buckets/tf-views/views/tf-view"
	check := func(when, description, filter string) {
		t.Helper()
		v, err := config.GetView(ctx, &loggingpb.GetViewRequest{Name: name})
		if err != nil {
			t.Fatalf("%s: GetView: %v", when, err)
		}
		if v.GetDescription() != description || v.GetFilter() != filter {
			t.Errorf("%s: the view = %q, %q; want %q, %q", when, v.GetDescription(), v.GetFilter(), description, filter)
		}
	}

	f1 := `SOURCE("projects/` + project + `") AND LOG_ID("app")`
	f2 := `SOURCE("projects/` + project + `") AND LOG_ID("audit")`
	m.write(module("first", f1))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first", f1)
	m.planClean()

	m.write(module("second", f2))
	m.apply()
	check("after the change", "second", f2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := config.GetView(ctx, &loggingpb.GetViewRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetView after destroy: %v; want NotFound", err)
	}
}
