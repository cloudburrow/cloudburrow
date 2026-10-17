//go:build compat

package compat

import (
	"errors"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryCreateInMissingDataset (#1584): a CREATE TABLE in a dataset
// that does not exist fails 404 notFound "Not found: Dataset p:ds was not
// found in location L" through jobs.query and as a failed job through
// jobs.insert, as BigQuery's job fails ("Troubleshoot query errors",
// Location not found), and leaves nothing: the same statement, run again,
// fails the same, not "already exists".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.getQueryResults
func TestBigQueryCreateInMissingDataset(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("dataset metadata: %v", err)
	}
	missing := ds.DatasetID + "_missing_1584"
	want := failedErr{code: 404, reason: "notFound",
		message: "Not found: Dataset " + c.Project() + ":" + missing + " was not found in location " + md.Location}
	for _, sql := range []string{
		"CREATE TABLE " + missing + ".x (a INT64)",
		"CREATE TABLE " + missing + ".y AS SELECT 1 AS a",
		"CREATE VIEW " + missing + ".v AS SELECT 1 AS a",
	} {
		for run := 0; run < 2; run++ {
			if _, err := c.Query(sql).Read(ctx); true {
				if got := failedQueryError(t, "jobs.query", err); got != want {
					t.Errorf("%s, jobs.query run %d: %v, want %v", sql, run, got, want)
				}
			}
			job, err := c.Query(sql).Run(ctx)
			if err != nil {
				t.Fatalf("%s, jobs.insert run %d: %v, want a job that fails", sql, run, err)
			}
			if _, err := job.Wait(ctx); true {
				if got := failedQueryError(t, "Job.Wait", err); got != want {
					t.Errorf("%s, Job.Wait run %d: %v, want %v", sql, run, got, want)
				}
			}
			st, err := job.Status(ctx)
			if err != nil {
				t.Fatalf("jobs.get: %v", err)
			}
			var be *bigquery.Error
			if st.State != bigquery.Done || !errors.As(st.Err(), &be) || be.Reason != want.reason || be.Message != want.message {
				t.Errorf("%s, jobs.get run %d: %+v, want DONE failed %v", sql, run, st.Err(), want)
			}
		}
	}
	if _, err := c.Dataset(missing).Metadata(ctx); err == nil {
		t.Errorf("the dataset %s exists", missing)
	}
}

// TestBigQueryRuntimeErrorReasons (#1585): the errors of a query the
// engine runs reach the official Go client with reason invalidQuery,
// BigQuery's for "an invalid query" (the error messages table), through
// jobs.query, jobs.insert, jobs.getQueryResults and jobs.get, not 400
// jobInternalError with the engine's text:
//
//   - a division by zero: "division by zero: 1 / 0" (the operators
//     reference: "Divide by zero operations return an error"; the message
//     is GoogleSQL's, UNVERIFIED against BigQuery; docs re-read
//     2026-10-17: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/operators);
//   - ERROR(message): the message (the debugging functions reference's
//     example gives "Found unexpected value: baz");
//   - a named parameter the request does not give: "Query parameter 'p'
//     not found at [1:8]" (UNVERIFIED; docs re-read 2026-10-17:
//     https://docs.cloud.google.com/bigquery/docs/parameterized-queries).
//
// A script's statement's error position counts from the start of the
// script, as BigQuery's (SELECT 1;\nSELECT FROM at [2:8]); and a table is
// named in a dry run's error as the client names it, not by the engine's
// storage name (p_ds_t).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults, bigquery.jobs.get
func TestBigQueryRuntimeErrorReasons(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	validationTable(t, h, ds, "t1585")
	d := ds.DatasetID
	for _, tc := range []struct {
		name, sql string
		params    []bigquery.QueryParameter
		message   string
		dry       bool
	}{
		{name: "division by zero", sql: "SELECT 1/0", message: "division by zero: 1 / 0"},
		{name: "division by zero of a column", sql: "SELECT 3/(COUNT(*)-COUNT(*)) FROM " + d + ".t1585", message: "division by zero: 3 / 0"},
		{name: "ERROR", sql: `SELECT ERROR("boom 1585")`, message: "boom 1585"},
		{name: "a missing parameter", sql: "SELECT @p", message: "Query parameter 'p' not found at [1:8]"},
		{name: "a missing parameter beside one given", sql: "SELECT @given,\n  @p",
			params: []bigquery.QueryParameter{{Name: "given", Value: 1}}, message: "Query parameter 'p' not found at [2:3]"},
		{name: "a script's syntax error", sql: "SELECT 1;\nSELECT FROM", message: "Syntax error: SELECT list must not be empty at [2:8]"},
		{name: "a script's unrecognized name", sql: "DECLARE x INT64; SELECT nope FROM " + d + ".t1585",
			message: "Unrecognized name: nope; Did you mean note? at [1:25]"},
		{name: "a script's statement in the middle", sql: "SELECT 1; SELECT nope FROM " + d + ".t1585; SELECT 2",
			message: "Unrecognized name: nope; Did you mean note? at [1:18]"},
		{name: "a script's division by zero", sql: "SELECT 1;\nSELECT 1/0", message: "division by zero: 1 / 0"},
		{name: "a dry run's column of a table", sql: "INSERT INTO " + d + ".t1585 (nosuch) VALUES (1)",
			message: "Column nosuch is not present in table " + d + ".t1585 at [1:" + itoa(len("INSERT INTO "+d+".t1585 (")+1) + "]", dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := func() *bigquery.Query {
				q := c.Query(tc.sql)
				q.Parameters = tc.params
				return q
			}
			want := failedErr{code: 400, reason: "invalidQuery", message: tc.message}
			check := func(what string, got failedErr) {
				t.Helper()
				if got != want {
					t.Errorf("%s: %v, want %v", what, got, want)
				}
			}
			if tc.dry {
				q := query()
				q.DryRun = true
				_, err := q.Run(ctx)
				check("a dry run", failedQueryError(t, "a dry run", err))
				return
			}
			_, err := query().Read(ctx)
			check("jobs.query", failedQueryError(t, "jobs.query", err))
			job, err := query().Run(ctx)
			if err != nil {
				t.Fatalf("jobs.insert: %v, want a job that fails", err)
			}
			_, err = job.Wait(ctx)
			check("Job.Wait", failedQueryError(t, "Job.Wait", err))
			st, err := job.Status(ctx)
			if err != nil {
				t.Fatalf("jobs.get: %v", err)
			}
			var be *bigquery.Error
			if st.State != bigquery.Done || !errors.As(st.Err(), &be) {
				t.Fatalf("jobs.get: %+v, want DONE with an errorResult", st)
			}
			check("jobs.get errorResult", failedErr{code: 400, reason: be.Reason, message: be.Message})
		})
	}
}
