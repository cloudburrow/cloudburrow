//go:build compat

package compat

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// The tests in this file are #955, #956, #957 and #958: a failed script's
// effect on the emulator's catalog, script variables of the name of an
// alias or a column, the extracts the front writes itself, and jobs.list's
// configurations, each driven through the official Go client and measured
// against the pinned emulator first (the measurements are in
// internal/bigqueryfront and docs/compatibility.md).

// TestBigQueryScriptVariableNamedAsAnAlias (#956): measured first, the
// emulator replaced every bare word of a variable's name in its script
// with its value, so `DECLARE n INT64 DEFAULT 1; SELECT 2 AS n` failed
// "Syntax error: Unexpected integer literal "1"". The front renames only
// the references to a variable: an alias, an INSERT's column list and a
// column of a subquery of another name keep their names. A statement where
// BigQuery may read a column or an alias of the variable's name instead is
// 501, and nothing is run.
func TestBigQueryScriptVariableNamedAsAnAlias(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := "`" + ds.DatasetID + ".vt`"
	if err := bqRun(ctx, c, "CREATE TABLE "+path+" (a INT64, n INT64)", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		for sql, want := range map[string][][]bigquery.Value{
			"DECLARE n INT64 DEFAULT 1; SELECT 2 AS n":                          {{int64(2)}},
			"DECLARE n INT64 DEFAULT 1; SELECT n + 1 AS m, 3 n2":                {{int64(2), int64(3)}},
			"DECLARE n INT64 DEFAULT 1; SELECT n, m FROM (SELECT 7 AS m) AS t":  {{int64(1), int64(7)}},
			"DECLARE s STRUCT<n INT64> DEFAULT STRUCT(5 AS n); SELECT s.n AS n": {{int64(5)}},
		} {
			if insert {
				job, err := c.Query(sql).Run(ctx)
				if err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				it, err := job.Read(ctx)
				if err != nil {
					t.Errorf("%s: jobs.insert: %v", sql, err)
					continue
				}
				var got [][]bigquery.Value
				for {
					var row []bigquery.Value
					if err := it.Next(&row); errors.Is(err, iterator.Done) {
						break
					} else if err != nil {
						t.Fatalf("%s: %v", sql, err)
					}
					got = append(got, row)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: jobs.insert returned %v, want %v", sql, got, want)
				}
			} else if got := rows(t, ctx, c, sql); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: returned %v, want %v", sql, got, want)
			}
		}
		for _, sql := range []string{
			"DECLARE n INT64 DEFAULT 1; SELECT n FROM (SELECT 7 AS n)",
			"DECLARE n INT64 DEFAULT 1; SELECT a FROM " + path + " WHERE a = n",
			"DECLARE n INT64 DEFAULT 1; INSERT INTO " + path + " (a) SELECT n FROM " + path,
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), 501, "notImplemented")
		}
	}
	sql := "DECLARE n INT64 DEFAULT 3; INSERT INTO " + path + " (a, n) VALUES (n, 4)"
	if err := bqRun(ctx, c, sql, false); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if got := rows(t, ctx, c, "SELECT a, n FROM "+path); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(3), int64(4)}}) {
		t.Errorf("an INSERT naming a column as the variable wrote %v, want [[3 4]]", got)
	}
}

// TestBigQueryFailedScriptLeavesTheCatalogInStep (#955): measured first,
// a failed script left the emulator's SQL catalog, or its list of tables,
// out of step with its tables, for every later query. Through jobs.query,
// which the emulator rolls back: a rolled-back DROP TABLE left a table
// listed and not found by a query; a rolled-back CREATE TABLE ... AS
// SELECT left one found and failing "sqlite3: SQL logic error: no such
// table"; a rolled-back CREATE TEMP TABLE left its columns for the next
// TEMP table of its name. Through a query job, which the emulator commits
// up to the failing statement, as BigQuery keeps it: a dropped table was
// still listed, and a table made was not. The front puts both back after
// the failure: through jobs.query, each table reads as before the script
// (and the script is 501, #935); through a query job, as the statements
// before the failing one left it, as in BigQuery, and the job fails with
// the emulator's error.
func TestBigQueryFailedScriptLeavesTheCatalogInStep(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		mode := map[bool]string{false: "q", true: "j"}[insert]
		p := func(table string) string { return "`" + ds.DatasetID + "." + table + "_" + mode + "`" }
		for _, sql := range []string{
			"CREATE TABLE " + p("keep") + " AS SELECT 1 AS a, STRUCT(2 AS b) AS s, [3, 4] AS arr",
			"CREATE TABLE " + p("keep2") + " AS SELECT 5 AS a",
			"CREATE VIEW " + p("view") + " AS SELECT 6 AS a",
		} {
			if err := bqRun(ctx, c, sql, false); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		ghost := "ghost_" + mode + "_" + strings.ReplaceAll(h.Project(), "-", "_")
		for _, sql := range []string{
			"DROP TABLE " + p("keep") + "; SELECT * FROM nope.nope",
			"DROP TABLE " + p("keep2") + "; CREATE TABLE " + p("keep2") + " AS SELECT 'z' AS z; SELECT * FROM nope.nope",
			"DROP VIEW " + p("view") + "; SELECT * FROM nope.nope",
			"CREATE TABLE " + p("c1") + " AS SELECT 1 AS a; SELECT * FROM nope.nope",
			"CREATE TEMP TABLE " + ghost + " AS SELECT 1 AS a; SELECT * FROM nope.nope",
		} {
			err := bqRun(ctx, c, sql, insert)
			if err == nil || !strings.Contains(err.Error(), "Table not found: nope.nope") {
				t.Fatalf("%s: %v, want the emulator's error", sql, err)
			}
			if insert && strings.Contains(err.Error(), "notImplemented") {
				t.Errorf("%s: %v, want the query job failed with the emulator's error", sql, err)
			}
		}
		want := map[string][][]bigquery.Value{
			"CREATE TEMP TABLE " + ghost + " AS SELECT 2 AS b; SELECT b FROM " + ghost: {{int64(2)}},
		}
		gone := []string{"c1"}
		if insert {
			want["SELECT z FROM "+p("keep2")] = [][]bigquery.Value{{"z"}}
			want["SELECT a FROM "+p("c1")] = [][]bigquery.Value{{int64(1)}}
			gone = []string{"keep", "view"}
		} else {
			want["SELECT a, s.b, arr FROM "+p("keep")] = [][]bigquery.Value{{int64(1), int64(2), []bigquery.Value{int64(3), int64(4)}}}
			want["SELECT a FROM "+p("keep2")] = [][]bigquery.Value{{int64(5)}}
			want["SELECT a FROM "+p("view")] = [][]bigquery.Value{{int64(6)}}
		}
		for sql, w := range want {
			if got := rows(t, ctx, c, sql); !reflect.DeepEqual(got, w) {
				t.Errorf("after the failed scripts (%s), %s returned %v, want %v", mode, sql, got, w)
			}
		}
		for _, name := range gone {
			table := ds.Table(name + "_" + mode)
			if tableExists(t, ctx, table) {
				t.Errorf("after the failed scripts (%s), %s is still listed", mode, table.TableID)
			}
			_, err := c.Query("SELECT * FROM " + p(name)).Read(ctx)
			if err == nil || strings.Contains(err.Error(), "no such table") || !strings.Contains(err.Error(), "not found") {
				t.Errorf("after the failed scripts (%s), a query of %s: %v, want not found", mode, table.TableID, err)
			}
		}
		listed := map[string]string{"keep2": "a"} // table → its first column, as tables.get lists it
		if insert {
			listed = map[string]string{"keep2": "z", "c1": "a"}
		}
		for name, col := range listed {
			md, err := ds.Table(name + "_" + mode).Metadata(ctx)
			if err != nil {
				t.Errorf("after the failed scripts (%s), %s: %v", mode, name, err)
			} else if got := md.Schema[0].Name; got != col {
				t.Errorf("after the failed scripts (%s), %s is listed with the column %s, want %s", mode, name, got, col)
			}
		}
		// A table of the name of one the script made and lost is made.
		if !insert {
			if got := rows(t, ctx, c, "CREATE TABLE "+p("c1")+" AS SELECT 7 AS z; SELECT z FROM "+p("c1")); !reflect.DeepEqual(got, [][]bigquery.Value{{int64(7)}}) {
				t.Errorf("CREATE TABLE of the name after the failed script: %v", got)
			}
		}
	}
}

// TestBigQueryExtractsTheFrontWrites (#957): measured first, the emulator
// wrote NEWLINE_DELIMITED_JSON values all as JSON strings, ignored GZIP
// and fieldDelimiter, and wrote an empty table with a header as an empty
// object. The front writes these files itself to the instance's own Cloud
// Storage, as BigQuery documents them: the CSV bytes are the emulator's
// comma-separated ones (TestBigQueryExtractToCloudStorage), with the
// delimiter, or compressed; JSON has INT64 values as strings and <, > and
// & in unicode notation; the empty table's file is its header row. The
// job is the front's, read back by jobs.get and jobs.list. A JSON row with
// a NULL is 501, as BigQuery's documentation does not give its form.
func TestBigQueryExtractsTheFrontWrites(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-write")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
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
		"CREATE TABLE `" + ds.DatasetID + ".src` AS SELECT 1 AS a, 'x,y' AS b, CAST(NULL AS STRING) AS n, TRUE AS bo, b'ab' AS by_, DATE '2020-01-02' AS d, NUMERIC '1.25' AS nu " +
			"UNION ALL SELECT 2, 'q\"r', 'z', FALSE, NULL, NULL, NULL",
		"CREATE TABLE `" + ds.DatasetID + ".js` AS SELECT 1 AS a, 'x<y>&z' AS s UNION ALL SELECT 9007199254740993, 'q\"r'",
		"CREATE TABLE `" + ds.DatasetID + ".jsnull` AS SELECT 1 AS a, CAST(NULL AS STRING) AS s",
		"CREATE TABLE `" + ds.DatasetID + ".empty` (a INT64, b STRING)",
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
	read := func(name string, gz bool) string {
		t.Helper()
		r, err := bucket.Object(name).ReadCompressed(true).NewReader(ctx)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			return ""
		}
		defer r.Close()
		var rd io.Reader = r
		if gz {
			zr, err := gzip.NewReader(r)
			if err != nil {
				t.Errorf("%s is not gzip: %v", name, err)
				return ""
			}
			rd = zr
		}
		b, _ := io.ReadAll(rd)
		return string(b)
	}
	// The emulator's comma-separated CSV of src, measured to be BigQuery's
	// (TestBigQueryExtractToCloudStorage).
	const csvWant = "a,b,n,bo,by_,d,nu\n1,\"x,y\",,true,YWI=,2020-01-02,1.25\n2,\"q\"\"r\",z,false,,,\n"

	if _, err := extract("src", func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.Compression = bigquery.Gzip }, uri("src-*.csv.gz")); err != nil {
		t.Fatalf("a GZIP CSV extract: %v", err)
	}
	if got := read("src-000000000000.csv.gz", true); got != csvWant {
		t.Errorf("the GZIP CSV extract holds %q, want %q", got, csvWant)
	}
	if _, err := extract("src", func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.FieldDelimiter = "\t" }, uri("src.tsv")); err != nil {
		t.Fatalf("a CSV extract with a tab: %v", err)
	}
	if got, want := read("src.tsv", false), "a\tb\tn\tbo\tby_\td\tnu\n1\tx,y\t\ttrue\tYWI=\t2020-01-02\t1.25\n2\t\"q\"\"r\"\tz\tfalse\t\t\t\n"; got != want {
		t.Errorf("the tab-separated extract holds %q, want %q", got, want)
	}
	if _, err := extract("empty", nil, uri("empty.csv")); err != nil {
		t.Fatalf("an extract of an empty table with a header: %v", err)
	}
	if got := read("empty.csv", false); got != "a,b\n" {
		t.Errorf("the empty table's extract holds %q, want its header row", got)
	}
	jsonWant := "{\"a\":\"1\",\"s\":\"x\\u003cy\\u003e\\u0026z\"}\n{\"a\":\"9007199254740993\",\"s\":\"q\\\"r\"}\n"
	job, err := extract("js", func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.DestinationFormat = bigquery.JSON }, uri("js.json"))
	if err != nil {
		t.Fatalf("a JSON extract: %v", err)
	}
	if got := read("js.json", false); got != jsonWant {
		t.Errorf("the JSON extract holds %q, want %q", got, jsonWant)
	}
	if _, err := extract("js", func(r *bigquery.GCSReference, _ *bigquery.Extractor) {
		r.DestinationFormat, r.Compression = bigquery.JSON, bigquery.Gzip
	}, uri("js.json.gz")); err != nil {
		t.Fatalf("a GZIP JSON extract: %v", err)
	}
	if got := read("js.json.gz", true); got != jsonWant {
		t.Errorf("the GZIP JSON extract holds %q, want %q", got, jsonWant)
	}

	// The job is the front's: jobs.get and jobs.list read it back.
	j, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if conf, err := j.Config(); err != nil || conf.(*bigquery.ExtractConfig).Dst.DestinationFormat != bigquery.JSON {
		t.Errorf("jobs.get of the JSON extract: %v %v", conf, err)
	}
	if st := j.LastStatus(); st == nil || st.State != bigquery.Done || st.Err() != nil {
		t.Errorf("jobs.get of the JSON extract: status %+v", st)
	}
	found := false
	it := c.Jobs(ctx)
	for {
		lj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if lj.ID() != job.ID() {
			continue
		}
		found = true
		if conf, err := lj.Config(); err != nil || conf == nil {
			t.Errorf("jobs.list of the JSON extract: config %v %v", conf, err)
		}
	}
	if !found {
		t.Errorf("jobs.list does not list the extract %s", job.ID())
	}

	// Not implemented: nothing is written.
	_, err = extract("jsnull", func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.DestinationFormat = bigquery.JSON }, uri("jsnull.json"))
	wantReason(t, "a JSON extract of a NULL", err, 501, "notImplemented")
	_, err = extract("src", func(r *bigquery.GCSReference, _ *bigquery.Extractor) { r.DestinationFormat = bigquery.JSON }, uri("src.json"))
	wantReason(t, "a JSON extract of a BOOL", err, 501, "notImplemented")
	for _, name := range []string{"jsnull.json", "src.json"} {
		if _, err := bucket.Object(name).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("%s: %v, want it not written", name, err)
		}
	}
}

// TestBigQueryJobListGivesConfigurations (#958): measured first, the
// emulator's jobs.list gave no job's configuration, whatever the
// projection, so the Go client's Job.Config() of a listed job was nil.
// Listed jobs now have their configuration: a query job's text as the
// client wrote it, from jobs.insert and from jobs.query.
func TestBigQueryJobListGivesConfigurations(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	suffix := strings.ReplaceAll(h.Project(), "-", "_")
	inserted := "SELECT 1 AS listed_" + suffix
	job, err := c.Query(inserted).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	queried := "DECLARE v INT64 DEFAULT 2; SELECT v AS queried_" + suffix
	qit, err := c.Query(queried).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{job.ID(): inserted}
	if qit.SourceJob() != nil {
		want[qit.SourceJob().ID()] = queried
	}
	got := map[string]string{}
	it := c.Jobs(ctx)
	for {
		lj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := want[lj.ID()]; !ok {
			continue
		}
		conf, err := lj.Config()
		if q, ok := conf.(*bigquery.QueryConfig); err == nil && ok {
			got[lj.ID()] = q.Q
		} else {
			got[lj.ID()] = "no query configuration"
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("jobs.list gave the configurations %v, want %v", got, want)
	}
}
