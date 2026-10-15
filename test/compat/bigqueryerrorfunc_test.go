//go:build compat

package compat

import (
	"errors"
	"reflect"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryErrorFunctionMessage (#1230): ERROR('boom') fails the query
// with "boom", as BigQuery's ERROR function does
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/debugging_functions#error):
// in jobs.query's error, in a query job's status.errorResult and errors,
// and in a script's @@error.message. The engine had put "failed to scan
// rows: " before it.
func TestBigQueryErrorFunctionMessage(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()

	// jobs.query.
	_, err := scriptRows(ctx, c, "SELECT ERROR('boom')", false)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Message != "boom" {
		t.Errorf("jobs.query: %v, want the message boom", err)
	}

	// A query job: errorResult and errors.
	job, err := c.Query("SELECT ERROR('boom')").Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := job.Wait(ctx)
	if st == nil {
		if st, err = job.Status(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if e := st.Err(); e == nil || func() bool { var be *bigquery.Error; return !errors.As(e, &be) || be.Message != "boom" }() {
		t.Errorf("the job's errorResult: %v, want the message boom", e)
	}
	if len(st.Errors) == 0 || st.Errors[0].Message != "boom" {
		t.Errorf("the job's errors: %v, want boom", st.Errors)
	}

	// @@error.message in a handler, through jobs.query and a query job.
	for _, insert := range []bool{false, true} {
		got, err := scriptRows(ctx, c, "BEGIN\n  SELECT ERROR('boom');\nEXCEPTION WHEN ERROR THEN\n  SELECT @@error.message AS m;\nEND", insert)
		if err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{"boom"}}) {
			t.Errorf("@@error.message (insert %v): %v %v, want boom", insert, got, err)
		}
	}
}
