//go:build compat

package compat

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryStructJSONField (#1783): a JSON field inside a STRUCT, a
// JSON field of an ARRAY<STRUCT>, and a REPEATED JSON column hold JSON
// however the row was written, through the official cloud.google.com/go
// bigquery client: tabledata.insertAll (the Inserter), a load job
// (NDJSON through a ReaderSource) and DML. JSON_VALUE reads into each,
// JSON_TYPE is the value's own, and the row reads back with the JSON
// unquoted. Measured before the engine patches (googlesqlite 0093,
// bigquery-emulator 0094): insertAll and loads stored the STRUCT's field
// as a JSON string (JSON_VALUE NULL, TO_JSON_STRING quoted) and an
// insertAll into a REPEATED JSON column failed 500.
//
// covers: bigquery.tabledata.insertAll, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryStructJSONField(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := "compat_sjson_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "s", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "n", Type: bigquery.StringFieldType},
			{Name: "d", Type: bigquery.JSONFieldType}}},
		{Name: "a", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "d", Type: bigquery.JSONFieldType}}},
		{Name: "aj", Type: bigquery.JSONFieldType, Repeated: true},
	}
	tb := c.Dataset(ds).Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("tables.insert: %v", err)
	}
	if err := tb.Inserter().Put(ctx, &bigquery.ValuesSaver{Schema: schema, Row: []bigquery.Value{int64(1),
		[]bigquery.Value{"a", `{"x":{"y":"z"}}`}, []bigquery.Value{[]bigquery.Value{`{"x":1}`}}, []bigquery.Value{`{"x":2}`, `3`}}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}
	src := bigquery.NewReaderSource(bytes.NewBufferString(`{"id":2,"s":{"n":"b","d":{"x":{"y":"w"}}},"a":[{"d":{"x":4}}],"aj":[{"x":5},6]}` + "\n"))
	src.SourceFormat = bigquery.JSON
	job, err := tb.LoaderFrom(src).Run(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("load: %v, %v", err, st.Err())
	}
	if err := mlRun(h, c, fmt.Sprintf(`INSERT INTO `+"`%s.t`"+` (id, s, a, aj) VALUES (3, STRUCT('c', JSON '{"x":{"y":"v"}}'), `+
		`[STRUCT(JSON '{"x":7}')], [JSON '{"x":8}', JSON '9'])`, ds)); err != nil {
		t.Fatalf("DML: %v", err)
	}
	it, err := c.Query(fmt.Sprintf("SELECT id, JSON_VALUE(s.d, '$.x.y'), JSON_TYPE(s.d), TO_JSON_STRING(s), JSON_VALUE(a[OFFSET(0)].d, '$.x'), "+
		"JSON_VALUE(aj[OFFSET(0)], '$.x'), JSON_TYPE(aj[OFFSET(1)]) FROM `%s.t` ORDER BY id", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	want := []string{
		`[1 z object {"n":"a","d":{"x":{"y":"z"}}} 1 2 number]`,
		`[2 w object {"n":"b","d":{"x":{"y":"w"}}} 4 5 number]`,
		`[3 v object {"n":"c","d":{"x":{"y":"v"}}} 7 8 number]`,
	}
	for i := 0; ; i++ {
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			if i != len(want) {
				t.Errorf("rows: %d, %v", i, err)
			}
			break
		}
		if i < len(want) && fmt.Sprint(row) != want[i] {
			t.Errorf("row %d = %v, want %s", i, row, want[i])
		}
	}
	// The row itself: the STRUCT's JSON field reads back as the JSON's text.
	it, err = c.Query(fmt.Sprintf("SELECT s FROM `%s.t` WHERE id = 1", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("query s: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || fmt.Sprint(row) != `[[a {"x":{"y":"z"}}]]` {
		t.Errorf("s = %v, %v", row, err)
	}
}
