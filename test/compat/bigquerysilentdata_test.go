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

// TestBigQueryBytesValuesAreWrittenAsTheirBytes (#1065): a BYTES value,
// base64 in a JSON load, a CSV load and a streamed row, is stored as the
// bytes it encodes, at the top level, in a RECORD and in a REPEATED
// column, from an upload and from Cloud Storage. A value whose bytes are
// not UTF-8 text (0xff), which the emulator cannot be handed, is 501
// through each path, and so is a CSV value with a carriage return; a JSON
// value that is not base64 fails the load, 400 invalid. Nothing is written
// by a load or a request refused.
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
		"ff.json":  `{"id":9,"b":"/w=="}` + "\n",
		"ok.csv":   "12,4piD\n",
		"ff.csv":   "19,/w==\n",
		"bad.json": `{"id":9,"b":"not base64!"}` + "\n",
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
	if got, want := q("SELECT id, TO_HEX(b), TO_HEX(r.x), ARRAY(SELECT TO_HEX(e) FROM UNNEST(a) e) FROM DS.j ORDER BY id"),
		"[[1 610062 e29883 [616263 0d0a 00]] [2 616263 <nil> []]]"; got != want {
		t.Errorf("JSON loads read back\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT id, TO_HEX(b) FROM DS.c ORDER BY id"), "[[11 616263] [12 e29883] [13 610062]]"; got != want {
		t.Errorf("CSV loads and the streamed row read back\n got %s\nwant %s", got, want)
	}

	// Refused: nothing is written.
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"JSON upload of 0xff", runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":9,"b":"/w=="}`+"\n", schema)))},
		{"JSON upload of 0xff in a RECORD", runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":9,"r":{"x":"/w=="}}`+"\n", schema)))},
		{"JSON upload of 0xff in a REPEATED column", runLoad(ctx, j.LoaderFrom(jsonSource(`{"id":9,"a":["YQ==","/w=="]}`+"\n", schema)))},
		{"JSON load of 0xff from Cloud Storage", runLoad(ctx, j.LoaderFrom(gcsSource(uri("ff.json"), bigquery.JSON, schema)))},
		{"CSV upload of 0xff", runLoad(ctx, cs.LoaderFrom(csvSource("19,/w==\n", flat)))},
		{"CSV upload of a carriage return", runLoad(ctx, cs.LoaderFrom(csvSource("19,DQo=\n", flat)))},
		{"CSV load of 0xff from Cloud Storage", runLoad(ctx, cs.LoaderFrom(gcsSource(uri("ff.csv"), bigquery.CSV, flat)))},
		{"Inserter.Put of 0xff", cs.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: flat, Row: []bigquery.Value{19, []byte{0xff}}}})},
	} {
		wantReason(t, tc.what, tc.err, 501, "notImplemented")
	}
	wantReason(t, "JSON load of a value that is not base64", runLoad(ctx, j.LoaderFrom(gcsSource(uri("bad.json"), bigquery.JSON, schema))),
		400, "invalid")
	if got, want := q("SELECT COUNT(*) FROM DS.j WHERE id = 9"), "[[0]]"; got != want {
		t.Errorf("refused JSON loads wrote rows: %s", got)
	}
	if got, want := q("SELECT COUNT(*) FROM DS.c WHERE id = 19"), "[[0]]"; got != want {
		t.Errorf("refused CSV loads and puts wrote rows: %s", got)
	}
}

// TestBigQueryNaNWritesAreNotImplemented (#1066): a NaN in a FLOAT64
// column, which the emulator's engine stores as NULL, is 501 through every
// write whose values the front reads: a JSON load (top level, RECORD,
// REPEATED; an upload and from Cloud Storage), a CSV load (upload and
// Cloud Storage), a streamed row (the generated client: the Go client
// cannot marshal a NaN), and a query parameter of a DML statement or a
// query; nothing is written. ±Infinity is written and reads back through
// each path.
func TestBigQueryNaNWritesAreNotImplemented(t *testing.T) {
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
		"nan.json": `{"id":9,"f":"NaN"}` + "\n",
		"nan.csv":  "9,nan\n",
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
	insertAll := func(f string) error {
		_, err := svc.Tabledata.InsertAll(project, ds.DatasetID, "fl", &bq.TableDataInsertAllRequest{
			Rows: []*bq.TableDataInsertAllRequestRows{{Json: map[string]bq.JsonValue{"id": 9, "f": f}}},
		}).Context(ctx).Do()
		return err
	}

	for _, tc := range []struct {
		what string
		err  error
	}{
		{"JSON upload", runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":9,"f":"NaN"}`+"\n", schema)))},
		{"JSON upload in a RECORD", runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":9,"r":{"g":"nan"}}`+"\n", schema)))},
		{"JSON upload in a REPEATED column", runLoad(ctx, tbl.LoaderFrom(jsonSource(`{"id":9,"a":[1.5,"NaN"]}`+"\n", schema)))},
		{"JSON load from Cloud Storage", runLoad(ctx, tbl.LoaderFrom(gcsSource(uri("nan.json"), bigquery.JSON, schema)))},
		{"CSV upload", runLoad(ctx, ds.Table("fc").LoaderFrom(csvSource("9,NaN\n", flat)))},
		{"CSV load from Cloud Storage", runLoad(ctx, ds.Table("fc").LoaderFrom(gcsSource(uri("nan.csv"), bigquery.CSV, flat)))},
		{"tabledata.insertAll", insertAll("NaN")},
		{"INSERT with a NaN parameter", runQueryJob(ctx, param("INSERT INTO DS.fl (id, f) VALUES (9, @p)", math.NaN()))},
		{"SELECT with a NaN parameter", runQueryJob(ctx, param("SELECT @p", math.NaN()))},
	} {
		wantReason(t, tc.what, tc.err, 501, "notImplemented")
	}
	var ge *googleapi.Error
	if _, err := param("SELECT @p", math.NaN()).Read(ctx); !errors.As(err, &ge) || ge.Code != 501 {
		t.Errorf("jobs.query with a NaN parameter: %v, want 501", err)
	}
	if got, want := q("SELECT COUNT(*) FROM DS.fl WHERE id = 9"), "[[0]]"; got != want {
		t.Errorf("refused writes wrote rows: %s", got)
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
	if err := insertAll("Infinity"); err != nil {
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
	if got, want := q("SELECT id, IS_INF(f), f > 0 FROM DS.fl ORDER BY id"),
		"[[1 true true] [2 true false] [3 true true] [4 true false] [5 true false] [6 true true] [9 true true]]"; got != want {
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
	var be *bigquery.Error
	if !errors.As(err, &be) || be.Reason != "invalid" {
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
		var be *bigquery.Error
		if !errors.As(err, &be) || be.Reason != "invalid" {
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
