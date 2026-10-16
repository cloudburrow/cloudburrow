//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Terraform resources of Cloud Logging's folder, organization and
// billing-account parents (#1403), on top of #1395's API.

func loggingConfigClient(t *testing.T, h *Harness) *vlogging.ConfigClient {
	t.Helper()
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	c, err := vlogging.NewConfigClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// restoreBucket puts a built-in bucket's retention and description back
// when the test ends: the provider does not delete one on destroy, and the
// organization is shared by every test.
func restoreBucket(t *testing.T, c *vlogging.ConfigClient, name string) {
	t.Helper()
	b, err := c.GetBucket(context.Background(), &loggingpb.GetBucketRequest{Name: name})
	if err != nil {
		t.Fatalf("GetBucket %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = c.UpdateBucket(context.Background(), &loggingpb.UpdateBucketRequest{Name: name,
			Bucket:     &loggingpb.LogBucket{RetentionDays: b.GetRetentionDays(), Description: b.GetDescription()},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days", "description"}}})
	})
}

// TestTerraformLoggingFolderAndOrganizationExclusionsAndBuckets (#1403):
// google_logging_folder_exclusion, google_logging_organization_exclusion,
// google_logging_folder_bucket_config and
// google_logging_organization_bucket_config (each on its parent's built-in
// _Default bucket, which the provider acquires rather than creates) apply
// with the official hashicorp/google provider through `cloudburrow
// terraform` (egress blocked), are read back with the official ConfigClient,
// plan clean, take a filter, description and retention change in place,
// plan clean again, and are destroyed: the exclusions are NOT_FOUND and the
// _Default buckets stay, as the provider documents for a built-in bucket.
// covers: google.logging.v2.ConfigServiceV2/CreateExclusion, google.logging.v2.ConfigServiceV2/GetExclusion, google.logging.v2.ConfigServiceV2/UpdateExclusion, google.logging.v2.ConfigServiceV2/DeleteExclusion, google.logging.v2.ConfigServiceV2/GetBucket, google.logging.v2.ConfigServiceV2/UpdateBucket
func TestTerraformLoggingFolderAndOrganizationExclusionsAndBuckets(t *testing.T) {
	h := New(t)
	config := loggingConfigClient(t, h)
	ctx := h.Context()
	folder, _ := loggingParentsFixture(t, h, "lge-")
	orgID := strings.TrimPrefix(localOrg, "organizations/")
	orgDefault := localOrg + "/locations/global/buckets/_Default"
	restoreBucket(t, config, orgDefault)
	orgExcl := "tf-org-" + fmt.Sprint(time.Now().UnixNano())
	module := func(severity, description string, retention int) string {
		return fmt.Sprintf(`
resource "google_logging_folder_exclusion" "f" {
  name        = "tf-folder"
  folder      = %[1]q
  description = %[3]q
  filter      = "severity<%[2]s"
}
resource "google_logging_organization_exclusion" "o" {
  name        = %[4]q
  org_id      = %[5]q
  description = %[3]q
  filter      = "severity<%[2]s"
}
resource "google_logging_folder_bucket_config" "f" {
  folder         = %[1]q
  location       = "global"
  bucket_id      = "_Default"
  retention_days = %[6]d
  description    = %[3]q
}
resource "google_logging_organization_bucket_config" "o" {
  organization   = %[5]q
  location       = "global"
  bucket_id      = "_Default"
  retention_days = %[6]d
  description    = %[3]q
}
`, folder, severity, description, orgExcl, orgID, retention)
	}
	exclusions := []string{folder + "/exclusions/tf-folder", localOrg + "/exclusions/" + orgExcl}
	buckets := []string{folder + "/locations/global/buckets/_Default", orgDefault}
	check := func(when, severity, description string, retention int32) {
		t.Helper()
		for _, name := range exclusions {
			e, err := config.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: name})
			if err != nil {
				t.Fatalf("%s: GetExclusion %s: %v", when, name, err)
			}
			if e.GetFilter() != "severity<"+severity || e.GetDescription() != description || e.GetDisabled() {
				t.Errorf("%s: exclusion %s = %v", when, name, e)
			}
		}
		for _, name := range buckets {
			b, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name})
			if err != nil {
				t.Fatalf("%s: GetBucket %s: %v", when, name, err)
			}
			if b.GetRetentionDays() != retention || b.GetDescription() != description {
				t.Errorf("%s: bucket %s = %v; want retention %d and description %q", when, name, b, retention, description)
			}
		}
	}
	m := newTFModule(t)
	m.write(module("WARNING", "first", 45))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "WARNING", "first", 45)
	m.planClean()
	m.write(module("ERROR", "second", 60))
	m.apply()
	check("after the update", "ERROR", "second", 60)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, name := range exclusions {
		if _, err := config.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: name}); status.Code(err) != codes.NotFound {
			t.Errorf("GetExclusion %s after destroy: %v; want NotFound", name, err)
		}
	}
	for _, name := range buckets {
		if _, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name}); err != nil {
			t.Errorf("GetBucket %s after destroy: %v; want it kept", name, err)
		}
	}
}

// TestTerraformLoggingOrganizationAndFolderSettings (#1403):
// google_logging_organization_settings applies with the official
// hashicorp/google provider through `cloudburrow terraform` (egress
// blocked), is read back with the official ConfigClient, plans clean, takes a
// storage_location change in place, plans clean again, and is destroyed,
// which only drops it from the state (settings cannot be deleted), so the
// organization's settings are put back after. The data sources
// google_logging_folder_settings and google_logging_organization_settings
// read both. google_logging_folder_settings does not apply: UpdateSettings
// configures only organizations, as logging_config.proto documents, and the
// provider reports the API's FAILED_PRECONDITION, which REST carries as 400.
// covers: google.logging.v2.ConfigServiceV2/GetSettings, google.logging.v2.ConfigServiceV2/UpdateSettings
func TestTerraformLoggingOrganizationAndFolderSettings(t *testing.T) {
	h := New(t)
	config := loggingConfigClient(t, h)
	ctx := h.Context()
	folder, _ := loggingParentsFixture(t, h, "lgs-")
	orgID := strings.TrimPrefix(localOrg, "organizations/")
	before, err := config.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: localOrg + "/settings"})
	if err != nil {
		t.Fatalf("GetSettings(org): %v", err)
	}
	t.Cleanup(func() {
		_, _ = config.UpdateSettings(context.Background(), &loggingpb.UpdateSettingsRequest{Name: localOrg + "/settings",
			Settings:   &loggingpb.Settings{StorageLocation: before.GetStorageLocation(), DisableDefaultSink: before.GetDisableDefaultSink()},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink"}}})
	})
	module := func(location string) string {
		return fmt.Sprintf(`
resource "google_logging_organization_settings" "o" {
  organization         = %[1]q
  storage_location     = %[2]q
  disable_default_sink = true
}
data "google_logging_organization_settings" "o" {
  organization = %[1]q
  depends_on   = [google_logging_organization_settings.o]
}
data "google_logging_folder_settings" "f" {
  folder = %[3]q
}
output "org_location" {
  value = data.google_logging_organization_settings.o.storage_location
}
output "folder_settings" {
  value = data.google_logging_folder_settings.f.name
}
`, orgID, location, strings.TrimPrefix(folder, "folders/"))
	}
	check := func(when, location string) {
		t.Helper()
		st, err := config.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: localOrg + "/settings"})
		if err != nil {
			t.Fatalf("%s: GetSettings(org): %v", when, err)
		}
		if st.GetStorageLocation() != location || !st.GetDisableDefaultSink() {
			t.Errorf("%s: the organization's settings are %v", when, st)
		}
	}
	m := newTFModule(t)
	m.write(module("europe-west1"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "europe-west1")
	if got := strings.TrimSpace(m.must("output", "-raw", "folder_settings")); got != folder+"/settings" {
		t.Errorf("data.google_logging_folder_settings name = %q; want %s/settings", got, folder)
	}
	m.planClean()
	m.write(module("us-central1"))
	m.apply()
	check("after the update", "us-central1")
	if got := strings.TrimSpace(m.must("output", "-raw", "org_location")); got != "us-central1" {
		t.Errorf("data.google_logging_organization_settings storage_location = %q", got)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("after destroy, which leaves settings as they are", "us-central1")

	// A folder's settings are read, not configured.
	f := newTFModule(t)
	f.write(fmt.Sprintf(`
resource "google_logging_folder_settings" "f" {
  folder               = %q
  disable_default_sink = true
}
`, strings.TrimPrefix(folder, "folders/")))
	f.must("init", "-input=false", "-no-color")
	out, err := f.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil || !strings.Contains(out, "Error 400") || !strings.Contains(out, "can only be configured for organizations") {
		t.Errorf("google_logging_folder_settings apply = %v; want the API's 400 refusal:\n%s", err, lastLines(out, 20))
	}
	if st, err := config.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: folder + "/settings"}); err != nil || st.GetDisableDefaultSink() {
		t.Errorf("the folder's settings after the refused apply = %v, %v", st, err)
	}
}

// TestTerraformLoggingBillingAccount (#1403): google_logging_billing_account_sink
// (with an embedded exclusion), google_logging_billing_account_exclusion and
// google_logging_billing_account_bucket_config on the account's _Default
// bucket apply with the official hashicorp/google provider through
// `cloudburrow terraform` (egress blocked), are read back with the official
// ConfigClient, plan clean, take a change in place, plan clean again, and are
// destroyed: the sink and exclusion are NOT_FOUND and _Default stays. The
// billing account is any ID of the documented form: Cloud Billing is not
// served, so nothing checks it exists (#1395).
// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/UpdateSink, google.logging.v2.ConfigServiceV2/DeleteSink, google.logging.v2.ConfigServiceV2/CreateExclusion, google.logging.v2.ConfigServiceV2/GetExclusion, google.logging.v2.ConfigServiceV2/UpdateExclusion, google.logging.v2.ConfigServiceV2/DeleteExclusion, google.logging.v2.ConfigServiceV2/GetBucket, google.logging.v2.ConfigServiceV2/UpdateBucket
func TestTerraformLoggingBillingAccount(t *testing.T) {
	h := New(t)
	config := loggingConfigClient(t, h)
	ctx := h.Context()
	accountID := fmt.Sprintf("0A0B0C-0D0E0F-%06d", time.Now().UnixNano()%1000000)
	account := "billingAccounts/" + accountID
	module := func(severity, description string, retention int) string {
		return fmt.Sprintf(`
resource "google_logging_billing_account_sink" "s" {
  name            = "tf-sink"
  billing_account = %[1]q
  description     = %[3]q
  destination     = "storage.googleapis.com/tf-billing-logs"
  filter          = "severity>=%[2]s"
  exclusions {
    name   = "no-debug"
    filter = "severity=DEBUG"
  }
}
resource "google_logging_billing_account_exclusion" "e" {
  name            = "tf-exclusion"
  billing_account = %[1]q
  description     = %[3]q
  filter          = "severity<%[2]s"
}
resource "google_logging_billing_account_bucket_config" "b" {
  billing_account = %[1]q
  location        = "global"
  bucket_id       = "_Default"
  retention_days  = %[4]d
  description     = %[3]q
}
`, accountID, severity, description, retention)
	}
	check := func(when, severity, description string, retention int32) {
		t.Helper()
		s, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: account + "/sinks/tf-sink"})
		if err != nil {
			t.Fatalf("%s: GetSink: %v", when, err)
		}
		if s.GetFilter() != "severity>="+severity || s.GetDescription() != description || s.GetIncludeChildren() ||
			len(s.GetExclusions()) != 1 || s.GetExclusions()[0].GetName() != "no-debug" {
			t.Errorf("%s: sink = %v", when, s)
		}
		e, err := config.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: account + "/exclusions/tf-exclusion"})
		if err != nil {
			t.Fatalf("%s: GetExclusion: %v", when, err)
		}
		if e.GetFilter() != "severity<"+severity || e.GetDescription() != description {
			t.Errorf("%s: exclusion = %v", when, e)
		}
		b, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: account + "/locations/global/buckets/_Default"})
		if err != nil {
			t.Fatalf("%s: GetBucket: %v", when, err)
		}
		if b.GetRetentionDays() != retention || b.GetDescription() != description {
			t.Errorf("%s: bucket = %v; want retention %d and description %q", when, b, retention, description)
		}
	}
	m := newTFModule(t)
	m.write(module("WARNING", "first", 45))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "WARNING", "first", 45)
	m.planClean()
	m.write(module("ERROR", "second", 60))
	m.apply()
	check("after the update", "ERROR", "second", 60)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: account + "/sinks/tf-sink"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSink after destroy: %v; want NotFound", err)
	}
	if _, err := config.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: account + "/exclusions/tf-exclusion"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetExclusion after destroy: %v; want NotFound", err)
	}
	if _, err := config.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: account + "/locations/global/buckets/_Default"}); err != nil {
		t.Errorf("GetBucket _Default after destroy: %v; want it kept", err)
	}
}
