//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// The tests in this file are #933, #935, #936, #938 and #939: BigQuery
// scripts, the text of the jobs the front changes, and extract jobs to
// Cloud Storage, each driven through the official Go client and measured
// against the pinned emulator first (the measurements are in
// internal/bigqueryfront and docs/compatibility.md).

// queryConfig returns a query job's text as jobs.get reports it.
func queryConfig(t *testing.T, ctx context.Context, c *bigquery.Client, id string) string {
	t.Helper()
	job, err := c.JobFromID(ctx, id)
	if err != nil {
		t.Fatalf("jobs.get %s: %v", id, err)
	}
	conf, err := job.Config()
	if err != nil {
		t.Fatalf("job %s: %v", id, err)
	}
	q, ok := conf.(*bigquery.QueryConfig)
	if !ok {
		t.Fatalf("job %s is a %T", id, conf)
	}
	return q.Q
}

// TestBigQueryScriptVariablesEndWithTheScript (#933): a script's variable
// is scoped to the script, as in BigQuery. Measured first: after `DECLARE
// zz INT64 DEFAULT 5; SELECT zz`, the emulator answered a later `SELECT zz
// AS v` with 5, and after `DECLARE w INT64 DEFAULT 9`, a later `SELECT 1
// AS w` failed with a syntax error, the 9 put in the alias's place. The
// script itself reads its variables, and its job shows the client's text.
func TestBigQueryScriptVariablesEndWithTheScript(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	suffix := strings.ReplaceAll(h.Project(), "-", "_")
	zz, w := "zz_"+suffix, "w_"+suffix
	for _, insert := range []bool{false, true} {
		script := "DECLARE " + zz + " INT64 DEFAULT 5; DECLARE " + w + " INT64 DEFAULT " + zz + " + 4; SET " + zz + " = " + zz + " * 2; SELECT " + zz + ", " + w
		if insert {
			job, err := c.Query(script).Run(ctx)
			if err != nil {
				t.Fatalf("jobs.insert: %v", err)
			}
			if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
				t.Fatalf("the script's job: %v %v", err, st)
			}
			if got := queryConfig(t, ctx, c, job.ID()); got != script {
				t.Errorf("jobs.get shows the query %q, want the client's %q", got, script)
			}
		} else if got := rows(t, ctx, c, script); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(10), int64(9)}}) {
			t.Errorf("the script returned %v, want [[10 9]]", got)
		}
		_, err := c.Query("SELECT " + zz + " AS v").Read(ctx)
		if err == nil || !strings.Contains(err.Error(), "Unrecognized name: "+zz) {
			t.Errorf("a later query named the script's variable: %v, want Unrecognized name: %s", err, zz)
		}
		if got := rows(t, ctx, c, "SELECT 1 AS "+w); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(1)}}) {
			t.Errorf("a later query with an alias named as the script's variable returned %v", got)
		}
	}
	_, err := c.Query("DECLARE " + zz + " INT64; SELECT " + zz + " + nope_" + suffix).Read(ctx)
	if err == nil || !strings.Contains(err.Error(), "nope_"+suffix) || strings.Contains(err.Error(), "cbvar_") {
		t.Errorf("an error in a script with a variable: %v", err)
	}
}

// TestBigQueryFailedScriptThatWroteIs501 (#935): BigQuery keeps what the
// statements of a failed script did before the failing one; the emulator
// rolls the whole script back (measured: `CREATE TABLE ds.e1 AS SELECT 1
// AS a; SELECT * FROM nope.nope; ...` failed 400 and ds.e1 did not exist;
// a rolled-back DROP TABLE even left the table unreadable, #955). So such
// a script is 501, naming the emulator's error, through jobs.query and as
// the failed job's error through jobs.insert. A failed script that changes
// nothing kept, and a syntax error, which fails a script before anything
// runs, are answered 400 as before.
func TestBigQueryFailedScriptThatWroteIs501(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	tt := "tt_" + strings.ReplaceAll(h.Project(), "-", "_")
	for i, insert := range []bool{false, true} {
		e1 := "e1_" + string(rune('a'+i))
		script := "CREATE TABLE " + path(e1) + " AS SELECT 1 AS a; SELECT * FROM nope.nope; CREATE TABLE " + path("e2") + " AS SELECT 1 AS a"
		if insert {
			_, be := jobError(t, ctx, c, script)
			if be == nil || be.Reason != "notImplemented" || !strings.Contains(be.Message, "Table not found: nope.nope") {
				t.Errorf("jobs.insert: %v, want the job failed notImplemented naming the emulator's error", be)
			}
		} else {
			err := bqRun(ctx, c, script, false)
			wantReason(t, "jobs.query", err, 501, "notImplemented")
			if err == nil || !strings.Contains(err.Error(), "Table not found: nope.nope") {
				t.Errorf("jobs.query: %v, want the emulator's error named", err)
			}
		}
		if tableExists(t, ctx, ds.Table(e1)) {
			t.Errorf("%s exists after the failed script", e1)
		}
		for _, sql := range []string{
			"SELECT 1; SELECT * FROM nope.nope",
			"CREATE TEMP TABLE " + tt + " AS SELECT 1 AS a; INSERT INTO " + tt + " VALUES (2); SELECT * FROM nope.nope",
			"CREATE TABLE " + path("e3") + " AS SELECT 1 AS a; SELEC 2",
		} {
			err := bqRun(ctx, c, sql, insert)
			var be *bigquery.Error
			if err == nil || strings.Contains(err.Error(), "notImplemented") || errors.As(err, &be) && be.Reason == "notImplemented" {
				t.Errorf("%s: %v, want the emulator's own error", sql, err)
			}
		}
	}
}

// TestBigQueryReplacingATempTableIs501 (#936): measured first, the
// emulator fails a script that replaces or drops a TEMP table it created,
// 400 "failed to delete table spec", where BigQuery runs it. Such a
// script is 501 and nothing of it is run. CREATE OR REPLACE TEMP TABLE of
// a name the script had not used runs.
func TestBigQueryReplacingATempTableIs501(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		for _, sql := range []string{
			"CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE OR REPLACE TEMP TABLE t AS SELECT 2 AS a; SELECT * FROM t",
			"CREATE TEMP TABLE t AS SELECT 1 AS a; DROP TABLE t; CREATE TEMP TABLE t AS SELECT 3 AS a; SELECT * FROM t",
			"CREATE TABLE `" + ds.DatasetID + ".kept` AS SELECT 1 AS a; CREATE TEMP TABLE t (a INT64); DROP TABLE IF EXISTS t",
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), 501, "notImplemented")
		}
		if tableExists(t, ctx, ds.Table("kept")) {
			t.Error("a statement of a script answered 501 was run")
		}
	}
	t2 := "t2_" + strings.ReplaceAll(h.Project(), "-", "_")
	if got := rows(t, ctx, c, "CREATE OR REPLACE TEMP TABLE "+t2+" AS SELECT 1 AS a; SELECT * FROM "+t2); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(1)}}) {
		t.Errorf("CREATE OR REPLACE TEMP TABLE of a new name: %v", got)
	}
}

// TestBigQueryTempTableColumnsAreChecked (#938): the columns of a CREATE
// TEMP TABLE ... AS SELECT whose query names a script variable or a table
// the script made are held to the column rules, 400 invalidQuery, and the
// script is not run. Measured first: the emulator made such a table with
// a column `b!`. A TEMP table with names BigQuery takes is made and read;
// after a statement that changes data, the columns are not read, 501.
func TestBigQueryTempTableColumnsAreChecked(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		for _, sql := range []string{
			"DECLARE x INT64 DEFAULT 1; CREATE TEMP TABLE tt AS SELECT x AS `b!`; SELECT * FROM tt",
			"CREATE TEMP TABLE s AS SELECT 1 AS a; CREATE TEMP TABLE tt AS SELECT STRUCT(a AS `c?`) AS r FROM s; SELECT * FROM tt",
		} {
			err := bqRun(ctx, c, sql, insert)
			if insert {
				var be *bigquery.Error
				if !errors.As(err, &be) || be.Reason != "invalidQuery" {
					wantReason(t, sql, err, 400, "invalidQuery")
				}
			} else {
				wantReason(t, sql, err, 400, "invalidQuery")
			}
		}
		sql := "CREATE TABLE `" + ds.DatasetID + ".src` AS SELECT 1 AS a; CREATE TEMP TABLE tt AS SELECT a FROM `" + ds.DatasetID + ".src`"
		wantReason(t, "after a statement that changes data", bqRun(ctx, c, sql, insert), 501, "notImplemented")
		if tableExists(t, ctx, ds.Table("src")) {
			t.Error("a statement of a script answered 501 was run")
		}
	}
	// Names of this run's own: a failed script leaves its TEMP tables in
	// the emulator's catalog (#955).
	s1, t1 := "s_"+strings.ReplaceAll(h.Project(), "-", "_"), "tt_ok_"+strings.ReplaceAll(h.Project(), "-", "_")
	got := rows(t, ctx, c, "DECLARE x INT64 DEFAULT 4; CREATE TEMP TABLE "+s1+" AS SELECT x AS a; CREATE TEMP TABLE "+t1+" AS SELECT a + x AS b FROM "+s1+"; SELECT b FROM "+t1)
	if !reflect.DeepEqual(got, [][]bigquery.Value{{int64(8)}}) {
		t.Errorf("a TEMP table with names BigQuery takes: %v, want [[8]]", got)
	}
}

// TestBigQueryReplacedTableJobShowsTheClientsQuery (#939): the front
// carries out a lone CREATE OR REPLACE TABLE of an existing table by
// sending the emulator another query (#918). Measured first: the job's
// configuration.query.query, in jobs.insert's answer and jobs.get, was
// that query, "... AS SELECT * FROM `ds._cloudburrow_replace_...`". Both
// show the client's. (The emulator's jobs.list gives no configuration.)
func TestBigQueryReplacedTableJobShowsTheClientsQuery(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".rep` AS SELECT 1 AS a", false); err != nil {
		t.Fatal(err)
	}
	sql := "CREATE OR REPLACE TABLE `" + ds.DatasetID + ".rep` AS SELECT 2 AS a"
	job, err := c.Query(sql).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("the replace: %v %v", err, st)
	}
	if conf, err := job.Config(); err != nil || conf.(*bigquery.QueryConfig).Q != sql {
		t.Errorf("jobs.insert answered with %v %v", conf, err)
	}
	if got := queryConfig(t, ctx, c, job.ID()); got != sql {
		t.Errorf("jobs.get shows %q, want %q", got, sql)
	}
	if got := rows(t, ctx, c, "SELECT a FROM `"+ds.DatasetID+".rep`"); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(2)}}) {
		t.Errorf("the replaced table holds %v", got)
	}
}

// instanceStorage returns a Cloud Storage client of the instance's own
// Cloud Storage, or skips the test when the instance has none.
func instanceStorage(t *testing.T, h *Harness) *storage.Client {
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
	return storageClient(t, h)
}

// TestBigQueryExtractToCloudStorage (#939): an extract job writes to the
// instance's own Cloud Storage (the emulator is given it as
// STORAGE_EMULATOR_HOST, never storage.googleapis.com), and the file is
// BigQuery's: measured first, the emulator failed an extract with no
// destinationFormat (BigQuery's default is CSV), named the file of a
// wildcard URI "out-*.csv", created a bucket that did not exist, ignored
// compression and fieldDelimiter, wrote JSON values all as strings, wrote
// a view and a nested schema to CSV, and a TIMESTAMP in a form not
// BigQuery's. Each of those is now written as BigQuery writes it, refused
// as BigQuery refuses it (400, 404), or 501, with nothing written.
func TestBigQueryExtractToCloudStorage(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-extract")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	missing := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-missing")
	t.Cleanup(func() {
		for _, b := range []*storage.BucketHandle{bucket, missing} {
			it := b.Objects(context.Background(), nil)
			for {
				o, err := it.Next()
				if err != nil {
					break
				}
				_ = b.Object(o.Name).Delete(context.Background())
			}
			_ = b.Delete(context.Background())
		}
	})
	for _, sql := range []string{
		"CREATE TABLE `" + ds.DatasetID + ".src` AS SELECT 1 AS a, 'x,y' AS b, CAST(NULL AS STRING) AS n, TRUE AS bo, b'ab' AS by_, DATE '2020-01-02' AS d, NUMERIC '1.25' AS nu " +
			"UNION ALL SELECT 2, 'q\"r', 'z', FALSE, NULL, NULL, NULL",
		"CREATE TABLE `" + ds.DatasetID + ".nested` AS SELECT 1 AS a, STRUCT(2 AS b) AS s",
		"CREATE TABLE `" + ds.DatasetID + ".ts` AS SELECT TIMESTAMP '2020-01-02 03:04:05' AS ts_col",
		"CREATE TABLE `" + ds.DatasetID + ".empty` (a INT64)",
		"CREATE VIEW `" + ds.DatasetID + ".v` AS SELECT 1 AS a",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	extract := func(table string, set func(*bigquery.GCSReference, *bigquery.Extractor), uris ...string) (*bigquery.Job, error) {
		ref := bigquery.NewGCSReference(uris...)
		e := ds.Table(table).ExtractorTo(ref)
		if set != nil {
			set(ref, e)
		}
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := e.Run(ectx)
		if err != nil {
			return nil, err
		}
		st, err := job.Wait(ectx)
		if err == nil {
			err = st.Err()
		}
		return job, err
	}
	uri := func(name string) string { return "gs://" + bucket.BucketName() + "/" + name }
	read := func(name string) string {
		t.Helper()
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			return ""
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}
	const want = "a,b,n,bo,by_,d,nu\n1,\"x,y\",,true,YWI=,2020-01-02,1.25\n2,\"q\"\"r\",z,false,,,\n"

	// CSV, the default, and a wildcard URI.
	if _, err := extract("src", nil, uri("plain.csv")); err != nil {
		t.Fatalf("a CSV extract: %v", err)
	}
	if got := read("plain.csv"); got != want {
		t.Errorf("the CSV extract wrote %q, want %q", got, want)
	}
	job, err := extract("src", nil, uri("out-*.csv"))
	if err != nil {
		t.Fatalf("an extract to a wildcard URI: %v", err)
	}
	if got := read("out-000000000000.csv"); got != want {
		t.Errorf("the wildcard extract wrote %q, want %q", got, want)
	}
	j, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if conf, err := j.Config(); err != nil || !reflect.DeepEqual(conf.(*bigquery.ExtractConfig).Dst.URIs, []string{uri("out-*.csv")}) {
		t.Errorf("jobs.get shows the extract's URIs as %v %v, want the client's", conf, err)
	}
	if _, err := extract("src", func(_ *bigquery.GCSReference, e *bigquery.Extractor) { e.DisableHeader = true }, uri("nohdr.csv")); err != nil {
		t.Fatal(err)
	}
	if got := read("nohdr.csv"); got != strings.TrimPrefix(want, "a,b,n,bo,by_,d,nu\n") {
		t.Errorf("an extract without a header wrote %q", got)
	}
	if _, err := extract("empty", func(_ *bigquery.GCSReference, e *bigquery.Extractor) { e.DisableHeader = true }, uri("empty.csv")); err != nil {
		t.Errorf("an extract of an empty table without a header: %v", err)
	}

	// Refused as BigQuery refuses them.
	_, err = extract("v", nil, uri("view.csv"))
	wantReason(t, "an extract of a view", err, 400, "invalid")
	_, err = extract("nested", nil, uri("nested.csv"))
	wantReason(t, "a CSV extract of a nested schema", err, 400, "invalid")
	_, err = extract("src", nil, "gs://"+missing.BucketName()+"/x.csv")
	wantReason(t, "an extract to a bucket that does not exist", err, 404, "notFound")
	if _, err := missing.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the missing bucket: %v, want it still missing", err)
	}

	// Not implemented: nothing is written.
	for name, set := range map[string]func(*bigquery.GCSReference, *bigquery.Extractor){
		"out.json":    func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.DestinationFormat = bigquery.JSON },
		"out.avro":    func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.DestinationFormat = bigquery.Avro },
		"out.parquet": func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.DestinationFormat = bigquery.Parquet },
		"out.csv.gz":  func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.Compression = bigquery.Gzip },
		"out.tsv":     func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.FieldDelimiter = "\t" },
	} {
		_, err := extract("src", set, uri(name))
		wantReason(t, "an extract to "+name, err, 501, "notImplemented")
	}
	_, err = extract("ts", nil, uri("ts.csv"))
	wantReason(t, "a CSV extract of a TIMESTAMP", err, 501, "notImplemented")
	_, err = extract("empty", nil, uri("empty-header.csv"))
	wantReason(t, "a CSV extract of an empty table with a header", err, 501, "notImplemented")
	_, err = extract("src", nil, uri("m1-*.csv"), uri("m2-*.csv"))
	wantReason(t, "an extract to two URIs", err, 501, "notImplemented")

	var names []string
	it := bucket.Objects(ctx, nil)
	for {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, o.Name)
	}
	if wantNames := []string{"empty.csv", "nohdr.csv", "out-000000000000.csv", "plain.csv"}; !reflect.DeepEqual(names, wantNames) {
		t.Errorf("the bucket holds %v, want %v", names, wantNames)
	}
}
