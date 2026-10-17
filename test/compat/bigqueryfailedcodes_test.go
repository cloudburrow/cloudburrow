//go:build compat

package compat

import (
	"errors"
	"fmt"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryFailedQueryErrorBothPaths (#1581): a query that fails
// before it runs reaches the official Go client with the same reason and
// message, and the HTTP code its reason documents on BigQuery's
// "Troubleshoot errors" page, whichever way it is run:
//
//   - Query.Read, jobs.query: the error, *googleapi.Error;
//   - Query.Run, jobs.insert: a job, whose Wait (jobs.getQueryResults)
//     answers that error, and whose status (jobs.get) is DONE with it as
//     errorResult;
//   - a dry run, jobs.insert with dryRun: the error, as jobs.query's,
//     and no job.
//
// Measured first: ALTER SCHEMA and DROP SCHEMA of a missing dataset were
// 404 notFound through Query.Read and 400 notFound through Job.Wait,
// CREATE SCHEMA of a dataset that exists 409 and 400 duplicate, and a dry
// run that failed was a job, DONE with its errorResult.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults, bigquery.jobs.get
func TestBigQueryFailedQueryErrorBothPaths(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	missing := d + "_missing_1581"
	for _, tc := range []struct {
		name, sql string
		code      int    // 0: the code its reason documents
		reason    string // "": any, the same through each path
		dry       bool   // its dry run fails the same
	}{
		{name: "unknown table", sql: "SELECT * FROM " + d + ".no_such_table_1581", dry: true},
		{name: "syntax error", sql: "SELEC 1", dry: true},
		{name: "ALTER SCHEMA of a missing dataset", sql: "ALTER SCHEMA " + missing + " SET OPTIONS (description = 'x')", code: 404, reason: "notFound"},
		{name: "DROP SCHEMA of a missing dataset", sql: "DROP SCHEMA " + missing, code: 404, reason: "notFound", dry: true},
		{name: "CREATE SCHEMA of a dataset that exists", sql: "CREATE SCHEMA " + d, code: 409, reason: "duplicate"},
		{name: "a script's child", sql: "SELECT 1; ALTER SCHEMA " + missing + " SET OPTIONS (description = 'x')", code: 404, reason: "notFound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Query(tc.sql).Read(ctx)
			want := failedQueryError(t, "jobs.query", err)
			if tc.reason != "" && (want.reason != tc.reason || want.code != tc.code) {
				t.Fatalf("jobs.query: %+v, want %d %s", want, tc.code, tc.reason)
			}
			if doc := failedReasonCodes[want.reason]; len(doc) > 0 && !hasCode(doc, want.code) {
				t.Errorf("jobs.query: %+v; the reason %s documents %v", want, want.reason, doc)
			}

			job, err := c.Query(tc.sql).Run(ctx)
			if err != nil {
				t.Fatalf("jobs.insert: %v, want a job that fails", err)
			}
			_, err = job.Wait(ctx)
			if got := failedQueryError(t, "Job.Wait (jobs.getQueryResults)", err); got != want {
				t.Errorf("Job.Wait (jobs.getQueryResults): %+v, want %+v as jobs.query", got, want)
			}
			st, err := job.Status(ctx)
			if err != nil {
				t.Fatalf("jobs.get: %v", err)
			}
			var be *bigquery.Error
			if st.State != bigquery.Done || !errors.As(st.Err(), &be) {
				t.Fatalf("jobs.get: %+v, want DONE with an errorResult", st)
			}
			if be.Reason != want.reason || be.Message != want.message {
				t.Errorf("jobs.get errorResult: %q %q, want %q %q", be.Reason, be.Message, want.reason, want.message)
			}
			if len(st.Errors) == 0 || st.Errors[0].Reason != want.reason {
				t.Errorf("jobs.get errors: %v, want %s first", st.Errors, want.reason)
			}

			if !tc.dry {
				return
			}
			q := c.Query(tc.sql)
			q.DryRun = true
			job, err = q.Run(ctx)
			if got := failedQueryError(t, "a dry run", err); got != want {
				t.Errorf("a dry run: %+v, want %+v as jobs.query", got, want)
			}
			if job != nil {
				t.Errorf("a dry run that failed answered a job, %s", job.ID())
			}
		})
	}
	// The front's own refusal, of what it does not serve: 501 through
	// each path, no job.
	refused := "ALTER SCHEMA " + d + " SET OPTIONS (bogus_option_1581 = 'x')"
	_, err := c.Query(refused).Read(ctx)
	want := failedQueryError(t, "jobs.query of an unserved statement", err)
	if want.code != 501 || want.reason != "notImplemented" {
		t.Fatalf("jobs.query of an unserved statement: %+v, want 501 notImplemented", want)
	}
	_, err = c.Query(refused).Run(ctx)
	if got := failedQueryError(t, "jobs.insert of an unserved statement", err); got != want {
		t.Errorf("jobs.insert of an unserved statement: %+v, want %+v", got, want)
	}
}

// failedReasonCodes are the HTTP codes of the error reasons these queries
// give, from https://cloud.google.com/bigquery/docs/error-messages.
var failedReasonCodes = map[string][]int{
	"duplicate": {409}, "invalid": {400}, "invalidQuery": {400}, "jobInternalError": {400},
	"notFound": {404}, "notImplemented": {501},
}

type failedErr struct {
	code            int
	reason, message string
}

func (e failedErr) String() string { return fmt.Sprintf("%d %s %q", e.code, e.reason, e.message) }

// failedQueryError reads err as a Google API error with one reason.
func failedQueryError(t *testing.T, what string, err error) failedErr {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || len(ge.Errors) == 0 {
		t.Fatalf("%s: %v, want a Google API error with a reason", what, err)
	}
	return failedErr{code: ge.Code, reason: ge.Errors[0].Reason, message: ge.Message}
}

func hasCode(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
