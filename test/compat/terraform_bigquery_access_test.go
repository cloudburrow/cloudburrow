//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
)

// TestTerraformBigQueryAccess (#1304): a google_bigquery_dataset_access
// granting a user READER, another authorizing a view of a second dataset,
// and a google_bigquery_row_access_policy on a table apply with the official
// hashicorp/google provider through `cloudburrow terraform`; the generated
// client reads the dataset's access list and the policy back; the plan is
// clean; the policy's filter and grantees change in place (dataset_access
// has no in-place update: every argument replaces it); and destroy removes
// the access entries, the policy and the datasets.
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.patch, bigquery.datasets.delete, bigquery.tables.insert, bigquery.tables.get, bigquery.tables.delete, bigquery.rowAccessPolicies.insert, bigquery.rowAccessPolicies.get, bigquery.rowAccessPolicies.update, bigquery.rowAccessPolicies.delete
func TestTerraformBigQueryAccess(t *testing.T) {
	h := New(t)
	// The emulator serves one project, which the client names.
	_, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	svc := bigqueryService(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	ds := "tfa_" + strings.ReplaceAll(h.Project(), "-", "_")
	views := ds + "_views"
	const user = "ada@example.com"

	module := func(region, grantee string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_dataset" "views" {
  project                    = %[1]q
  dataset_id                 = %[3]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_table" "t" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.d.dataset_id
  table_id            = "orders"
  deletion_protection = false
  schema = jsonencode([
    { name = "id", type = "INTEGER", mode = "NULLABLE" },
    { name = "region", type = "STRING", mode = "NULLABLE" },
  ])
}
resource "google_bigquery_table" "v" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.views.dataset_id
  table_id            = "orders_view"
  deletion_protection = false
  view {
    query          = "SELECT id FROM `+"`%[1]s.%[2]s.orders`"+`"
    use_legacy_sql = false
  }
  depends_on = [google_bigquery_table.t]
}
resource "google_bigquery_dataset_access" "user" {
  project       = %[1]q
  dataset_id    = google_bigquery_dataset.d.dataset_id
  role          = "READER"
  user_by_email = %[4]q
}
resource "google_bigquery_dataset_access" "view" {
  project    = %[1]q
  dataset_id = google_bigquery_dataset.d.dataset_id
  view {
    project_id = %[1]q
    dataset_id = google_bigquery_dataset.views.dataset_id
    table_id   = google_bigquery_table.v.table_id
  }
}
resource "google_bigquery_row_access_policy" "p" {
  project          = %[1]q
  dataset_id       = google_bigquery_dataset.d.dataset_id
  table_id         = google_bigquery_table.t.table_id
  policy_id        = "only_region"
  filter_predicate = "region = '%[5]s'"
  grantees         = [%[6]q]
}
`, project, ds, views, user, region, grantee)
	}
	check := func(when, region, grantee string) {
		t.Helper()
		d, err := svc.Datasets.Get(project, ds).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, datasets.get: %v", when, err)
		}
		var reader, view bool
		for _, a := range d.Access {
			reader = reader || (a.Role == "READER" && a.UserByEmail == user)
			view = view || (a.View != nil && a.View.DatasetId == views && a.View.TableId == "orders_view")
		}
		if !reader || !view {
			t.Errorf("%s, the dataset's access = %+v; want READER for %s and the authorized view", when, d.Access, user)
		}
		p, err := svc.RowAccessPolicies.Get(project, ds, "orders", "only_region").Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, rowAccessPolicies.get: %v", when, err)
		}
		if p.FilterPredicate != "region = '"+region+"'" || strings.Join(p.Grantees, ",") != grantee {
			t.Errorf("%s, the row access policy = %q, %v; want region %s for %s", when, p.FilterPredicate, p.Grantees, region, grantee)
		}
	}

	m.write(module("EU", "user:"+user))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "EU", "user:"+user)
	m.planClean()

	m.write(module("US", "group:burrowers@example.com"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the policy change was not one in-place update:\n%s", lastLines(out, 20))
	}
	check("after the policy change", "US", "group:burrowers@example.com")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, d := range []string{ds, views} {
		_, err := svc.Datasets.Get(project, d).Context(ctx).Do()
		if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
			t.Errorf("dataset %s after destroy: %v; want 404", d, err)
		}
	}
}
