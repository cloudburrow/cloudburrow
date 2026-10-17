//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Storage Intelligence configurations and folder capabilities (#1519).
// Both are stored, and nothing acts on them: Storage Intelligence's
// features and App Hub are APIs CloudBurrow does not serve.

// intelligenceName is the configuration name of kind/id.
func intelligenceName(kind, id string) string {
	return kind + "/" + id + "/locations/global/intelligenceConfig"
}

// randomFolderID is a folder number no other test uses: the storage
// server keeps a folder's configuration whether or not the folder exists.
func randomFolderID() string { return strconv.FormatInt(100000000000+rand.Int64N(800000000000), 10) }

// TestStorageIntelligenceConfigs: through the official Storage Control
// client's gRPC and REST transports, a project with no configuration
// inherits and nothing is effective; STANDARD with a bucket filter is set
// over REST and read over gRPC, effective for the project, with the
// project's number in its name; a folder's TRIAL carries its expiry; the
// organization's configuration is read; DISABLED with a filter, an unknown
// mask path and a name of the wrong kind are INVALID_ARGUMENT; and going
// back to INHERIT clears the filter.
//
// covers: google.storage.control.v2.StorageControl/GetProjectIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateProjectIntelligenceConfig
// covers: google.storage.control.v2.StorageControl/GetFolderIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateFolderIntelligenceConfig
// covers: google.storage.control.v2.StorageControl/GetOrganizationIntelligenceConfig
func TestStorageIntelligenceConfigs(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	g := storageControlGRPC(t, h)
	r, err := control.NewStorageControlRESTClient(ctx, option.WithEndpoint(h.Endpoint(EnvStorage)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewStorageControlRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	project := intelligenceName("projects", h.Project())
	t.Cleanup(func() {
		_, _ = g.UpdateProjectIntelligenceConfig(context.Background(), &controlpb.UpdateProjectIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: project, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	})

	got, err := g.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: project})
	if err != nil || got.GetEditionConfig() != controlpb.IntelligenceConfig_INHERIT || !strings.HasPrefix(got.GetName(), "projects/") {
		t.Fatalf("unset = %v, %v", got, err)
	}
	if num := strings.Split(got.GetName(), "/")[1]; num == h.Project() || strings.Trim(num, "0123456789") != "" {
		t.Errorf("name %s; want the project's number", got.GetName())
	}

	filter := &controlpb.IntelligenceConfig_Filter{
		CloudStorageBuckets: &controlpb.IntelligenceConfig_Filter_ExcludedCloudStorageBuckets{
			ExcludedCloudStorageBuckets: &controlpb.IntelligenceConfig_Filter_CloudStorageBuckets{BucketIdRegexes: []string{"^tmp-.*"}}},
		CloudStorageLocations: &controlpb.IntelligenceConfig_Filter_IncludedCloudStorageLocations{
			IncludedCloudStorageLocations: &controlpb.IntelligenceConfig_Filter_CloudStorageLocations{Locations: []string{"us-central1"}}},
	}
	up, err := r.UpdateProjectIntelligenceConfig(ctx, &controlpb.UpdateProjectIntelligenceConfigRequest{
		IntelligenceConfig: &controlpb.IntelligenceConfig{Name: project, EditionConfig: controlpb.IntelligenceConfig_STANDARD, Filter: filter},
		UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	if err != nil || up.GetEditionConfig() != controlpb.IntelligenceConfig_STANDARD || up.GetUpdateTime() == nil {
		t.Fatalf("REST update to STANDARD = %v, %v", up, err)
	}
	got, err = g.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: project})
	eff := got.GetEffectiveIntelligenceConfig()
	if err != nil || got.GetFilter().GetExcludedCloudStorageBuckets().GetBucketIdRegexes()[0] != "^tmp-.*" ||
		got.GetFilter().GetIncludedCloudStorageLocations().GetLocations()[0] != "us-central1" ||
		eff.GetEffectiveEdition() != controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_STANDARD || eff.GetIntelligenceConfig() != got.GetName() {
		t.Errorf("gRPC read after STANDARD = %v, %v", got, err)
	}

	folder := intelligenceName("folders", randomFolderID())
	t.Cleanup(func() {
		_, _ = g.UpdateFolderIntelligenceConfig(context.Background(), &controlpb.UpdateFolderIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: folder, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	})
	f, err := g.UpdateFolderIntelligenceConfig(ctx, &controlpb.UpdateFolderIntelligenceConfigRequest{
		IntelligenceConfig: &controlpb.IntelligenceConfig{Name: folder, EditionConfig: controlpb.IntelligenceConfig_TRIAL},
		UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config"}}})
	if err != nil || f.GetEditionConfig() != controlpb.IntelligenceConfig_TRIAL || f.GetTrialConfig().GetExpireTime() == nil ||
		f.GetEffectiveIntelligenceConfig().GetIntelligenceConfig() != folder {
		t.Errorf("folder TRIAL = %v, %v", f, err)
	}
	if f, err := r.GetFolderIntelligenceConfig(ctx, &controlpb.GetFolderIntelligenceConfigRequest{Name: folder}); err != nil || f.GetEditionConfig() != controlpb.IntelligenceConfig_TRIAL {
		t.Errorf("REST folder get = %v, %v", f, err)
	}
	if o, err := r.GetOrganizationIntelligenceConfig(ctx, &controlpb.GetOrganizationIntelligenceConfigRequest{Name: intelligenceName("organizations", strings.TrimPrefix(localOrg, "organizations/"))}); err != nil || o.GetEditionConfig() == controlpb.IntelligenceConfig_EDITION_CONFIG_UNSPECIFIED {
		t.Errorf("organization get = %v, %v", o, err)
	}

	for what, req := range map[string]*controlpb.UpdateProjectIntelligenceConfigRequest{
		"DISABLED with a filter": {IntelligenceConfig: &controlpb.IntelligenceConfig{Name: project, EditionConfig: controlpb.IntelligenceConfig_DISABLED, Filter: filter},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}},
		"a mask of labels": {IntelligenceConfig: &controlpb.IntelligenceConfig{Name: project, EditionConfig: controlpb.IntelligenceConfig_DISABLED},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}},
		"a folder's name": {IntelligenceConfig: &controlpb.IntelligenceConfig{Name: folder, EditionConfig: controlpb.IntelligenceConfig_DISABLED},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"edition_config"}}},
	} {
		if _, err := g.UpdateProjectIntelligenceConfig(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v; want InvalidArgument", what, err)
		}
	}

	back, err := r.UpdateProjectIntelligenceConfig(ctx, &controlpb.UpdateProjectIntelligenceConfigRequest{
		IntelligenceConfig: &controlpb.IntelligenceConfig{Name: project, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
		UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	if err != nil || back.GetEditionConfig() != controlpb.IntelligenceConfig_INHERIT || back.GetFilter() != nil ||
		!strings.HasPrefix(back.GetEffectiveIntelligenceConfig().GetIntelligenceConfig(), "organizations/") &&
			back.GetEffectiveIntelligenceConfig().GetEffectiveEdition() != controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_NONE {
		t.Errorf("back to INHERIT = %v, %v", back, err)
	}
}

// TestGcloudStorageIntelligenceConfigs: gcloud storage intelligence-configs
// enable (with a bucket filter), describe, update --inherit-from-parent and
// disable on the test's project, and describe and disable on a sub-folder,
// through the egress-guarded session, read back with the official client.
//
// covers: google.storage.control.v2.StorageControl/GetProjectIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateProjectIntelligenceConfig
// covers: google.storage.control.v2.StorageControl/GetFolderIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateFolderIntelligenceConfig
func TestGcloudStorageIntelligenceConfigs(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	c := storageControlGRPC(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	project := intelligenceName("projects", h.Project())
	t.Cleanup(func() {
		_, _ = c.UpdateProjectIntelligenceConfig(context.Background(), &controlpb.UpdateProjectIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: project, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
	})
	read := func() *controlpb.IntelligenceConfig {
		t.Helper()
		got, err := c.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: project})
		if err != nil {
			t.Fatalf("GetProjectIntelligenceConfig: %v", err)
		}
		return got
	}

	g.must("storage", "intelligence-configs", "enable", "--project="+h.Project(), "--exclude-bucket-id-regexes=^scratch-.*")
	if got := read(); got.GetEditionConfig() != controlpb.IntelligenceConfig_STANDARD ||
		got.GetFilter().GetExcludedCloudStorageBuckets().GetBucketIdRegexes()[0] != "^scratch-.*" {
		t.Errorf("after enable = %v", got)
	}
	var described struct {
		EditionConfig string
		Name          string
	}
	out := g.must("storage", "intelligence-configs", "describe", "--project="+h.Project(), "--format=json")
	if err := json.Unmarshal([]byte(out), &described); err != nil || described.EditionConfig != "STANDARD" {
		t.Errorf("describe = %s (%v)", out, err)
	}
	g.must("storage", "intelligence-configs", "update", "--project="+h.Project(), "--inherit-from-parent")
	if got := read(); got.GetEditionConfig() != controlpb.IntelligenceConfig_INHERIT || got.GetFilter() != nil {
		t.Errorf("after update --inherit-from-parent = %v", got)
	}
	g.must("storage", "intelligence-configs", "disable", "--project="+h.Project())
	if got := read(); got.GetEditionConfig() != controlpb.IntelligenceConfig_DISABLED ||
		got.GetEffectiveIntelligenceConfig().GetEffectiveEdition() != controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_NONE {
		t.Errorf("after disable = %v", got)
	}

	folderID := randomFolderID()
	if out := g.must("storage", "intelligence-configs", "describe", "--sub-folder="+folderID, "--format=value(editionConfig)"); strings.TrimSpace(out) != "INHERIT" {
		t.Errorf("describe a sub-folder = %q; want INHERIT", out)
	}
	g.must("storage", "intelligence-configs", "disable", "--sub-folder="+folderID)
	if got, err := c.GetFolderIntelligenceConfig(ctx, &controlpb.GetFolderIntelligenceConfigRequest{Name: intelligenceName("folders", folderID)}); err != nil || got.GetEditionConfig() != controlpb.IntelligenceConfig_DISABLED {
		t.Errorf("sub-folder after disable = %v, %v", got, err)
	}
}

// TestTerraformStorageIntelligenceConfigs: a
// google_storage_control_project_intelligence_config (STANDARD with bucket
// and location filters), a _folder_ one (TRIAL) and an _organization_ one
// (DISABLED) apply, read back through the official client, plan clean;
// changing the project's filter and edition and the folder's edition
// applies in place and plans clean; destroy runs.
//
// covers: google.storage.control.v2.StorageControl/GetProjectIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateProjectIntelligenceConfig
// covers: google.storage.control.v2.StorageControl/GetFolderIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateFolderIntelligenceConfig
// covers: google.storage.control.v2.StorageControl/GetOrganizationIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateOrganizationIntelligenceConfig
func TestTerraformStorageIntelligenceConfigs(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	c := storageControlGRPC(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	folderID := randomFolderID()
	orgID := strings.TrimPrefix(localOrg, "organizations/")
	org := intelligenceName("organizations", orgID)
	t.Cleanup(func() {
		for _, name := range []string{org, intelligenceName("folders", folderID), intelligenceName("projects", m.project)} {
			req := &controlpb.UpdateOrganizationIntelligenceConfigRequest{
				IntelligenceConfig: &controlpb.IntelligenceConfig{Name: name, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
				UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}}
			switch {
			case strings.HasPrefix(name, "organizations/"):
				_, _ = c.UpdateOrganizationIntelligenceConfig(context.Background(), req)
			case strings.HasPrefix(name, "folders/"):
				_, _ = c.UpdateFolderIntelligenceConfig(context.Background(), &controlpb.UpdateFolderIntelligenceConfigRequest{IntelligenceConfig: req.IntelligenceConfig, UpdateMask: req.UpdateMask})
			default:
				_, _ = c.UpdateProjectIntelligenceConfig(context.Background(), &controlpb.UpdateProjectIntelligenceConfigRequest{IntelligenceConfig: req.IntelligenceConfig, UpdateMask: req.UpdateMask})
			}
		}
	})
	module := func(projectEdition, regex, folderEdition string) string {
		return fmt.Sprintf(`
resource "google_storage_control_project_intelligence_config" "p" {
  name           = %q
  edition_config = %q
  filter {
    excluded_cloud_storage_buckets {
      bucket_id_regexes = [%q]
    }
    included_cloud_storage_locations {
      locations = ["us-central1", "us-east1"]
    }
  }
}

resource "google_storage_control_folder_intelligence_config" "f" {
  name           = %q
  edition_config = %q
}

resource "google_storage_control_organization_intelligence_config" "o" {
  name           = %q
  edition_config = "DISABLED"
}
`, m.project, projectEdition, regex, folderID, folderEdition, orgID)
	}
	get := func(name string) *controlpb.IntelligenceConfig {
		t.Helper()
		var got *controlpb.IntelligenceConfig
		var err error
		switch {
		case strings.HasPrefix(name, "organizations/"):
			got, err = c.GetOrganizationIntelligenceConfig(ctx, &controlpb.GetOrganizationIntelligenceConfigRequest{Name: name})
		case strings.HasPrefix(name, "folders/"):
			got, err = c.GetFolderIntelligenceConfig(ctx, &controlpb.GetFolderIntelligenceConfigRequest{Name: name})
		default:
			got, err = c.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: name})
		}
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return got
	}

	m.write(module("STANDARD", "^tmp-.*", "TRIAL"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if p := get(intelligenceName("projects", m.project)); p.GetEditionConfig() != controlpb.IntelligenceConfig_STANDARD ||
		p.GetFilter().GetExcludedCloudStorageBuckets().GetBucketIdRegexes()[0] != "^tmp-.*" || len(p.GetFilter().GetIncludedCloudStorageLocations().GetLocations()) != 2 {
		t.Errorf("project after apply = %v", p)
	}
	if f := get(intelligenceName("folders", folderID)); f.GetEditionConfig() != controlpb.IntelligenceConfig_TRIAL || f.GetTrialConfig().GetExpireTime() == nil {
		t.Errorf("folder after apply = %v", f)
	}
	if o := get(org); o.GetEditionConfig() != controlpb.IntelligenceConfig_DISABLED {
		t.Errorf("organization after apply = %v", o)
	}
	m.planClean()

	m.write(module("TRIAL", "^scratch-.*", "STANDARD"))
	m.apply()
	if p := get(intelligenceName("projects", m.project)); p.GetEditionConfig() != controlpb.IntelligenceConfig_TRIAL ||
		p.GetFilter().GetExcludedCloudStorageBuckets().GetBucketIdRegexes()[0] != "^scratch-.*" {
		t.Errorf("project after the change = %v", p)
	}
	if f := get(intelligenceName("folders", folderID)); f.GetEditionConfig() != controlpb.IntelligenceConfig_STANDARD {
		t.Errorf("folder after the change = %v", f)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	p := get(intelligenceName("projects", m.project))
	t.Logf("project after destroy: edition %s", p.GetEditionConfig())
}

// capabilitiesV3 is Google's generated cloudresourcemanager v3 client, the
// one with folders.capabilities (the Go GAPIC v3 client has none).
func capabilitiesV3(t *testing.T, h *Harness) *crmv3.Service {
	t.Helper()
	c, err := crmv3.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvResourceManager)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestResourceManagerFolderCapabilities: through Google's generated v3
// client, a new folder's app-management capability is false; a patch is a
// done operation found by operations.get, after which the capability is
// true and the folder lists it in configuredCapabilities; an unknown
// capability is 400 and a missing folder's 404; patching it false removes
// it from the folder.
//
// covers: cloudresourcemanager.folders.capabilities.get, cloudresourcemanager.folders.capabilities.patch
func TestResourceManagerFolderCapabilities(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := capabilitiesV3(t, h)
	fc, _, _ := foldersClients(t, h)
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "cap-")}})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	name := folder.GetName() + "/capabilities/app-management"

	if got, err := c.Folders.Capabilities.Get(name).Context(ctx).Do(); err != nil || got.Name != name || got.Value {
		t.Fatalf("get unset = %+v, %v", got, err)
	}
	patch, err := c.Folders.Capabilities.Patch(name, &crmv3.Capability{Name: name, Value: true}).UpdateMask("value").Context(ctx).Do()
	if err != nil || !patch.Done {
		t.Fatalf("patch = %+v, %v", patch, err)
	}
	if got, err := c.Operations.Get(patch.Name).Context(ctx).Do(); err != nil || !got.Done || !strings.Contains(string(got.Response), `"value":true`) {
		t.Errorf("operations.get = %+v, %v", got, err)
	}
	if got, err := c.Folders.Capabilities.Get(name).Context(ctx).Do(); err != nil || !got.Value {
		t.Errorf("get after patch = %+v, %v", got, err)
	}
	if got, err := c.Folders.Get(folder.GetName()).Context(ctx).Do(); err != nil || len(got.ConfiguredCapabilities) != 1 || got.ConfiguredCapabilities[0] != name {
		t.Errorf("folder = %+v, %v; want configuredCapabilities [%s]", got, err, name)
	}
	var gerr *googleapi.Error
	if _, err := c.Folders.Capabilities.Patch(folder.GetName()+"/capabilities/other", &crmv3.Capability{Value: true}).Context(ctx).Do(); !asAPIError(err, &gerr) || gerr.Code != 400 {
		t.Errorf("an unknown capability = %v; want 400", err)
	}
	if _, err := c.Folders.Capabilities.Get("folders/1/capabilities/app-management").Context(ctx).Do(); !asAPIError(err, &gerr) || gerr.Code != 404 {
		t.Errorf("a missing folder = %v; want 404", err)
	}
	if _, err := c.Folders.Capabilities.Patch(name, &crmv3.Capability{Name: name, Value: false}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Folders.Get(folder.GetName()).Context(ctx).Do(); err != nil || len(got.ConfiguredCapabilities) != 0 {
		t.Errorf("folder after patching false = %+v, %v", got, err)
	}
}

func asAPIError(err error, out **googleapi.Error) bool {
	e, ok := err.(*googleapi.Error)
	*out = e
	return ok
}

// TestTerraformResourceManagerCapability: a google_folder and a
// google_resource_manager_capability enabling app-management on it apply,
// read back through the generated v3 client, plan clean; setting the value
// false applies in place and plans clean; destroy runs.
//
// covers: cloudresourcemanager.folders.capabilities.get, cloudresourcemanager.folders.capabilities.patch
func TestTerraformResourceManagerCapability(t *testing.T) {
	h := New(t)
	c := capabilitiesV3(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	label := folderLabel(h, "tfcap-")
	module := func(value bool) string {
		return fmt.Sprintf(`
resource "google_folder" "f" {
  display_name        = %q
  parent              = %q
  deletion_protection = false
}

resource "google_resource_manager_capability" "c" {
  parent          = google_folder.f.name
  capability_name = "app-management"
  value           = %t
}

output "folder" {
  value = google_folder.f.name
}
`, label, localOrg, value)
	}
	m.write(module(true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	folder := strings.TrimSpace(m.must("output", "-raw", "folder"))
	name := folder + "/capabilities/app-management"
	if got, err := c.Folders.Capabilities.Get(name).Context(ctx).Do(); err != nil || !got.Value {
		t.Errorf("after apply = %+v, %v", got, err)
	}
	m.planClean()

	m.write(module(false))
	m.apply()
	if got, err := c.Folders.Capabilities.Get(name).Context(ctx).Do(); err != nil || got.Value {
		t.Errorf("after setting false = %+v, %v", got, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
