//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// Writes the emulator stored as other data, silently (#1065, #1066,
// #1067). Each was measured first through the front with this client, on
// the pinned image: a BYTES value "/w==" (0xff) loaded by JSON and by CSV,
// and put with Inserter.Put, read back 2f773d3d, its base64 text; a NaN
// from a JSON load, a CSV load, a query parameter and IEEE_DIVIDE(0, 0)
// read back NULL; and WRITE_TRUNCATE_DATA of a JSON or CSV load, an
// upload or from Cloud Storage, appended the rows to the table's.

func jsonSource(data string, schema bigquery.Schema) *bigquery.ReaderSource {
	s := bigquery.NewReaderSource(strings.NewReader(data))
	s.SourceFormat = bigquery.JSON
	s.Schema = schema
	return s
}

func csvSource(data string, schema bigquery.Schema) *bigquery.ReaderSource {
	s := bigquery.NewReaderSource(strings.NewReader(data))
	s.SourceFormat = bigquery.CSV
	s.Schema = schema
	return s
}

func gcsSource(uri string, format bigquery.DataFormat, schema bigquery.Schema) *bigquery.GCSReference {
	g := bigquery.NewGCSReference(uri)
	g.SourceFormat = format
	g.Schema = schema
	return g
}

// runQueryJob runs sql as a query job with q's settings and returns the
// error of jobs.insert or of the job.
func runQueryJob(ctx context.Context, q *bigquery.Query) error {
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	j, err := q.Run(qctx)
	if err != nil {
		return err
	}
	st, err := j.Wait(qctx)
	if err != nil {
		return err
	}
	return st.Err()
}

// TestBigQueryBytesValuesAreWrittenAsTheirBytes (#1065, #1075): a BYTES
// value, base64 in a JSON load, a CSV load and a streamed row, is stored
// as the bytes it encodes, at the top level, in a RECORD and in a REPEATED
// column, from an upload and from Cloud Storage, bytes that are not UTF-8
// text (0xff) and a carriage return in a CSV value included: the emulator
// CloudBurrow builds decodes the base64 itself (#1061), where the pinned
// one stored the base64 text and the front could hand it only UTF-8 text
// (501 for the rest, #1074). A JSON value that is not base64 fails the
// load, 400 invalid, and writes nothing.
func TestBigQueryBytesValuesAreWrittenAsTheirBytes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "b", Type: bigquery.BytesFieldType},
		{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.BytesFieldType}}},
		{Name: "a", Type: bigquery.BytesFieldType, Repeated: true},
	}
	flat := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "b", Type: bigquery.BytesFieldType}}
	bucket := loadBucket(t, h, "bq-bytes", map[string]string{
		"ok.json":  `{"id":2,"b":"YWJj"}` + "\n",
		"ff.json":  `{"id":12,"b":"/w=="}` + "\n",
		"ok.csv":   "12,4piD\n",
		"ff.csv":   "21,/w==\n",
		"bad.json": `{"id":99,"b":"not base64!"}` + "\n",
	})
	uri := func(name string) string { return "gs://" + bucket.BucketName() + "/" + name }

	j := ds.Table("j")
	if err := runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":1,"b":"YQBi","r":{"x":"4piD"},"a":["YWJj","DQo=","AA=="]}`+"\n", schema))); err != nil {
		t.Fatalf("JSON upload: %v", err)
	}
	if err := runLoad(ctx, j.LoaderFrom(gcsSource(uri("ok.json"), bigquery.JSON, schema))); err != nil {
		t.Fatalf("JSON load from Cloud Storage: %v", err)
	}
	cs := ds.Table("c")
	if err := runLoad(ctx, cs.LoaderFrom(csvSource("11,YWJj\n", flat))); err != nil {
		t.Fatalf("CSV upload: %v", err)
	}
	if err := runLoad(ctx, cs.LoaderFrom(gcsSource(uri("ok.csv"), bigquery.CSV, flat))); err != nil {
		t.Fatalf("CSV load from Cloud Storage: %v", err)
	}
	if err := cs.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: flat, Row: []bigquery.Value{13, []byte("a\x00b")}}}); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	// Bytes that are not UTF-8 text, and a carriage return in CSV.
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"JSON upload of 0xff", runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":9,"b":"/w=="}`+"\n", schema)))},
		{"JSON upload of 0xff in a RECORD", runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":10,"r":{"x":"/w=="}}`+"\n", schema)))},
		{"JSON upload of 0xff in a REPEATED column", runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":11,"a":["YQ==","/w=="]}`+"\n", schema)))},
		{"JSON load of 0xff from Cloud Storage", runLoad(ctx, j.LoaderFrom(gcsSource(uri("ff.json"), bigquery.JSON, schema)))},
		{"CSV upload of 0xff", runLoad(ctx, cs.LoaderFrom(csvSource("19,/w==\n", flat)))},
		{"CSV upload of a carriage return", runLoad(ctx, cs.LoaderFrom(csvSource("20,DQo=\n", flat)))},
		{"CSV load of 0xff from Cloud Storage", runLoad(ctx, cs.LoaderFrom(gcsSource(uri("ff.csv"), bigquery.CSV, flat)))},
		{"Inserter.Put of 0xff", cs.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: flat, Row: []bigquery.Value{22, []byte{0xff}}}})},
	} {
		if tc.err != nil {
			t.Errorf("%s: %v", tc.what, tc.err)
		}
	}
	if got, want := q("SELECT id, TO_HEX(b), TO_HEX(r.x), ARRAY(SELECT TO_HEX(e) FROM UNNEST(a) e) FROM DS.j ORDER BY id"),
		"[[1 610062 e29883 [616263 0d0a 00]] [2 616263 <nil> []] [9 ff <nil> []] [10 <nil> ff []] [11 <nil> <nil> [61 ff]] [12 ff <nil> []]]"; got != want {
		t.Errorf("JSON loads read back\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT id, TO_HEX(b) FROM DS.c ORDER BY id"),
		"[[11 616263] [12 e29883] [13 610062] [19 ff] [20 0d0a] [21 ff] [22 ff]]"; got != want {
		t.Errorf("CSV loads and the streamed rows read back\n got %s\nwant %s", got, want)
	}

	wantReason(t, "JSON load of a value that is not base64", runLoad(ctx, j.LoaderFrom(gcsSource(uri("bad.json"), bigquery.JSON, schema))),
		400, "invalid")
	if got, want := q("SELECT COUNT(*) FROM DS.j WHERE id = 99"), "[[0]]"; got != want {
		t.Errorf("the refused JSON load wrote rows: %s", got)
	}
}

// TestBigQueryNaNIsWrittenAndReadBack (#1066): a NaN in a FLOAT64 column is
// written and read back NaN (IS_NAN true) through every write: a JSON load
// (top level, RECORD, REPEATED; an upload and from Cloud Storage), a CSV
// load (upload and Cloud Storage), a streamed row (the generated client:
// the Go client cannot marshal a NaN), a query parameter of a DML
// statement, and IEEE_DIVIDE(0, 0) in an INSERT; and a NaN parameter and
// IEEE_DIVIDE(0, 0) read NaN in a query. The pinned emulator's engine
// stored and computed every NaN as NULL (SQLite makes a NaN NULL), so the
// front answered these writes 501 (#1074); the engine CloudBurrow builds
// keeps a NaN (#1061). ±Infinity is written and reads back through each
// path.
func TestBigQueryNaNIsWrittenAndReadBack(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "f", Type: bigquery.FloatFieldType},
		{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "g", Type: bigquery.FloatFieldType}}},
		{Name: "a", Type: bigquery.FloatFieldType, Repeated: true},
	}
	flat := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "f", Type: bigquery.FloatFieldType}}
	bucket := loadBucket(t, h, "bq-nan", map[string]string{
		"nan.json": `{"id":12,"f":"NaN"}` + "\n",
		"nan.csv":  "14,nan\n",
		"inf.json": `{"id":2,"f":"-Infinity"}` + "\n",
		"inf.csv":  "4,-inf\n",
	})
	uri := func(name string) string { return "gs://" + bucket.BucketName() + "/" + name }
	tbl := ds.Table("fl")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	param := func(sql string, v float64) *bigquery.Query {
		qq := c.Query(strings.ReplaceAll(sql, "DS.", ds.DatasetID+"."))
		qq.Parameters = []bigquery.QueryParameter{{Name: "p", Value: v}}
		return qq
	}
	svc := generatedBigQuery(t, h)
	insertAll := func(id int, f string) error {
		_, err := svc.Tabledata.InsertAll(project, ds.DatasetID, "fl", &bq.TableDataInsertAllRequest{
			Rows: []*bq.TableDataInsertAllRequestRows{{Json: map[string]bq.JsonValue{"id": id, "f": f}}},
		}).Context(ctx).Do()
		return err
	}

	for _, tc := range []struct {
		what string
		err  error
	}{
		{"JSON upload", runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":9,"f":"NaN"}`+"\n", schema)))},
		{"JSON upload in a RECORD", runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":10,"r":{"g":"nan"}}`+"\n", schema)))},
		{"JSON upload in a REPEATED column", runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":11,"a":[1.5,"NaN"]}`+"\n", schema)))},
		{"JSON load from Cloud Storage", runLoad(ctx, tbl.LoaderFrom(gcsSource(uri("nan.json"), bigquery.JSON, schema)))},
		{"CSV upload", runLoad(ctx, ds.Table("fc").LoaderFrom(csvSource("13,NaN\n", flat)))},
		{"CSV load from Cloud Storage", runLoad(ctx, ds.Table("fc").LoaderFrom(gcsSource(uri("nan.csv"), bigquery.CSV, flat)))},
		{"tabledata.insertAll", insertAll(15, "NaN")},
		{"INSERT with a NaN parameter", runQueryJob(ctx, param("INSERT INTO DS.fl (id, f) VALUES (16, @p)", math.NaN()))},
		{"INSERT of IEEE_DIVIDE(0, 0)", runQueryJob(ctx, param("INSERT INTO DS.fl (id, f) VALUES (17, IEEE_DIVIDE(0, 0) + @p)", 0))},
	} {
		if tc.err != nil {
			t.Errorf("%s: %v", tc.what, tc.err)
		}
	}
	if got, want := q("SELECT id, IS_NAN(f), IS_NAN(r.g), ARRAY(SELECT IS_NAN(e) FROM UNNEST(a) e) FROM DS.fl WHERE id BETWEEN 9 AND 17 ORDER BY id"),
		"[[9 true <nil> []] [10 <nil> true []] [11 <nil> <nil> [false true]] [12 true <nil> []] [15 true <nil> []] [16 true <nil> []] [17 true <nil> []]]"; got != want {
		t.Errorf("NaN read back\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT id, IS_NAN(f) FROM DS.fc ORDER BY id"), "[[13 true] [14 true]]"; got != want {
		t.Errorf("NaN from CSV read back\n got %s\nwant %s", got, want)
	}
	for _, qq := range []*bigquery.Query{param("SELECT @p", math.NaN()), c.Query("SELECT IEEE_DIVIDE(0, 0)")} {
		it, err := qq.Read(ctx)
		var row []bigquery.Value
		if err == nil {
			err = it.Next(&row)
		}
		if f, ok := func() (float64, bool) {
			if len(row) != 1 {
				return 0, false
			}
			f, ok := row[0].(float64)
			return f, ok
		}(); err != nil || !ok || !math.IsNaN(f) {
			t.Errorf("%s read %v %v, want NaN", qq.Q, row, err)
		}
	}

	// ±Infinity through each path.
	if err := runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":1,"f":"Infinity"}`+"\n", schema))); err != nil {
		t.Errorf("JSON upload of ±Infinity: %v", err)
	}
	if err := runLoad(ctx, tbl.LoaderFrom(gcsSource(uri("inf.json"), bigquery.JSON, schema))); err != nil {
		t.Errorf("JSON load of -Infinity from Cloud Storage: %v", err)
	}
	if err := runLoad(ctx, tbl.LoaderFrom(csvSource("3,inf\n", flat))); err != nil {
		t.Errorf("CSV upload of inf: %v", err)
	}
	if err := runLoad(ctx, tbl.LoaderFrom(gcsSource(uri("inf.csv"), bigquery.CSV, flat))); err != nil {
		t.Errorf("CSV load of -inf from Cloud Storage: %v", err)
	}
	if err := insertAll(18, "Infinity"); err != nil {
		t.Errorf("tabledata.insertAll of Infinity: %v", err)
	}
	if _, err := svc.Tabledata.InsertAll(project, ds.DatasetID, "fl", &bq.TableDataInsertAllRequest{
		Rows: []*bq.TableDataInsertAllRequestRows{{Json: map[string]bq.JsonValue{"id": 5, "f": "-Infinity"}}},
	}).Context(ctx).Do(); err != nil {
		t.Errorf("tabledata.insertAll of -Infinity: %v", err)
	}
	if err := runQueryJob(ctx, param("INSERT INTO DS.fl (id, f) VALUES (6, @p)", math.Inf(1))); err != nil {
		t.Errorf("INSERT of an Infinity parameter: %v", err)
	}
	if got, want := q("SELECT id, IS_INF(f), f > 0 FROM DS.fl WHERE id NOT BETWEEN 9 AND 17 ORDER BY id"),
		"[[1 true true] [2 true false] [3 true true] [4 true false] [5 true false] [6 true true] [18 true true]]"; got != want {
		t.Errorf("±Infinity read back\n got %s\nwant %s", got, want)
	}
}

// TestBigQueryWriteTruncateDataReplacesTheRows (#1067, #1080): a load with
// WRITE_TRUNCATE_DATA replaces the table's rows and keeps its schema and
// description: a JSON and a CSV upload, and a JSON load of two objects and
// a CSV load from Cloud Storage; into a table that does not exist, it
// makes it. The job reads back WRITE_TRUNCATE_DATA, with outputRows the
// rows it loaded. A query job into an existing table, which the emulator
// appends to whatever its writeDisposition: WRITE_EMPTY (the default)
// into one with rows fails, duplicate, and changes nothing;
// WRITE_TRUNCATE_DATA replaces the rows with the result (a query that
// reads the table reads it as it was) and reads back so from jobs.get;
// WRITE_TRUNCATE with the table's columns replaces the rows, and with
// others takes the result's schema, keeping the table's description;
// WRITE_TRUNCATE_DATA keeps the table's schema (#1083): the result's
// columns written by name, in another order or without a NULLABLE one
// (NULL), into REQUIRED columns and fields when the result has no NULL
// there; a column the table does not have, a NULL for a REQUIRED column
// or field and a missing REQUIRED column fail the job, invalid, and
// another type, and ALLOW_FIELD_ADDITION with a new column, are 501,
// the table unchanged; a failed query leaves the table; WRITE_APPEND
// appends; no scratch table is left.
func TestBigQueryWriteTruncateDataReplacesTheRows(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}}
	bucket := loadBucket(t, h, "bq-truncate", map[string]string{
		"t/one.json": `{"id":5,"s":"e"}` + "\n",
		"t/two.json": `{"id":6,"s":"f"}` + "\n",
		"t.csv":      "7,g\n",
	})
	tbl := ds.Table("t")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema, Description: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":1,"s":"a"}`+"\n"+`{"id":2,"s":"b"}`+"\n", schema))); err != nil {
		t.Fatal(err)
	}
	truncate := func(src bigquery.LoadSource) *bigquery.Loader {
		l := tbl.LoaderFrom(src)
		l.WriteDisposition = bigquery.WriteTruncateData
		return l
	}
	job, err := truncate(jsonSource(`{"id":3,"s":"c"}`+"\n", schema)).Run(ctx)
	if err != nil {
		t.Fatalf("JSON upload: %v", err)
	}
	st, err := job.Wait(ctx)
	if err != nil || st.Err() != nil {
		t.Fatalf("JSON upload: %v %v", err, st.Err())
	}
	if ls, _ := st.Statistics.Details.(*bigquery.LoadStatistics); ls == nil || ls.OutputRows != 1 {
		t.Errorf("JSON upload: statistics %+v, want outputRows 1", st.Statistics)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[3 c]]"; got != want {
		t.Errorf("after a JSON upload with WRITE_TRUNCATE_DATA: %s, want %s", got, want)
	}
	job, err = truncate(csvSource("4,d\n", schema)).Run(ctx)
	if err != nil {
		t.Fatalf("CSV upload: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("CSV upload: %v %v", err, st.Err())
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[4 d]]"; got != want {
		t.Errorf("after a CSV upload with WRITE_TRUNCATE_DATA: %s, want %s", got, want)
	}
	again, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	if cfg, err := again.Config(); err != nil {
		t.Errorf("the load's configuration: %v", err)
	} else if lc, ok := cfg.(*bigquery.LoadConfig); !ok || lc.WriteDisposition != bigquery.WriteTruncateData {
		t.Errorf("jobs.get of the load: %+v, want WRITE_TRUNCATE_DATA", cfg)
	}
	if err := runLoad(ctx, truncate(gcsSource("gs://"+bucket.BucketName()+"/t/*.json", bigquery.JSON, schema))); err != nil {
		t.Fatalf("JSON load from Cloud Storage: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[5 e] [6 f]]"; got != want {
		t.Errorf("after a JSON load from Cloud Storage with WRITE_TRUNCATE_DATA: %s, want %s", got, want)
	}
	if err := runLoad(ctx, truncate(gcsSource("gs://"+bucket.BucketName()+"/t.csv", bigquery.CSV, schema))); err != nil {
		t.Fatalf("CSV load from Cloud Storage: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[7 g]]"; got != want {
		t.Errorf("after a CSV load from Cloud Storage with WRITE_TRUNCATE_DATA: %s, want %s", got, want)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := schemaText(md.Schema); got != schemaText(schema) || md.Description != "kept" {
		t.Errorf("the table after the loads: %s %q", got, md.Description)
	}
	fresh := ds.Table("fresh")
	fl := fresh.LoaderFrom(jsonSource(`{"id":8,"s":"h"}`+"\n", schema))
	fl.WriteDisposition = bigquery.WriteTruncateData
	if err := runLoad(ctx, fl); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA into a new table: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.fresh"), "[[8 h]]"; got != want {
		t.Errorf("the new table: %s, want %s", got, want)
	}

	// Query jobs with a destination table (#1080).
	query := func(sql string, write bigquery.TableWriteDisposition, dst string) (*bigquery.Job, error) {
		qq := c.Query(strings.ReplaceAll(sql, "DS.", ds.DatasetID+"."))
		qq.Dst = ds.Table(dst)
		qq.WriteDisposition = write
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		j, err := qq.Run(qctx)
		if err != nil {
			return nil, err
		}
		st, err := j.Wait(qctx)
		if err != nil {
			return j, err
		}
		return j, st.Err()
	}
	// WRITE_EMPTY, and the default, fail the job: the table has rows.
	for _, write := range []bigquery.TableWriteDisposition{bigquery.WriteEmpty, ""} {
		_, err := query("SELECT 9 AS id, 'i' AS s", write, "t")
		var be *bigquery.Error
		var ge *googleapi.Error
		if !(errors.As(err, &be) && be.Reason == "duplicate") && !(errors.As(err, &ge) && len(ge.Errors) == 1 &&
			ge.Errors[0].Reason == "duplicate") {
			t.Errorf("a query job with writeDisposition %q into a table with rows: %v, want the job failed duplicate", write, err)
		}
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[7 g]]"; got != want {
		t.Errorf("failed WRITE_EMPTY jobs changed the table: %s, want %s", got, want)
	}
	// WRITE_TRUNCATE_DATA replaces the rows, and reads the table as it was.
	j, err := query("SELECT id + 1 AS id, CONCAT(s, 'x') AS s FROM DS.t UNION ALL SELECT 20, 'u'", bigquery.WriteTruncateData, "t")
	if err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA query job: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[8 gx] [20 u]]"; got != want {
		t.Errorf("after a WRITE_TRUNCATE_DATA query job: %s, want %s", got, want)
	}
	it, err := j.Read(ctx)
	if err != nil {
		t.Fatalf("the job's rows: %v", err)
	}
	var n int
	for {
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Errorf("the job's rows: %d, want 2", n)
	}
	again, err = c.JobFromID(ctx, j.ID())
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	if cfg, err := again.Config(); err != nil {
		t.Errorf("the job's configuration: %v", err)
	} else if qc, ok := cfg.(*bigquery.QueryConfig); !ok || qc.WriteDisposition != bigquery.WriteTruncateData || qc.Dst == nil ||
		qc.Dst.TableID != "t" {
		t.Errorf("jobs.get of the query job: %+v, want WRITE_TRUNCATE_DATA into t", cfg)
	}
	// WRITE_TRUNCATE with the table's columns keeps the table.
	if _, err := query("SELECT 30 AS id, 'w' AS s", bigquery.WriteTruncate, "t"); err != nil {
		t.Fatalf("WRITE_TRUNCATE query job: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[30 w]]"; got != want {
		t.Errorf("after a WRITE_TRUNCATE query job: %s, want %s", got, want)
	}
	// WRITE_TRUNCATE_DATA keeps the table's schema (#1083): the result's
	// columns by name, one it does not have NULL; one the table does not
	// have fails the job, invalid; another type is 501; neither changes
	// the table.
	if _, err := query("SELECT 'r' AS s, 41 AS id", bigquery.WriteTruncateData, "t"); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA with the columns in another order: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[41 r]]"; got != want {
		t.Errorf("after WRITE_TRUNCATE_DATA with the columns in another order: %s, want %s", got, want)
	}
	if _, err := query("SELECT 'only s' AS s", bigquery.WriteTruncateData, "t"); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA without a NULLABLE column: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[<nil> only s]]"; got != want {
		t.Errorf("after WRITE_TRUNCATE_DATA without a NULLABLE column: %s, want %s", got, want)
	}
	if _, err := query("SELECT 30 AS id, 'w' AS s", bigquery.WriteTruncateData, "t"); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA: %v", err)
	}
	_, err = query("SELECT 1.5 AS z", bigquery.WriteTruncateData, "t")
	if !jobFailedInvalid(err) {
		t.Errorf("WRITE_TRUNCATE_DATA with a column the table does not have: %v, want the job failed invalid", err)
	}
	_, err = query("SELECT 1.5 AS id, 's' AS s", bigquery.WriteTruncateData, "t")
	wantReason(t, "a WRITE_TRUNCATE_DATA query job with a FLOAT64 for an INT64 column", err, 501, "notImplemented")
	qa := c.Query("SELECT 1 AS id, 's' AS s, 2 AS extra")
	qa.Dst, qa.WriteDisposition = tbl, bigquery.WriteTruncateData
	qa.SchemaUpdateOptions = []string{"ALLOW_FIELD_ADDITION"}
	_, err = qa.Run(ctx)
	wantReason(t, "a WRITE_TRUNCATE_DATA query job with ALLOW_FIELD_ADDITION and a new column", err, 501, "notImplemented")
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[30 w]]"; got != want {
		t.Errorf("after the refused WRITE_TRUNCATE_DATA jobs: %s, want %s", got, want)
	}
	// Into a table with REQUIRED columns and fields: written when the
	// result has no NULL there, and its schema kept; a NULL, or a
	// REQUIRED column the result does not have, fails the job, invalid.
	req := ds.Table("req")
	reqSchema := bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType, Required: true}, {Name: "b", Type: bigquery.IntegerFieldType},
		{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.StringFieldType, Required: true},
			{Name: "y", Type: bigquery.IntegerFieldType}}}}
	if err := req.Create(ctx, &bigquery.TableMetadata{Schema: reqSchema}); err != nil {
		t.Fatal(err)
	}
	if _, err := query("SELECT 'a1' AS a, 1 AS b, STRUCT(9 AS y, 'k' AS x) AS r", bigquery.WriteTruncateData, "req"); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA into REQUIRED columns: %v", err)
	}
	const reqRows = "[[a1 1 k 9]]"
	if got := q("SELECT a, b, r.x, r.y FROM DS.req"); got != reqRows {
		t.Errorf("after WRITE_TRUNCATE_DATA into REQUIRED columns: %s, want %s", got, reqRows)
	}
	if md, err := req.Metadata(ctx); err != nil || schemaText(md.Schema) != schemaText(reqSchema) {
		t.Errorf("the table with REQUIRED columns after WRITE_TRUNCATE_DATA: %v %v, want %s", md, err, schemaText(reqSchema))
	}
	for _, sql := range []string{"SELECT CAST(NULL AS STRING) AS a, 2 AS b", "SELECT 'a2' AS a, STRUCT(CAST(NULL AS STRING) AS x, 1 AS y) AS r",
		"SELECT 3 AS b"} {
		_, err := query(sql, bigquery.WriteTruncateData, "req")
		if !jobFailedInvalid(err) {
			t.Errorf("WRITE_TRUNCATE_DATA of %s into REQUIRED columns: %v, want the job failed invalid", sql, err)
		}
	}
	if got := q("SELECT a, b, r.x, r.y FROM DS.req"); got != reqRows {
		t.Errorf("after the failed WRITE_TRUNCATE_DATA jobs: %s, want %s", got, reqRows)
	}
	// WRITE_TRUNCATE with other columns takes the result's schema, and
	// keeps the description.
	if _, err := query("SELECT 1.5 AS z, [1, 2] AS arr", bigquery.WriteTruncate, "t"); err != nil {
		t.Fatalf("WRITE_TRUNCATE query job with other columns: %v", err)
	}
	if got, want := q("SELECT z, arr FROM DS.t"), "[[1.5 [1 2]]]"; got != want {
		t.Errorf("after a WRITE_TRUNCATE query job with other columns: %s, want %s", got, want)
	}
	md, err = tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := schemaText(md.Schema), schemaText(bigquery.Schema{{Name: "z", Type: bigquery.FloatFieldType},
		{Name: "arr", Type: bigquery.IntegerFieldType, Repeated: true}}); got != want || md.Description != "kept" {
		t.Errorf("the table after WRITE_TRUNCATE with other columns: %s %q, want %s", got, md.Description, want)
	}
	// A query that fails leaves the table.
	if _, err := query("SELECT ERROR('no')", bigquery.WriteTruncate, "t"); err == nil {
		t.Errorf("a failing WRITE_TRUNCATE query job succeeded")
	}
	if got, want := q("SELECT z, arr FROM DS.t"), "[[1.5 [1 2]]]"; got != want {
		t.Errorf("after a failed WRITE_TRUNCATE query job: %s, want %s", got, want)
	}
	// WRITE_APPEND appends; a new table is made.
	if _, err := query("SELECT 2.5 AS z, [3] AS arr", bigquery.WriteAppend, "t"); err != nil {
		t.Errorf("WRITE_APPEND query job: %v", err)
	}
	if got, want := q("SELECT z FROM DS.t ORDER BY z"), "[[1.5] [2.5]]"; got != want {
		t.Errorf("after WRITE_APPEND: %s, want %s", got, want)
	}
	if _, err := query("SELECT 9 AS id, 'i' AS s", bigquery.WriteTruncateData, "made"); err != nil {
		t.Errorf("WRITE_TRUNCATE_DATA query job into a new table: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.made"), "[[9 i]]"; got != want {
		t.Errorf("the query job's new table: %s, want %s", got, want)
	}
	// No scratch table is left.
	tit := ds.Tables(ctx)
	for {
		tb, err := tit.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(tb.TableID, "_cloudburrow") {
			t.Errorf("a scratch table was left: %s", tb.TableID)
		}
	}
}

// jobFailedInvalid reports whether err is a query job that failed with
// reason invalid, as Job.Wait gives it: the job's status (a
// *bigquery.Error), or jobs.getQueryResults answering the job's error with
// its HTTP status (a *googleapi.Error), as BigQuery does for a failed job.
func jobFailedInvalid(err error) bool {
	var be *bigquery.Error
	if errors.As(err, &be) {
		return be.Reason == "invalid"
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		for _, e := range ge.Errors {
			if e.Reason == "invalid" {
				return true
			}
		}
	}
	return false
}
