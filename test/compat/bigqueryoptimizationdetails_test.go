//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryJobsOptimizationDetails (#1458): region JOBS and
// JOBS_BY_PROJECT have query_info.optimization_details, NULL as
// "If no history-based optimizations were applied, NULL is produced for
// optimization_details" (history-based-optimizations, read 2026-11-18)
// and the emulator applies none; the page's own estimation query,
// ARRAY_LENGTH(JSON_QUERY_ARRAY(query_info.optimization_details.optimizations)),
// runs and reads NULL; a SELECT * of JOBS_BY_PROJECT decodes; and
// JOBS_BY_USER has no such field, as "Only the JOBS_BY_PROJECT view has
// this column" (information-schema-jobs, read 2026-11-18). Before #1458's
// change every JOBS view's query_info had no such field, as JOBS_BY_USER's
// still has none.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryJobsOptimizationDetails(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	if err := bqRun(ctx, c, "CREATE TABLE "+d+".od (n INT64)", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	j, err := c.Query("SELECT n FROM " + d + ".od WHERE n > 1").Run(qctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if st, err := j.Wait(qctx); err != nil || st.Err() != nil {
		t.Fatalf("query: %v, %v", err, st)
	}
	id := j.ID()
	for _, insert := range []bool{false, true} {
		for _, view := range []string{"JOBS", "JOBS_BY_PROJECT"} {
			_, rows, err := infoSchemaRead(ctx, c, "SELECT query_info IS NULL, query_info.query_hashes.normalized_literals IS NULL, "+
				"query_info.optimization_details IS NULL, "+
				"ARRAY_LENGTH(JSON_QUERY_ARRAY(query_info.optimization_details.optimizations)) "+
				"FROM `region-us`.INFORMATION_SCHEMA."+view+" WHERE job_id = '"+id+"'", insert)
			if err != nil {
				t.Fatalf("insert=%v: %s: %v", insert, view, err)
			}
			want := []bigquery.Value{false, false, true, nil}
			if len(rows) != 1 || len(rows[0]) != len(want) {
				t.Fatalf("insert=%v: %s: %v", insert, view, rows)
			}
			for i, w := range want {
				if rows[0][i] != w {
					t.Errorf("insert=%v: %s column %d: %v, want %v", insert, view, i, rows[0][i], w)
				}
			}
		}
		schema, rows, err := infoSchemaRead(ctx, c, "SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT WHERE job_id = '"+id+"'", insert)
		if err != nil || len(rows) != 1 {
			t.Fatalf("insert=%v: SELECT * of JOBS_BY_PROJECT: %v, %v", insert, rows, err)
		}
		found := false
		for _, f := range schema {
			if f.Name != "query_info" {
				continue
			}
			for _, g := range f.Schema {
				if g.Name == "optimization_details" {
					found = true
					if g.Type != bigquery.JSONFieldType {
						t.Errorf("insert=%v: optimization_details is %s, want JSON", insert, g.Type)
					}
				}
			}
		}
		if !found {
			t.Errorf("insert=%v: SELECT * of JOBS_BY_PROJECT: query_info has no optimization_details", insert)
		}
		_, _, err = infoSchemaRead(ctx, c, "SELECT query_info.optimization_details FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_USER WHERE job_id = '"+id+"'", insert)
		if err == nil || !strings.Contains(err.Error(), "optimization_details") {
			t.Errorf("insert=%v: JOBS_BY_USER optimization_details: %v, want a 400 naming the field", insert, err)
		}
	}
}
