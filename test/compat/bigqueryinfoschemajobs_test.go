//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryInformationSchemaJobs (#1275): `region-us`.INFORMATION_SCHEMA.JOBS
// (and JOBS_BY_PROJECT, JOBS_BY_USER), through jobs.query and jobs.insert,
// has BigQuery's columns and a row for each job the project ran: a query
// job with labels and its client's text, a DML job with its row counts, a
// failed query with its error_result. Another region is 404;
// JOBS_BY_ORGANIZATION 501. Measured first: the engine failed it 400
// "Table not found".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInformationSchemaJobs(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := "`" + ds.DatasetID + ".t`"
	if err := bqRun(ctx, c, "CREATE TABLE "+tbl+" (a INT64)", false); err != nil {
		t.Fatal(err)
	}
	run := func(sql string, labels map[string]string) *bigquery.Job {
		t.Helper()
		q := c.Query(sql)
		q.Labels = labels
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := job.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		return job
	}
	queryJob := run("SELECT 1275 AS n", map[string]string{"issue": "jobs"})
	dmlJob := run("INSERT INTO "+tbl+" (a) VALUES (1), (2)", nil)
	failed := c.Query("SELECT * FROM " + ds.DatasetID + ".no_such_table_1275")
	failedJob, err := failed.Run(ctx)
	var failedID string
	if err == nil {
		failedID = failedJob.ID()
		_, _ = failedJob.Wait(ctx)
	}

	read := func(sql string, insert bool) (bigquery.Schema, [][]bigquery.Value, error) {
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		q := c.Query(sql)
		var it *bigquery.RowIterator
		var err error
		if insert {
			job, err := q.Run(qctx)
			if err != nil {
				return nil, nil, err
			}
			if it, err = job.Read(qctx); err != nil {
				return nil, nil, err
			}
		} else if it, err = q.Read(qctx); err != nil {
			return nil, nil, err
		}
		var rows [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return it.Schema, rows, nil
			}
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, row)
		}
	}
	ids := "'" + queryJob.ID() + "', '" + dmlJob.ID() + "', '" + failedID + "'"
	for _, insert := range []bool{false, true} {
		schema, _, err := read("SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id = '"+queryJob.ID()+"'", insert)
		if err != nil {
			t.Fatalf("insert=%v: %v", insert, err)
		}
		types := map[string]string{}
		for _, f := range schema {
			types[f.Name] = string(f.Type)
		}
		for name, typ := range map[string]string{"job_id": "STRING", "creation_time": "TIMESTAMP", "start_time": "TIMESTAMP",
			"end_time": "TIMESTAMP", "job_type": "STRING", "statement_type": "STRING", "state": "STRING", "query": "STRING",
			"error_result": "RECORD", "total_bytes_processed": "INTEGER", "total_bytes_billed": "INTEGER", "project_id": "STRING",
			"labels": "RECORD", "dml_statistics": "RECORD", "destination_table": "RECORD", "referenced_tables": "RECORD",
			"cache_hit": "BOOLEAN", "priority": "STRING", "parent_job_id": "STRING", "user_email": "STRING"} {
			if types[name] != typ {
				t.Errorf("insert=%v: column %s is %q, want %s", insert, name, types[name], typ)
			}
		}
		_, rows, err := read("SELECT job_id, job_type, statement_type, state, query, project_id, "+
			"(SELECT value FROM UNNEST(labels) WHERE key = 'issue'), dml_statistics.inserted_row_count, "+
			"error_result.reason, creation_time "+
			"FROM `"+project+"`.`region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT j WHERE j.job_id IN ("+ids+")", insert)
		if err != nil {
			t.Fatalf("insert=%v: %v", insert, err)
		}
		byID := map[string][]bigquery.Value{}
		for _, r := range rows {
			byID[r[0].(string)] = r
		}
		if r := byID[queryJob.ID()]; r == nil || r[1] != "QUERY" || r[2] != "SELECT" || r[3] != "DONE" || r[4] != "SELECT 1275 AS n" ||
			r[5] != project || r[6] != "jobs" {
			t.Errorf("insert=%v: the query job's row %v", insert, r)
		} else if ct, ok := r[9].(time.Time); !ok || advanceBigQueryClock(t, h, 0).Sub(ct) > time.Hour || advanceBigQueryClock(t, h, 0).Sub(ct) < -time.Minute { // #1487
			t.Errorf("insert=%v: creation_time %v", insert, r[9])
		}
		if r := byID[dmlJob.ID()]; r == nil || r[2] != "INSERT" || r[7] != int64(2) {
			t.Errorf("insert=%v: the DML job's row %v", insert, r)
		}
		if failedID != "" {
			if r := byID[failedID]; r == nil || r[3] != "DONE" || r[8] == nil {
				t.Errorf("insert=%v: the failed job's row %v", insert, r)
			}
		}
		_, rows, err = read("SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_USER WHERE job_id IN ("+ids+")", insert)
		if err != nil || len(rows) != 1 || rows[0][0].(int64) < 2 {
			t.Errorf("insert=%v: JOBS_BY_USER gave %v, %v", insert, rows, err)
		}
		var e *googleapi.Error
		// A query that names US: one naming no location runs in the
		// region it reads (#1375).
		eu := c.Query("SELECT * FROM `region-eu`.INFORMATION_SCHEMA.JOBS")
		eu.Location = "US"
		if insert {
			var job *bigquery.Job
			if job, err = eu.Run(ctx); err == nil {
				_, err = job.Read(ctx)
			}
		} else {
			_, err = eu.Read(ctx)
		}
		if !errors.As(err, &e) || e.Code != http.StatusNotFound || !strings.Contains(e.Message, "region-eu") {
			t.Errorf("insert=%v: another region gave %v, want 404", insert, err)
		}
		// Built (#1392, TestBigQueryJobsByFolderAndOrganization): the
		// project's jobs, or 400 when it is in no organization.
		_, _, err = read("SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_ORGANIZATION", insert)
		if err != nil && (!errors.As(err, &e) || e.Code != http.StatusBadRequest) {
			t.Errorf("insert=%v: JOBS_BY_ORGANIZATION gave %v, want rows or 400", insert, err)
		}
	}
}
