//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryCSVAutodetectHeaderRows (#1457): with autodetect, no schema
// and a new table, skipLeadingRows and the header row are read as
// JobConfigurationLoad documents them: unset, a first row BigQuery would
// not take as a header (all strings over STRING columns, or typed) is data
// and the columns get generic names; N skips N-1 rows and looks for the
// header in row N, which is skipped when it is none.
// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.tables.get
func TestBigQueryCSVAutodetectHeaderRows(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, c2 := range []struct {
		table, data string
		skip        int64
		schema      string
		rows        string
	}{
		{"all_strings", "a,b\nx,y\n", 0, "string_field_0 STRING, string_field_1 STRING", "[[a b] [x y]]"},
		{"typed_first_row", "1,x\n2,y\n", 0, "int64_field_0 INTEGER, string_field_1 STRING", "[[1 x] [2 y]]"},
		{"header_in_row_3", "title\nnote\nname,n\nbob,3\n", 3, "name STRING, n INTEGER", "[[bob 3]]"},
		{"row_2_not_a_header", "junk\n1,x\n2,y\n", 2, "int64_field_0 INTEGER, string_field_1 STRING", "[[2 y]]"},
		{"row_1_all_strings", "a,b\nx,y\n", 1, "string_field_0 STRING, string_field_1 STRING", "[[x y]]"},
	} {
		src := bigquery.NewReaderSource(strings.NewReader(c2.data))
		src.AutoDetect = true
		src.SkipLeadingRows = c2.skip
		table := ds.Table(c2.table)
		if err := runLoad(ctx, table.LoaderFrom(src)); err != nil {
			t.Errorf("%s: %v", c2.table, err)
			continue
		}
		md, err := table.Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c2.table, err)
		}
		var schema []string
		for _, f := range md.Schema {
			schema = append(schema, f.Name+" "+string(f.Type))
		}
		if got := strings.Join(schema, ", "); got != c2.schema {
			t.Errorf("%s: schema %s, want %s", c2.table, got, c2.schema)
		}
		if got := fmt.Sprint(rows(t, ctx, c, "SELECT * FROM `"+ds.DatasetID+"."+c2.table+"` ORDER BY 1")); got != c2.rows {
			t.Errorf("%s: rows %s, want %s", c2.table, got, c2.rows)
		}
	}
}

// TestBigQueryCSVNameMatchAfterSkippedRows (#1457): with sourceColumnMatch
// NAME, "Column names are read from the last skipped row based on Header
// rows to skip" (loading-data-cloud-storage-csv).
// covers: bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryCSVNameMatchAfterSkippedRows(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	src := bigquery.NewReaderSource(strings.NewReader("title\nnote,x\nb,a\nx,1\ny,2\n"))
	src.Schema = loadSchema
	src.SkipLeadingRows = 3
	src.SourceColumnMatch = bigquery.SourceColumnMatchName
	if err := runLoad(ctx, ds.Table("by_name").LoaderFrom(src)); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+".by_name` ORDER BY a")); got != "[[1 x] [2 y]]" {
		t.Errorf("rows %s, want [[1 x] [2 y]]", got)
	}
}

// TestBigQueryCSVDelimiterAndQuoteBytes (#1457): in ISO-8859-1 data any
// one character is a fieldDelimiter or quote, the raw bytes split before
// they are decoded; a quote of several characters is its first byte; a
// delimiter outside ASCII in UTF-8 data stays 501.
// covers: bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryCSVDelimiterAndQuoteBytes(t *testing.T) {
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
		want        string
	}{
		{"latin1", "1\xfe\xabcaf\xe9\xfex\xab\n2\xfe\xe9\n", func(s *bigquery.ReaderSource) {
			s.Encoding, s.FieldDelimiter, s.Quote = bigquery.ISO_8859_1, "þ", "«"
		}, "[[1 caféþx] [2 é]]"},
		{"two_char_quote", "1,'a,b'\n", func(s *bigquery.ReaderSource) { s.Quote = "'x" }, "[[1 a,b]]"},
	} {
		if err := load(c2.table, c2.data, c2.set); err != nil {
			t.Errorf("%s: %v", c2.table, err)
			continue
		}
		if got := fmt.Sprint(rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+"."+c2.table+"` ORDER BY a")); got != c2.want {
			t.Errorf("%s: rows %s, want %s", c2.table, got, c2.want)
		}
	}
	err := load("utf8_e", "1é\xc3\xa9x\n", func(s *bigquery.ReaderSource) { s.FieldDelimiter = "é" })
	wantReason(t, "a delimiter outside ASCII in UTF-8 data", err, http.StatusNotImplemented, "notImplemented")
}

// TestBigQueryParquetLoadNaNAndUnlistedTypes (#1457): a NaN in a Parquet
// file loads as NaN; a column type BigQuery's conversion table does not
// list is refused, 400 invalid, with no table made.
// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.tables.get
func TestBigQueryParquetLoadNaNAndUnlistedTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	if err := runLoad(ctx, ds.Table("nan").LoaderFrom(parquetUpload(parquetFixture(t, "nan.parquet"), nil))); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT f, IS_NAN(f) FROM `"+ds.DatasetID+".nan` ORDER BY IS_NAN(f)")); got != "[[1 false] [NaN true]]" {
		t.Errorf("rows %s, want [[1 false] [NaN true]]", got)
	}
	for _, file := range []string{"ts_ns.parquet", "json.parquet"} {
		table := ds.Table("t_" + strings.TrimSuffix(file, ".parquet"))
		err := runLoad(ctx, table.LoaderFrom(parquetUpload(parquetFixture(t, file), nil)))
		wantReason(t, file, err, http.StatusBadRequest, "invalid")
		if err == nil || !strings.Contains(err.Error(), "does not list") {
			t.Errorf("%s: %v", file, err)
		}
		if tableExists(t, ctx, table) {
			t.Errorf("%s: the table was made", file)
		}
	}
}
