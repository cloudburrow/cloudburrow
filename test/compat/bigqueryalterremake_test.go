//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryAlterTableRenameAndDropColumn (#1322): ALTER TABLE RENAME
// COLUMN, DROP COLUMN and RENAME TO, as BigQuery's DDL reference documents
// them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_rename_to_statement),
// through jobs.query and jobs.insert. Each change is read back through
// Table.Metadata and the table's rows: a renamed column keeps its place,
// type, mode, description and values; a dropped one is gone; a renamed
// table keeps its schema, rows, description, labels and partitioning, and
// the old name is gone. Refused as BigQuery refuses them: a missing column
// (400), a new name a column has (400), dropping the partitioning column
// (400), a table name taken (409 "Already Exists"; a query job's error is
// read back 400).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tabledata.list
func TestBigQueryAlterTableRenameAndDropColumn(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name, renamed := "remake_q", "remade_q"
		if insert {
			name, renamed = "remake_j", "remade_j"
		}
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{
			Schema: bigquery.Schema{
				{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
				{Name: "note", Type: bigquery.StringFieldType, Description: "a note"},
				{Name: "gone", Type: bigquery.StringFieldType},
				{Name: "day", Type: bigquery.DateFieldType},
				{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
			},
			Description:      "kept",
			Labels:           map[string]string{"env": "test"},
			TimePartitioning: &bigquery.TimePartitioning{Field: "day"},
		}); err != nil {
			t.Fatal(err)
		}
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		run("INSERT INTO " + ref + " (id, note, gone, day, tags) VALUES (1, 'one', 'x', DATE '2024-01-02', ['a', 'b']), " +
			"(2, NULL, 'y', DATE '2024-01-03', [])")

		for sql, code := range map[string]int{
			"ALTER TABLE " + ref + " DROP COLUMN nope":           http.StatusBadRequest,
			"ALTER TABLE " + ref + " RENAME COLUMN nope TO x":    http.StatusBadRequest,
			"ALTER TABLE " + ref + " RENAME COLUMN note TO gone": http.StatusBadRequest,
			"ALTER TABLE " + ref + " DROP COLUMN day":            http.StatusBadRequest,
		} {
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), code)
		}

		run("ALTER TABLE " + ref + " RENAME COLUMN note TO remark, DROP COLUMN gone, DROP COLUMN IF EXISTS nope, " +
			"RENAME COLUMN IF EXISTS nope TO other")
		md, err := tbl.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range md.Schema {
			names = append(names, f.Name)
		}
		if len(md.Schema) != 4 || md.Schema[0].Name != "id" || !md.Schema[0].Required || md.Schema[1].Name != "remark" ||
			md.Schema[1].Type != bigquery.StringFieldType || md.Schema[1].Description != "a note" || md.Schema[2].Name != "day" ||
			md.Schema[3].Name != "tags" || !md.Schema[3].Repeated {
			t.Errorf("insert %v: schema after RENAME COLUMN and DROP COLUMN: %v", insert, names)
		}
		if md.Description != "kept" || md.Labels["env"] != "test" || md.TimePartitioning == nil || md.TimePartitioning.Field != "day" {
			t.Errorf("insert %v: the table's properties were not kept: %q %v %+v", insert, md.Description, md.Labels, md.TimePartitioning)
		}
		readRows := func(tb *bigquery.Table) [][]bigquery.Value {
			t.Helper()
			it := tb.Read(ctx)
			var rows [][]bigquery.Value
			for {
				var row []bigquery.Value
				err := it.Next(&row)
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("read %s: %v", tb.TableID, err)
				}
				rows = append(rows, row)
			}
			return rows
		}
		check := func(tb *bigquery.Table) {
			t.Helper()
			rows := readRows(tb)
			if len(rows) != 2 {
				t.Fatalf("insert %v: %s has %d rows, want 2", insert, tb.TableID, len(rows))
			}
			byID := map[int64][]bigquery.Value{}
			for _, r := range rows {
				byID[r[0].(int64)] = r
			}
			one, two := byID[1], byID[2]
			if len(one) != 4 || one[1] != "one" || len(one[3].([]bigquery.Value)) != 2 || two == nil || two[1] != nil {
				t.Errorf("insert %v: %s rows %v", insert, tb.TableID, rows)
			}
		}
		check(tbl)

		other := ds.Table("taken_" + name)
		if err := other.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}}); err != nil {
			t.Fatal(err)
		}
		err = bqRun(ctx, c, "ALTER TABLE "+ref+" RENAME TO taken_"+name, insert)
		var e *googleapi.Error
		if insert {
			// A failed query job's error is read through getQueryResults,
			// 400 with the job's message.
			if !errors.As(err, &e) || e.Code != http.StatusBadRequest || !strings.Contains(e.Message, "Already Exists") {
				t.Errorf("RENAME TO a taken name (job): %v, want 400 Already Exists", err)
			}
		} else {
			wantHTTPStatus(t, "RENAME TO a taken name", err, http.StatusConflict)
		}

		run("ALTER TABLE " + ref + " RENAME TO " + renamed)
		if tableExists(t, ctx, tbl) {
			t.Errorf("insert %v: %s is still there after RENAME TO", insert, name)
		}
		nt := ds.Table(renamed)
		md, err = nt.Metadata(ctx)
		if err != nil {
			t.Fatalf("insert %v: %s: %v", insert, renamed, err)
		}
		if len(md.Schema) != 4 || md.Schema[1].Name != "remark" || md.Description != "kept" || md.Labels["env"] != "test" ||
			md.TimePartitioning == nil || md.TimePartitioning.Field != "day" {
			t.Errorf("insert %v: %s after RENAME TO: %+v", insert, renamed, md)
		}
		check(nt)
	}
}
