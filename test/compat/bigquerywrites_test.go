//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// Writes the emulator made partly, or refused, and the front now carries
// out (#1077, #1078, #1079, #1080). Each was measured first through the
// front with this client, on the pinned image: a JSON load of two objects
// from Cloud Storage whose second failed left the first object's rows (the
// table's own gone under WRITE_TRUNCATE); a BYTES query parameter was
// refused as a STRING; a RECORD holding ±Infinity failed a JSON load,
// "json: unsupported value", and ±Infinity read back "+Inf"; a query job
// with WRITE_TRUNCATE, WRITE_TRUNCATE_DATA or WRITE_EMPTY appended to a
// table with rows (501 since #1067).

// TestBigQueryLoadOfSeveralObjectsIsOneJob (#1079): a load of several
// objects from Cloud Storage loads all of them or none. A JSON load (with
// no BYTES or FLOAT64 column, which the emulator read itself), a CSV load
// and a Parquet load whose second object fails leave the table as it was,
// with WRITE_TRUNCATE and WRITE_APPEND; the same loads of good objects
// load every row. A CSV load with no schema into a table that does not
// exist is refused, 400 invalid, and makes nothing.
func TestBigQueryLoadOfSeveralObjectsIsOneJob(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}}
	ab := parquetFixture(t, "ab.parquet")
	bucket := loadBucket(t, h, "bq-several", map[string]string{
		"bad/a.json": `{"id":1,"s":"a"}` + "\n",
		"bad/b.json": `{"id":"x","s":"b"}` + "\n",
		"ok/a.json":  `{"id":1,"s":"a"}` + "\n",
		"ok/b.json":  `{"id":2,"s":"b"}` + "\n" + `{"id":3,"s":"c"}` + "\n",
		"bad/a.csv":  "1,a\n",
		"bad/b.csv":  "x,b\n",
		"ok/a.csv":   "4,d\n",
		"ok/b.csv":   "5,e\n",
		"n/a.csv":    "id,s\n1,a\n",
		"pq/a.pq":    ab,
		"pq/b.pq":    "not a Parquet file",
		"pqok/a.pq":  ab,
		"pqok/b.pq":  ab,
	})
	uri := func(name string) string { return "gs://" + bucket.BucketName() + "/" + name }
	tbl := ds.Table("t")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: schema, Row: []bigquery.Value{7, "old"}}}); err != nil {
		t.Fatal(err)
	}
	load := func(src bigquery.LoadSource, write bigquery.TableWriteDisposition) error {
		l := tbl.LoaderFrom(src)
		l.WriteDisposition = write
		return runLoad(ctx, l)
	}
	for _, write := range []bigquery.TableWriteDisposition{bigquery.WriteTruncate, bigquery.WriteAppend} {
		for _, tc := range []struct {
			what string
			src  bigquery.LoadSource
		}{
			{"JSON", gcsSource(uri("bad/*.json"), bigquery.JSON, schema)},
			{"JSON with no schema", gcsSource(uri("bad/*.json"), bigquery.JSON, nil)},
			{"JSON of two URIs", func() bigquery.LoadSource {
				g := bigquery.NewGCSReference(uri("bad/a.json"), uri("bad/b.json"))
				g.SourceFormat = bigquery.JSON
				return g
			}()},
			{"CSV", gcsSource(uri("bad/*.csv"), bigquery.CSV, schema)},
			{"Parquet", gcsSource(uri("pq/*.pq"), bigquery.Parquet, nil)},
		} {
			if err := load(tc.src, write); err == nil {
				t.Errorf("%s load with %s of a bad second object succeeded", tc.what, write)
			}
			if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[7 old]]"; got != want {
				t.Fatalf("after a failed %s load with %s: %s, want %s", tc.what, write, got, want)
			}
		}
	}
	if err := load(gcsSource(uri("ok/*.json"), bigquery.JSON, nil), bigquery.WriteAppend); err != nil {
		t.Fatalf("JSON load of two objects: %v", err)
	}
	if err := load(gcsSource(uri("ok/*.csv"), bigquery.CSV, schema), bigquery.WriteAppend); err != nil {
		t.Fatalf("CSV load of two objects: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[1 a] [2 b] [3 c] [4 d] [5 e] [7 old]]"; got != want {
		t.Errorf("after the loads of two objects: %s, want %s", got, want)
	}
	if err := load(gcsSource(uri("ok/*.json"), bigquery.JSON, nil), bigquery.WriteTruncate); err != nil {
		t.Fatalf("JSON load of two objects with WRITE_TRUNCATE: %v", err)
	}
	if got, want := q("SELECT id, s FROM DS.t ORDER BY id"), "[[1 a] [2 b] [3 c]]"; got != want {
		t.Errorf("after WRITE_TRUNCATE of two objects: %s, want %s", got, want)
	}
	pt := ds.Table("p")
	if err := runLoad(ctx, pt.LoaderFrom(gcsSource(uri("pqok/*.pq"), bigquery.Parquet, nil))); err != nil {
		t.Fatalf("Parquet load of two objects: %v", err)
	}
	if got, want := q("SELECT COUNT(*) FROM DS.p"), "[[6]]"; got != want {
		t.Errorf("the Parquet load of two objects: %s, want %s", got, want)
	}
	wantReason(t, "a CSV load with no schema into a new table", runLoad(ctx, ds.Table("none").LoaderFrom(
		gcsSource(uri("n/a.csv"), bigquery.CSV, nil))), 400, "invalid")
	if _, err := ds.Table("none").Metadata(ctx); err == nil {
		t.Errorf("the refused CSV load made its table")
	}
}

// TestBigQueryBytesParameters (#1078): a BYTES query parameter is BYTES in
// the query and round-trips exactly: SELECT @p reads back the bytes ff 00
// 61, an INSERT stores them, a positional parameter, a NULL and an
// ARRAY<BYTES> too; jobs.get shows the parameter as the client
// sent it. A STRUCT parameter with a BYTES field is 501.
func TestBigQueryBytesParameters(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	raw := []byte{0xff, 0x00, 0x61}
	query := func(sql string, params ...bigquery.QueryParameter) *bigquery.Query {
		qq := c.Query(strings.ReplaceAll(sql, "DS.", ds.DatasetID+"."))
		qq.Parameters = params
		return qq
	}
	read := func(qq *bigquery.Query) [][]bigquery.Value {
		t.Helper()
		it, err := qq.Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", qq.Q, err)
		}
		var out [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("%s: %v", qq.Q, err)
			}
			out = append(out, row)
		}
	}
	p := func(v any) bigquery.QueryParameter { return bigquery.QueryParameter{Name: "p", Value: v} }
	got := read(query("SELECT @p, TO_HEX(@p), '@p'", p(raw)))
	if len(got) != 1 || !bytes.Equal(got[0][0].([]byte), raw) || got[0][1] != "ff0061" || got[0][2] != "@p" {
		t.Errorf("SELECT @p: %v", got)
	}
	if got := fmt.Sprint(read(query("SELECT TO_HEX(?), ?", bigquery.QueryParameter{Value: raw}, bigquery.QueryParameter{Value: int64(2)}))); got != "[[ff0061 2]]" {
		t.Errorf("positional: %s", got)
	}
	if got := fmt.Sprint(read(query("SELECT @p IS NULL", p([]byte(nil))))); got != "[[true]]" {
		t.Errorf("a NULL BYTES: %s", got)
	}
	if got := fmt.Sprint(read(query("SELECT ARRAY(SELECT TO_HEX(e) FROM UNNEST(@p) e WITH OFFSET o ORDER BY o)", p([][]byte{raw, []byte("ab")})))); got != "[[[ff0061 6162]]]" {
		t.Errorf("ARRAY<BYTES>: %s", got)
	}
	tbl := ds.Table("bt")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "b", Type: bigquery.BytesFieldType}, {Name: "a", Type: bigquery.BytesFieldType, Repeated: true}}}); err != nil {
		t.Fatal(err)
	}
	job, err := query("INSERT INTO DS.bt (id, b, a) VALUES (1, @p, @a)", p(raw), bigquery.QueryParameter{Name: "a", Value: [][]byte{raw}}).Run(ctx)
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("INSERT: %v %v", err, st.Err())
	}
	if got := fmt.Sprint(read(query("SELECT id, TO_HEX(b), TO_HEX(a[OFFSET(0)]) FROM DS.bt"))); got != "[[1 ff0061 ff0061]]" {
		t.Errorf("the inserted row: %s", got)
	}
	if got := read(query("SELECT b FROM DS.bt WHERE b = @p", p(raw))); len(got) != 1 || !bytes.Equal(got[0][0].([]byte), raw) {
		t.Errorf("WHERE b = @p: %v", got)
	}
	again, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	cfg, err := again.Config()
	if err != nil {
		t.Fatalf("the job's configuration: %v", err)
	}
	if qc, ok := cfg.(*bigquery.QueryConfig); !ok || !strings.Contains(qc.Q, "@p") || strings.Contains(qc.Q, "FROM_BASE64") ||
		len(qc.Parameters) != 2 || !bytes.Equal(qc.Parameters[0].Value.([]byte), raw) {
		t.Errorf("jobs.get of the INSERT: %+v", cfg)
	}
	type withBytes struct{ X []byte }
	var ge *googleapi.Error
	if _, err := query("SELECT @p.x", p(withBytes{raw})).Read(ctx); !errors.As(err, &ge) || ge.Code != 501 {
		t.Errorf("a STRUCT with a BYTES field: %v, want 501", err)
	}
}

// TestBigQueryInfinityInRecordsAndArrays (#1077): ±Infinity in a RECORD,
// in a REPEATED RECORD and in a REPEATED column is written by a JSON load
// (an upload and from Cloud Storage) and a streamed row, and reads back;
// and FLOAT64 ±Infinity reads back from tabledata.list, jobs.query and
// jobs.getQueryResults as "Infinity" and "-Infinity", as BigQuery writes
// them, at the top level, in a RECORD and in a REPEATED column, while a
// STRING "+Inf" is read as it was.
func TestBigQueryInfinityInRecordsAndArrays(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "s", Type: bigquery.StringFieldType},
		{Name: "f", Type: bigquery.FloatFieldType},
		{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "g", Type: bigquery.FloatFieldType},
			{Name: "n", Type: bigquery.StringFieldType},
		}},
		{Name: "rr", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "g", Type: bigquery.FloatFieldType}}},
		{Name: "a", Type: bigquery.FloatFieldType, Repeated: true},
	}
	bucket := loadBucket(t, h, "bq-inf", map[string]string{
		"inf.json": `{"id":2,"r":{"g":"Infinity","n":"x"}}` + "\n",
	})
	tbl := ds.Table("fi")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	data := `{"id":1,"s":"+Inf","f":"Infinity","r":{"g":"-Infinity"},"rr":[{"g":1.5},{"g":"Infinity"}],"a":["-Infinity",2]}` + "\n" +
		`{"id":3,"r":{"g":0.5,"n":"y"}}` + "\n"
	if err := runLoad(ctx, tbl.LoaderFrom(jsonSource(data, schema))); err != nil {
		t.Fatalf("JSON upload: %v", err)
	}
	if err := runLoad(ctx, tbl.LoaderFrom(gcsSource("gs://"+bucket.BucketName()+"/inf.json", bigquery.JSON, schema))); err != nil {
		t.Fatalf("JSON load from Cloud Storage: %v", err)
	}
	svc := generatedBigQuery(t, h)
	if _, err := svc.Tabledata.InsertAll(project, ds.DatasetID, "fi", &bq.TableDataInsertAllRequest{
		Rows: []*bq.TableDataInsertAllRequestRows{{Json: map[string]bq.JsonValue{"id": 4, "r": map[string]any{"g": "-Infinity"}}}},
	}).Context(ctx).Do(); err != nil {
		t.Fatalf("tabledata.insertAll: %v", err)
	}
	inf, neg := math.Inf(1), math.Inf(-1)
	want := fmt.Sprint([][]bigquery.Value{
		{int64(1), "+Inf", inf, []bigquery.Value{neg, nil}, []bigquery.Value{[]bigquery.Value{1.5}, []bigquery.Value{inf}}, []bigquery.Value{neg, 2.0}},
		{int64(2), nil, nil, []bigquery.Value{inf, "x"}, []bigquery.Value{}, []bigquery.Value{}},
		{int64(3), nil, nil, []bigquery.Value{0.5, "y"}, []bigquery.Value{}, []bigquery.Value{}},
		{int64(4), nil, nil, []bigquery.Value{neg, nil}, []bigquery.Value{}, []bigquery.Value{}},
	})
	if got := q("SELECT id, s, f, r, rr, a FROM DS.fi ORDER BY id"); got != want {
		t.Errorf("read back\n got %s\nwant %s", got, want)
	}

	// The REST rows, as BigQuery writes them.
	cell := func(row *bq.TableRow, i int) any { return row.F[i].V }
	check := func(what string, rs []*bq.TableRow) {
		t.Helper()
		var one *bq.TableRow
		for _, r := range rs {
			if cell(r, 0) == "1" {
				one = r
			}
		}
		if one == nil {
			t.Fatalf("%s: no row 1 in %d rows", what, len(rs))
		}
		rv, _ := cell(one, 3).(map[string]any)
		rf, _ := rv["f"].([]any)
		g, _ := rf[0].(map[string]any)
		arr, _ := cell(one, 5).([]any)
		a0, _ := arr[0].(map[string]any)
		if cell(one, 1) != "+Inf" || cell(one, 2) != "Infinity" || g["v"] != "-Infinity" || a0["v"] != "-Infinity" {
			t.Errorf("%s: row 1 %s", what, mustJSONCompat(one))
		}
	}
	list, err := svc.Tabledata.List(project, ds.DatasetID, "fi").Context(ctx).Do()
	if err != nil {
		t.Fatalf("tabledata.list: %v", err)
	}
	check("tabledata.list", list.Rows)
	legacy := false
	qr, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: "SELECT id, s, f, r, rr, a FROM " + ds.DatasetID + ".fi",
		UseLegacySql: &legacy}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.query: %v", err)
	}
	check("jobs.query", qr.Rows)
	res, err := svc.Jobs.GetQueryResults(project, qr.JobReference.JobId).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.getQueryResults: %v", err)
	}
	check("jobs.getQueryResults", res.Rows)
}

func mustJSONCompat(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
