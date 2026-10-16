//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryProjectDefaultLocation (#1375): ALTER PROJECT SET OPTIONS
// (`default_location` = 'EU') makes EU the location of a dataset created
// with none and of a job whose location cannot be determined, as BigQuery
// documents (https://cloud.google.com/bigquery/docs/default-configuration);
// `region-eu`.INFORMATION_SCHEMA.PROJECT_OPTIONS and
// EFFECTIVE_PROJECT_OPTIONS show it; NULL clears it, back to US. One inside
// a script's block is refused 501. Before, an instance served no
// default_location: the default was always US. The other configurations,
// ALTER ORGANIZATION and a script are TestBigQueryProjectConfigurations'.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.insert, bigquery.datasets.get
func TestBigQueryProjectDefaultLocation(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	run := func(sql string) error {
		job, err := c.Query(sql).Run(ctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	if err := run("ALTER PROJECT SET OPTIONS (`default_location` = 'EU')"); err != nil {
		t.Fatalf("ALTER PROJECT: %v", err)
	}
	t.Cleanup(func() { _ = run("ALTER PROJECT `" + project + "` SET OPTIONS (`default_location` = NULL)") })

	name := strings.ReplaceAll(project, "-", "_") + "_defloc"
	ds := c.Dataset(name)
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	if md, err := ds.Metadata(ctx); err != nil || md.Location != "EU" {
		t.Errorf("a dataset made with no location: %v (%v), want EU", md, err)
	}
	job, err := c.Query("SELECT 1").Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job.Location() != "EU" {
		t.Errorf("SELECT 1 ran in %q, want EU", job.Location())
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	for _, insert := range []bool{false, true} {
		schema, rows, err := infoSchemaRead(ctx, c, "SELECT option_name, project_id, option_value FROM `region-eu`.INFORMATION_SCHEMA.PROJECT_OPTIONS", insert)
		if err != nil {
			t.Fatalf("PROJECT_OPTIONS insert=%v: %v", insert, err)
		}
		if len(schema) != 3 || len(rows) != 1 || rows[0][0] != "default_location" || rows[0][1] != project || rows[0][2] != "EU" {
			t.Errorf("PROJECT_OPTIONS insert=%v: %v", insert, rows)
		}
	}
	_, rows, err := infoSchemaRead(ctx, c, "SELECT option_name, option_set_level, option_set_on_id, option_value FROM `region-eu`.INFORMATION_SCHEMA.EFFECTIVE_PROJECT_OPTIONS", false)
	if err != nil || len(rows) != 1 || rows[0][0] != "default_location" || rows[0][1] != "PROJECTS" || rows[0][2] != project || rows[0][3] != "EU" {
		t.Errorf("EFFECTIVE_PROJECT_OPTIONS: %v (%v)", rows, err)
	}

	// #1459: one inside a block, IF or loop of a script is carried out in
	// its turn, as BigQuery runs each statement of a script: one in a
	// branch not taken sets nothing, and one in a branch taken clears the option.
	if err := run("IF FALSE THEN ALTER PROJECT SET OPTIONS (`default_location` = 'US'); END IF; SELECT 1"); err != nil {
		t.Fatalf("ALTER PROJECT in an IF not taken: %v", err)
	}
	job, err = c.Query("SELECT 1").Run(ctx)
	if err != nil || job.Location() != "EU" {
		t.Errorf("after an IF not taken, SELECT 1 ran in %v (%v), want EU", job, err)
	}
	if err := run("IF TRUE THEN\n  ALTER PROJECT SET OPTIONS (`default_location` = NULL);\nEND IF"); err != nil {
		t.Fatalf("ALTER PROJECT NULL in an IF taken: %v", err)
	}
	job, err = c.Query("SELECT 1").Run(ctx)
	if err != nil || job.Location() != "US" {
		t.Errorf("after NULL, SELECT 1 ran in %v (%v), want US", job, err)
	}
	_, rows, err = infoSchemaRead(ctx, c, "SELECT option_name FROM `region-us`.INFORMATION_SCHEMA.PROJECT_OPTIONS", false)
	if err != nil || len(rows) != 0 {
		t.Errorf("PROJECT_OPTIONS after NULL: %v (%v)", rows, err)
	}
}

// TestBigQueryRegionViewsListTheRegionsDatasets (#1375): a region-qualified
// dataset view (`region-us`.INFORMATION_SCHEMA.TABLES, COLUMNS, ...) lists
// only the datasets in that region; one with no location given runs in its
// region; SCHEMATA has ddl and sync_status; and SCHEMATA in a script gives
// the location. Before (docs/gaps.md, #1371): region-us gave every
// dataset's rows whatever its location, and in a script SCHEMATA was the
// engine's, location NULL. And (#1417) ddl has DEFAULT COLLATE, and
// SCHEMATA after a CREATE SCHEMA in the same script shows the new dataset;
// before, it was read as the script started, and the dataset was missing.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryRegionViewsListTheRegionsDatasets(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	base := strings.ReplaceAll(project, "-", "_") + "_regionviews"
	us, eu := c.Dataset(base+"_us"), c.Dataset(base+"_eu")
	for d, loc := range map[*bigquery.Dataset]string{us: "US", eu: "EU"} {
		if err := d.Create(ctx, &bigquery.DatasetMetadata{Location: loc, Description: "in " + loc}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteWithContents(ctx) })
		if err := d.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, view := range []string{"TABLES", "COLUMNS", "TABLE_OPTIONS", "SCHEMATA"} {
		col := "table_schema"
		if view == "SCHEMATA" {
			col = "schema_name"
		}
		for _, region := range []string{"us", "eu"} {
			for _, insert := range []bool{false, true} {
				sql := "SELECT DISTINCT " + col + " FROM `region-" + region + "`.INFORMATION_SCHEMA." + view +
					" WHERE " + col + " IN ('" + us.DatasetID + "', '" + eu.DatasetID + "')"
				_, rows, err := infoSchemaRead(ctx, c, sql, insert)
				want := map[string]string{"us": us.DatasetID, "eu": eu.DatasetID}[region]
				if view == "TABLE_OPTIONS" {
					// No option is set on either table: no rows at all.
					if err != nil || len(rows) != 0 {
						t.Errorf("%s: %v (%v)", sql, rows, err)
					}
					continue
				}
				if err != nil || len(rows) != 1 || rows[0][0] != want {
					t.Errorf("insert=%v %s: %v (%v), want %s alone", insert, sql, rows, err, want)
				}
			}
		}
	}

	_, rows, err := infoSchemaRead(ctx, c, "SELECT ddl, sync_status FROM `region-eu`.INFORMATION_SCHEMA.SCHEMATA WHERE schema_name = '"+eu.DatasetID+"'", false)
	want := "CREATE SCHEMA `" + project + "." + eu.DatasetID + "`\nOPTIONS(\n  description=\"in EU\",\n  location=\"eu\"\n);"
	if err != nil || len(rows) != 1 || rows[0][0] != want || rows[0][1] != nil {
		t.Errorf("SCHEMATA ddl: %q (%v), want %q", rows, err, want)
	}

	// #1417: DEFAULT COLLATE in ddl, and SCHEMATA in a script sees a dataset the script made.
	coll := c.Dataset(base + "_coll")
	if err := coll.Create(ctx, &bigquery.DatasetMetadata{Location: "US", DefaultCollation: "und:ci"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coll.DeleteWithContents(ctx) })
	_, rows, err = infoSchemaRead(ctx, c, "SELECT ddl FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA WHERE schema_name = '"+coll.DatasetID+"'", false)
	want = "CREATE SCHEMA `" + project + "." + coll.DatasetID + "`\nDEFAULT COLLATE 'und:ci'\nOPTIONS(\n  location=\"us\"\n);"
	if err != nil || len(rows) != 1 || rows[0][0] != want {
		t.Errorf("SCHEMATA ddl with a collation: %q (%v), want %q", rows, err, want)
	}
	made := c.Dataset(base + "_made")
	t.Cleanup(func() { _ = made.DeleteWithContents(ctx) })
	for _, insert := range []bool{false, true} {
		sql := "CREATE SCHEMA IF NOT EXISTS " + made.DatasetID + " OPTIONS (location = 'US');\n" +
			"SELECT schema_name, location FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA WHERE schema_name = '" + made.DatasetID + "'"
		_, rows, err := infoSchemaRead(ctx, c, sql, insert)
		if err != nil || len(rows) != 1 || rows[0][0] != made.DatasetID || rows[0][1] != "US" {
			t.Errorf("insert=%v SCHEMATA after CREATE SCHEMA in the script: %v (%v)", insert, rows, err)
		}
	}

	script := "DECLARE n INT64 DEFAULT 1;\nSELECT schema_name, location FROM " + us.DatasetID + ".INFORMATION_SCHEMA.SCHEMATA"
	for _, insert := range []bool{false, true} {
		_, rows, err := infoSchemaRead(ctx, c, script, insert)
		if err != nil || len(rows) != 1 || rows[0][0] != us.DatasetID || rows[0][1] != "US" {
			t.Errorf("insert=%v SCHEMATA in a script: %v (%v)", insert, rows, err)
		}
	}
}
