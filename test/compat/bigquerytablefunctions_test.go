//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// Table functions (#1061, #1047, #1043).
//
// The pinned goccy/bigquery-emulator v0.8.1's SQL engine freed each table
// function once Go's garbage collector ran while its catalog still pointed
// at it, and the next call failed ("wasm trap", "Table-valued function not
// found") or crashed the emulator, losing every dataset; it could not drop
// one either ("Statement not supported: DropTableFunctionStatement").
// Measured on the emulator alone, v0.8.1's image: CREATE TABLE FUNCTION,
// then 30 queries each returning 50,000 rows, then a call: SIGSEGV, exit 2.
// CloudBurrow's build (third_party/bigquery-emulator) keeps the function
// and carries out DROP TABLE FUNCTION; the same sequence, and 90 queries,
// then answered the call.

// churnEngine runs n queries that each return 50,000 rows, so that the
// emulator's Go heap grows and its garbage collector runs, and a CREATE
// TABLE and DROP TABLE every tenth, which rebuild the engine's catalog: the
// sequence that freed a table function in v0.8.1 (above). It reads the
// first row of each only. Then it runs check, and deletes the queries'
// jobs: the emulator keeps a job's rows (up to 24 hours and 256 MiB since
// #1086, which also stopped every request from reading them: a SELECT 1
// had taken 0.2 s after 20 of these, and 0.01 s once they were deleted),
// and it is the rows kept that made v0.8.1 fail (measured
// through the front: with each job deleted at once, 40 queries did not set
// it off; kept, they did, "wasm trap: runtime error: index out of range"),
// so they are kept until check has run.
func churnEngine(t *testing.T, ctx context.Context, c *bigquery.Client, d string, n int, check func()) {
	t.Helper()
	var jobs []*bigquery.Job
	defer func() {
		for _, job := range jobs {
			if err := job.Delete(ctx); err != nil {
				t.Errorf("delete churn job %s: %v", job.ID(), err)
			}
		}
	}()
	for i := 1; i <= n; i++ {
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		it, err := c.Query(fmt.Sprintf("SELECT n + %d AS n FROM UNNEST(GENERATE_ARRAY(1, 50000)) AS n", i)).Read(qctx)
		if err == nil {
			var row []bigquery.Value
			err = it.Next(&row)
			if job := it.SourceJob(); job != nil {
				jobs = append(jobs, job)
			}
		}
		cancel()
		if err != nil {
			t.Fatalf("churn query %d: %v", i, err)
		}
		if i%10 == 0 {
			tbl := fmt.Sprintf("%s.churn_%d", d, i)
			for _, sql := range []string{"CREATE TABLE " + tbl + " (a INT64)", "DROP TABLE " + tbl} {
				if err := bqRun(ctx, c, sql, false); err != nil {
					t.Fatalf("churn %s: %v", sql, err)
				}
			}
		}
	}
	check()
}

// TestBigQueryTableFunctions, through the official Go client: CREATE TABLE
// FUNCTION through jobs.query and a query job, and routines.insert of a
// TABLE_VALUED_FUNCTION routine, each called with its arguments; every one
// still answers after the engine has run many queries and rebuilt its
// catalog; CREATE OR REPLACE TABLE FUNCTION replaces one, which also
// survives that; DROP TABLE FUNCTION drops one (a call is then "not
// found"), IF EXISTS of one that is gone does nothing, and without it
// fails; a table function made again after a drop has its new body; a TEMP
// table function is called in its script; and the dataset outlives all of
// it, which it did not when the emulator crashed.
func TestBigQueryTableFunctions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	// Longer than h.Context's minute: the 80 queries of 50,000 rows take
	// most of one on an emulator that has run the rest of the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d := ds.DatasetID

	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64, s STRING) AS (SELECT x AS y, s AS t)", false); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION through jobs.query: %v", err)
	}
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tj(x INT64) AS (SELECT n * x AS y FROM UNNEST([1, 2, 3]) AS n)", true); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION as a query job: %v", err)
	}
	err := ds.Routine("tr").Create(ctx, &bigquery.RoutineMetadata{
		Type:     "TABLE_VALUED_FUNCTION",
		Language: "SQL",
		Body:     "SELECT x + 100 AS y",
		Arguments: []*bigquery.RoutineArgument{
			{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}},
		},
	})
	if err != nil {
		t.Fatalf("routines.insert of a TABLE_VALUED_FUNCTION: %v", err)
	}
	calls := map[string]string{
		"SELECT * FROM " + d + ".tf(1, 'a')":                      "[1 a]",
		"SELECT SUM(y) FROM " + d + ".tj(2)":                      "[12]",
		"SELECT * FROM " + d + ".tr(1)":                           "[101]",
		"SELECT t.y FROM " + d + ".tf(7, 'b') AS t WHERE t.y > 0": "[7]",
	}
	check := func(when string) {
		t.Helper()
		for sql, want := range calls {
			if got := bqValue(ctx, c, sql); got != want {
				t.Errorf("%s: %s = %s, want %s", when, sql, got, want)
			}
		}
	}
	check("after CREATE")
	churnEngine(t, ctx, c, d, 40, func() { check("after 40 queries of 50,000 rows") })

	// Replaced, through jobs.query and a query job; each survives the same.
	for i, insert := range []bool{false, true} {
		sql := fmt.Sprintf("CREATE OR REPLACE TABLE FUNCTION %s.tf(x INT64, s STRING) AS (SELECT x + %d AS y, CONCAT(s, '!') AS t)", d, 10*(i+1))
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("CREATE OR REPLACE TABLE FUNCTION (query job %v): %v", insert, err)
		}
	}
	delete(calls, "SELECT t.y FROM "+d+".tf(7, 'b') AS t WHERE t.y > 0")
	calls["SELECT * FROM "+d+".tf(1, 'a')"] = "[21 a!]"
	check("after CREATE OR REPLACE")
	churnEngine(t, ctx, c, d, 40, func() { check("after CREATE OR REPLACE and 40 more queries") })

	// Dropped.
	if err := bqRun(ctx, c, "DROP TABLE FUNCTION "+d+".tf", false); err != nil {
		t.Fatalf("DROP TABLE FUNCTION: %v", err)
	}
	if err := bqRun(ctx, c, "DROP TABLE FUNCTION "+d+".tj", true); err != nil {
		t.Fatalf("DROP TABLE FUNCTION as a query job: %v", err)
	}
	for _, name := range []string{"tf(1, 'a')", "tj(2)"} {
		if got := bqValue(ctx, c, "SELECT * FROM "+d+"."+name); !strings.Contains(got, "not found") {
			t.Errorf("after DROP TABLE FUNCTION, %s = %s, want not found", name, got)
		}
	}
	if err := bqRun(ctx, c, "DROP TABLE FUNCTION IF EXISTS "+d+".tf", false); err != nil {
		t.Errorf("DROP TABLE FUNCTION IF EXISTS of one that is gone: %v", err)
	}
	if err := bqRun(ctx, c, "DROP TABLE FUNCTION "+d+".tf", false); err == nil {
		t.Errorf("DROP TABLE FUNCTION of one that is gone succeeded")
	}
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x * 3 AS y)", false); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION after DROP: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+d+".tf(2)"); got != "[6]" {
		t.Errorf("made again, tf(2) = %s, want [6]", got)
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+d+".tr(1)"); got != "[101]" {
		t.Errorf("after dropping the others, tr(1) = %s, want [101]", got)
	}

	// A TEMP table function lives in its script.
	it, err := c.Query("CREATE TEMP TABLE FUNCTION tt(x INT64) AS (SELECT x + 1 AS y); SELECT * FROM tt(4)").Read(ctx)
	var row []bigquery.Value
	if err == nil {
		err = it.Next(&row)
	}
	if err != nil || fmt.Sprint(row) != "[5]" {
		t.Errorf("a TEMP table function in its script: %v %v, want [5]", row, err)
	}

	if _, err := ds.Metadata(ctx); err != nil {
		t.Errorf("the dataset is gone (did the emulator restart?): %v", err)
	}
}

// TestBigQueryDropSchemaCascadeDropsTableFunctions (#1061): DROP SCHEMA
// CASCADE of a dataset with a table function drops it (a call is then "not
// found"), and a dataset made again under the name has none; RESTRICT of
// one fails "still in use". It was 501 against the pinned v0.8.1, whose
// engine could not drop a table function.
func TestBigQueryDropSchemaCascadeDropsTableFunctions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := strings.ReplaceAll(h.Project(), "-", "_") + "_cascade"
	t.Cleanup(func() { _ = c.Dataset(name).DeleteWithContents(context.Background()) })
	if err := bqRun(ctx, c, "CREATE SCHEMA "+name+"; CREATE TABLE FUNCTION "+name+".tf(x INT64) AS (SELECT x AS y)", true); err != nil {
		t.Fatalf("CREATE SCHEMA and TABLE FUNCTION: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+name+".tf(3)"); got != "[3]" {
		t.Fatalf("tf(3) = %s, want [3]", got)
	}
	err := bqRun(ctx, c, "DROP SCHEMA "+name, false)
	wantReason(t, "DROP SCHEMA RESTRICT of a dataset with a table function", err, http.StatusBadRequest, "resourceInUse")
	if err := bqRun(ctx, c, "DROP SCHEMA "+name+" CASCADE", false); err != nil {
		t.Fatalf("DROP SCHEMA CASCADE: %v", err)
	}
	_, err = c.Dataset(name).Metadata(ctx)
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != http.StatusNotFound {
		t.Errorf("after DROP SCHEMA CASCADE, datasets.get = %v, want 404", err)
	}
	if err := bqRun(ctx, c, "CREATE SCHEMA "+name, false); err != nil {
		t.Fatalf("CREATE SCHEMA again: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+name+".tf(3)"); !strings.Contains(got, "not found") {
		t.Errorf("in the dataset made again, tf(3) = %s, want not found", got)
	}
}
