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
// delimiter is its first byte, in UTF-8 data the first of its UTF-8 form;
// a quote outside ISO-8859-1 stays 501.
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
		// #1457, second part: a delimiter of several characters is its
		// first byte; in UTF-8 data a delimiter outside ASCII splits on the
		// first byte of its UTF-8 form, the rest of it part of the next
		// value, read as U+FFFD; a quote is its ISO-8859-1 byte.
		{"two_char_delim", "1|x\n2|y\n", func(s *bigquery.ReaderSource) { s.FieldDelimiter = "||" }, "[[1 x] [2 y]]"},
		{"utf8_delim", "1þx\n", func(s *bigquery.ReaderSource) { s.FieldDelimiter = "þ" }, "[[1 \ufffdx]]"},
		{"utf8_quote", "1,\xabx,y\xab\n", func(s *bigquery.ReaderSource) { s.Quote = "«" }, "[[1 x,y]]"},
	} {
		if err := load(c2.table, c2.data, c2.set); err != nil {
			t.Errorf("%s: %v", c2.table, err)
			continue
		}
		if got := fmt.Sprint(rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+"."+c2.table+"` ORDER BY a")); got != c2.want {
			t.Errorf("%s: rows %s, want %s", c2.table, got, c2.want)
		}
	}
	err := load("snowman_quote", "1,x\n", func(s *bigquery.ReaderSource) { s.Quote = "☃" })
	wantReason(t, "a quote outside ISO-8859-1", err, http.StatusNotImplemented, "notImplemented")
}

// TestBigQueryCSVEmptyStringWithNullMarker (#1457): with a nullMarker, an
// empty value loads into a STRING column as an empty string and the
// marker as NULL ("For STRING and BYTE columns, BigQuery interprets the
// empty string as an empty value", JobConfigurationLoad), through the
// emulator's patch 0038; into an INTEGER column it is 400 invalid.
// covers: bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryCSVEmptyStringWithNullMarker(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table, data string) error {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.Schema = loadSchema
		src.NullMarker = `\N`
		return runLoad(ctx, ds.Table(table).LoaderFrom(src))
	}
	if err := load("empty", "1,\n2,\\N\n3,x\n"); err != nil {
		t.Fatal(err)
	}
	q := "SELECT a, b IS NULL, LENGTH(b) FROM `" + ds.DatasetID + ".empty` ORDER BY a"
	if got := fmt.Sprint(rows(t, ctx, c, q)); got != "[[1 false 0] [2 true <nil>] [3 false 1]]" {
		t.Errorf("rows %s, want [[1 false 0] [2 true <nil>] [3 false 1]]", got)
	}
	wantReason(t, "an empty INTEGER", load("empty_int", ",x\n"), http.StatusBadRequest, "invalid")
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

// TestBigQueryLoadBadValuesUnderMaxBadRecords (#1457): with maxBadRecords,
// a CSV or JSON record holding a value its column's type refuses is a
// bad record ("the maximum number of rows containing errors that can be
// ignored"), left out and counted; a BOOL in each documented form loads.
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.query
func TestBigQueryLoadBadValuesUnderMaxBadRecords(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	schema := bigquery.Schema{
		{Name: "i", Type: bigquery.IntegerFieldType},
		{Name: "b", Type: bigquery.BooleanFieldType},
		{Name: "d", Type: bigquery.DateFieldType},
	}
	csvSrc := bigquery.NewReaderSource(strings.NewReader("1,yes,2024-01-02\nx,t,2024-01-02\n2,maybe,2024-01-02\n" +
		"3,N,2024-02-30\n4,F,\n"))
	csvSrc.Schema, csvSrc.MaxBadRecords = schema, 3
	jsonSrc := bigquery.NewReaderSource(strings.NewReader(`{"i":1,"b":"yes","d":"2024-01-02"}` + "\n" + `{"i":"x"}` +
		"\n" + `{"i":2,"b":"maybe"}` + "\n" + `{"i":3,"d":"2024-02-30"}` + "\n" + `{"i":4,"b":"F"}` + "\n"))
	jsonSrc.SourceFormat, jsonSrc.Schema, jsonSrc.MaxBadRecords = bigquery.JSON, schema, 3
	for _, c2 := range []struct {
		table string
		src   *bigquery.ReaderSource
	}{{"csv_bad", csvSrc}, {"json_bad", jsonSrc}} {
		job, err := ds.Table(c2.table).LoaderFrom(c2.src).Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c2.table, err)
		}
		st, err := job.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Errorf("%s: %v", c2.table, err)
			continue
		}
		if ls, ok := st.Statistics.Details.(*bigquery.LoadStatistics); !ok || ls.OutputRows != 2 || len(st.Errors) != 3 {
			t.Errorf("%s: statistics %+v and %d errors, want 2 rows and 3 bad records", c2.table, st.Statistics.Details, len(st.Errors))
		}
		q := "SELECT i, b, d IS NULL FROM `" + ds.DatasetID + "." + c2.table + "` ORDER BY i"
		if got := fmt.Sprint(rows(t, ctx, c, q)); got != "[[1 true false] [4 false true]]" {
			t.Errorf("%s: rows %s, want [[1 true false] [4 false true]]", c2.table, got)
		}
	}
	csv := bigquery.NewReaderSource(strings.NewReader("x,t,2024-01-02\n1,t,2024-01-02\n"))
	csv.Schema = schema
	if err := runLoad(ctx, ds.Table("csv_no_max").LoaderFrom(csv)); err == nil {
		t.Error("a bad value without maxBadRecords loaded")
	}
}

// TestBigQueryJSONLoadMixedTypesIntoJSONColumn (#1457): a JSON column
// holds a field whose values mix types, as the JSON loading page's
// example loads "address" as a STRING, a STRUCT or an ARRAY ("The address
// field is loaded into a column with type JSON that allows it to hold the
// mixed types").
// covers: bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryJSONLoadMixedTypesIntoJSONColumn(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	src := bigquery.NewReaderSource(strings.NewReader(`{"id":"1","address":"123 First Avenue"}` + "\n" +
		`{"id":"2","address":{"city":"New York","zip":"33333"}}` + "\n" +
		`{"id":"3","address":[{"city":"New York"},"321 Main Street"]}` + "\n" + `{"id":"4","address":7}` + "\n"))
	src.SourceFormat = bigquery.JSON
	src.Schema = bigquery.Schema{{Name: "id", Type: bigquery.StringFieldType}, {Name: "address", Type: bigquery.JSONFieldType}}
	if err := runLoad(ctx, ds.Table("mixed").LoaderFrom(src)); err != nil {
		t.Fatal(err)
	}
	q := "SELECT id, JSON_TYPE(address), TO_JSON_STRING(address) FROM `" + ds.DatasetID + ".mixed` ORDER BY id"
	want := `[[1 string "123 First Avenue"] [2 object {"city":"New York","zip":"33333"}] ` +
		`[3 array [{"city":"New York"},"321 Main Street"]] [4 number 7]]`
	if got := fmt.Sprint(rows(t, ctx, c, q)); got != want {
		t.Errorf("rows %s, want %s", got, want)
	}
}
