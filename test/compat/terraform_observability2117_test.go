//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestTerraformObservability2117 (#2117): the google_observability_*
// resources of hashicorp/google (the pinned googleProviderVersion; the
// newest release, 8.11.0, has the same six), through `cloudburrow
// terraform` with egress blocked and observability_custom_endpoint on the
// Logging port: google_observability_project_settings (global, with a
// default storage location) and its data source, a google_observability_bucket
// on _Trace, a google_observability_link on its Spans dataset, which makes
// a BigQuery dataset, and a google_observability_trace_scope. Each is read
// back with the official cloud.google.com/go/observability client, plans
// clean, takes a description change in place, plans clean again and is
// destroyed: the link and its dataset and the trace scope are gone, and
// the settings and bucket stay, as the provider documents ("cannot be
// deleted from Google Cloud").
// covers: observability.projects.locations.getSettings, observability.projects.locations.updateSettings, observability.projects.locations.operations.get, observability.projects.locations.buckets.create, observability.projects.locations.buckets.get, observability.projects.locations.buckets.patch, observability.projects.locations.buckets.datasets.links.create, observability.projects.locations.buckets.datasets.links.get, observability.projects.locations.buckets.datasets.links.patch, observability.projects.locations.buckets.datasets.links.delete, observability.projects.locations.traceScopes.create, observability.projects.locations.traceScopes.get, observability.projects.locations.traceScopes.patch, observability.projects.locations.traceScopes.delete
func TestTerraformObservability2117(t *testing.T) {
	h := New(t)
	logging := runShardEndpoint(h, envSinkLogging, EnvLogging)
	bq, _ := bigqueryClient(t, h) // the link makes a BigQuery dataset
	c := observabilityClient(t, h, logging)
	m := newTFModule(t)
	ctx := h.Context()

	project := linkProject(t) // the link's dataset is made in BigQuery's project
	global := "projects/" + project + "/locations/global"
	before, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"})
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.UpdateSettings(context.Background(), &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings",
			DefaultStorageLocation: before.GetDefaultStorageLocation()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	// An earlier run, or TestObservabilityLinks2117, may have made _Trace;
	// the provider then updates it, as its create does on Google.
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e9)
	linkID, scopeID := "tf_spans_"+sfx, "tf-scope-"+sfx
	module := func(description string) string {
		return fmt.Sprintf(`
resource "google_observability_project_settings" "global" {
  project                  = %[1]q
  location                 = "global"
  default_storage_location = "us"
}
data "google_observability_project_settings" "global" {
  project    = %[1]q
  location   = "global"
  depends_on = [google_observability_project_settings.global]
}
resource "google_observability_bucket" "trace" {
  project      = %[1]q
  location     = "us"
  bucket_id    = "_Trace"
  display_name = "Traces"
  description  = %[2]q
  depends_on   = [google_observability_project_settings.global]
}
resource "google_observability_link" "spans" {
  project      = %[1]q
  location     = google_observability_bucket.trace.location
  bucket       = google_observability_bucket.trace.bucket_id
  dataset      = "Spans"
  link_id      = %[3]q
  display_name = "Spans in BigQuery"
  description  = %[2]q
}
resource "google_observability_trace_scope" "scope" {
  project        = %[1]q
  location       = "global"
  trace_scope_id = %[4]q
  resource_names = ["projects/%[1]s"]
  description    = %[2]q
}
output "service_account" {
  value = data.google_observability_project_settings.global.service_account_id
}
`, project, description, linkID, scopeID)
	}
	bucket := "projects/" + project + "/locations/us/buckets/_Trace"
	link := bucket + "/datasets/Spans/links/" + linkID
	scope := global + "/traceScopes/" + scopeID
	check := func(when, description string) {
		t.Helper()
		st, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"})
		if err != nil || st.GetDefaultStorageLocation() != "us" {
			t.Errorf("%s: GetSettings = %v, %v; want default storage location us", when, st, err)
		}
		if b, err := c.GetBucket(ctx, &obspb.GetBucketRequest{Name: bucket}); err != nil || b.GetDescription() != description || b.GetDisplayName() != "Traces" {
			t.Errorf("%s: GetBucket = %v, %v; want description %q", when, b, err, description)
		}
		if l, err := c.GetLink(ctx, &obspb.GetLinkRequest{Name: link}); err != nil || l.GetDescription() != description || l.GetDisplayName() != "Spans in BigQuery" {
			t.Errorf("%s: GetLink = %v, %v; want description %q", when, l, err, description)
		}
		if s, err := c.GetTraceScope(ctx, &obspb.GetTraceScopeRequest{Name: scope}); err != nil || s.GetDescription() != description ||
			len(s.GetResourceNames()) != 1 || s.GetResourceNames()[0] != "projects/"+project {
			t.Errorf("%s: GetTraceScope = %v, %v; want description %q", when, s, err, description)
		}
	}

	m.write(module("first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first")
	if sa := strings.TrimSpace(m.must("output", "-raw", "service_account")); sa != before.GetServiceAccountId() || !strings.HasSuffix(sa, "@gcp-sa-observability.iam.gserviceaccount.com") {
		t.Errorf("the data source's service_account_id = %q; want %q", sa, before.GetServiceAccountId())
	}
	if md, err := bq.Dataset(linkID).Table("_AllSpans").Metadata(ctx); err != nil || md.Type != bigquery.ViewTable {
		t.Errorf("_AllSpans of the Terraform link = %v, %v; want a view", md, err)
	}
	m.planClean()

	m.write(module("second"))
	m.apply()
	check("after the update", "second")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetLink(ctx, &obspb.GetLinkRequest{Name: link}); obsCode(err) != codes.NotFound {
		t.Errorf("GetLink after destroy: %v; want NOT_FOUND", err)
	}
	var ge *googleapi.Error
	if _, err := bq.Dataset(linkID).Metadata(ctx); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("the link's dataset after destroy = %v; want 404", err)
	}
	if _, err := c.GetTraceScope(ctx, &obspb.GetTraceScopeRequest{Name: scope}); obsCode(err) != codes.NotFound {
		t.Errorf("GetTraceScope after destroy: %v; want NOT_FOUND", err)
	}
	if _, err := c.GetBucket(ctx, &obspb.GetBucketRequest{Name: bucket}); err != nil {
		t.Errorf("GetBucket after destroy: %v; want it kept, as the API has no delete", err)
	}
}

// TestTerraformObservabilityParents2117 (#2117):
// google_observability_folder_settings and
// google_observability_organization_settings, and their data sources,
// through `cloudburrow terraform` with egress blocked: a folder's and the
// organization's default storage location apply, read back with the
// official client, plan clean, change in place, plan clean, and stay after
// destroy, as the provider documents; the organization's is restored.
// covers: observability.folders.locations.getSettings, observability.folders.locations.updateSettings, observability.folders.locations.operations.get, observability.organizations.locations.getSettings, observability.organizations.locations.updateSettings, observability.organizations.locations.operations.get
func TestTerraformObservabilityParents2117(t *testing.T) {
	h := New(t)
	folder, _ := loggingParentsFixture(t, h, "tfobs-")
	c := observabilityClient(t, h, h.Endpoint(EnvLogging))
	m := newTFModule(t)
	ctx := h.Context()
	orgGlobal := localOrg + "/locations/global/settings"
	before, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: orgGlobal})
	if err != nil {
		t.Fatalf("GetSettings %s: %v", orgGlobal, err)
	}
	t.Cleanup(func() {
		if op, err := c.UpdateSettings(context.Background(), &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: orgGlobal,
			DefaultStorageLocation: before.GetDefaultStorageLocation()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	folderID := strings.TrimPrefix(folder, "folders/")
	orgID := strings.TrimPrefix(localOrg, "organizations/")
	module := func(loc string) string {
		return fmt.Sprintf(`
resource "google_observability_folder_settings" "f" {
  folder                   = %[1]q
  location                 = "global"
  default_storage_location = %[3]q
}
resource "google_observability_organization_settings" "o" {
  organization             = %[2]q
  location                 = "global"
  default_storage_location = %[3]q
}
data "google_observability_folder_settings" "f" {
  folder     = %[1]q
  location   = "global"
  depends_on = [google_observability_folder_settings.f]
}
data "google_observability_organization_settings" "o" {
  organization = %[2]q
  location     = "global"
  depends_on   = [google_observability_organization_settings.o]
}
output "folder_account" {
  value = data.google_observability_folder_settings.f.service_account_id
}
output "org_account" {
  value = data.google_observability_organization_settings.o.service_account_id
}
`, folderID, orgID, loc)
	}
	check := func(when, loc string) {
		t.Helper()
		for _, name := range []string{folder + "/locations/global/settings", orgGlobal} {
			if st, err := c.GetSettings(ctx, &obspb.GetSettingsRequest{Name: name}); err != nil || st.GetDefaultStorageLocation() != loc {
				t.Errorf("%s: GetSettings %s = %v, %v; want %s", when, name, st, err, loc)
			}
		}
	}
	m.write(module("us"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "us")
	if got := strings.TrimSpace(m.must("output", "-raw", "folder_account")); got != "service-folder-"+folderID+"@gcp-sa-observability.iam.gserviceaccount.com" {
		t.Errorf("the folder data source's service_account_id = %q", got)
	}
	if got := strings.TrimSpace(m.must("output", "-raw", "org_account")); got != "service-org-"+orgID+"@gcp-sa-observability.iam.gserviceaccount.com" {
		t.Errorf("the organization data source's service_account_id = %q", got)
	}
	m.planClean()
	m.write(module("europe-west1"))
	m.apply()
	check("after the update", "europe-west1")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("after destroy", "europe-west1")
}
