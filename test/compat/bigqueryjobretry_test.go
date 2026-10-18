//go:build compat

package compat

import (
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

// TestBigQueryInsertOfAnExistingJobIs409 (#1622): the official Go client
// retries a jobs.insert with its own job ID after a transient failure, and
// its first attempt may have made the job. BigQuery answers an insert of
// a job ID the project has with 409 duplicate and runs nothing
// (https://cloud.google.com/bigquery/docs/error-messages). Measured first:
// CloudBurrow ran the query again and failed 400 jobInternalError "failed
// to add job: job … is already created". The second Run of the ID is 409
// duplicate, the job read by its ID is the first one, and a DML statement
// sent twice under one ID ran once.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryInsertOfAnExistingJobIs409(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := "`" + ds.DatasetID + ".t`"
	if err := bqRun(ctx, c, "CREATE TABLE "+tbl+" (a INT64)", false); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	for _, sql := range []string{"SELECT 1622 AS n", "INSERT INTO " + tbl + " (a) VALUES (1622)"} {
		id := fmt.Sprintf("retried_1622_%d_%d", stamp, len(sql))
		q := c.Query(sql)
		q.JobID = id
		first, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("first Run of %s: %v", id, err)
		}
		if _, err := first.Wait(ctx); err != nil {
			t.Fatalf("first job %s: %v", id, err)
		}
		again := c.Query(sql)
		again.JobID = id
		_, err = again.Run(ctx)
		var e *googleapi.Error
		if !errors.As(err, &e) || e.Code != http.StatusConflict || len(e.Errors) == 0 || e.Errors[0].Reason != "duplicate" {
			t.Fatalf("second Run of %s: %v, want 409 duplicate", id, err)
		}
		if want := "Already Exists: Job " + project + ":"; !strings.HasPrefix(e.Message, want) || !strings.HasSuffix(e.Message, "."+id) {
			t.Errorf("second Run of %s: message %q, want %s<location>.%s", id, e.Message, want, id)
		}
		job, err := c.JobFromID(ctx, id)
		if err != nil {
			t.Fatalf("JobFromID %s: %v", id, err)
		}
		st, err := job.Status(ctx)
		if err != nil || st.State != bigquery.Done || st.Err() != nil {
			t.Fatalf("job %s: %+v %v, want DONE without an error", id, st, err)
		}
		cfg, err := job.Config()
		if err != nil {
			t.Fatal(err)
		}
		if qc, ok := cfg.(*bigquery.QueryConfig); !ok || qc.Q != sql {
			t.Errorf("job %s: configuration %+v, want the first insert's query %q", id, cfg, sql)
		}
	}
	it, err := c.Query("SELECT COUNT(*) FROM " + tbl + " WHERE a = 1622").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil && !errors.Is(err, iterator.Done) {
		t.Fatal(err)
	}
	if len(row) != 1 || row[0] != int64(1) {
		t.Errorf("rows the INSERT sent twice made: %v, want 1", row)
	}
}
