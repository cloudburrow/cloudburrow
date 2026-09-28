//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// generatedBigQuery is the generated REST client the Go client is built
// on, google.golang.org/api/bigquery/v2, for fields and requests the Go
// client does not expose.
func generatedBigQuery(t *testing.T, h *Harness) *bq.Service {
	t.Helper()
	svc, err := bq.NewService(h.Context(), option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}
	return svc
}

// TestBigQueryJobWithoutReference (#973): a jobs.insert with no
// jobReference, sent through the generated client: a query job that
// succeeds and one that fails, a load from Cloud Storage and an upload
// are each given a job ID in the request's project, which jobs.get
// finds. Measured first: the query jobs and the load were answered 500
// "nil pointer dereference", which the client retried.
func TestBigQueryJobWithoutReference(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	svc := generatedBigQuery(t, h)
	ctx := h.Context()
	if err := ds.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: loadSchema}); err != nil {
		t.Fatal(err)
	}
	bucket := loadBucket(t, h, "bq-noref", map[string]string{"x.json": "{\"a\":1,\"b\":\"x\"}\n"})
	dest := &bq.TableReference{ProjectId: project, DatasetId: ds.DatasetID, TableId: "t"}
	schema := &bq.TableSchema{Fields: []*bq.TableFieldSchema{{Name: "a", Type: "INTEGER"}, {Name: "b", Type: "STRING"}}}
	for _, c2 := range []struct {
		name  string
		conf  *bq.JobConfiguration
		media string
		fails bool
	}{
		{"query", &bq.JobConfiguration{Query: &bq.JobConfigurationQuery{Query: "SELECT 1", UseLegacySql: new(bool)}}, "", false},
		{"failing query", &bq.JobConfiguration{Query: &bq.JobConfigurationQuery{Query: "SELECT * FROM nope.nope", UseLegacySql: new(bool)}}, "", true},
		{"load from Cloud Storage", &bq.JobConfiguration{Load: &bq.JobConfigurationLoad{SourceUris: []string{"gs://" + bucket.BucketName() + "/x.json"},
			SourceFormat: "NEWLINE_DELIMITED_JSON", DestinationTable: dest, Schema: schema}}, "", false},
		{"upload", &bq.JobConfiguration{Load: &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", DestinationTable: dest,
			Schema: schema}}, "{\"a\":2,\"b\":\"y\"}\n", false},
	} {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		call := svc.Jobs.Insert(project, &bq.Job{Configuration: c2.conf}).Context(cctx)
		if c2.media != "" {
			call = call.Media(strings.NewReader(c2.media))
		}
		job, err := call.Do()
		cancel()
		if err != nil {
			t.Errorf("%s: jobs.insert: %v", c2.name, err)
			continue
		}
		if job.JobReference == nil || job.JobReference.JobId == "" || job.JobReference.ProjectId != project {
			t.Errorf("%s: jobReference %+v", c2.name, job.JobReference)
			continue
		}
		if failed := job.Status != nil && job.Status.ErrorResult != nil; failed != c2.fails {
			t.Errorf("%s: status %+v", c2.name, job.Status)
		}
		got, err := svc.Jobs.Get(project, job.JobReference.JobId).Context(ctx).Do()
		if err != nil || got.JobReference.JobId != job.JobReference.JobId {
			t.Errorf("%s: jobs.get of %s: %v", c2.name, job.JobReference.JobId, err)
		}
	}
	if got := rows(t, ctx, c, "SELECT a FROM "+ds.DatasetID+".t ORDER BY a"); fmt.Sprint(got) != "[[1] [2]]" {
		t.Errorf("the loads loaded %v", got)
	}
}

// TestBigQueryJobTimes (#971): a job's statistics.creationTime, startTime
// and endTime are milliseconds since the epoch, when the front received
// it and the emulator answered, for an upload, a load from Cloud Storage
// and a query job, in the jobs.insert answer (Job.LastStatus), jobs.get
// (Job.Status) and jobs.list (Client.Jobs), read by the official Go
// client, and as the generated client reads them. Measured first: the
// load from Cloud Storage and the query job read back 1970-01-21 (the
// emulator's seconds), and the upload the zero time.
func TestBigQueryJobTimes(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	svc := generatedBigQuery(t, h)
	ctx := h.Context()
	bucket := loadBucket(t, h, "bq-times", map[string]string{"x.json": "{\"a\":1,\"b\":\"x\"}\n"})
	type timed struct {
		name   string
		job    *bigquery.Job
		before time.Time
		after  time.Time
	}
	var jobs []timed
	run := func(name string, start func() (*bigquery.Job, error)) {
		t.Helper()
		before := time.Now()
		job, err := start()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after := time.Now()
		jobs = append(jobs, timed{name, job, before, after})
	}
	run("upload", func() (*bigquery.Job, error) {
		src := bigquery.NewReaderSource(strings.NewReader("{\"a\":1,\"b\":\"x\"}\n"))
		src.SourceFormat, src.Schema = bigquery.JSON, loadSchema
		return ds.Table("u").LoaderFrom(src).Run(ctx)
	})
	run("load from Cloud Storage", func() (*bigquery.Job, error) {
		g := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/x.json")
		g.SourceFormat, g.Schema = bigquery.JSON, loadSchema
		return ds.Table("g").LoaderFrom(g).Run(ctx)
	})
	run("query job", func() (*bigquery.Job, error) { return c.Query("SELECT 1").Run(ctx) })

	check := func(what string, j timed, st *bigquery.JobStatus) {
		t.Helper()
		if st == nil || st.Statistics == nil {
			t.Errorf("%s of %s: no statistics", what, j.name)
			return
		}
		s := st.Statistics
		// The front's clock is the cluster's, which may differ from the
		// test's by a little (measured: 6 ms behind).
		const skew = 2 * time.Second
		if s.CreationTime.Before(j.before.Add(-skew)) || s.EndTime.After(j.after.Add(skew)) || !s.StartTime.Equal(s.CreationTime) || s.EndTime.Before(s.StartTime) {
			t.Errorf("%s of %s: created %v, started %v, ended %v; want within [%v, %v]", what, j.name,
				s.CreationTime, s.StartTime, s.EndTime, j.before, j.after)
		}
	}
	for _, j := range jobs {
		check("jobs.insert", j, j.job.LastStatus())
		st, err := j.job.Status(ctx)
		if err != nil {
			t.Fatalf("jobs.get of %s: %v", j.name, err)
		}
		check("jobs.get", j, st)
		listed, ok := listedStatus(t, ctx, c, j.job.ID())
		if !ok {
			t.Errorf("jobs.list does not list the %s", j.name)
			continue
		}
		check("jobs.list", j, listed)
		raw, err := svc.Jobs.Get(project, j.job.ID()).Context(ctx).Do()
		if err != nil || raw.Statistics.CreationTime < 1e12 || raw.Statistics.EndTime < raw.Statistics.CreationTime {
			t.Errorf("jobs.get of %s through the generated client: %v %+v", j.name, err, raw)
		}
	}
}

// TestBigQueryJobListPagesAndFilters (#972): jobs.list, through the
// official Go client's Client.Jobs: newest first; paged by its page size
// (maxResults) and page token; filtered by State (stateFilter),
// MinCreationTime and MaxCreationTime and ParentJobID; AllUsers lists the
// same jobs. The front's own queries (the one it runs to read a CREATE
// TABLE ... AS SELECT's columns) are not listed. Measured first: the
// emulator listed every job, oldest first, whatever the request asked.
func TestBigQueryJobListPagesAndFilters(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	start := time.Now().Add(-time.Millisecond)
	var ids []string
	for _, sql := range []string{"SELECT 1", "CREATE TABLE " + ds.DatasetID + ".ctas AS SELECT 1 AS a", "SELECT 3"} {
		job, err := c.Query(sql).Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if _, err := job.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		ids = append(ids, job.ID())
	}
	end := time.Now().Add(time.Millisecond)
	list := func(configure func(*bigquery.JobIterator)) []string {
		t.Helper()
		it := c.Jobs(ctx)
		it.MinCreationTime, it.MaxCreationTime = start, end
		if configure != nil {
			configure(it)
		}
		var got []string
		for {
			j, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return got
			}
			if err != nil {
				t.Fatalf("jobs.list: %v", err)
			}
			got = append(got, j.ID())
		}
	}
	newest := fmt.Sprint([]string{ids[2], ids[1], ids[0]})
	if got := list(nil); fmt.Sprint(got) != newest {
		t.Errorf("jobs.list of this test's jobs: %v, want %s (newest first, no query of the front's own)", got, newest)
	}
	if got := list(func(it *bigquery.JobIterator) { it.AllUsers = true }); fmt.Sprint(got) != newest {
		t.Errorf("jobs.list with AllUsers: %v", got)
	}
	if got := list(func(it *bigquery.JobIterator) { it.State = bigquery.Done }); fmt.Sprint(got) != newest {
		t.Errorf("jobs.list of DONE jobs: %v", got)
	}
	if got := list(func(it *bigquery.JobIterator) { it.State = bigquery.Running }); len(got) != 0 {
		t.Errorf("jobs.list of RUNNING jobs: %v", got)
	}
	if got := list(func(it *bigquery.JobIterator) { it.ParentJobID = ids[0] }); len(got) != 0 {
		t.Errorf("jobs.list of a query's child jobs: %v", got)
	}
	st, err := c.JobFromID(ctx, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	created := st.LastStatus().Statistics.CreationTime
	if got := list(func(it *bigquery.JobIterator) { it.MinCreationTime = created }); fmt.Sprint(got) != fmt.Sprint([]string{ids[2], ids[1]}) {
		t.Errorf("jobs.list from the second job's creation: %v", got)
	}
	if got := list(func(it *bigquery.JobIterator) { it.MaxCreationTime = created }); fmt.Sprint(got) != fmt.Sprint([]string{ids[1], ids[0]}) {
		t.Errorf("jobs.list up to the second job's creation: %v", got)
	}

	// Pages of two: the first has a page token, the second the last job.
	it := c.Jobs(ctx)
	it.MinCreationTime, it.MaxCreationTime = start, end
	pager := iterator.NewPager(it, 2, "")
	var page []*bigquery.Job
	tok, err := pager.NextPage(&page)
	if err != nil || len(page) != 2 || tok == "" || page[0].ID() != ids[2] {
		t.Fatalf("first page: %v, token %q, %v", len(page), tok, err)
	}
	page = nil
	tok, err = pager.NextPage(&page)
	if err != nil || len(page) != 1 || tok != "" || page[0].ID() != ids[0] {
		t.Errorf("second page: %d jobs, token %q, %v", len(page), tok, err)
	}
}

// TestBigQueryParquetLoadWithoutSchema (#970, #988): a Parquet load whose
// job gives no schema, through the official Go client, into a table that
// exists loads its rows, from an upload and from Cloud Storage; into a
// table that does not exist it makes the table with the file's schema
// (#988; #970 answered it 501, as the front did not read the file) and
// loads the rows. Measured first: each was 500 "nil pointer dereference",
// and the load from Cloud Storage did not return.
func TestBigQueryParquetLoadWithoutSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	parquet, err := base64.StdEncoding.DecodeString(loadStatsParquet)
	if err != nil {
		t.Fatal(err)
	}
	bucket := loadBucket(t, h, "bq-parquet", map[string]string{"x.parquet": string(parquet)})
	upload := func() bigquery.LoadSource {
		s := bigquery.NewReaderSource(strings.NewReader(string(parquet)))
		s.SourceFormat = bigquery.Parquet
		return s
	}
	gcs := func() bigquery.LoadSource {
		s := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/x.parquet")
		s.SourceFormat = bigquery.Parquet
		return s
	}
	for name, src := range map[string]func() bigquery.LoadSource{"upload": upload, "gcs": gcs} {
		table := "existing_" + name
		if err := ds.Table(table).Create(ctx, &bigquery.TableMetadata{Schema: loadSchema}); err != nil {
			t.Fatal(err)
		}
		if err := runLoad(ctx, ds.Table(table).LoaderFrom(src())); err != nil {
			t.Errorf("%s into a table that exists: %v", name, err)
			continue
		}
		if got := rows(t, ctx, c, "SELECT a, b FROM "+ds.DatasetID+"."+table+" ORDER BY a"); fmt.Sprint(got) != "[[1 x] [2 y] [3 z]]" {
			t.Errorf("%s into a table that exists loaded %v", name, got)
		}

		if err := runLoad(ctx, ds.Table("new_"+name).LoaderFrom(src())); err != nil {
			t.Errorf("%s into a new table: %v", name, err)
			continue
		}
		md, err := ds.Table("new_" + name).Metadata(ctx)
		if err != nil || len(md.Schema) != 2 || md.Schema[0].Name != "a" || md.Schema[0].Type != bigquery.IntegerFieldType ||
			md.Schema[1].Name != "b" || md.Schema[1].Type != bigquery.StringFieldType {
			t.Errorf("%s into a new table: schema %+v, %v", name, md, err)
		}
		if got := rows(t, ctx, c, "SELECT a, b FROM "+ds.DatasetID+".new_"+name+" ORDER BY a"); fmt.Sprint(got) != "[[1 x] [2 y] [3 z]]" {
			t.Errorf("%s into a new table loaded %v", name, got)
		}
	}
}

// TestBigQueryCSVExtractOfAnEmptyString (#975): a CSV extract, through the
// official Go client's Extractor, of a table whose STRING column holds an
// empty value is 501 and writes nothing: the emulator (measured) and the
// front write it as they write a NULL, an empty field, and BigQuery's
// documentation does not give its form. A table with a NULL and no empty
// value is extracted, the NULL as an empty field; a JSON extract of the
// empty value writes it.
func TestBigQueryCSVExtractOfAnEmptyString(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	sc := instanceStorage(t, h)
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-empty")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := bucket.Objects(context.Background(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	for _, sql := range []string{
		"CREATE TABLE " + ds.DatasetID + ".withnull AS SELECT 1 AS n, CAST(NULL AS STRING) AS s UNION ALL SELECT 2, 'x'",
		"CREATE TABLE " + ds.DatasetID + ".withempty AS SELECT 1 AS n, '' AS s UNION ALL SELECT 2, 'x'",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	extract := func(table, object string, format bigquery.DataFormat) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + object)
		ref.DestinationFormat = format
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table(table).ExtractorTo(ref).Run(ectx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ectx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	read := func(object string) (string, error) {
		r, err := bucket.Object(object).NewReader(ctx)
		if err != nil {
			return "", err
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		return string(b), err
	}
	if err := extract("withnull", "null.csv", bigquery.CSV); err != nil {
		t.Fatalf("CSV extract with a NULL: %v", err)
	}
	if got, err := read("null.csv"); err != nil || !strings.Contains(got, "1,\n") {
		t.Errorf("CSV extract with a NULL wrote %q, %v", got, err)
	}
	err := extract("withempty", "empty.csv", bigquery.CSV)
	wantReason(t, "CSV extract with an empty STRING", err, http.StatusNotImplemented, "notImplemented")
	if _, err := read("empty.csv"); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the refused extract wrote an object: %v", err)
	}
	if err := extract("withempty", "empty.json", bigquery.JSON); err != nil {
		t.Fatalf("JSON extract with an empty STRING: %v", err)
	}
	if got, err := read("empty.json"); err != nil || !strings.Contains(got, `"s":""`) {
		t.Errorf("JSON extract with an empty STRING wrote %q, %v", got, err)
	}
}

// TestBigQueryFailedScriptFunctionsAndDropSchema (#976), through the
// official Go client: a script given to jobs.query (Query.Read) that makes
// a function and then fails is 501, and the function is then not there
// (measured first: it was, where the 501 says nothing was kept), so the
// same CREATE FUNCTION later makes it with its new body; a function that
// existed before is left as it was, and so for a table function (#1061:
// it was 501 before anything ran, as the pinned engine could not drop one);
// a DROP FUNCTION in such a script is 501 before anything runs. As a query job,
// which the emulator commits as BigQuery keeps it, the function is kept.
// (DROP SCHEMA, 501 here since #976, is carried out since #990:
// TestBigQueryDropSchema.)
func TestBigQueryFailedScriptFunctionsAndDropSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	const fail = "; SELECT * FROM nope.nope"
	value := func(sql string) string {
		t.Helper()
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			return "error: " + err.Error()
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprint(row)
	}

	err := bqRun(ctx, c, "CREATE FUNCTION "+d+".f(x INT64) AS (x + 1)"+fail, false)
	wantReason(t, "a failed script that makes a function", err, http.StatusNotImplemented, "notImplemented")
	if got := value("SELECT " + d + ".f(1)"); !strings.Contains(got, "Function not found") {
		t.Errorf("after the failed script, f(1) = %s, want not found", got)
	}
	if err := bqRun(ctx, c, "CREATE FUNCTION "+d+".f(x INT64) AS (x + 2)", false); err != nil {
		t.Fatalf("CREATE FUNCTION after it: %v", err)
	}
	if got := value("SELECT " + d + ".f(1)"); got != "[3]" {
		t.Errorf("f(1) = %s, want [3]", got)
	}

	if err := bqRun(ctx, c, "CREATE FUNCTION "+d+".g(x INT64) AS (x * 10)", false); err != nil {
		t.Fatal(err)
	}
	err = bqRun(ctx, c, "CREATE OR REPLACE FUNCTION "+d+".g(x INT64) AS (x + 1)"+fail, false)
	wantReason(t, "a failed script that replaces a function", err, http.StatusNotImplemented, "notImplemented")
	err = bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x AS y)"+fail, false)
	wantReason(t, "a failed script that makes a table function", err, http.StatusNotImplemented, "notImplemented")
	if got := value("SELECT * FROM " + d + ".tf(1)"); !strings.Contains(got, "not found") {
		t.Errorf("after the failed script, tf(1) = %s, want not found", got)
	}
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x + 1 AS y)", false); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION after it: %v", err)
	}
	if got := value("SELECT * FROM " + d + ".tf(1)"); got != "[2]" {
		t.Errorf("tf(1) = %s, want [2]", got)
	}
	for _, sql := range []string{"DROP FUNCTION " + d + ".g" + fail} {
		err := bqRun(ctx, c, sql, false)
		wantReason(t, sql, err, http.StatusNotImplemented, "notImplemented")
		if err != nil && !strings.Contains(err.Error(), "Nothing was run") {
			t.Errorf("%s: %v", sql, err)
		}
	}
	if got := value("SELECT " + d + ".g(1)"); got != "[10]" {
		t.Errorf("g(1) = %s, want [10], as before the scripts", got)
	}

	// A query job keeps it, as BigQuery keeps what ran before the failure.
	_, jerr := jobError(t, ctx, c, "CREATE FUNCTION "+d+".k(x INT64) AS (x + 5)"+fail)
	if jerr == nil {
		t.Errorf("the failing query job succeeded")
	}
	if got := value("SELECT " + d + ".k(1)"); got != "[6]" {
		t.Errorf("after the failed query job, k(1) = %s, want [6]", got)
	}
}
