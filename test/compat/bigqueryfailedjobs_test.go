//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryFailedQueriesAreJobs (#1397): a query that fails is a job,
// as jobs.query's default jobCreationMode, JOB_CREATION_REQUIRED, makes
// it: N queries that succeed and N of each kind that fail (jobs.query of
// a missing table and of a syntax error, a query job of a missing table,
// a query job whose destination dataset does not exist) are all listed by
// jobs.list and INFORMATION_SCHEMA.JOBS, DONE, each failed one with its
// error. Measured first: the 2N failed jobs.query were missing from both,
// and the query job into a missing dataset was refused 404 with no job.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.list, bigquery.jobs.get
func TestBigQueryFailedQueriesAreJobs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	mark := fmt.Sprintf("m1397_%d", time.Now().UnixNano())
	start := time.Now().Add(-time.Second)
	const n = 3
	wantErr := map[string]string{} // the query's kind: what its error says
	for i := range n {
		// jobs.query (the client's fast path).
		if _, err := c.Query(fmt.Sprintf("SELECT %d AS ok /* %s ok */", i, mark)).Read(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Query(fmt.Sprintf("SELECT %d FROM %s.no_such_table_1397 /* %s missing */", i, ds.DatasetID, mark)).Read(ctx); err == nil {
			t.Fatal("a query of a missing table succeeded")
		}
		if _, err := c.Query(fmt.Sprintf("SELEC %d /* %s syntax */", i, mark)).Read(ctx); err == nil {
			t.Fatal("a query with a syntax error succeeded")
		}
		// jobs.insert.
		job, err := c.Query(fmt.Sprintf("SELECT %d FROM %s.no_such_table_1397 /* %s jobmissing */", i, ds.DatasetID, mark)).Run(ctx)
		if err != nil {
			t.Fatalf("query job of a missing table: %v, want a job that fails", err)
		}
		if st, err := job.Wait(ctx); err == nil && st.Err() == nil {
			t.Fatal("a query job of a missing table succeeded")
		}
		q := c.Query(fmt.Sprintf("SELECT %d AS a /* %s nodataset */", i, mark))
		q.Dst = c.Dataset("no_such_dataset_1397").Table("t")
		job, err = q.Run(ctx)
		if err != nil {
			t.Fatalf("query job into a missing dataset: %v, want a job that fails", err)
		}
		st, err := job.Wait(ctx)
		if err == nil && (st == nil || st.Err() == nil) {
			t.Fatal("a query job into a missing dataset succeeded")
		}
		got, err := job.Status(ctx)
		if err != nil || got.State != bigquery.Done || got.Err() == nil {
			t.Fatalf("jobs.get of the query job into a missing dataset: %+v, %v; want DONE and failed", got, err)
		}
	}
	wantErr["missing"], wantErr["jobmissing"] = "no_such_table_1397", "no_such_table_1397"
	wantErr["syntax"], wantErr["nodataset"] = "", "no_such_dataset_1397"

	kindOf := func(sql string) string {
		i := strings.Index(sql, mark+" ")
		if i < 0 {
			return ""
		}
		return strings.TrimSuffix(strings.Fields(sql[i+len(mark)+1:])[0], "*/")
	}
	// jobs.list.
	it := c.Jobs(ctx)
	it.AllUsers = true
	it.MinCreationTime = start
	count := map[string]int{}
	for {
		job, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := job.Config()
		if err != nil {
			t.Fatal(err)
		}
		qc, ok := cfg.(*bigquery.QueryConfig)
		if !ok {
			continue
		}
		kind := kindOf(qc.Q)
		if kind == "" {
			continue
		}
		count[kind]++
		st := job.LastStatus()
		if st.State != bigquery.Done {
			t.Errorf("job %s (%s): state %v, want DONE", job.ID(), kind, st.State)
		}
		if kind == "ok" {
			if st.Err() != nil {
				t.Errorf("job %s (%s): %v, want none", job.ID(), kind, st.Err())
			}
		} else if st.Err() == nil || !strings.Contains(st.Err().Error(), wantErr[kind]) {
			t.Errorf("job %s (%s): error %v, want one naming %q", job.ID(), kind, st.Err(), wantErr[kind])
		}
	}
	for _, kind := range []string{"ok", "missing", "syntax", "jobmissing", "nodataset"} {
		if count[kind] != n {
			t.Errorf("jobs.list listed %d %s jobs, want %d (all: %v)", count[kind], kind, n, count)
		}
	}

	// INFORMATION_SCHEMA.JOBS, unfiltered and filtered.
	for _, where := range []string{"", " WHERE creation_time >= TIMESTAMP_MILLIS(" + fmt.Sprint(start.UnixMilli()) + ")"} {
		qctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		rows, err := c.Query("SELECT query, state, error_result.message FROM `region-us`.INFORMATION_SCHEMA.JOBS" + where).Read(qctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		count := map[string]int{}
		for {
			var row []bigquery.Value
			err := rows.Next(&row)
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			sql, _ := row[0].(string)
			kind := kindOf(sql)
			if kind == "" || strings.Contains(sql, "INFORMATION_SCHEMA") {
				continue
			}
			count[kind]++
			msg, _ := row[2].(string)
			if row[1] != "DONE" || kind == "ok" && row[2] != nil || kind != "ok" && (msg == "" || !strings.Contains(msg, wantErr[kind])) {
				t.Errorf("JOBS%s: %s row %v, want DONE and its error", where, kind, row)
			}
		}
		cancel()
		for _, kind := range []string{"ok", "missing", "syntax", "jobmissing", "nodataset"} {
			if count[kind] != n {
				t.Errorf("JOBS%s: %d %s rows, want %d (all: %v)", where, count[kind], kind, n, count)
			}
		}
	}
}
