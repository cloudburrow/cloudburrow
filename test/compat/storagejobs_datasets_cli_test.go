//go:build compat

package compat

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
)

// TestGcloudStorageInsightsDatasetConfigs (#1976): gcloud storage
// insights dataset-configs create (source projects, a project-scoped
// service agent), describe, list, create-link, delete-link and delete,
// against the local Storage Insights.
func TestGcloudStorageInsightsDatasetConfigs(t *testing.T) {
	h := New(t)
	project := insightsDatasetProject(t, h)
	c := insightsClient(t, h)
	g := newGcloudSession(t, h)
	setStorageIntelligenceOf(t, h, project, "STANDARD")
	t.Cleanup(func() { setStorageIntelligenceOf(t, h, project, "INHERIT") })
	id := "gcl_" + strings.ReplaceAll(h.Project(), "-", "_")
	name := "projects/" + project + "/locations/us-central1/datasetConfigs/" + id
	t.Cleanup(func() {
		if op, err := c.UnlinkDataset(h.Context(), &storageinsightspb.UnlinkDatasetRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
		if op, err := c.DeleteDatasetConfig(h.Context(), &storageinsightspb.DeleteDatasetConfigRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	g.must("storage", "insights", "dataset-configs", "create", id, "--location", "us-central1", "--organization", "100000000001",
		"--retention-period-days", "14", "--source-projects", "123456789012", "--identity", "IDENTITY_TYPE_PER_PROJECT",
		"--description", "from gcloud", "--project", project)
	got, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: name})
	if err != nil || got.GetRetentionPeriodDays() != 14 || got.GetDescription() != "from gcloud" ||
		got.GetIdentity().GetType() != storageinsightspb.Identity_IDENTITY_TYPE_PER_PROJECT ||
		len(got.GetSourceProjects().GetProjectNumbers()) != 1 || got.GetOrganizationNumber() != 100000000001 {
		t.Fatalf("after gcloud create: %+v, %v", got, err)
	}
	if out := g.must("storage", "insights", "dataset-configs", "describe", id, "--location", "us-central1", "--project", project,
		"--format=value(retentionPeriodDays)"); strings.TrimSpace(out) != "14" {
		t.Errorf("describe: %q", out)
	}
	if out := g.must("storage", "insights", "dataset-configs", "list", "--location", "us-central1", "--project", project,
		"--format=value(name)"); !strings.Contains(out, id) {
		t.Errorf("list lacks %s:\n%s", id, out)
	}
	g.must("storage", "insights", "dataset-configs", "create-link", id, "--location", "us-central1", "--project", project)
	if got, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: name}); err != nil || !got.GetLink().GetLinked() {
		t.Fatalf("after create-link: %+v, %v", got, err)
	}
	g.must("storage", "insights", "dataset-configs", "delete-link", id, "--location", "us-central1", "--project", project)
	g.must("storage", "insights", "dataset-configs", "delete", id, "--location", "us-central1", "--project", project)
	if _, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: name}); storageJobsCode(err).String() != "NotFound" {
		t.Errorf("after gcloud delete: %v", err)
	}
}

// TestTerraformStorageInsightsDatasetConfig (#1976):
// google_storage_insights_dataset_config with source projects and a bucket
// filter applies, links its dataset, plans clean, updates its retention and
// unlinks in place (link_dataset = false), and destroys.
func TestTerraformStorageInsightsDatasetConfig(t *testing.T) {
	h := New(t)
	project := insightsDatasetProject(t, h)
	c := insightsClient(t, h)
	setStorageIntelligenceOf(t, h, project, "STANDARD")
	t.Cleanup(func() { setStorageIntelligenceOf(t, h, project, "INHERIT") })
	m := newTFModule(t)
	id := "tf_" + strings.ReplaceAll(h.Project(), "-", "_")
	name := "projects/" + project + "/locations/us-central1/datasetConfigs/" + id
	t.Cleanup(func() {
		if op, err := c.UnlinkDataset(h.Context(), &storageinsightspb.UnlinkDatasetRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
		if op, err := c.DeleteDatasetConfig(h.Context(), &storageinsightspb.DeleteDatasetConfigRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	module := func(days int, link bool) string {
		return fmt.Sprintf(`
resource "google_storage_insights_dataset_config" "d" {
  project               = %[1]q
  location              = "us-central1"
  dataset_config_id     = %[2]q
  retention_period_days = %[3]d
  description           = "from terraform"
  link_dataset          = %[4]t
  identity {
    type = "IDENTITY_TYPE_PER_CONFIG"
  }
  source_projects {
    project_numbers = ["123456789012"]
  }
  include_cloud_storage_buckets {
    cloud_storage_buckets {
      bucket_prefix_regex = "tf-.*"
    }
  }
}
`, project, id, days, link)
	}
	m.write(module(10, true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	got, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: name})
	if err != nil || got.GetRetentionPeriodDays() != 10 || !got.GetLink().GetLinked() {
		t.Fatalf("after apply: %+v, %v", got, err)
	}
	m.planClean()
	// A linked configuration cannot be deleted, as Google documents, so
	// link_dataset = false (an unlink) comes before destroy.
	m.write(module(20, false))
	m.apply()
	got, err = c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: name})
	if err != nil || got.GetRetentionPeriodDays() != 20 || got.GetUid() == "" || got.GetLink().GetLinked() {
		t.Errorf("after the in-place update: %+v, %v", got, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: name}); err == nil {
		t.Error("the configuration is still there after destroy: " + strconv.Quote(name))
	}
}
