package bigqueryfront

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// TestExtractText (#957): each type's CSV and JSON text, from the value
// tabledata.list gives (measured against the pinned emulator).
func TestExtractText(t *testing.T) {
	rng := func(elem string) field {
		return field{Name: "r", Type: "RANGE", RangeElementType: &struct {
			Type string `json:"type"`
		}{elem}}
	}
	for _, c := range []struct {
		fl       field
		in, want string
	}{
		{field{Type: "TIMESTAMP"}, "1577934245.123456", "2020-01-02 03:04:05.123456 UTC"},
		{field{Type: "TIMESTAMP"}, "1.577934245E9", "2020-01-02 03:04:05 UTC"},
		{field{Type: "DATETIME"}, "2020-01-02T03:04:05.5", "2020-01-02T03:04:05.5"},
		{field{Type: "DATETIME"}, "2020-01-02 03:04:05", "2020-01-02T03:04:05"},
		{field{Type: "TIME"}, "03:04:05.25", "03:04:05.25"},
		{field{Type: "FLOAT"}, "1e+20", "1e+20"},
		{field{Type: "INTERVAL"}, "1-2 3 4:5:6.789", "1-2 3 4:5:6.789"},
		{field{Type: "GEOGRAPHY"}, "POINT(1 2)", "POINT(1 2)"},
		{rng("DATE"), "[2020-01-01, UNBOUNDED)", "[2020-01-01, UNBOUNDED)"},
		{rng("TIMESTAMP"), "[1577836800000000, 1577923200000000)", "[2020-01-01 00:00:00 UTC, 2020-01-02 00:00:00 UTC)"},
		{rng("DATETIME"), "[2020-01-01 00:00:00, UNBOUNDED)", "[2020-01-01T00:00:00, UNBOUNDED)"},
	} {
		if got, err := extractText(c.fl, c.in); err != nil || got != c.want {
			t.Errorf("%s %q: %q, %v; want %q", c.fl.Type, c.in, got, err, c.want)
		}
	}
	for _, s := range []string{"NaN", "Infinity", "-Infinity"} {
		if _, err := extractText(field{Type: "FLOAT"}, s); err == nil {
			t.Errorf("FLOAT64 %s was written", s)
		}
	}
}

// TestJSONExtractRow (#957): NULLs are left out, RECORDs are objects,
// REPEATED columns arrays, and each type has its JSON form.
func TestJSONExtractRow(t *testing.T) {
	v := func(x any) map[string]any { return map[string]any{"v": x} }
	fields := []field{{Name: "i", Type: "INTEGER"}, {Name: "f", Type: "FLOAT"}, {Name: "b", Type: "BOOLEAN"},
		{Name: "n", Type: "NUMERIC"}, {Name: "ts", Type: "TIMESTAMP"}, {Name: "j", Type: "JSON"}, {Name: "nil", Type: "STRING"},
		{Name: "a", Type: "INTEGER", Mode: "REPEATED"}, {Name: "e", Type: "STRING", Mode: "REPEATED"},
		{Name: "r", Type: "RECORD", Fields: []field{{Name: "s", Type: "STRING"}, {Name: "g", Type: "GEOGRAPHY"}}}}
	row := []any{"1", "1.5", "true", "1.25", "1577934245", `{"b":"<x>","a":[1,2.5]}`, nil,
		[]any{v("1"), v("2")}, []any{}, map[string]any{"f": []any{v("x"), v(nil)}}}
	got, err := jsonExtractRow(fields, row)
	want := `{"i":"1","f":1.5,"b":true,"n":"1.25","ts":"2020-01-02 03:04:05 UTC","j":{"a":[1,2.5],"b":"\u003cx\u003e"},"a":["1","2"],"r":{"s":"x"}}` + "\n"
	if err != nil || string(got) != want {
		t.Errorf("%s, %v; want %s", got, err, want)
	}
	if _, err := jsonExtractRow([]field{{Name: "f", Type: "FLOAT"}}, []any{"NaN"}); err == nil {
		t.Errorf("a NaN was written")
	}
	if _, err := jsonExtractRow([]field{{Name: "s", Type: "STRING", Mode: "REQUIRED"}}, []any{nil}); err == nil {
		t.Errorf("a NULL in a REQUIRED column was written")
	}
}

// TestExtractFiles (#957): over extractFileLimit, a wildcard URI's rows are
// shared between numbered files, each at most the limit.
func TestExtractFiles(t *testing.T) {
	defer func(old int64) { extractFileLimit = old }(extractFileLimit)
	extractFileLimit = 100
	rows := make([][]any, 10)
	for i := range rows {
		rows[i] = []any{"x"}
	}
	files := extractFiles(writtenExtract{uris: []string{"gs://b/a-*.csv", "gs://b/one.csv"}, size: 450}, rows)
	var names []string
	for _, f := range files {
		names = append(names, f.name)
	}
	// 5 rows, 225 bytes, to the wildcard: three files of 2, 2 and 1.
	if strings.Join(names, " ") != "gs://b/a-000000000000.csv gs://b/a-000000000001.csv gs://b/a-000000000002.csv gs://b/one.csv" ||
		len(files[0].rows) != 2 || len(files[2].rows) != 1 || len(files[3].rows) != 5 {
		t.Errorf("files %v", names)
	}
	if files := extractFiles(writtenExtract{uris: []string{"gs://b/a-*.csv"}}, nil); len(files) != 1 || files[0].name != "gs://b/a-000000000000.csv" {
		t.Errorf("an empty table: %v", files)
	}
}

// TestExtractOverOneFile (#957): a table over 1 GB fails the job with
// BigQuery's message without a wildcard, and is sharded with one.
func TestExtractOverOneFile(t *testing.T) {
	defer func(old int64) { extractFileLimit = old }(extractFileLimit)
	extractFileLimit = 10
	uploads := map[string]string{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/storage/v1/b/b":
			_, _ = io.WriteString(w, `{"name":"b"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/b/o":
			b, _ := io.ReadAll(r.Body)
			uploads[r.URL.Query().Get("name")] = string(b)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "{}", http.StatusNotFound)
		}
	}))
	defer storage.Close()
	tables := map[string]string{
		tablesBase + "big":      `{"type":"TABLE","numRows":"3","numBytes":"30","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`,
		tablesBase + "big/data": `{"rows":[{"f":[{"v":"1"}]},{"f":[{"v":"2"}]},{"f":[{"v":"3"}]}],"totalRows":"3"}`,
	}
	h := Wrap(&jobsEmulator{tables: tables}, WithStorage(storage.URL))
	job := func(id, uri string) string {
		return `{"jobReference":{"jobId":"` + id + `"},"configuration":{"extract":{"sourceTable":{"projectId":"p","datasetId":"ds","tableId":"big"},"destinationUris":["` + uri + `"],"destinationFormat":"NEWLINE_DELIMITED_JSON"}}}`
	}
	code, got := do(t, h, "POST", base+"/jobs", job("one", "gs://b/big.json"))
	st, _ := got["status"].(map[string]any)
	er, _ := st["errorResult"].(map[string]any)
	if code != 200 || er["reason"] != "invalid" || er["message"] != "Table p:ds.big too large to be exported to a single file. "+
		"Specify a uri including a * to shard export. See 'Exporting data into one or more files' in "+
		"https://cloud.google.com/bigquery/docs/exporting-data." || len(uploads) != 0 {
		t.Errorf("no wildcard: %d %v, wrote %v", code, got, uploads)
	}
	code, got = do(t, h, "POST", base+"/jobs", job("many", "gs://b/big-*.json"))
	counts := got["statistics"].(map[string]any)["extract"].(map[string]any)["destinationUriFileCounts"].([]any)
	if code != 200 || len(uploads) != 3 || uploads["big-000000000002.json"] != "{\"a\":\"3\"}\n" || counts[0] != "3" {
		t.Errorf("a wildcard: %d %v, wrote %v", code, got, uploads)
	}
}

// TestParquetExtractOfIntervalRangeAndWKT (#957): INTERVAL as a STRING,
// RANGE as a group of start and end, and GEOGRAPHY without
// nativeGeographyExportEnabled as a WKT STRING, read back by Arrow.
func TestParquetExtractOfIntervalRangeAndWKT(t *testing.T) {
	fields := []field{{Name: "i", Type: "INTERVAL"}, {Name: "g", Type: "GEOGRAPHY"},
		{Name: "r", Type: "RANGE", RangeElementType: &struct {
			Type string `json:"type"`
		}{"TIMESTAMP"}}}
	rows := [][]any{{"1-2 3 4:5:6.789", "POINT(1 2)", "[1577836800000000, UNBOUNDED)"}, {nil, nil, nil}}
	data, msg := encodeExtract(writtenExtract{parquet: true, fields: fields}, rows)
	if msg != "" {
		t.Fatal(msg)
	}
	pf, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := fr.ReadTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Release()
	if s := tbl.Schema().String(); !strings.Contains(s, "i: type=utf8") || !strings.Contains(s, "g: type=utf8") ||
		// arrow-go v18's reader: the bounds are written isAdjustedToUTC false
		// (extractparquet.go), which v18 reads as no time zone and v15 read
		// as UTC; the file is the same.
		!strings.Contains(s, "r: type=struct<start: timestamp[us] nullable, end: timestamp[us] nullable>") {
		t.Errorf("schema %s", s)
	}
	var got []string
	for i := 0; i < int(tbl.NumCols()); i++ {
		got = append(got, tbl.Column(i).Data().Chunk(0).String())
	}
	if j := strings.Join(got, " | "); j != `["1-2 3 4:5:6.789" (null)] | ["POINT(1 2)" (null)] | {[1577836800000000 (null)] [(null) (null)]}` {
		t.Errorf("values %s", j)
	}
}
