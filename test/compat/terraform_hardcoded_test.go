//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	arapi "google.golang.org/api/artifactregistry/v1"
	"google.golang.org/api/option"
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
	shardEndpoint(h, envRunResourceManager, EnvResourceManager)
	h.Endpoint(EnvRun) // skips where the Run port is not served
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
// The wrapper cannot refuse the resource for it; its opt-in
// --intercept-hardcoded serves the call locally
// (TestTerraformBigQueryTableDropColumnIntercepted). When a provider honours
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

// TestTerraformBigQueryTableDropColumnIntercepted (#1578): with `cloudburrow
// terraform --intercept-hardcoded`, the provider's hard-coded
// rowAccessPolicies call reaches this instance's BigQuery front through the
// wrapper's local HTTPS intercept (an ephemeral CA the Terraform run alone
// trusts, a loopback proxy that serves bigquery.googleapis.com locally and
// refuses everything else), so a dropped column plans as Google's would:
//   - with no row access policy, the plan updates the table in place, the
//     apply drops the column (read back through the official client) and
//     the next plan is clean;
//   - with a row access policy on the table, dropping a column plans a
//     replacement, as the provider does against Google, where egress
//     blocked it planned in place (TestTerraformBigQueryTableDropColumnHardCoded).
//
// On macOS a provider built with a Go before 1.27 ignores SSL_CERT_FILE,
// which the wrapper names; the test then skips. Linux, CI's, honours it.
func TestTerraformBigQueryTableDropColumnIntercepted(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	m := newTFModule(t)
	m.flags = append(m.flags, "--intercept-hardcoded")
	ds := "tfdropi_" + strings.ReplaceAll(h.Project(), "-", "_")
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
	colA := `{ name = "a", type = "STRING", mode = "NULLABLE" }`
	colB := `{ name = "b", type = "STRING", mode = "NULLABLE" }`
	m.write(module(colA + ", " + colB))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()

	m.write(module(colA))
	out := m.must("plan", "-input=false", "-no-color")
	if strings.Contains(out, "ignores SSL_CERT_FILE") {
		t.Skipf("the provider cannot trust the intercept's CA on this OS:\n%s", lastLines(out, 5))
	}
	if !interceptServed(out) {
		t.Fatalf("the plan's rowAccessPolicies call did not go through the intercept:\n%s", lastLines(out, 30))
	}
	if !strings.Contains(out, "update in-place") || strings.Contains(out, "must be replaced") {
		t.Fatalf("with no row access policy, dropping a column should update in place:\n%s", lastLines(out, 40))
	}
	m.apply()
	md, err := c.Dataset(ds).Table("t").Metadata(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != 1 || md.Schema[0].Name != "a" {
		t.Errorf("after the apply the table's schema is %v, want only a", md.Schema)
	}
	m.planClean()

	// Back to two columns (an addition, in place), then a policy.
	m.write(module(colA + ", " + colB))
	m.apply()
	m.planClean()
	if _, err := c.Query("CREATE ROW ACCESS POLICY p ON `" + ds + ".t` GRANT TO ('allUsers') FILTER USING (TRUE)").Read(h.Context()); err != nil {
		t.Fatalf("CREATE ROW ACCESS POLICY: %v", err)
	}
	m.write(module(colA))
	out = m.must("plan", "-input=false", "-no-color")
	if !interceptServed(out) {
		t.Fatalf("the plan's rowAccessPolicies call did not go through the intercept:\n%s", lastLines(out, 30))
	}
	if !strings.Contains(out, "must be replaced") {
		t.Errorf("with a row access policy, dropping a column should replace the table:\n%s", lastLines(out, 40))
	}
	m.write(module(colA + ", " + colB)) // so the cleanup's destroy plans
}

// interceptServed reports whether the wrapper's intercept says it served
// at least one request to bigquery.googleapis.com.
func interceptServed(out string) bool { return interceptServedHost(out, "bigquery.googleapis.com") }

// interceptServedHost reports whether the wrapper's intercept says it
// served at least one request to host.
func interceptServedHost(out, host string) bool {
	suffix := " request(s) to " + host + " locally"
	return strings.Contains(out, suffix) && !strings.Contains(out, "served 0"+suffix)
}

// covers: run.projects.locations.list, google.cloud.location.Locations/ListLocations
//
// TestTerraformLocationsIntercepted (#1578): with `cloudburrow terraform
// --intercept-hardcoded`, the data sources google_cloud_run_locations and
// google_artifact_registry_locations, which hashicorp/google builds from
// https://run.googleapis.com/v1/ and https://artifactregistry.googleapis.com/v1/
// whatever its custom endpoints say, are no longer refused: the wrapper's
// local intercept serves both hosts from the Run port, and each lists what
// the official generated client's first page lists (Cloud Run v1's
// projects.locations.list, and Artifact Registry's under its own prefix),
// as the provider, which does not page, reads Google's. Without the flag
// they stay refused (TestTerraformRefusesHardCodedDataSources). On macOS a
// provider built with a Go before 1.27 ignores SSL_CERT_FILE, which the
// wrapper names; the test then skips. Linux, CI's, honours it.
func TestTerraformLocationsIntercepted(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envRunResourceManager, EnvResourceManager)
	endpoint := h.Endpoint(EnvRun) // skips where the Run port is not served
	rv1 := runV1Client(t, h)
	rl, err := rv1.Projects.Locations.List("projects/" + h.Project()).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("run v1 projects.locations.list: %v", err)
	}
	ar, err := arapi.NewService(h.Context(), option.WithEndpoint("http://"+endpoint+"/artifactregistry/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	al, err := ar.Projects.Locations.List("projects/" + h.Project()).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("artifactregistry projects.locations.list: %v", err)
	}
	var wantRun, wantAR []string
	for _, l := range rl.Locations {
		wantRun = append(wantRun, l.LocationId)
	}
	for _, l := range al.Locations {
		wantAR = append(wantAR, l.LocationId)
	}
	sort.Strings(wantRun)
	sort.Strings(wantAR)
	if len(wantRun) == 0 || len(wantAR) == 0 {
		t.Fatalf("the clients list %d Cloud Run and %d Artifact Registry locations", len(wantRun), len(wantAR))
	}

	m := newTFModule(t)
	m.flags = append(m.flags, "--intercept-hardcoded")
	m.write(`
data "google_cloud_run_locations" "run" {}
data "google_artifact_registry_locations" "ar" {}
output "run" { value = data.google_cloud_run_locations.run.locations }
output "ar"  { value = data.google_artifact_registry_locations.ar.locations }
`)
	m.must("init", "-input=false", "-no-color")
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if strings.Contains(out, "ignores SSL_CERT_FILE") {
		t.Skipf("the provider cannot trust the intercept's CA on this OS:\n%s", lastLines(out, 5))
	}
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, lastLines(out, 30))
	}
	for _, host := range []string{"run.googleapis.com", "artifactregistry.googleapis.com"} {
		if !interceptServedHost(out, host) {
			t.Errorf("the data sources' calls to %s did not go through the intercept:\n%s", host, lastLines(out, 30))
		}
	}
	for _, c := range []struct {
		name string
		want []string
	}{{"run", wantRun}, {"ar", wantAR}} {
		var got []string
		if err := json.Unmarshal([]byte(m.must("output", "-json", c.name)), &got); err != nil {
			t.Fatalf("output %s: %v", c.name, err)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("output %s = %v; the official client lists %v", c.name, got, c.want)
		}
	}
	m.planClean()
}
