//go:build compat

package compat

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// slowQuery is a query that takes the engine some seconds: a COUNT of a
// cross join of three arrays, n^3 rows. Its answer is n^3.
func slowQuery(n int) string {
	return fmt.Sprintf("SELECT COUNT(*) AS n FROM UNNEST(GENERATE_ARRAY(1, %d)) a, UNNEST(GENERATE_ARRAY(1, %d)) b, UNNEST(GENERATE_ARRAY(1, %d)) c", n, n, n)
}

// slowN sizes slowQuery to run for several seconds on an instance.
const slowN = 380

// TestBigQueryAsyncJobs (#1819): a job outlives the request that started
// it, as on BigQuery.
//
//   - jobs.query with a short timeoutMs answers jobComplete false and the
//     jobReference of the job, which "will be present even if the original
//     request timed out"
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query);
//     jobs.get answers it RUNNING, jobs.getQueryResults with a short
//     timeoutMs answers jobComplete false, and polled, its rows
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/getQueryResults).
//   - jobs.insert, through the Go client's Query.Run, answers the job
//     RUNNING; Job.Wait polls it to DONE and Job.Read reads its rows.
//   - jobs.cancel "Requests that a job be cancelled. This call will return
//     immediately, and the client will need to poll for the job status"
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/cancel):
//     a cancelled query and a cancelled INSERT end DONE with reason
//     "stopped" ("This status code returns when a job is canceled",
//     https://cloud.google.com/bigquery/docs/error-messages), and the
//     INSERT wrote nothing.
//
// Measured first: jobs.query and jobs.insert answered only once the job
// was done, whatever timeoutMs said, and jobs.cancel of a job could only
// come after its end.
//
// While the query runs, INFORMATION_SCHEMA.JOBS and jobs.list list it
// RUNNING, at once: BigQuery runs jobs side by side (#1823). Measured
// first: the engine ran one statement at a time, and a JOBS query or a
// jobs.list sent while slowQuery ran waited for its end (10 to 21 s).
//
// covers: bigquery.jobs.query, bigquery.jobs.getQueryResults, bigquery.jobs.get, bigquery.jobs.insert, bigquery.jobs.cancel, bigquery.jobs.list
func TestBigQueryAsyncJobs(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := generatedBigQuery(t, h)
	ctx := h.Context()
	want := int64(slowN) * slowN * slowN

	// jobs.query, with a timeoutMs the query outlasts.
	start := time.Now()
	resp, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: slowQuery(slowN), UseLegacySql: new(bool), TimeoutMs: 300}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.query: %v", err)
	}
	if resp.JobComplete || resp.JobReference == nil || resp.JobReference.JobId == "" || resp.TotalRows != 0 || len(resp.Rows) != 0 {
		t.Fatalf("jobs.query of a slow query with timeoutMs 300: jobComplete %v, jobReference %+v, %d rows; want jobComplete false and the job",
			resp.JobComplete, resp.JobReference, len(resp.Rows))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("jobs.query with timeoutMs 300 answered after %v", d)
	}
	id, loc := resp.JobReference.JobId, resp.JobReference.Location
	job, err := svc.Jobs.Get(project, id).Location(loc).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.get %s: %v", id, err)
	}
	if job.Status == nil || job.Status.State != "RUNNING" {
		t.Fatalf("jobs.get of the running job: status %+v, want RUNNING", job.Status)
	}
	if job.Configuration == nil || job.Configuration.Query == nil || job.Configuration.Query.Query != slowQuery(slowN) {
		t.Errorf("jobs.get of the running job: configuration %+v, want its query", job.Configuration)
	}
	// INFORMATION_SCHEMA.JOBS and jobs.list, while it runs (#1823): each
	// answers before the query ends, and lists it RUNNING.
	listStart := time.Now()
	jit, err := c.Query("SELECT state FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id = '" + id + "'").Read(ctx)
	if err != nil {
		t.Fatalf("INFORMATION_SCHEMA.JOBS while the job runs: %v", err)
	}
	var state []bigquery.Value
	if err := jit.Next(&state); err != nil || fmt.Sprint(state) != "[RUNNING]" {
		t.Errorf("INFORMATION_SCHEMA.JOBS of the running job: %v, %v; want [RUNNING]", state, err)
	}
	listed, err := svc.Jobs.List(project).StateFilter("running").Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.list while the job runs: %v", err)
	}
	found := false
	for _, lj := range listed.Jobs {
		if lj.JobReference != nil && lj.JobReference.JobId == id {
			found = lj.State == "RUNNING"
		}
	}
	if !found {
		t.Errorf("jobs.list stateFilter running: %d jobs, want job %s RUNNING", len(listed.Jobs), id)
	}
	if job, err := svc.Jobs.Get(project, id).Location(loc).Context(ctx).Do(); err != nil || job.Status == nil || job.Status.State != "RUNNING" {
		t.Errorf("the job had ended before INFORMATION_SCHEMA.JOBS and jobs.list answered (%v): %+v, %v; want it still RUNNING", time.Since(listStart), job, err)
	}
	res, err := svc.Jobs.GetQueryResults(project, id).Location(loc).TimeoutMs(100).Context(ctx).Do()
	if err != nil {
		t.Fatalf("getQueryResults: %v", err)
	}
	if res.JobComplete || res.TotalRows != 0 {
		t.Fatalf("getQueryResults with timeoutMs 100 of the running job: jobComplete %v, totalRows %d; want false", res.JobComplete, res.TotalRows)
	}
	for polls := 0; !res.JobComplete; polls++ {
		if polls > 30 {
			t.Fatal("the job never completed")
		}
		if res, err = svc.Jobs.GetQueryResults(project, id).Location(loc).TimeoutMs(10000).Context(ctx).Do(); err != nil {
			t.Fatalf("getQueryResults: %v", err)
		}
	}
	if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0].F[0].V) != fmt.Sprint(want) || res.JobReference.JobId != id {
		t.Fatalf("getQueryResults once complete: job %s, rows %+v; want %d of job %s", res.JobReference.JobId, res.Rows, want, id)
	}
	if job, err = svc.Jobs.Get(project, id).Location(loc).Context(ctx).Do(); err != nil || job.Status.State != "DONE" || job.Status.ErrorResult != nil {
		t.Fatalf("jobs.get of the ended job: %+v, %v; want DONE", job, err)
	}

	// jobs.insert, through the Go client.
	j, err := c.Query(slowQuery(slowN)).Run(ctx)
	if err != nil {
		t.Fatalf("Query.Run: %v", err)
	}
	if st := j.LastStatus(); st == nil || st.State != bigquery.Running {
		t.Fatalf("Query.Run of a slow query: status %+v, want RUNNING", st)
	}
	if st, err := j.Status(ctx); err != nil || st.State != bigquery.Running {
		t.Fatalf("Job.Status of the running job: %+v, %v", st, err)
	}
	if st, err := j.Wait(ctx); err != nil || st.Err() != nil || !st.Done() {
		t.Fatalf("Job.Wait: %+v, %v", st, err)
	}
	rit, err := j.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row []bigquery.Value
	if err := rit.Next(&row); err != nil || fmt.Sprint(row) != fmt.Sprintf("[%d]", want) {
		t.Fatalf("Job.Read: %v, %v; want [%d]", row, err, want)
	}

	// jobs.cancel of a running query, and of a running INSERT.
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_async")
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	table := "`" + project + "." + ds.DatasetID + ".counts`"
	if err := bqRun(ctx, c, "CREATE TABLE "+table+" (n INT64)", false); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{slowQuery(slowN), "INSERT INTO " + table + " (n) " + slowQuery(slowN)} {
		j, err := c.Query(sql).Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if st := j.LastStatus(); st == nil || st.State != bigquery.Running {
			t.Fatalf("%s: status %+v, want RUNNING", sql, st)
		}
		if err := j.Cancel(ctx); err != nil {
			t.Fatalf("Job.Cancel: %v", err)
		}
		// Job.Wait of a query reads jobs.getQueryResults, which answers a
		// failed job with its error; Job.Status reads the job.
		if _, err := j.Wait(ctx); err != nil {
			var ge *googleapi.Error
			if !errors.As(err, &ge) || len(ge.Errors) == 0 || ge.Errors[0].Reason != "stopped" {
				t.Fatalf("Job.Wait after Cancel: %v, want the job's error, stopped", err)
			}
		}
		st, err := j.Status(ctx)
		if err != nil {
			t.Fatalf("Job.Status after Cancel: %v", err)
		}
		var e *bigquery.Error
		if !st.Done() || !errors.As(st.Err(), &e) || e.Reason != "stopped" {
			t.Fatalf("%s cancelled: state %v, %v; want DONE, reason stopped", sql, st.State, st.Err())
		}
	}
	it, err := c.Query("SELECT COUNT(*) FROM " + table).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n []bigquery.Value
	if err := it.Next(&n); err != nil && !errors.Is(err, iterator.Done) || fmt.Sprint(n) != "[0]" {
		t.Errorf("rows the cancelled INSERT wrote: %v, %v; want none", n, err)
	}
}
