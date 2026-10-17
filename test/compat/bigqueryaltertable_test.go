//go:build compat

package compat

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryAlterTable (#1227): ALTER TABLE ADD COLUMN, SET OPTIONS,
// ALTER COLUMN SET OPTIONS and ALTER COLUMN DROP NOT NULL, as BigQuery's
// DDL reference documents them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_add_column_statement),
// alone and in a script, through jobs.query and jobs.insert, each read back
// through Table.Metadata, and the rows of a column added read NULL.
func TestBigQueryAlterTable(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name := "alter_q"
		if insert {
			name = "alter_j"
		}
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
			{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		}, Labels: map[string]string{"old": "1"}}); err != nil {
			t.Fatal(err)
		}
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		run("INSERT INTO " + ref + " (id) VALUES (1)")
		run("ALTER TABLE " + ref + " ADD COLUMN note STRING OPTIONS (description = 'a note'), " +
			"ADD COLUMN tags ARRAY<STRING>, ADD COLUMN IF NOT EXISTS id INT64, " +
			"ADD COLUMN s STRUCT<x INT64, y ARRAY<STRING>, z STRING NOT NULL>, ADD COLUMN code STRING(10)")
		run("ALTER TABLE " + ref + " SET OPTIONS (description = 'described', friendly_name = 'Friendly', " +
			"labels = [('env', 'test'), ('team', 'data')], expiration_timestamp = TIMESTAMP '2099-01-02 03:04:05 UTC')")
		run("DECLARE d STRING DEFAULT 'the id'; ALTER TABLE " + ref + " ALTER COLUMN id SET OPTIONS (description = d); " +
			"ALTER TABLE " + ref + " ALTER COLUMN id DROP NOT NULL; ALTER TABLE IF EXISTS " + ds.DatasetID + ".no_such ADD COLUMN q INT64")

		md, err := tbl.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if md.Description != "described" || md.Name != "Friendly" || len(md.Labels) != 2 || md.Labels["env"] != "test" ||
			md.Labels["team"] != "data" || !md.ExpirationTime.Equal(time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)) {
			t.Errorf("insert %v: options: %q %q %v %v", insert, md.Description, md.Name, md.Labels, md.ExpirationTime)
		}
		want := []struct {
			name     string
			typ      bigquery.FieldType
			repeated bool
			required bool
			desc     string
		}{
			{"id", bigquery.IntegerFieldType, false, false, "the id"},
			{"note", bigquery.StringFieldType, false, false, "a note"},
			{"tags", bigquery.StringFieldType, true, false, ""},
			{"s", bigquery.RecordFieldType, false, false, ""},
			{"code", bigquery.StringFieldType, false, false, ""},
		}
		if len(md.Schema) != len(want) {
			t.Fatalf("insert %v: schema %v", insert, md.Schema)
		}
		for i, w := range want {
			f := md.Schema[i]
			if f.Name != w.name || f.Type != w.typ || f.Repeated != w.repeated || f.Required != w.required || f.Description != w.desc {
				t.Errorf("insert %v: column %d: %+v, want %+v", insert, i, f, w)
			}
		}
		if s := md.Schema[3].Schema; len(s) != 3 || s[0].Name != "x" || !s[1].Repeated || !s[2].Required {
			t.Errorf("insert %v: s's fields: %+v", insert, s)
		}
		if md.Schema[4].MaxLength != 10 {
			t.Errorf("insert %v: code's maxLength %d, want 10", insert, md.Schema[4].MaxLength)
		}
		// The new columns are the table's: NULL in the row it had, written by DML.
		run("INSERT INTO " + ref + " (id, note, tags) VALUES (2, 'n', ['a'])")
		got, err := scriptRows(ctx, c, "SELECT id, note, ARRAY_LENGTH(tags) FROM "+ref+" ORDER BY id", false)
		if err != nil || len(got) != 2 || got[0][1] != nil || got[1][1] != "n" || got[1][2] != int64(1) {
			t.Errorf("insert %v: rows %v %v", insert, got, err)
		}

		// Clearing an option with NULL; labels replaced as a whole.
		run("ALTER TABLE " + ref + " SET OPTIONS (description = NULL, labels = [('only', 'one')], expiration_timestamp = NULL)")
		md, err = tbl.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if md.Description != "" || len(md.Labels) != 1 || md.Labels["only"] != "one" || !md.ExpirationTime.IsZero() {
			t.Errorf("insert %v: cleared: %q %v %v", insert, md.Description, md.Labels, md.ExpirationTime)
		}

		// What BigQuery refuses.
		for sql, code := range map[string]int{
			"ALTER TABLE " + ref + " ADD COLUMN note INT64":               http.StatusBadRequest,
			"ALTER TABLE " + ref + " ADD COLUMN r STRING NOT NULL":        http.StatusBadRequest,
			"ALTER TABLE " + ref + " ALTER COLUMN nope DROP NOT NULL":     http.StatusBadRequest,
			"ALTER TABLE " + ds.DatasetID + ".no_such ADD COLUMN q INT64": http.StatusNotFound,
		} {
			err := bqRun(ctx, c, sql, insert)
			var e *googleapi.Error
			if code == http.StatusNotFound && insert {
				// A failed query job's error is read through
				// getQueryResults, 400 with the job's reason.
				if !errors.As(err, &e) || len(e.Errors) == 0 || e.Errors[0].Reason != "notFound" {
					t.Errorf("%s (job): %v, want reason notFound", sql, err)
				}
				continue
			}
			wantHTTPStatus(t, sql, err, code)
		}
		// What CloudBurrow does not carry out, before anything of it runs
		// (ADD COLUMN with DROP COLUMN is carried out since #1389).
		// (SET DATA TYPE to an ARRAY is 400 since #1416: a change of mode.)
		err = bqRun(ctx, c, "ALTER TABLE "+ref+" ADD COLUMN late STRING, SET OPTIONS (not_an_option = TRUE)", insert)
		wantHTTPStatus(t, "SET OPTIONS (not_an_option)", err, http.StatusNotImplemented)
		var e *googleapi.Error
		if errors.As(err, &e) && (len(e.Errors) != 1 || e.Errors[0].Reason != "notImplemented") {
			t.Errorf("SET OPTIONS (not_an_option): %+v", e.Errors)
		}
		if md, err := tbl.Metadata(ctx); err != nil || len(md.Schema) != len(want) {
			t.Errorf("insert %v: after the 501 the table changed: %v %v", insert, md, err)
		}
	}
}
