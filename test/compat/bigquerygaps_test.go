//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// The tests in this file are #916, #917, #918 and #919: what #901 left of
// BigQuery's DDL, scripts and autodetect loads, each driven through the
// official Go client and measured against the pinned emulator first (the
// measurements are in internal/bigqueryfront and docs/compatibility.md).

// wantReason checks that err is a googleapi error with the given status
// and reason.
func wantReason(t *testing.T, what string, err error, code int, reason string) {
	t.Helper()
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != code || len(e.Errors) != 1 || e.Errors[0].Reason != reason {
		t.Errorf("%s returned %v, want HTTP %d %s", what, err, code, reason)
	}
}

// jobError runs sql as a query job (jobs.insert) and returns the job and
// the error its status reports.
func jobError(t *testing.T, ctx context.Context, c *bigquery.Client, sql string) (*bigquery.Job, *bigquery.Error) {
	t.Helper()
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	job, err := c.Query(sql).Run(qctx)
	if err != nil {
		t.Fatalf("%s: jobs.insert: %v", sql, err)
	}
	st, err := job.Wait(qctx)
	if err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) {
			return job, &bigquery.Error{Reason: ge.Errors[0].Reason, Message: ge.Message}
		}
		t.Fatalf("%s: Wait: %v", sql, err)
	}
	var be *bigquery.Error
	if !errors.As(st.Err(), &be) {
		return job, nil
	}
	return job, be
}

// listedStatus returns the status jobs.list gives the job with the ID.
func listedStatus(t *testing.T, ctx context.Context, c *bigquery.Client, id string) (*bigquery.JobStatus, bool) {
	t.Helper()
	it := c.Jobs(ctx)
	it.AllUsers = true
	for {
		j, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil, false
		}
		if err != nil {
			t.Fatalf("jobs.list: %v", err)
		}
		if j.ID() == id {
			return j.LastStatus(), true
		}
	}
}

func rows(t *testing.T, ctx context.Context, c *bigquery.Client, sql string) [][]bigquery.Value {
	t.Helper()
	it, err := c.Query(sql).Read(ctx)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var out [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out = append(out, row)
	}
}

// TestBigQueryViewNamesAndColumns (#916): a view's ID is held to the
// table ID rule and the columns its query gives to the column rules, 400
// invalidQuery through jobs.query and jobs.insert, and 400 invalid through
// tables.insert (Table.Create with a ViewQuery); nothing is made. Measured
// first: the emulator made CREATE VIEW ds.`v!` and a view with a column
// `x!`, and failed a CREATE OR REPLACE VIEW of an existing view, 400,
// leaving it unreadable. A view with names BigQuery takes is made and
// read, and CREATE OR REPLACE VIEW replaces an existing one. CREATE
// MATERIALIZED VIEW and a view's column list, which the emulator does not
// support (400 "not supported", measured), are 501.
func TestBigQueryViewNamesAndColumns(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("base")+" AS SELECT 1 AS a, 'x' AS b", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		for view, sql := range map[string]string{
			"v!":     "CREATE VIEW " + path("v!") + " AS SELECT a FROM " + path("base"),
			"cols":   "CREATE VIEW " + path("cols") + " AS SELECT a AS `x!` FROM " + path("base"),
			"nested": "CREATE OR REPLACE VIEW " + path("nested") + " AS SELECT STRUCT(a AS `y?`) AS s FROM " + path("base"),
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery")
			if tableExists(t, ctx, ds.Table(view)) {
				t.Errorf("the refused %s made %s", sql, view)
			}
		}
		for _, sql := range []string{
			"CREATE VIEW " + path("listed") + " (c, d) AS SELECT a, b FROM " + path("base"),
			"CREATE MATERIALIZED VIEW " + path("mv") + " AS SELECT a FROM " + path("base"),
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusNotImplemented, "notImplemented")
		}
	}
	err := ds.Table("tv").Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT a AS `w!` FROM " + path("base")})
	wantReason(t, "tables.insert of a view with a column w!", err, http.StatusBadRequest, "invalid")
	if tableExists(t, ctx, ds.Table("tv")) {
		t.Error("the refused tables.insert made its view")
	}

	if err := bqRun(ctx, c, "CREATE VIEW "+path("good")+" AS SELECT a AS `first name` FROM "+path("base"), false); err != nil {
		t.Fatalf("a view with names BigQuery takes: %v", err)
	}
	if got := rows(t, ctx, c, "SELECT * FROM "+path("good")); len(got) != 1 || got[0][0] != int64(1) {
		t.Errorf("the view read %v", got)
	}
	if err := bqRun(ctx, c, "CREATE OR REPLACE VIEW "+path("good")+" AS SELECT b FROM "+path("base"), false); err != nil {
		t.Fatalf("CREATE OR REPLACE VIEW of an existing view: %v", err)
	}
	md, err := ds.Table("good").Metadata(ctx)
	if err != nil || len(md.Schema) != 1 || md.Schema[0].Name != "b" {
		t.Fatalf("the replaced view: %+v %v", md, err)
	}
	if got := rows(t, ctx, c, "SELECT * FROM "+path("good")); len(got) != 1 || got[0][0] != "x" {
		t.Errorf("the replaced view read %v", got)
	}
	if err := ds.Table("tgood").Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT a FROM " + path("base")}); err != nil {
		t.Errorf("tables.insert of a view BigQuery takes: %v", err)
	}
}

// TestBigQueryCreateTableAsSelectCheckedAfterTheScript (#917): a CREATE
// TABLE ... AS SELECT whose query cannot run alone, as it names a script
// variable or a table an earlier statement makes, is checked after the
// script ran, in the table it made. Measured first: the emulator made
// tables with columns `a!` and `b!` from such scripts, and gave no session
// to a query with createSession, so the earlier statements cannot be run
// first. A refused name deletes the table and fails the query, 400
// invalidQuery through jobs.query, and a job failed so through jobs.insert,
// which jobs.get reports too; the earlier statement's table is kept, as
// BigQuery keeps it. Names BigQuery takes are made.
func TestBigQueryCreateTableAsSelectCheckedAfterTheScript(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }

	// The emulator keeps a script's variables after it (measured: a later
	// query read one), so each script declares one no other has.
	variable := func() string { return fmt.Sprintf("v%d", time.Now().UnixNano()) }
	x := variable()
	sql := "DECLARE " + x + " INT64 DEFAULT 1; CREATE TABLE " + path("var") + " AS SELECT " + x + " AS `a!`"
	wantReason(t, sql, bqRun(ctx, c, sql, false), http.StatusBadRequest, "invalidQuery")
	if tableExists(t, ctx, ds.Table("var")) {
		t.Error("the refused script left its table")
	}
	x = variable()
	sql = "DECLARE " + x + " INT64 DEFAULT 1; CREATE TABLE " + path("var") + " AS SELECT " + x + " AS `a!`"
	job, be := jobError(t, ctx, c, sql)
	if be == nil || be.Reason != "invalidQuery" || !strings.Contains(be.Message, `"a!"`) {
		t.Errorf("jobs.insert: the job's error is %v, want invalidQuery naming a!", be)
	}
	if st, err := job.Status(ctx); err != nil || st.Err() == nil {
		t.Errorf("jobs.get reports %v %v, want the job failed", st, err)
	}
	if tableExists(t, ctx, ds.Table("var")) {
		t.Error("the refused query job left its table")
	}

	sql = "CREATE TABLE " + path("first") + " AS SELECT 1 AS a; CREATE TABLE " + path("second") + " AS SELECT a AS `b!` FROM " + path("first")
	wantReason(t, sql, bqRun(ctx, c, sql, false), http.StatusBadRequest, "invalidQuery")
	if tableExists(t, ctx, ds.Table("second")) {
		t.Error("the refused statement left its table")
	}
	if !tableExists(t, ctx, ds.Table("first")) {
		t.Error("the earlier statement's table is gone")
	}

	x = variable()
	sql = "DECLARE " + x + " INT64 DEFAULT 7; CREATE TABLE " + path("ok") + " AS SELECT " + x + " AS `first name`"
	if err := bqRun(ctx, c, sql, false); err != nil {
		t.Fatalf("a script whose names BigQuery takes: %v", err)
	}
	if got := rows(t, ctx, c, "SELECT * FROM "+path("ok")); len(got) != 1 || got[0][0] != int64(7) {
		t.Errorf("the table read %v", got)
	}
}

// TestBigQueryExceptionHandlersRaiseAndCreateOrReplace (#918). Measured
// first: the emulator never ran an EXCEPTION handler (a failing block
// failed the script with its error), reported RAISE done, and failed
// CREATE OR REPLACE TABLE on an existing table with 400 "table is already
// created". A script with a handler that fails is 501, through jobs.query,
// and through jobs.insert as a failed job that jobs.get and jobs.list
// report; one whose block succeeds runs. RAISE is 501. CREATE OR REPLACE
// TABLE replaces the table, from a query that reads the table itself too,
// leaves no other table behind, and keeps the table when the replacement
// fails; inside a script of several statements it is 501.
func TestBigQueryExceptionHandlersRaiseAndCreateOrReplace(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }

	sql := "BEGIN SELECT * FROM " + path("missing") + "; EXCEPTION WHEN ERROR THEN CREATE TABLE " + path("handled") + " (a INT64); END"
	wantReason(t, sql, bqRun(ctx, c, sql, false), http.StatusNotImplemented, "notImplemented")
	job, be := jobError(t, ctx, c, sql)
	if be == nil || be.Reason != "notImplemented" {
		t.Errorf("jobs.insert: the job's error is %v, want notImplemented", be)
	}
	if st, err := job.Status(ctx); err != nil || st.Err() == nil {
		t.Errorf("jobs.get reports %v %v, want the job failed", st, err)
	}
	if st, ok := listedStatus(t, ctx, c, job.ID()); !ok || st == nil || st.Err() == nil {
		t.Errorf("jobs.list reports %v (listed %v), want the job failed", st, ok)
	}
	if tableExists(t, ctx, ds.Table("handled")) {
		t.Error("the handler's table was made")
	}
	if got := rows(t, ctx, c, "BEGIN SELECT 1 AS n; EXCEPTION WHEN ERROR THEN SELECT 2 AS n; END"); len(got) != 1 || got[0][0] != int64(1) {
		t.Errorf("a script whose block succeeds read %v", got)
	}
	wantReason(t, "RAISE", bqRun(ctx, c, "RAISE USING MESSAGE = 'boom'", false), http.StatusNotImplemented, "notImplemented")

	if err := bqRun(ctx, c, "CREATE TABLE "+path("r")+" AS SELECT 1 AS a UNION ALL SELECT 2", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		if err := bqRun(ctx, c, "CREATE OR REPLACE TABLE "+path("r")+" AS SELECT a + 10 AS a FROM "+path("r"), insert); err != nil {
			t.Fatalf("CREATE OR REPLACE TABLE of an existing table (insert %v): %v", insert, err)
		}
	}
	if got := rows(t, ctx, c, "SELECT a FROM "+path("r")+" ORDER BY a"); len(got) != 2 || got[0][0] != int64(21) || got[1][0] != int64(22) {
		t.Errorf("the replaced table read %v, want 21 and 22", got)
	}
	if err := bqRun(ctx, c, "CREATE OR REPLACE TABLE "+path("r")+" (c STRING)", false); err != nil {
		t.Fatalf("CREATE OR REPLACE TABLE with a column list: %v", err)
	}
	md, err := ds.Table("r").Metadata(ctx)
	if err != nil || len(md.Schema) != 1 || md.Schema[0].Name != "c" {
		t.Fatalf("the replaced table: %+v %v", md, err)
	}
	if err := bqRun(ctx, c, "CREATE OR REPLACE TABLE "+path("r")+" AS SELECT * FROM "+path("missing"), false); err == nil {
		t.Error("a replacement from a missing table succeeded")
	}
	if md, err := ds.Table("r").Metadata(ctx); err != nil || len(md.Schema) != 1 || md.Schema[0].Name != "c" {
		t.Errorf("a failed replacement changed the table: %+v %v", md, err)
	}
	sql = "SELECT 1; CREATE OR REPLACE TABLE " + path("r") + " (d INT64)"
	wantReason(t, sql, bqRun(ctx, c, sql, false), http.StatusNotImplemented, "notImplemented")
	it := ds.Tables(ctx)
	var names []string
	for {
		tbl, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tbl.TableID)
	}
	if strings.Join(names, ",") != "r" {
		t.Errorf("the dataset holds %v, want only r", names)
	}
}

// TestBigQueryAutodetectHeaderAndResumableLoads (#919). Measured first:
// the emulator took a CSV's first row as its header whatever it held
// (a,b / c,d gave columns a and b; 1,2 / 3,4 columns "1" and "2"), ignored
// skipLeadingRows, answered a resumable upload's first request with a
// Location of its own listen address, http://0.0.0.0:9051, which the Go
// client retried until it gave up, and listed a load the front failed as
// succeeded. BigQuery takes the first row as the header only when it holds
// only strings and another row does not: a load it would read otherwise is
// 501, and so is skipLeadingRows 2. A load through a resumable upload (the
// Go client's, in 256 KiB chunks) loads every row and is checked as any
// other: a header BigQuery refuses fails the job, which jobs.list reports
// failed.
func TestBigQueryAutodetectHeaderAndResumableLoads(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table, data string, set func(*bigquery.ReaderSource, *bigquery.Loader)) (*bigquery.Job, error) {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.AutoDetect = true
		l := ds.Table(table).LoaderFrom(src)
		if set != nil {
			set(src, l)
		}
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return l.Run(lctx)
	}
	for table, data := range map[string]string{"strings": "a,b\nc,d\n", "numbers": "1,2\n3,4\n"} {
		_, err := load(table, data, nil)
		wantReason(t, "an autodetect load of "+strings.ReplaceAll(data, "\n", " / "), err, http.StatusNotImplemented, "notImplemented")
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("the refused load left %s", table)
		}
	}
	_, err := load("skip2", "x\na,n\nb,1\n", func(s *bigquery.ReaderSource, _ *bigquery.Loader) { s.SkipLeadingRows = 2 })
	wantReason(t, "skipLeadingRows 2", err, http.StatusNotImplemented, "notImplemented")

	chunked := func(_ *bigquery.ReaderSource, l *bigquery.Loader) {
		l.MediaOptions = []googleapi.MediaOption{googleapi.ChunkSize(googleapi.MinUploadChunkSize)}
	}
	var b strings.Builder
	b.WriteString("id,note\n")
	n := 0
	for b.Len() < 3*googleapi.MinUploadChunkSize {
		n++
		fmt.Fprintf(&b, "%d,%s\n", n, strings.Repeat("x", 100))
	}
	job, err := load("resumable", b.String(), chunked)
	if err != nil {
		t.Fatalf("a resumable upload: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("a resumable upload's job: %v %v", err, st.Err())
	}
	if got := countRows(t, h, ds.Table("resumable")); got != n {
		t.Errorf("the resumable upload loaded %d rows, want %d", got, n)
	}

	bad := "a b!,note\n" + strings.TrimPrefix(b.String(), "id,note\n")
	job, err = load("resumable_bad", bad, chunked)
	if err != nil {
		t.Fatalf("a resumable upload with a header BigQuery refuses was refused before it ran: %v", err)
	}
	st, err := job.Wait(ctx)
	var be *bigquery.Error
	if err != nil || !errors.As(st.Err(), &be) || be.Reason != "invalid" || !strings.Contains(be.Message, `"a b!"`) {
		t.Errorf("the job's error is %v %v, want invalid naming a b!", err, st.Err())
	}
	if tableExists(t, ctx, ds.Table("resumable_bad")) {
		t.Error("the failed resumable load left its table")
	}
	if st, ok := listedStatus(t, ctx, c, job.ID()); !ok || st == nil || st.Err() == nil {
		t.Errorf("jobs.list reports %v (listed %v), want the job failed", st, ok)
	}
}

// TestBigQueryAutodetectLoadFromCloudStorage (#919): a load from a gs://
// URI reads the instance's own Cloud Storage. Measured first: the
// emulator, given no STORAGE_EMULATOR_HOST, dialled storage.googleapis.com
// for it. A CSV with a header BigQuery takes loads its rows; one with a
// header BigQuery refuses fails the job, as a load from a file does. The
// Cloud Storage endpoint is the one `cloudburrow env` gives, so the test
// runs on an instance with both services.
func TestBigQueryAutodetectLoadFromCloudStorage(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	endpoint := strings.TrimSpace(os.Getenv(EnvStorage))
	if endpoint == "" {
		cli := os.Getenv(EnvCLI)
		if cli == "" {
			t.Skipf("neither %s nor %s is set", EnvStorage, EnvCLI)
		}
		out, err := exec.Command(cli, append([]string{"env", "--format", "json"}, strings.Fields(os.Getenv(EnvCLIArgs))...)...).Output()
		if err != nil {
			t.Fatalf("cloudburrow env --format json: %v", err)
		}
		var exported map[string]string
		if err := json.Unmarshal(out, &exported); err != nil {
			t.Fatalf("env --format json: %v\n%s", err, out)
		}
		if endpoint = exported["STORAGE_EMULATOR_HOST"]; endpoint == "" {
			t.Skip("the instance does not have storage enabled")
		}
		t.Setenv(EnvStorage, endpoint)
	}
	sc := storageClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-load")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		for _, o := range []string{"good.csv", "bad.csv"} {
			_ = bucket.Object(o).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	for name, data := range map[string]string{"good.csv": "first name,n\nbob,3\nann,4\n", "bad.csv": "a b!,n\nbob,3\n"} {
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	load := func(table, object string) (*bigquery.JobStatus, error) {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + object)
		ref.AutoDetect = true
		ref.SourceFormat = bigquery.CSV
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table(table).LoaderFrom(ref).Run(lctx)
		if err != nil {
			return nil, err
		}
		return job.Wait(lctx)
	}
	st, err := load("from_gcs", "good.csv")
	if err != nil || st.Err() != nil {
		t.Fatalf("a load from Cloud Storage: %v %v", err, st)
	}
	if got := countRows(t, h, ds.Table("from_gcs")); got != 2 {
		t.Errorf("the load from Cloud Storage wrote %d rows, want 2", got)
	}
	st, err = load("from_gcs_bad", "bad.csv")
	var be *bigquery.Error
	if err != nil || !errors.As(st.Err(), &be) || be.Reason != "invalid" {
		t.Errorf("a load from Cloud Storage with a header BigQuery refuses: %v %v, want the job failed invalid", err, st)
	}
	if tableExists(t, ctx, ds.Table("from_gcs_bad")) {
		t.Error("the failed load from Cloud Storage left its table")
	}
}
