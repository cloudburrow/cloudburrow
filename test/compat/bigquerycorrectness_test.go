//go:build compat

package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// The tests in this file are #931, #932, #934 and #937, each driven
// through the official Go client and measured against the pinned emulator
// first (the measurements are in internal/bigqueryfront and
// docs/compatibility.md).

// TestBigQueryCSVLoadWithSchemaLoadsEveryRow (#931): a CSV load with a
// schema, or into a table that has one, reads every row as data unless
// skipLeadingRows says otherwise, as BigQuery documents ("The default
// value is 0"), and puts each value in the column at its position.
// Measured first: the emulator took the first row as a header whatever
// skipLeadingRows said, so a file of three rows loaded two and a file of
// one row loaded none, with the job reported done and no error; and it
// failed skipLeadingRows 2. A resumable upload is loaded the same way. A
// load from Cloud Storage is read by the front (#944).
func TestBigQueryCSVLoadWithSchemaLoadsEveryRow(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	schema := bigquery.Schema{
		{Name: "a", Type: bigquery.IntegerFieldType},
		{Name: "b", Type: bigquery.StringFieldType},
	}
	load := func(table, data string, set func(*bigquery.ReaderSource), opts ...googleapi.MediaOption) error {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.Schema = schema
		if set != nil {
			set(src)
		}
		l := ds.Table(table).LoaderFrom(src)
		l.MediaOptions = opts
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := l.Run(lctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(lctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	want := func(table string, wantRows ...[]bigquery.Value) {
		t.Helper()
		got := rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+"."+table+"` ORDER BY a")
		if !reflect.DeepEqual(got, wantRows) {
			t.Errorf("%s holds %v, want %v", table, got, wantRows)
		}
	}

	if err := load("three", "1,x\n2,y\n3,z\n", nil); err != nil {
		t.Fatalf("a load of three rows with a schema and no header: %v", err)
	}
	want("three", []bigquery.Value{int64(1), "x"}, []bigquery.Value{int64(2), "y"}, []bigquery.Value{int64(3), "z"})

	if err := load("one", "7,only", nil); err != nil {
		t.Fatalf("a load of one row: %v", err)
	}
	want("one", []bigquery.Value{int64(7), "only"})

	// skipLeadingRows 1 skips a header whose names are the columns' in
	// another order: the values still go by position.
	if err := load("header", "b,a\n1,x\n2,y\n", func(s *bigquery.ReaderSource) { s.SkipLeadingRows = 1 }); err != nil {
		t.Fatalf("a load with skipLeadingRows 1: %v", err)
	}
	want("header", []bigquery.Value{int64(1), "x"}, []bigquery.Value{int64(2), "y"})

	if err := load("two", "title\n\"a note, over\ntwo lines\"\n1,x\n", func(s *bigquery.ReaderSource) { s.SkipLeadingRows = 2 }); err != nil {
		t.Fatalf("a load with skipLeadingRows 2: %v", err)
	}
	want("two", []bigquery.Value{int64(1), "x"})

	// Into an existing table, with no schema in the load.
	existing := ds.Table("existing")
	if err := existing.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	src := bigquery.NewReaderSource(strings.NewReader("4,p\n5,q\n"))
	job, err := existing.LoaderFrom(src).Run(ctx)
	if err != nil {
		t.Fatalf("a load into an existing table: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("a load into an existing table: %v %v", err, st)
	}
	want("existing", []bigquery.Value{int64(4), "p"}, []bigquery.Value{int64(5), "q"})

	// A resumable upload (256 KiB chunks), whose first row is row 0.
	var b bytes.Buffer
	n := 0
	for b.Len() < 3*googleapi.MinUploadChunkSize {
		fmt.Fprintf(&b, "%d,row %d\n", n, n)
		n++
	}
	if err := load("resumable", b.String(), nil, googleapi.ChunkSize(googleapi.MinUploadChunkSize)); err != nil {
		t.Fatalf("a resumable upload: %v", err)
	}
	if got := countRows(t, h, ds.Table("resumable")); got != n {
		t.Errorf("the resumable upload loaded %d rows, want %d", got, n)
	}
	if got := rows(t, ctx, c, "SELECT b FROM `"+ds.DatasetID+".resumable` WHERE a = 0"); len(got) != 1 {
		t.Errorf("the resumable upload's first row: %v", got)
	}

	// From Cloud Storage, the front reads the objects itself (#944,
	// TestBigQueryCSVLoadFromCloudStorage): one that is not there is 404
	// before anything is loaded.
	ref := bigquery.NewGCSReference("gs://" + strings.ReplaceAll(h.Project(), "_", "-") + "-none/data.csv")
	ref.Schema = schema
	_, err = ds.Table("from_gcs").LoaderFrom(ref).Run(ctx)
	wantReason(t, "a load from a bucket that does not exist", err, http.StatusNotFound, "notFound")
	if tableExists(t, ctx, ds.Table("from_gcs")) {
		t.Error("the refused load from Cloud Storage made its table")
	}
}

// TestBigQueryFailedJobsReadBackFailed (#934): a job that failed reads
// back failed through jobs.get (Job.Status, Client.JobFromID) and jobs.list
// (Client.Jobs), as BigQuery keeps its errorResult. Measured first: the
// emulator answered a failing query job's jobs.insert with errorResult
// "Table not found", then answered jobs.get of it with state DONE and no
// error; and answered a failing load 400, then jobs.get of it the same
// way, and listed it with no status.
func TestBigQueryFailedJobsReadBackFailed(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	failed := func(what string, st *bigquery.JobStatus) {
		t.Helper()
		if st == nil || st.State != bigquery.Done || st.Err() == nil {
			t.Errorf("%s: %+v, want done with an error", what, st)
		}
	}

	job, jerr := jobError(t, ctx, c, "SELECT * FROM nope_"+ds.DatasetID+".nope")
	if jerr == nil {
		t.Fatal("a query of a table that does not exist succeeded")
	}
	st, err := job.Status(ctx)
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	failed("jobs.get of the failed query job", st)
	again, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatalf("JobFromID: %v", err)
	}
	failed("JobFromID of the failed query job", again.LastStatus())
	if st, ok := listedStatus(t, ctx, c, job.ID()); !ok {
		t.Error("jobs.list does not list the failed query job")
	} else {
		failed("jobs.list of the failed query job", st)
	}

	// A load the emulator answers 400: a row with more values than the
	// schema has columns.
	src := bigquery.NewReaderSource(strings.NewReader("1,x\n2,y,extra\n"))
	src.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}, {Name: "b", Type: bigquery.StringFieldType}}
	l := ds.Table("bad_load").LoaderFrom(src)
	l.JobID = "bad_load_" + ds.DatasetID
	if lj, err := l.Run(ctx); err == nil {
		if st, err := lj.Wait(ctx); err == nil && st.Err() == nil {
			t.Fatal("a load with a row of three values into two columns succeeded")
		}
	}
	lj, err := c.JobFromID(ctx, l.JobID)
	if err != nil {
		t.Fatalf("JobFromID of the failed load: %v", err)
	}
	failed("JobFromID of the failed load", lj.LastStatus())
	if st, ok := listedStatus(t, ctx, c, l.JobID); !ok {
		t.Error("jobs.list does not list the failed load")
	} else {
		failed("jobs.list of the failed load", st)
	}

	// A job that succeeds still reads back succeeded.
	ok, jerr := jobError(t, ctx, c, "SELECT 1")
	if jerr != nil {
		t.Fatalf("SELECT 1: %v", jerr)
	}
	if st, err := ok.Status(ctx); err != nil || st.Err() != nil {
		t.Errorf("jobs.get of a job that succeeded: %v %v", err, st)
	}
}

// TestBigQueryCreateIfNotExistsOfAnExistingOne (#932): CREATE TABLE and
// CREATE VIEW ... IF NOT EXISTS of a table or view that exists succeed and
// change nothing, through jobs.query and jobs.insert, alone and in a
// script, as BigQuery documents ("the CREATE statement has no effect").
// Measured first: the emulator failed every form, 400 "table is already
// created" or "SQL logic error: table ... already exists", and a script
// with one failed as a whole. One that does not exist is created.
func TestBigQueryCreateIfNotExistsOfAnExistingOne(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + path("t") + " AS SELECT 1 AS a",
		"CREATE VIEW " + path("v") + " AS SELECT a FROM " + path("t"),
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, insert := range []bool{false, true} {
		for _, sql := range []string{
			"CREATE TABLE IF NOT EXISTS " + path("t") + " AS SELECT 2 AS a",
			"CREATE TABLE IF NOT EXISTS " + path("t") + " (z STRING)",
			"CREATE VIEW IF NOT EXISTS " + path("v") + " AS SELECT 5 AS a",
			"CREATE TABLE IF NOT EXISTS " + path("v") + " (z STRING)",
			"CREATE TABLE IF NOT EXISTS " + path("t") + " (z STRING); CREATE TABLE IF NOT EXISTS " + path("t") + " AS SELECT 3 AS a",
		} {
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Errorf("%s (jobs.insert %v): %v", sql, insert, err)
			}
		}
	}
	for table, want := range map[string]string{"t": "[[1]]", "v": "[[1]]"} {
		if got := fmt.Sprint(rows(t, ctx, c, "SELECT * FROM "+path(table))); got != want {
			t.Errorf("%s holds %s after IF NOT EXISTS, want %s", table, got, want)
		}
	}

	// A script whose earlier statement makes the table, and later ones
	// that must still run.
	script := "CREATE TABLE " + path("s") + " (a INT64); CREATE TABLE IF NOT EXISTS " + path("s") + " AS SELECT 9 AS a; " +
		"CREATE TABLE " + path("after") + " AS SELECT 4 AS a"
	if err := bqRun(ctx, c, script, false); err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	if !tableExists(t, ctx, ds.Table("after")) {
		t.Error("the statement after the IF NOT EXISTS was not run")
	}
	if got := countRows(t, h, ds.Table("s")); got != 0 {
		t.Errorf("s holds %d rows, want 0", got)
	}

	// One that does not exist is made.
	if err := bqRun(ctx, c, "CREATE TABLE IF NOT EXISTS "+path("fresh")+" AS SELECT 6 AS a", true); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT * FROM "+path("fresh"))); got != "[[6]]" {
		t.Errorf("fresh holds %s, want [[6]]", got)
	}
}

// TestBigQueryCreateTableLikeCopyCloneNotImplemented (#937): CREATE TABLE
// ... LIKE, COPY and CLONE and CREATE SNAPSHOT TABLE are 501
// notImplemented, naming the statement, through jobs.query and
// jobs.insert, and make nothing. Measured first: the emulator answered
// each 400 "CREATE TABLE LIKE is not supported" (COPY, CLONE, with or
// without OR REPLACE) or "Statement not supported:
// CreateSnapshotTableStatement".
func TestBigQueryCreateTableLikeCopyCloneNotImplemented(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("src")+" AS SELECT 1 AS a", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		for table, sql := range map[string]string{
			"like":  "CREATE TABLE " + path("like") + " LIKE " + path("src"),
			"copy":  "CREATE TABLE " + path("copy") + " COPY " + path("src"),
			"clone": "CREATE OR REPLACE TABLE " + path("clone") + " CLONE " + path("src"),
			"snap":  "CREATE SNAPSHOT TABLE " + path("snap") + " CLONE " + path("src"),
		} {
			err := bqRun(ctx, c, sql, insert)
			wantReason(t, sql, err, http.StatusNotImplemented, "notImplemented")
			var e *googleapi.Error
			if errors.As(err, &e) && !strings.Contains(e.Message, "Not implemented here: ") {
				t.Errorf("%s: %q does not name what is not implemented", sql, e.Message)
			}
			if tableExists(t, ctx, ds.Table(table)) {
				t.Errorf("the refused %s made %s", sql, table)
			}
		}
	}
}
