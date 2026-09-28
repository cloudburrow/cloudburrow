//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// expireQueryResultsPath is the BigQuery front's path that deletes query
// results at once (internal/bigqueryfront/resultsexpiry.go).
const expireQueryResultsPath = "/cloudburrow/bigquery-expire-query-results"

// TestBigQueryExpiredQueryResultsAreNotFound (#1059): a query job's result
// table, once deleted as BigQuery deletes it after about a day (here at
// once, through the front's path, for the job named), is answered 404
// notFound, at jobs.getQueryResults (Job.Read) and at tabledata.list of
// its destination (Table.Read), as BigQuery answers a table that does not
// exist; jobs.get still names the destination; a later job's result still
// reads. Measured first: after tables.delete of a job's result table,
// the emulator answered jobs.getQueryResults of the job with its rows,
// from the job it keeps, and tabledata.list of the table 404 notFound
// "table … is not found".
func TestBigQueryExpiredQueryResultsAreNotFound(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	run := func() *bigquery.Job {
		t.Helper()
		job, err := c.Query("SELECT 7 AS n").Run(ctx)
		if err != nil {
			t.Fatalf("query job: %v", err)
		}
		if _, err := job.Wait(ctx); err != nil {
			t.Fatalf("query job %s: %v", job.ID(), err)
		}
		return job
	}
	old, recent := run(), run()

	resp, err := http.Post(h.Endpoint(EnvBigQuery)+expireQueryResultsPath+"?job="+url.QueryEscape(old.ID()), "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", expireQueryResultsPath, err)
	}
	var got struct{ Deleted int }
	err = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || got.Deleted != 1 {
		t.Fatalf("POST %s: %d %+v %v, want 1 deleted", expireQueryResultsPath, resp.StatusCode, got, err)
	}

	notFound := func(what string, err error, text string) {
		t.Helper()
		var e *googleapi.Error
		if !errors.As(err, &e) || e.Code != 404 || len(e.Errors) == 0 || e.Errors[0].Reason != "notFound" ||
			!strings.Contains(e.Message, text) {
			t.Errorf("%s: %v, want 404 notFound %q", what, err, text)
		}
	}
	readErr := func(it *bigquery.RowIterator, err error) error {
		if err != nil {
			return err
		}
		var row []bigquery.Value
		if err := it.Next(&row); !errors.Is(err, iterator.Done) {
			return err
		}
		return nil
	}
	notFound("Job.Read of the expired job", readErr(old.Read(ctx)), "Not found: Table")
	cfg, err := old.Config()
	if err != nil {
		t.Fatalf("jobs.get of the expired job: %v", err)
	}
	dst := cfg.(*bigquery.QueryConfig).Dst
	if dst == nil || dst.DatasetID != queryResultsDataset || dst.TableID != old.ID() {
		t.Fatalf("jobs.get of the expired job: destination %+v", dst)
	}
	notFound("Table.Read of the expired job's destination", readErr(dst.Read(ctx), nil), "is not found") // the emulator's text

	it, err := recent.Read(ctx)
	if err != nil {
		t.Fatalf("Job.Read of the recent job: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || !reflect.DeepEqual(row, []bigquery.Value{int64(7)}) {
		t.Errorf("Job.Read of the recent job: %v %v, want [7]", row, err)
	}
}
