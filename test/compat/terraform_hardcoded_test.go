//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
)

// TestTerraformRefusesHardCodedDataSources (#1578): three data sources whose
// URLs hashicorp/google 8.4.0 builds from a fixed Google host, whatever the
// custom endpoints say, are refused by `cloudburrow terraform` before
// Terraform runs, though the Run port and Resource Manager are served: a
// plan of google_cloud_run_locations (https://run.googleapis.com/v1/),
// google_artifact_registry_locations
// (https://artifactregistry.googleapis.com/v1/) or
// google_iam_testable_permissions (https://iam.googleapis.com/v1/) fails
// naming it and why, and `init` (offline) still runs. Before #1578 the
// wrapper let them through to Google.
func TestTerraformRefusesHardCodedDataSources(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun) // skips where the Run port is not served
	h.Endpoint(EnvResourceManager)
	for _, c := range []struct{ typ, body, host string }{
		{"google_cloud_run_locations", `data "google_cloud_run_locations" "l" {}`, "run.googleapis.com"},
		{"google_artifact_registry_locations", `data "google_artifact_registry_locations" "l" {}`, "artifactregistry.googleapis.com"},
		{"google_iam_testable_permissions", `data "google_iam_testable_permissions" "p" {
  full_resource_name = "//cloudresourcemanager.googleapis.com/projects/p"
}`, "iam.googleapis.com"},
	} {
		t.Run(c.typ, func(t *testing.T) {
			m := newTFModule(t)
			m.write("\n" + c.body + "\n")
			m.must("init", "-input=false", "-no-color")
			out, err := m.tf("plan", "-input=false", "-no-color")
			if err == nil {
				t.Fatalf("plan of %s ran:\n%s", c.typ, lastLines(out, 20))
			}
			if !strings.Contains(out, "refusing to run terraform plan") || !strings.Contains(out, c.typ+" (") || !strings.Contains(out, c.host) {
				t.Errorf("plan of %s failed, but not with the wrapper's refusal naming it and %s:\n%s", c.typ, c.host, lastLines(out, 20))
			}
		})
	}
}

// TestTerraformBigQueryTableDropColumnHardCoded (#1578) measures a gap: a
// plan whose google_bigquery_table schema drops a column makes
// hashicorp/google 8.4.0 list the table's row access policies (to decide
// whether to replace it) at https://bigquery.googleapis.com/bigquery/v2/,
// whatever bigquery_custom_endpoint says (bigQueryTableHasRowAccessPolicy
// in resource_bigquery_table.go). The provider ignores that call's error,
// so the plan succeeds, proposing an in-place update, but the request goes
// to Google (here to the closed proxy; its debug log shows the request).
// The wrapper cannot refuse the resource for it. When a provider honours
// the endpoint this test fails: then docs/gaps.md's row moves.
func TestTerraformBigQueryTableDropColumnHardCoded(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	m := newTFModule(t)
	ds := "tfdrop_" + strings.ReplaceAll(h.Project(), "-", "_")
	module := func(columns string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_table" "t" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.d.dataset_id
  table_id            = "t"
  deletion_protection = false
  schema              = jsonencode([%[3]s])
}
`, project, ds, columns)
	}
	both := `{ name = "a", type = "STRING", mode = "NULLABLE" }, { name = "b", type = "STRING", mode = "NULLABLE" }`
	m.write(module(both))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	m.write(module(`{ name = "a", type = "STRING", mode = "NULLABLE" }`))
	t.Setenv("TF_LOG", "DEBUG") // noGoogleEgress passes it on
	out, err := m.tf("plan", "-input=false", "-no-color")
	t.Setenv("TF_LOG", "")
	if err != nil {
		t.Fatalf("a plan dropping a column failed: %v\n%s", err, lastLines(out, 20))
	}
	path := "/bigquery/v2/projects/" + project + "/datasets/" + ds + "/tables/t/rowAccessPolicies"
	if !strings.Contains(out, "GET "+path) || !strings.Contains(out, "Host: bigquery.googleapis.com") {
		t.Errorf("a plan dropping a column did not call Google's rowAccessPolicies; if the provider now honours bigquery_custom_endpoint, update docs/gaps.md:\n%s", lastLines(out, 40))
	}
	if !strings.Contains(out, "update in-place") {
		t.Errorf("the plan did not update the table in place:\n%s", lastLines(out, 40))
	}
	m.write(module(both)) // so the cleanup's destroy plans
}
