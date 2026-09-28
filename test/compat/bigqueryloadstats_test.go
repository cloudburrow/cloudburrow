//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// loadStatsParquet is a Parquet file of three rows, (1, "x"), (2, "y") and
// (3, "z"), in columns a (INT64) and b (STRING): written by
// github.com/parquet-go/parquet-go v0.29.0's parquet.Write, which this
// module does not depend on.
const loadStatsParquet = "UEFSMRUGFTAVMBWOtty8BUwVBhUAFQYVABUAFQASHBgIAwAAAAAAAAAYCAEAAAAAAAAAFgAoCAMAAAAAAAAAGAgBAAAAAAAAAAAA" +
	"AAEAAAAAAAAAAgAAAAAAAAADAAAAAAAAABUGFSIVIhXg0bXEBkwVBhUAFQYVDBUAFQASHBgBehgBeBYAKAF6GAF4AAAAgAEEAwIB" +
	"AQAAAAMAAAB4eXoZEgAZGAgBAAAAAAAAABkYCAMAAAAAAAAAFQAZFgAAGRIAGRgBeBkYAXoVABkWAAAZHBYIFcABFgAAABkcFsgB" +
	"FXoWAAAAFQQZPEgEcHJvdxUEABUEFYABFQAYAWElJEysE0ARAAAAFQwlABgBYiUATBwAAAAWBhkcGSwmABwVBBkVABkYAWEVABYG" +
	"FsABFsABJgg8NgAoCAMAAAAAAAAAGAgBAAAAAAAAAAAZHBUGFQAVAgAAFqIDFRYWwgIVPgAmABwVDBkVDBkYAWIVABYGFnoWeibI" +
	"ATw2ACgBehgBeAAZHBUGFQwVAgA8FgYAABa4AxUWFoADFSIAFroCFgYZDBYIFroCFAAAGQwYN2dpdGh1Yi5jb20vcGFycXVldC1n" +
	"by9wYXJxdWV0LWdvIHZlcnNpb24gMC4yOS4wKGJ1aWxkICkZLBwAABwAAAAHAQAAUEFSMQ=="

// TestBigQueryLoadsReportCounts (#966): a load whose records the front
// does not read reports statistics.load and status DONE, in the jobs.insert
// answer, jobs.get and jobs.list: a NEWLINE_DELIMITED_JSON load into a new
// table, appended to one with rows and replacing them (WRITE_TRUNCATE), a
// Parquet load, a CSV load the front passes on as it is
// (allowQuotedNewlines and preserveAsciiControlCharacters, no other
// option), and NEWLINE_DELIMITED_JSON loads from Cloud Storage, of two
// objects and of a wildcard. outputRows is the rows loaded, inputFiles
// and inputFileBytes the upload's or the objects'; outputBytes is not
// reported (#965).
//
// Read through the official Go client, cloud.google.com/go/bigquery
// (JobStatus.Statistics, LoadStatistics), and through the generated client
// it is built on, google.golang.org/api/bigquery/v2, for jobs.get and
// jobs.list as they are sent. Measured first, on the front of #960: each
// of these loads read back with no statistics.load, and from jobs.insert
// and jobs.list with no status; an AVRO or ORC load is 400 "not support
// sourceFormat" from the emulator, so it has none to report.
func TestBigQueryLoadsReportCounts(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}
	parquet, err := base64.StdEncoding.DecodeString(loadStatsParquet)
	if err != nil {
		t.Fatal(err)
	}
	const json3 = "{\"a\":1,\"b\":\"x\"}\n{\"a\":2,\"b\":\"y\"}\n{\"a\":3,\"b\":\"z\"}\n"
	const json1 = "{\"a\":4,\"b\":\"w\"}\n"
	const plainCSV = "1,x\n2,\"y\nz\"\n"
	bucket := loadBucket(t, h, "bq-stats", map[string]string{"j/one.json": json3, "j/two.json": json1})
	uri := func(name string) string { return "gs://" + bucket.BucketName() + "/" + name }

	// withRows makes the table with two rows in it.
	withRows := func(table string) {
		t.Helper()
		if err := ds.Table(table).Create(ctx, &bigquery.TableMetadata{Schema: loadSchema}); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		src := bigquery.NewReaderSource(strings.NewReader("{\"a\":8,\"b\":\"r\"}\n{\"a\":9,\"b\":\"s\"}\n"))
		src.SourceFormat = bigquery.JSON
		if err := runLoad(ctx, ds.Table(table).LoaderFrom(src)); err != nil {
			t.Fatalf("the first load into %s: %v", table, err)
		}
	}
	reader := func(data string, format bigquery.DataFormat) *bigquery.ReaderSource {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.SourceFormat = format
		src.Schema = loadSchema
		return src
	}
	gcs := func(names ...string) *bigquery.GCSReference {
		uris := make([]string, len(names))
		for i, n := range names {
			uris[i] = uri(n)
		}
		r := bigquery.NewGCSReference(uris...)
		r.SourceFormat = bigquery.JSON
		r.Schema = loadSchema
		return r
	}
	for _, c2 := range []struct {
		table          string
		existing       bool
		src            bigquery.LoadSource
		truncate       bool
		rows, files    int64
		bytes          int64
		tableRowsAfter int
	}{
		{"json_new", false, reader(json3, bigquery.JSON), false, 3, 1, int64(len(json3)), 3},
		{"json_append", true, reader(json3, bigquery.JSON), false, 3, 1, int64(len(json3)), 5},
		{"json_truncate", true, reader(json3, bigquery.JSON), true, 3, 1, int64(len(json3)), 3},
		{"parquet", false, reader(string(parquet), bigquery.Parquet), false, 3, 1, int64(len(parquet)), 3},
		{"csv_plain", false, func() bigquery.LoadSource {
			s := reader(plainCSV, bigquery.CSV)
			s.AllowQuotedNewlines = true
			s.PreserveASCIIControlCharacters = true
			return s
		}(), false, 2, 1, int64(len(plainCSV)), 2},
		{"gcs_json", false, gcs("j/one.json", "j/two.json"), false, 4, 2, int64(len(json3) + len(json1)), 4},
		{"gcs_json_wildcard", true, gcs("j/*.json"), false, 4, 2, int64(len(json3) + len(json1)), 6},
	} {
		if c2.existing {
			withRows(c2.table)
		}
		l := ds.Table(c2.table).LoaderFrom(c2.src)
		if c2.truncate {
			l.WriteDisposition = bigquery.WriteTruncate
		}
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		job, err := l.Run(lctx)
		if err != nil {
			cancel()
			t.Errorf("%s: jobs.insert: %v", c2.table, err)
			continue
		}
		inserted := job.LastStatus()
		st, err := job.Wait(lctx)
		cancel()
		if err != nil || st.Err() != nil {
			t.Errorf("%s: the load failed: %v %v", c2.table, err, st)
			continue
		}
		listed, ok := listedStatus(t, ctx, c, job.ID())
		if !ok {
			t.Errorf("%s: jobs.list does not list the job", c2.table)
		}
		for what, s := range map[string]*bigquery.JobStatus{"jobs.insert": inserted, "jobs.get": st, "jobs.list": listed} {
			if s == nil || s.State != bigquery.Done || s.Statistics == nil {
				t.Errorf("%s: %s: status %+v, want DONE with statistics", c2.table, what, s)
				continue
			}
			ls, ok := s.Statistics.Details.(*bigquery.LoadStatistics)
			if !ok {
				t.Errorf("%s: %s: statistics %T, want LoadStatistics", c2.table, what, s.Statistics.Details)
				continue
			}
			if ls.OutputRows != c2.rows || ls.InputFiles != c2.files || ls.InputFileBytes != c2.bytes || ls.OutputBytes != 0 {
				t.Errorf("%s: %s: LoadStatistics %+v, want %d rows from %d files of %d bytes, no outputBytes",
					c2.table, what, ls, c2.rows, c2.files, c2.bytes)
			}
		}
		got, err := svc.Jobs.Get(project, job.ID()).Context(ctx).Do()
		if err != nil {
			t.Errorf("%s: bigquery/v2 jobs.get: %v", c2.table, err)
			continue
		}
		if got.Status == nil || got.Status.State != "DONE" || got.Statistics == nil || got.Statistics.Load == nil ||
			got.Statistics.Load.OutputRows != c2.rows || got.Statistics.Load.InputFileBytes != c2.bytes {
			t.Errorf("%s: bigquery/v2 jobs.get: status %+v, statistics %+v", c2.table, got.Status, got.Statistics)
		}
		if n := countRows(t, h, ds.Table(c2.table)); n != c2.tableRowsAfter {
			t.Errorf("%s: the table has %d rows, want %d", c2.table, n, c2.tableRowsAfter)
		}
	}
}
