//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
)

// TestBigQueryUndeleteKeepsRoutinesAndModels (#1483): a dataset's
// routines and BigQuery ML models come back with it. BigQuery's undelete
// restores a dataset "to the state it was in when it was deleted"
// (https://cloud.google.com/bigquery/docs/restore-deleted-datasets, read
// 2026-10-16), and deleteContents deletes "all the tables, views, routines
// and models in the dataset"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/datasets/delete).
//
// A SQL function made by CREATE FUNCTION and one by routines.insert, a
// table function, a stored procedure and a LINEAR_REG model, through
// datasets.delete with deleteContents and datasets.undelete, and through
// DROP SCHEMA … CASCADE and UNDROP SCHEMA: routines.list and models.list
// give them back with their creation times, and each one still runs. While
// the dataset is deleted, none of them can be called. DROP SCHEMA RESTRICT
// of a dataset with a model fails resourceInUse, as datasets.delete
// without deleteContents does (#1465).
//
// Measured first, through the front: after datasets.delete with
// deleteContents the function still ran, and datasets.undelete failed 404
// (since #1465 the front's datasets.delete of the models had taken the
// request before the deletion was kept). No clock is moved: the window's
// expiry is TestBigQueryUndeleteAndRestore's.
//
// covers: bigquery.datasets.undelete, bigquery.datasets.delete, bigquery.routines.insert, bigquery.routines.list, bigquery.routines.get, bigquery.models.list, bigquery.models.get, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryUndeleteKeepsRoutinesAndModels(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()
	name := "compat_undelete_routines_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(name)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func(sql string) []string {
		t.Helper()
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return readRows(t, it, sql)
	}
	p := func(id string) string { return "`" + name + "." + id + "`" }

	schema, err := bigquery.InferSchema(mlRow{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.Table("train").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("Table.Create: %v", err)
	}
	if err := ds.Table("train").Inserter().Put(ctx, mlRows()); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	run("CREATE FUNCTION " + p("twice") + "(x INT64) RETURNS INT64 AS (x * 2)")
	if err := ds.Routine("thrice").Create(ctx, &bigquery.RoutineMetadata{
		Type:       "SCALAR_FUNCTION",
		Language:   "SQL",
		Body:       "x * 3",
		ReturnType: &bigquery.StandardSQLDataType{TypeKind: "INT64"},
		Arguments:  []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
	}); err != nil {
		t.Fatalf("routines.insert thrice: %v", err)
	}
	run("CREATE TABLE FUNCTION " + p("upto") + "(n INT64) AS (SELECT x AS v FROM UNNEST(GENERATE_ARRAY(1, n)) AS x)")
	run("CREATE PROCEDURE " + p("add") + "(a INT64, b INT64) BEGIN SELECT a + b AS s; END")
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL %s OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS "+
		"SELECT x1, x2, c, y FROM %s", p("lin"), p("train"))); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}

	routines := func() map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		it := ds.Routines(ctx)
		for {
			r, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("routines.list: %v", err)
			}
			md, err := r.Metadata(ctx)
			if err != nil {
				t.Fatalf("routines.get %s: %v", r.RoutineID, err)
			}
			out[r.RoutineID] = md.CreationTime.UnixMilli()
		}
	}
	models := func() []string {
		t.Helper()
		var out []string
		it := ds.Models(ctx)
		for {
			m, err := it.Next()
			if errors.Is(err, iterator.Done) {
				sort.Strings(out)
				return out
			}
			if err != nil {
				t.Fatalf("models.list: %v", err)
			}
			out = append(out, m.ModelID)
		}
	}
	before := routines()
	if ids := len(before); ids != 4 {
		t.Fatalf("routines before the deletion: %v, want twice, thrice, upto and add", before)
	}
	lin, err := ds.Model("lin").Metadata(ctx)
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	check := func(what string) {
		t.Helper()
		if got := routines(); !reflect.DeepEqual(got, before) {
			t.Errorf("%s: routines (id: creation) %v, want %v", what, got, before)
		}
		if got := models(); !reflect.DeepEqual(got, []string{"lin"}) {
			t.Errorf("%s: models %v, want [lin]", what, got)
		}
		if md, err := ds.Model("lin").Metadata(ctx); err != nil || md.Type != "LINEAR_REGRESSION" || !md.CreationTime.Equal(lin.CreationTime) {
			t.Errorf("%s: models.get %+v, %v; want the LINEAR_REGRESSION model made at %v", what, md, err, lin.CreationTime)
		}
		if got := read(fmt.Sprintf("SELECT %s.twice(4), %s.thrice(4)", "`"+name+"`", "`"+name+"`")); !reflect.DeepEqual(got, []string{"8|12"}) {
			t.Errorf("%s: twice(4), thrice(4) = %v, want [8|12]", what, got)
		}
		if got := read("SELECT v FROM " + p("upto") + "(3) ORDER BY v"); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
			t.Errorf("%s: upto(3) = %v, want [1 2 3]", what, got)
		}
		if got := read("CALL " + p("add") + "(2, 3)"); !reflect.DeepEqual(got, []string{"5"}) {
			t.Errorf("%s: CALL add(2, 3) = %v, want [5]", what, got)
		}
		got := read(fmt.Sprintf("SELECT ROUND(predicted_y, 3) FROM ML.PREDICT(MODEL %s, (SELECT 2 AS x1, 1.5 AS x2, 'b' AS c))", p("lin")))
		if !reflect.DeepEqual(got, []string{"10.5"}) {
			t.Errorf("%s: ML.PREDICT = %v, want [10.5]", what, got)
		}
	}
	gone := func(what string) {
		t.Helper()
		if _, err := c.Query("SELECT " + p("twice") + "(1)").Read(ctx); err == nil {
			t.Errorf("%s: twice(1) still runs", what)
		}
	}
	check("before the deletion")

	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatalf("DeleteWithContents: %v", err)
	}
	gone("after datasets.delete")
	if _, err := svc.Datasets.Undelete(project, name, &bq.UndeleteDatasetRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("datasets.undelete: %v", err)
	}
	check("after datasets.undelete")

	wantReason(t, "DROP SCHEMA RESTRICT of a dataset with a model", bqRun(ctx, c, "DROP SCHEMA "+name+" RESTRICT", false),
		http.StatusBadRequest, "resourceInUse")
	run("DROP SCHEMA " + name + " CASCADE")
	gone("after DROP SCHEMA")
	run("UNDROP SCHEMA " + name)
	check("after UNDROP SCHEMA")
}
