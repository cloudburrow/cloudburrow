//go:build compat

package compat

import (
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryEngineErrorReasons (#1582): a query the engine cannot parse
// or analyze reaches the official Go client with BigQuery's reason, HTTP
// code and message, not the engine's 400 jobInternalError "failed to
// analyze: ...", through jobs.query (Query.Read), jobs.insert and
// jobs.getQueryResults (Query.Run, Job.Wait), jobs.get (Job.Status) and a
// dry run:
//
//   - a table that does not exist: 404 notFound "Not found: Table
//     project:dataset.table was not found in location L", or of a dataset
//     that does not exist "Not found: Dataset project:dataset was not
//     found in location L" ("Troubleshoot query errors"; the official
//     Python client's system tests expect NotFound for SELECT * FROM
//     not_a_real_dataset.doesnt_exist);
//   - a syntax error, an unrecognized name and every other analysis
//     error: 400 invalidQuery (the official Go client's
//     TestIntegration_QueryErrors expects invalidQuery for "blah blah
//     broken"), the position written " at [line:column]" (the official
//     .NET client's sample: "Unrecognized name: playr; Did you mean
//     player? at [1:8]").
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults, bigquery.jobs.get
func TestBigQueryEngineErrorReasons(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	validationTable(t, h, ds, "t1582")
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("dataset metadata: %v", err)
	}
	p, d, loc := c.Project(), ds.DatasetID, md.Location
	missing := d + "_missing_1582"
	for _, tc := range []struct {
		name string
		// sql is the query; run, whose job is run (a statement that makes
		// something is given another name).
		sql      func(run int) string
		defaults bool // the dataset is the query's default
		code     int
		reason   string
		message  string // the whole message, or with prefix, its start
		prefix   bool
		dry      bool
	}{
		{name: "unknown table", sql: func(int) string { return "SELECT * FROM " + d + ".no_such_1582" },
			code: 404, reason: "notFound", message: "Not found: Table " + p + ":" + d + ".no_such_1582 was not found in location " + loc, dry: true},
		{name: "unknown table, project-qualified", sql: func(int) string { return "SELECT * FROM `" + p + "." + d + ".no_such_1582`" },
			code: 404, reason: "notFound", message: "Not found: Table " + p + ":" + d + ".no_such_1582 was not found in location " + loc, dry: true},
		{name: "unknown table of the default dataset", sql: func(int) string { return "SELECT * FROM no_such_1582" }, defaults: true,
			code: 404, reason: "notFound", message: "Not found: Table " + p + ":" + d + ".no_such_1582 was not found in location " + loc, dry: true},
		{name: "unknown dataset", sql: func(int) string { return "SELECT * FROM " + missing + ".t" },
			code: 404, reason: "notFound", message: "Not found: Dataset " + p + ":" + missing + " was not found in location " + loc, dry: true},
		{name: "DML of an unknown table", sql: func(int) string { return "DELETE FROM " + d + ".no_such_1582 WHERE TRUE" },
			code: 404, reason: "notFound", message: "Not found: Table " + p + ":" + d + ".no_such_1582 was not found in location " + loc},
		{name: "CREATE TABLE in an unknown dataset", sql: func(run int) string { return "CREATE TABLE " + missing + ".x" + string(rune('a'+run)) + " (a INT64)" },
			code: 404, reason: "notFound", message: "Not found: Dataset " + p + ":" + missing + " was not found in location " + loc},
		{name: "unrecognized name", sql: func(int) string { return "SELECT nope FROM " + d + ".t1582" },
			code: 400, reason: "invalidQuery", message: "Unrecognized name: nope; Did you mean note? at [1:8]", dry: true},
		{name: "syntax error", sql: func(int) string { return "blah blah broken" },
			code: 400, reason: "invalidQuery", message: `Syntax error: Unexpected identifier "blah" at [1:1]`, dry: true},
		{name: "no matching signature", sql: func(int) string { return "SELECT LENGTH(1)" },
			code: 400, reason: "invalidQuery", message: "No matching signature for function LENGTH", prefix: true, dry: true},
		{name: "unknown function", sql: func(int) string { return "SELECT no_such_fn_1582(1)" },
			code: 400, reason: "invalidQuery", message: "Function not found: no_such_fn_1582", prefix: true, dry: true},
		{name: "a literal that does not cast", sql: func(int) string { return `SELECT CAST("x" AS INT64)` },
			code: 400, reason: "invalidQuery", message: `Could not cast literal "x" to type INT64 at [1:8]`, dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := func(run int) *bigquery.Query {
				q := c.Query(tc.sql(run))
				if tc.defaults {
					q.DefaultProjectID, q.DefaultDatasetID = p, d
				}
				return q
			}
			check := func(what string, got failedErr) {
				t.Helper()
				ok := got.message == tc.message
				if tc.prefix {
					ok = strings.HasPrefix(got.message, tc.message)
				}
				if got.code != tc.code || got.reason != tc.reason || !ok {
					t.Errorf("%s: %v, want %d %s %q", what, got, tc.code, tc.reason, tc.message)
				}
			}
			_, err := query(0).Read(ctx)
			want := failedQueryError(t, "jobs.query", err)
			check("jobs.query", want)

			job, err := query(1).Run(ctx)
			if err != nil {
				t.Fatalf("jobs.insert: %v, want a job that fails", err)
			}
			_, err = job.Wait(ctx)
			check("Job.Wait (jobs.getQueryResults)", failedQueryError(t, "Job.Wait", err))
			st, err := job.Status(ctx)
			if err != nil {
				t.Fatalf("jobs.get: %v", err)
			}
			var be *bigquery.Error
			if st.State != bigquery.Done || !errors.As(st.Err(), &be) {
				t.Fatalf("jobs.get: %+v, want DONE with an errorResult", st)
			}
			check("jobs.get errorResult", failedErr{code: tc.code, reason: be.Reason, message: be.Message})
			if len(st.Errors) == 0 || st.Errors[0].Reason != tc.reason {
				t.Errorf("jobs.get errors: %v, want %s first", st.Errors, tc.reason)
			}
			if !tc.dry {
				return
			}
			q := query(2)
			q.DryRun = true
			_, err = q.Run(ctx)
			check("a dry run", failedQueryError(t, "a dry run", err))
		})
	}
}
