//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestTerraformBigQueryPartitionedTableAndJob (#1304): a google_bigquery_table
// partitioned by day on a TIMESTAMP column, with a partition expiration,
// clustering and a column with a default value expression, and a
// google_bigquery_job running a query into a destination table, apply with
// the official hashicorp/google provider through `cloudburrow terraform`;
// the official Go client reads the table's partitioning, clustering, schema
// and the job back; the plan is clean; the table's description, labels and
// clustering change in place; and destroy removes the tables and the
// dataset (a job is only dropped from state, as on Google).
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.delete, bigquery.tables.insert, bigquery.tables.get, bigquery.tables.patch, bigquery.tables.delete, bigquery.jobs.insert, bigquery.jobs.get
func TestTerraformBigQueryPartitionedTableAndJob(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	m := newTFModule(t)
	ctx := h.Context()
	ds := "tfp_" + strings.ReplaceAll(h.Project(), "-", "_")
	jobID := "tf_job_" + strings.ReplaceAll(h.Project(), "-", "_")

	module := func(description, team, clustering string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_table" "events" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.d.dataset_id
  table_id            = "events"
  deletion_protection = false
  description         = %[4]q
  labels              = { team = %[5]q }
  time_partitioning {
    type          = "DAY"
    field         = "ts"
    expiration_ms = 7776000000
  }
  clustering = [%[6]s]
  schema = jsonencode([
    { name = "ts", type = "TIMESTAMP", mode = "REQUIRED" },
    { name = "region", type = "STRING", mode = "NULLABLE", defaultValueExpression = "'unknown'" },
    { name = "kind", type = "STRING", mode = "NULLABLE" },
  ])
}
resource "google_bigquery_job" "q" {
  project  = %[1]q
  job_id   = %[3]q
  location = "US"
  query {
    query              = "SELECT 1 AS n, 'one' AS word"
    use_legacy_sql     = false
    create_disposition = "CREATE_IF_NEEDED"
    write_disposition  = "WRITE_TRUNCATE"
    destination_table {
      project_id = %[1]q
      dataset_id = google_bigquery_dataset.d.dataset_id
      table_id   = "answers"
    }
  }
}
`, project, ds, jobID, description, team, clustering)
	}
	check := func(when, description, team string, clustering []string) {
		t.Helper()
		md, err := c.Dataset(ds).Table("events").Metadata(ctx)
		if err != nil {
			t.Fatalf("%s, tables.get: %v", when, err)
		}
		if md.Description != description || md.Labels["team"] != team {
			t.Errorf("%s, description %q labels %v; want %q, team=%s", when, md.Description, md.Labels, description, team)
		}
		tp := md.TimePartitioning
		if tp == nil || tp.Type != bigquery.DayPartitioningType || tp.Field != "ts" || tp.Expiration.Milliseconds() != 7776000000 {
			t.Errorf("%s, time partitioning = %+v; want DAY on ts, 90 days", when, tp)
		}
		if md.Clustering == nil || strings.Join(md.Clustering.Fields, ",") != strings.Join(clustering, ",") {
			t.Errorf("%s, clustering = %+v; want %v", when, md.Clustering, clustering)
		}
		var def string
		for _, f := range md.Schema {
			if f.Name == "region" {
				def = f.DefaultValueExpression
			}
		}
		if len(md.Schema) != 3 || !md.Schema[0].Required || def != "'unknown'" {
			t.Errorf("%s, schema = %d fields, ts required %v, region's default %q; want 3, true, 'unknown'",
				when, len(md.Schema), len(md.Schema) > 0 && md.Schema[0].Required, def)
		}
	}
	checkJob := func() {
		t.Helper()
		j, err := c.JobFromIDLocation(ctx, jobID, "US")
		if err != nil {
			t.Fatalf("jobs.get %s: %v", jobID, err)
		}
		st, err := j.Status(ctx)
		if err != nil || st.State != bigquery.Done || st.Err() != nil {
			t.Fatalf("the job's status = %+v, %v; want DONE with no error", st, err)
		}
		var row []bigquery.Value
		rows := c.Dataset(ds).Table("answers").Read(ctx)
		if err := rows.Next(&row); err != nil {
			t.Fatalf("reading the job's destination table: %v", err)
		}
		if len(row) != 2 || fmt.Sprint(row[0]) != "1" || row[1] != "one" {
			t.Errorf("the destination table's row = %v; want [1 one]", row)
		}
	}

	m.write(module("raw events", "data", `"region"`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "raw events", "data", []string{"region"})
	checkJob()
	m.planClean()

	m.write(module("events by region and kind", "platform", `"region", "kind"`))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the table change was not one in-place update:\n%s", lastLines(out, 20))
	}
	check("after the change", "events by region and kind", "platform", []string{"region", "kind"})
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := c.Dataset(ds).Metadata(ctx)
	if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
		t.Errorf("dataset %s after destroy: %v; want 404", ds, err)
	}
}
