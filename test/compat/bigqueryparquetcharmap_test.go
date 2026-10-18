//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// arrowParquet is a Parquet file written by Apache Arrow's Go Parquet
// writer (pqarrow) of one record of schema, its columns filled by build.
func arrowParquet(t *testing.T, schema *arrow.Schema, build func(*array.RecordBuilder)) string {
	t.Helper()
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	build(b)
	rec := b.NewRecord()
	defer rec.Release()
	var buf bytes.Buffer
	w, err := pqarrow.NewFileWriter(schema, &buf, parquet.NewWriterProperties(), pqarrow.DefaultWriterProps())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestBigQueryParquetColumnNameCharacterMap (#1373): under
// columnNameCharacterMap V1 or V2 a Parquet load into a new table renames
// the file's columns as BigQuery documents: unsupported characters
// "(except periods in Parquet file column names) are replaced with
// underscores" (load statements, column_name_character_map, which
// "Applies to CSV and Parquet data"). The file, written by Arrow's Parquet
// writer, has columns "first name", "a!" and a group "rec?" whose field is
// "in ner!"; through the official Go client's
// Loader.ColumnNameCharacterMap, V1 gives first_name, a_ and rec_ with
// in_ner_, and V2 (flexible names) first name, a_ and rec_ with in ner_,
// at every depth. Table.Metadata reads the names, the types are kept, and
// the rows read back by the new names through a query and
// tabledata.list. As for a CSV (TestBigQueryColumnNameCharacterMap), names
// the same once replaced ("a!", "a?") and a name still refused once
// replaced (a V1 leading digit, "1a") stay 501 with no table, BigQuery
// not documenting how it names them; a period, kept ("x.y"), is 400
// (#1547: "Periods are not replaced by underscores in Parquet file column
// names when a column name character map is used", UNVERIFIED that the
// load then fails). A flexible
// name loads under STRICT, the default, as it is. Measured first: each of
// the V1 and V2 loads was 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.tabledata.list
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/schemas
func TestBigQueryParquetColumnNameCharacterMap(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table, data string, charMap bigquery.ColumnNameCharacterMap) (*bigquery.Job, error) {
		l := ds.Table(table).LoaderFrom(parquetUpload(data, nil))
		l.ColumnNameCharacterMap = charMap
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return l.Run(lctx)
	}

	inner := arrow.StructOf(arrow.Field{Name: "in ner!", Type: arrow.PrimitiveTypes.Int64, Nullable: true})
	file := arrowParquet(t, arrow.NewSchema([]arrow.Field{
		{Name: "first name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "a!", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "rec?", Type: inner, Nullable: true},
	}, nil), func(b *array.RecordBuilder) {
		b.Field(0).(*array.StringBuilder).AppendValues([]string{"bob", "ann"}, nil)
		b.Field(1).(*array.Int64Builder).AppendValues([]int64{3, 4}, nil)
		sb := b.Field(2).(*array.StructBuilder)
		for _, v := range []int64{7, 8} {
			sb.Append(true)
			sb.FieldBuilder(0).(*array.Int64Builder).Append(v)
		}
	})
	for _, tc := range []struct {
		table   string
		charMap bigquery.ColumnNameCharacterMap
		want    []string // first name, a!, rec?, in ner!
	}{
		{"pqmap_v1", bigquery.V1ColumnNameCharacterMap, []string{"first_name", "a_", "rec_", "in_ner_"}},
		{"pqmap_v2", bigquery.V2ColumnNameCharacterMap, []string{"first name", "a_", "rec_", "in ner_"}},
	} {
		job, err := load(tc.table, file, tc.charMap)
		if err != nil {
			t.Fatalf("%s: the load was refused: %v", tc.table, err)
		}
		if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
			t.Fatalf("%s: the load failed: %v %v", tc.table, err, st.Err())
		}
		again, err := c.JobFromID(ctx, job.ID())
		if err != nil {
			t.Fatal(err)
		}
		if st, err := again.Status(ctx); err != nil || st.Err() != nil {
			t.Errorf("%s: jobs.get reports %v %v, want the job done", tc.table, st, err)
		}
		md, err := ds.Table(tc.table).Metadata(ctx)
		if err != nil || len(md.Schema) != 3 {
			t.Fatalf("%s: Table.Metadata: %+v %v", tc.table, md, err)
		}
		s := md.Schema
		got := []string{s[0].Name, s[1].Name, s[2].Name, ""}
		if len(s[2].Schema) == 1 {
			got[3] = s[2].Schema[0].Name
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: columns %q, want %q", tc.table, got, tc.want)
		}
		if s[0].Type != bigquery.StringFieldType || s[1].Type != bigquery.IntegerFieldType || s[2].Type != bigquery.RecordFieldType {
			t.Errorf("%s: types %s, %s, %s, want STRING, INTEGER, RECORD", tc.table, s[0].Type, s[1].Type, s[2].Type)
		}
		read := rows(t, ctx, c, fmt.Sprintf("SELECT `%s`, a_, rec_.`%s` FROM `%s.%s` ORDER BY a_",
			tc.want[0], tc.want[3], ds.DatasetID, tc.table))
		if fmt.Sprint(read) != "[[bob 3 7] [ann 4 8]]" {
			t.Errorf("%s reads %v", tc.table, read)
		}
		if n := countRows(t, h, ds.Table(tc.table)); n != 2 {
			t.Errorf("%s: tabledata.list has %d rows, want 2", tc.table, n)
		}
	}

	ints := func(names ...string) string {
		fields := make([]arrow.Field, len(names))
		for i, n := range names {
			fields[i] = arrow.Field{Name: n, Type: arrow.PrimitiveTypes.Int64, Nullable: true}
		}
		return arrowParquet(t, arrow.NewSchema(fields, nil), func(b *array.RecordBuilder) {
			for i := range names {
				b.Field(i).(*array.Int64Builder).Append(int64(i))
			}
		})
	}
	for _, tc := range []struct {
		table   string
		data    string
		charMap bigquery.ColumnNameCharacterMap
	}{
		{"pqmap_same", ints("a!", "a?"), bigquery.V1ColumnNameCharacterMap},
		{"pqmap_digit", ints("1a"), bigquery.V1ColumnNameCharacterMap},
	} {
		_, err := load(tc.table, tc.data, tc.charMap)
		wantHTTPStatus(t, "a Parquet load under "+string(tc.charMap)+" it cannot rename ("+tc.table+")", err,
			http.StatusNotImplemented)
		if tableExists(t, ctx, ds.Table(tc.table)) {
			t.Errorf("the refused load left %s", tc.table)
		}
	}

	// #1547: a period, kept ("Periods are not replaced by underscores in
	// Parquet file column names when a column name character map is
	// used"), fails the load, as an unsupported character does.
	_, err := load("pqmap_period", ints("x.y"), bigquery.V2ColumnNameCharacterMap)
	wantHTTPStatus(t, "a Parquet name with a period under V2", err, http.StatusBadRequest)
	if tableExists(t, ctx, ds.Table("pqmap_period")) {
		t.Errorf("the refused load left pqmap_period")
	}

	job, err := load("pqmap_strict", ints("first name", "n"), "")
	if err != nil {
		t.Fatalf("a flexible name under STRICT was refused: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("a flexible name under STRICT failed: %v %v", err, st.Err())
	}
	read := rows(t, ctx, c, fmt.Sprintf("SELECT `first name`, n FROM `%s.pqmap_strict`", ds.DatasetID))
	if fmt.Sprint(read) != "[[0 1]]" {
		t.Errorf("pqmap_strict reads %v", read)
	}
}
