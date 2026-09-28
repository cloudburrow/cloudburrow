//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
)

// The tests in this file are #944, #945 and #946, each driven through the
// official Go client and measured against the pinned emulator first (the
// measurements are in internal/bigqueryfront and docs/compatibility.md).

// loadBucket makes a bucket in the instance's Cloud Storage holding
// objects, deleted when the test ends. The Cloud Storage endpoint is the
// one `cloudburrow env` gives when CLOUDBURROW_TEST_STORAGE is not set, so
// the test runs on an instance with both services; without Cloud Storage
// it skips.
func loadBucket(t *testing.T, h *Harness, suffix string, objects map[string]string) *storage.BucketHandle {
	t.Helper()
	if strings.TrimSpace(os.Getenv(EnvStorage)) == "" {
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
		if exported["STORAGE_EMULATOR_HOST"] == "" {
			t.Skip("the instance does not have storage enabled")
		}
		t.Setenv(EnvStorage, exported["STORAGE_EMULATOR_HOST"])
	}
	sc := storageClient(t, h)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-" + suffix)
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		for name := range objects {
			_ = bucket.Object(name).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	for name, data := range objects {
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return bucket
}

// runLoad runs a load and waits for it, with a deadline of its own, so a
// 500 the client would retry shows as a timeout.
func runLoad(ctx context.Context, l *bigquery.Loader) error {
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

var loadSchema = bigquery.Schema{
	{Name: "a", Type: bigquery.IntegerFieldType},
	{Name: "b", Type: bigquery.StringFieldType},
}

// TestBigQueryCSVLoadFromCloudStorage (#944): a CSV load from Cloud
// Storage with a schema, or into a table that has one, loads every row of
// each file unless skipLeadingRows says otherwise, each value to the
// column at its position, as a load from the client does (#931), with a
// wildcard URI too; an object that does not exist is 404 notFound and
// loads nothing. Measured first (#931): the emulator, which reads the
// objects itself, took the first row of each file as a header whatever
// skipLeadingRows said, and mapped a header naming the columns in another
// order by name.
func TestBigQueryCSVLoadFromCloudStorage(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	bucket := loadBucket(t, h, "bq-gcs", map[string]string{
		"plain.csv":       "1,x\n2,y\n3,z",
		"header.csv":      "b,a\n4,p\n",
		"parts/one.csv":   "a,b\n5,q\n",
		"parts/two.csv":   "a,b\n6,r\n7,s\n",
		"parts/three.txt": "not,loaded\n",
	})
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := func(objects ...string) *bigquery.GCSReference {
		uris := make([]string, len(objects))
		for i, o := range objects {
			uris[i] = "gs://" + bucket.BucketName() + "/" + o
		}
		r := bigquery.NewGCSReference(uris...)
		r.Schema = loadSchema
		return r
	}
	want := func(table string, wantRows ...[]bigquery.Value) {
		t.Helper()
		got := rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+"."+table+"` ORDER BY a")
		if !reflect.DeepEqual(got, wantRows) {
			t.Errorf("%s holds %v, want %v", table, got, wantRows)
		}
	}

	if err := runLoad(ctx, ds.Table("plain").LoaderFrom(ref("plain.csv"))); err != nil {
		t.Fatalf("a load of a file with no header: %v", err)
	}
	want("plain", []bigquery.Value{int64(1), "x"}, []bigquery.Value{int64(2), "y"}, []bigquery.Value{int64(3), "z"})

	r := ref("header.csv")
	r.SkipLeadingRows = 1
	if err := runLoad(ctx, ds.Table("header").LoaderFrom(r)); err != nil {
		t.Fatalf("a load with skipLeadingRows 1: %v", err)
	}
	want("header", []bigquery.Value{int64(4), "p"})

	r = ref("parts/*.csv")
	r.SkipLeadingRows = 1
	if err := runLoad(ctx, ds.Table("parts").LoaderFrom(r)); err != nil {
		t.Fatalf("a load of a wildcard: %v", err)
	}
	want("parts", []bigquery.Value{int64(5), "q"}, []bigquery.Value{int64(6), "r"}, []bigquery.Value{int64(7), "s"})

	// Into an existing table, with no schema in the load, from two URIs.
	existing := ds.Table("existing")
	if err := existing.Create(ctx, &bigquery.TableMetadata{Schema: loadSchema}); err != nil {
		t.Fatal(err)
	}
	r = ref("plain.csv", "plain.csv")
	r.Schema = nil
	if err := runLoad(ctx, existing.LoaderFrom(r)); err != nil {
		t.Fatalf("a load into an existing table: %v", err)
	}
	if got := countRows(t, h, existing); got != 6 {
		t.Errorf("the load of two URIs into an existing table wrote %d rows, want 6", got)
	}

	for what, r := range map[string]*bigquery.GCSReference{
		"a missing object":                ref("none.csv"),
		"a wildcard that matches nothing": ref("none*"),
	} {
		err := runLoad(ctx, ds.Table("missing").LoaderFrom(r))
		wantReason(t, what, err, http.StatusNotFound, "notFound")
	}
	if tableExists(t, ctx, ds.Table("missing")) {
		t.Error("a load of a missing object made its table")
	}
}

// TestBigQueryCSVLoadOptions (#945): a CSV load's fieldDelimiter, quote,
// allowJaggedRows and nullMarker are honoured, from the client and from
// Cloud Storage; an empty value with a nullMarker, which the emulator
// would load as NULL, is 501 for a STRING column and 400 for another,
// and loads nothing. Measured first: the emulator read the data with
// encoding/csv's defaults, so a tab- or pipe-delimited file, a file quoted
// with ' and a short row with allowJaggedRows failed 400 "wrong number of
// fields"; with quote "" it loaded "x" as x; with nullMarker \N it loaded
// the text \N.
func TestBigQueryCSVLoadOptions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table, data string, set func(*bigquery.ReaderSource)) error {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.Schema = loadSchema
		set(src)
		return runLoad(ctx, ds.Table(table).LoaderFrom(src))
	}
	for _, c2 := range []struct {
		table, data string
		set         func(*bigquery.ReaderSource)
		want        [][]bigquery.Value
	}{
		{"tab", "1\tx,y\n2\tz\n", func(s *bigquery.ReaderSource) { s.FieldDelimiter = "\t" },
			[][]bigquery.Value{{int64(1), "x,y"}, {int64(2), "z"}}},
		{"pipe", "1|\"p|q\"\n", func(s *bigquery.ReaderSource) { s.FieldDelimiter = "|" },
			[][]bigquery.Value{{int64(1), "p|q"}}},
		{"quote", "1,'x,''y'''\n", func(s *bigquery.ReaderSource) { s.Quote = "'" },
			[][]bigquery.Value{{int64(1), "x,'y'"}}},
		{"no_quote", "1,\"x\"\n", func(s *bigquery.ReaderSource) { s.Quote = ""; s.ForceZeroQuote = true },
			[][]bigquery.Value{{int64(1), "\"x\""}}},
		{"jagged", "1\n2,y\n", func(s *bigquery.ReaderSource) { s.AllowJaggedRows = true },
			[][]bigquery.Value{{int64(1), nil}, {int64(2), "y"}}},
		{"null_marker", "1,\\N\n\\N,y\n", func(s *bigquery.ReaderSource) { s.NullMarker = `\N` },
			[][]bigquery.Value{{nil, "y"}, {int64(1), nil}}},
	} {
		if err := load(c2.table, c2.data, c2.set); err != nil {
			t.Errorf("%s: %v", c2.table, err)
			continue
		}
		got := rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+"."+c2.table+"` ORDER BY a")
		if !reflect.DeepEqual(got, c2.want) {
			t.Errorf("%s holds %v, want %v", c2.table, got, c2.want)
		}
	}

	// Short rows without allowJaggedRows fail, as BigQuery fails them.
	if err := load("not_jagged", "1\n", func(*bigquery.ReaderSource) {}); err == nil {
		t.Error("a short row without allowJaggedRows loaded")
	}
	err := load("empty_string", "1,\n", func(s *bigquery.ReaderSource) { s.NullMarker = `\N` })
	wantReason(t, "an empty STRING value with a nullMarker", err, http.StatusNotImplemented, "notImplemented")
	err = load("empty_int", ",x\n", func(s *bigquery.ReaderSource) { s.NullMarker = `\N` })
	var be *bigquery.Error
	if err == nil || !(errors.As(err, &be) && be.Reason == "invalid" || strings.Contains(err.Error(), "invalid")) {
		t.Errorf("an empty INTEGER value with a nullMarker: %v, want invalid", err)
	}
	for _, table := range []string{"empty_string", "empty_int"} {
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("the failed load made %s", table)
		}
	}
	err = load("two_bytes", "1||x\n", func(s *bigquery.ReaderSource) { s.FieldDelimiter = "||" })
	wantReason(t, "a fieldDelimiter of two characters", err, http.StatusNotImplemented, "notImplemented")

	// autodetect with a schema: skipLeadingRows says how many rows are not
	// data; unset, BigQuery decides from the data, which is 501.
	err = load("auto_schema", "a,b\n1,x\n", func(s *bigquery.ReaderSource) { s.AutoDetect = true })
	wantReason(t, "autodetect with a schema and no skipLeadingRows", err, http.StatusNotImplemented, "notImplemented")
	if err := load("auto_schema", "a,b\n1,x\n", func(s *bigquery.ReaderSource) { s.AutoDetect = true; s.SkipLeadingRows = 1 }); err != nil {
		t.Errorf("autodetect with a schema and skipLeadingRows 1: %v", err)
	} else if got := rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+".auto_schema`"); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(1), "x"}}) {
		t.Errorf("auto_schema holds %v", got)
	}

	// From Cloud Storage.
	bucket := loadBucket(t, h, "bq-opts", map[string]string{"data.tsv": "h\th\n8\tt\n"})
	ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/data.tsv")
	ref.Schema = loadSchema
	ref.FieldDelimiter = "\t"
	ref.SkipLeadingRows = 1
	if err := runLoad(ctx, ds.Table("from_gcs").LoaderFrom(ref)); err != nil {
		t.Fatalf("a tab-delimited load from Cloud Storage: %v", err)
	}
	if got := rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+".from_gcs`"); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(8), "t"}}) {
		t.Errorf("from_gcs holds %v", got)
	}
}

// TestBigQueryCreateSchemaOfAnExistingDataset (#946): CREATE SCHEMA of a
// dataset that exists fails "Already Exists", through jobs.query (409
// duplicate) and jobs.insert (the job failed duplicate); CREATE SCHEMA IF
// NOT EXISTS succeeds and changes nothing. Measured first: the emulator
// ran both as done, and kept the dataset's tables.
func TestBigQueryCreateSchemaOfAnExistingDataset(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	if err := ds.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: loadSchema}); err != nil {
		t.Fatal(err)
	}
	sql := "CREATE SCHEMA " + ds.DatasetID + " OPTIONS(description='changed')"
	wantReason(t, "CREATE SCHEMA of an existing dataset (jobs.query)", bqRun(ctx, c, sql, false), http.StatusConflict, "duplicate")
	job, be := jobError(t, ctx, c, sql)
	if be == nil || be.Reason != "duplicate" || !strings.Contains(be.Message, "Already Exists: Dataset") {
		t.Errorf("CREATE SCHEMA of an existing dataset (jobs.insert): %v, want the job failed duplicate", be)
	}
	if st, err := job.Status(ctx); err != nil || st.Err() == nil {
		t.Errorf("jobs.get of the failed job: %v %v, want it failed", err, st)
	}
	for _, insert := range []bool{false, true} {
		if err := bqRun(ctx, c, "CREATE SCHEMA IF NOT EXISTS "+ds.DatasetID, insert); err != nil {
			t.Errorf("CREATE SCHEMA IF NOT EXISTS (jobs.insert %v): %v", insert, err)
		}
	}
	if !tableExists(t, ctx, ds.Table("t")) {
		t.Error("the dataset's table is gone")
	}
}
