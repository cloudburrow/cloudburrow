//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryDatasetWithoutLocationIsInTheDefault (#1371): a dataset
// created with no location is in the default location, US (an instance
// configures no default_location), as datasets.get, datasets.list,
// tables.get, INFORMATION_SCHEMA.SCHEMATA and its jobs report it; a
// location given is kept; and a query names the location of the dataset
// it reads. Measured first: the dataset read back with no location at all
// (`bq cp` failed with KeyError: 'location'), SCHEMATA's location was NULL,
// and jobs.query's jobReference had none.
//
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.list, bigquery.tables.get, bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryDatasetWithoutLocationIsInTheDefault(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c) // Create(ctx, nil): no location
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Location != "US" {
		t.Errorf("datasets.get location %q, want US", md.Location)
	}
	eu := c.Dataset(ds.DatasetID + "_eu")
	if err := eu.Create(ctx, &bigquery.DatasetMetadata{Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eu.DeleteWithContents(ctx) })
	if md, err := eu.Metadata(ctx); err != nil || md.Location != "EU" {
		t.Errorf("a dataset made in EU reads location %q (%v)", md.Location, err)
	}
	// The location cannot change once set, US included.
	if _, err := ds.Update(ctx, bigquery.DatasetMetadataToUpdate{Description: "d"}, ""); err != nil {
		t.Errorf("Update: %v", err)
	}
	if md, _ := ds.Metadata(ctx); md == nil || md.Location != "US" {
		t.Errorf("after an update the location is %v", md)
	}

	for _, d := range []*bigquery.Dataset{ds, eu} {
		tb := d.Table("t")
		if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}}); err != nil {
			t.Fatal(err)
		}
		tmd, err := tb.Metadata(ctx)
		want := map[*bigquery.Dataset]string{ds: "US", eu: "EU"}[d]
		if err != nil || tmd.Location != want {
			t.Errorf("tables.get of %s.t: location %q (%v), want %s", d.DatasetID, tmd.Location, err, want)
		}
	}

	// A query with no location runs in its dataset's, else US.
	for _, c2 := range []struct{ sql, want string }{
		{"SELECT 1", "US"},
		{"SELECT a FROM `" + project + "." + ds.DatasetID + ".t`", "US"},
		{"SELECT a FROM `" + project + "." + eu.DatasetID + ".t`", "EU"},
	} {
		job, err := c.Query(c2.sql).Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c2.sql, err)
		}
		if job.Location() != c2.want {
			t.Errorf("%s: job location %q, want %s", c2.sql, job.Location(), c2.want)
		}
		if _, err := job.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", c2.sql, err)
		}
		again, err := c.JobFromIDLocation(ctx, job.ID(), "")
		if err != nil || again.Location() != c2.want {
			t.Errorf("%s: jobs.get location %v (%v), want %s", c2.sql, again, err, c2.want)
		}
	}

	// INFORMATION_SCHEMA.SCHEMATA's location, region- and dataset-qualified.
	for _, insert := range []bool{false, true} {
		for _, from := range []string{"`" + project + "`.`region-us`.INFORMATION_SCHEMA.SCHEMATA", ds.DatasetID + ".INFORMATION_SCHEMA.SCHEMATA"} {
			schema, rows, err := infoSchemaRead(ctx, c, "SELECT * FROM "+from+" WHERE schema_name IN ('"+ds.DatasetID+"', '"+
				eu.DatasetID+"') ORDER BY schema_name", insert)
			if err != nil {
				t.Fatalf("insert=%v %s: %v", insert, from, err)
			}
			if got, w := infoSchemaColumns(schema), "catalog_name STRING, schema_name STRING, schema_owner STRING, "+
				"creation_time TIMESTAMP, last_modified_time TIMESTAMP, location STRING, default_collation_name STRING"; got != w {
				t.Errorf("%s columns:\n got %s\nwant %s", from, got, w)
			}
			// region-us has the US dataset, not the EU one.
			if len(rows) != 1 || rows[0][0] != project || rows[0][1] != ds.DatasetID || rows[0][5] != "US" || rows[0][3] == nil {
				t.Errorf("insert=%v %s: rows %v", insert, from, rows)
			}
		}
	}
	q := c.Query("SELECT schema_name, location FROM `" + project + "`.`region-eu`.INFORMATION_SCHEMA.SCHEMATA WHERE schema_name = '" + eu.DatasetID + "'")
	q.Location = "EU"
	rows, err := locationRows(ctx, q)
	if err != nil || len(rows) != 1 || rows[0][0] != eu.DatasetID || rows[0][1] != "EU" {
		t.Errorf("region-eu SCHEMATA: %v (%v)", rows, err)
	}
}

// TestBqCopyFromADatasetWithoutLocation (#1371): bq cp of a table in a
// dataset made without a location, through the harness's bq: it read the
// dataset's location from datasets.get and failed with KeyError:
// 'location'. bq ls --format=json and bq show give the location too.
//
// covers: bigquery.datasets.get, bigquery.datasets.list, bigquery.tables.get, bigquery.jobs.insert
func TestBqCopyFromADatasetWithoutLocation(t *testing.T) {
	h := New(t)
	vars := bqEnv(t, h)
	b := newBqSession(t, vars, "--nouse_google_auth", bqToken)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	dataset := strings.ReplaceAll(project, "-", "_") + "_bqcp"
	ds := c.Dataset(dataset)
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })

	b.must("mk", "--dataset", project+":"+dataset)
	b.must("mk", "--table", dataset+".src", "id:INTEGER,name:STRING")
	if err := ds.Table("src").Inserter().Put(ctx, []*bigquery.ValuesSaver{
		{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}},
			Row: []bigquery.Value{1, "a"}},
	}); err != nil {
		t.Fatal(err)
	}

	var shown map[string]any
	if err := json.Unmarshal([]byte(b.must("show", "--format=json", project+":"+dataset)), &shown); err != nil || shown["location"] != "US" {
		t.Errorf("bq show of the dataset: location %v (%v)", shown["location"], err)
	}
	var listed []map[string]any
	if err := json.Unmarshal([]byte(b.must("ls", "--format=json")), &listed); err != nil {
		t.Fatalf("bq ls: %v", err)
	}
	found := false
	for _, d := range listed {
		if ref, _ := d["datasetReference"].(map[string]any); ref != nil && ref["datasetId"] == dataset {
			found = true
			if d["location"] != "US" {
				t.Errorf("bq ls: location %v", d["location"])
			}
		}
	}
	if !found {
		t.Errorf("bq ls did not list %s: %v", dataset, listed)
	}

	b.must("cp", dataset+".src", dataset+".dst")
	rows, err := locationRows(ctx, c.Query("SELECT id, name FROM `"+project+"."+dataset+".dst`"))
	if err != nil || len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != "a" {
		t.Errorf("the copy holds %v (%v)", rows, err)
	}
	// Again onto the table that now exists: bq reads its location
	// (tables.get) first, and with -f copies over it.
	b.must("cp", "-f", dataset+".src", dataset+".dst")
	rows, err = locationRows(ctx, c.Query("SELECT id, name FROM `"+project+"."+dataset+".dst`"))
	if err != nil || len(rows) != 1 {
		t.Errorf("after cp -f the copy holds %v (%v)", rows, err)
	}
}

// locationRows reads q's rows.
func locationRows(ctx context.Context, q *bigquery.Query) ([][]bigquery.Value, error) {
	it, err := q.Read(ctx)
	if err != nil {
		return nil, err
	}
	var out [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
}
