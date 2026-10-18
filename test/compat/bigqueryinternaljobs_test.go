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
	"google.golang.org/api/iterator"
)

// TestBigQueryFrontsOwnJobsStayUnlisted (#1555): the queries CloudBurrow's
// front runs itself (a DELETE's checks, the change history's bounds, a
// table kept on deletion, the region views', the table of
// INFORMATION_SCHEMA.JOBS rows) are no client's jobs, and jobs.list and
// INFORMATION_SCHEMA.JOBS and JOBS_BY_PROJECT list only the client's, before
// and after a restart of the front. Measured first: the front knew its own
// jobs in memory alone, and after a restart listed each as the client's
// (1,306 jobs before TestBigQueryStorageWriteStreamsOutliveAFrontRestart
// restarted it, 3,684 after, among them 290 SELECT
// MIN(googlesqlite_change_micros) and 257 CREATE TABLE of the deleted
// tables' copies). A client's jobs.insert of a job ID the front names its
// own with is refused.
//
// covers: bigquery.jobs.list, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryFrontsOwnJobsStayUnlisted(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	// Since the test began on the instance's clock, which a test before it
	// may have moved on (advanceBigQueryClock), and with it the creation
	// times of that test's jobs (#1629).
	start := advanceBigQueryClock(t, h, 0).Add(-2 * time.Second)
	mine := map[string]bool{}
	run := func(sql string) {
		t.Helper()
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := c.Query(sql).Run(qctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		mine[job.ID()] = true
		st, err := job.Wait(qctx)
		if err != nil || st.Err() != nil {
			t.Fatalf("%s: %v %v", sql, err, st.Err())
		}
	}
	read := func(sql string) [][]bigquery.Value {
		t.Helper()
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		it, err := c.Query(sql).Read(qctx) // jobs.query
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if it.SourceJob() != nil {
			mine[it.SourceJob().ID()] = true
		}
		var rows [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return rows
			}
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			rows = append(rows, row)
		}
	}
	tbl := "`" + ds.DatasetID + ".t`"
	run("CREATE TABLE " + tbl + " (a INT64, b STRING) OPTIONS(enable_change_history=true)")
	run("INSERT INTO " + tbl + " (a, b) VALUES (1, 'x'), (2, 'y'), (3, 'z')")
	run("DELETE FROM " + tbl + " WHERE a = 2")
	run("UPDATE " + tbl + " SET b = 'w' WHERE a = 3")
	read("SELECT a, _CHANGE_TYPE FROM CHANGES(TABLE " + tbl + ", NULL, NULL)")
	read("SELECT a FROM APPENDS(TABLE " + tbl + ", NULL, NULL)")
	run("CREATE TABLE `" + ds.DatasetID + ".gone` AS SELECT 1 AS a")
	run("DROP TABLE `" + ds.DatasetID + ".gone`") // kept as a copy, for undelete
	read("SELECT table_name FROM `region-us`.INFORMATION_SCHEMA.TABLES WHERE table_schema = '" + ds.DatasetID + "'")
	read("SELECT schema_name FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA")

	// A job not the client's, of this test's dataset or of the front's
	// own statements, listed since the test began.
	theFronts := func(id, text string) bool {
		if mine[id] {
			return false
		}
		return strings.HasPrefix(id, "_cloudburrow_") || strings.Contains(text, ds.DatasetID) ||
			strings.Contains(text, "googlesqlite_change") || strings.Contains(text, "_cloudburrow_dropped")
	}
	check := func(when string) {
		t.Helper()
		it := c.Jobs(ctx)
		it.AllUsers = true
		it.MinCreationTime = start
		listed := map[string]bool{}
		for {
			job, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("%s: jobs.list: %v", when, err)
			}
			listed[job.ID()] = true
			text := ""
			if cfg, err := job.Config(); err == nil {
				if q, ok := cfg.(*bigquery.QueryConfig); ok {
					text = q.Q
				}
			}
			if theFronts(job.ID(), text) {
				t.Errorf("%s: jobs.list lists the front's own job %s: %.200s", when, job.ID(), text)
			}
		}
		for id := range mine {
			if !listed[id] {
				t.Errorf("%s: jobs.list does not list the client's job %s", when, id)
			}
		}
		for _, view := range []string{"`region-us`.INFORMATION_SCHEMA.JOBS", "`" + project + "`.`region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT"} {
			before := map[string]bool{}
			for id := range mine {
				before[id] = true
			}
			rows := read(fmt.Sprintf("SELECT job_id, IFNULL(query, ''), IFNULL(parent_job_id, '') FROM %s WHERE creation_time >= TIMESTAMP_MILLIS(%d)", view, start.UnixMilli()))
			seen := map[string]bool{}
			for _, r := range rows {
				id, _ := r[0].(string)
				text, _ := r[1].(string)
				parent, _ := r[2].(string)
				seen[id] = true
				if !mine[parent] && theFronts(id, text) { // a child of a script of the client's is the client's
					t.Errorf("%s: %s has the front's own job %s: %.200s", when, view, id, text)
				}
			}
			for id := range before {
				if !seen[id] {
					t.Errorf("%s: %s has no row of the client's job %s", when, view, id)
				}
			}
		}
	}
	check("before a restart of the front")

	// A client may not name its job as the front's.
	q := c.Query("SELECT 1")
	q.JobID = "_cloudburrow_" + strings.ReplaceAll(h.Project(), "-", "_")
	if _, err := q.Run(ctx); err == nil {
		t.Error("jobs.insert of a job ID beginning _cloudburrow_: accepted, want 400")
	} else if e := (*googleapi.Error)(nil); !errors.As(err, &e) || e.Code != http.StatusBadRequest {
		t.Errorf("jobs.insert of a job ID beginning _cloudburrow_: %v, want 400", err)
	}

	restartFront(t, "bigquery", "bigquery")
	// A deadline of its own (#1632): the restart waits for the kubelet,
	// whose back-off grows with each restart of the front's container
	// (40 s at its fourth, measured), which the test's took in too.
	ctx = h.Context()
	check("after a restart of the front")
}
