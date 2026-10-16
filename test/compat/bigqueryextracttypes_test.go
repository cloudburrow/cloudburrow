//go:build compat

package compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/linkedin/goavro/v2"
)

// TestBigQueryExtractOfEveryType (#957), through the official Go client:
// CSV and NEWLINE_DELIMITED_JSON extracts of every column type, each
// value in its documented text form (a TIMESTAMP "… UTC", a DATETIME with
// a "T", an INTERVAL "Y-M D H:M:S.F", a RANGE "[lower, upper)", a
// GEOGRAPHY as WKT), JSON with its NULLs left out and RECORDs and REPEATED
// columns nested; CSV of a nested table refused with BigQuery's message;
// AVRO of an INTERVAL; and PARQUET of an INTERVAL, a RANGE and a GEOGRAPHY
// without nativeGeographyExportEnabled (a WKT string). Each file is read
// back from the instance's Cloud Storage and decoded with encoding/csv,
// encoding/json, goavro and Arrow.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryExtractOfEveryType(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-types")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	names := []string{"flat.csv", "flat.json", "nested.json", "nested.csv", "flat.avro", "flat.parquet"}
	t.Cleanup(func() {
		for _, n := range names {
			_ = bucket.Object(n).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	for _, sql := range []string{
		"CREATE TABLE `" + ds.DatasetID + ".flat` (f FLOAT64, ts TIMESTAMP, dt DATETIME, tm TIME, j JSON, g GEOGRAPHY, " +
			"i INTERVAL, r RANGE<DATE>, rt RANGE<TIMESTAMP>, b BOOL, s STRING)",
		"INSERT `" + ds.DatasetID + ".flat` VALUES (1.5, TIMESTAMP '2020-01-02 03:04:05.123456', DATETIME '2020-01-02 03:04:05.5', " +
			"TIME '03:04:05.25', JSON '{\"b\": \"<x>\", \"a\": [1, 2]}', ST_GEOGPOINT(1, 2), INTERVAL '1-2 3 4:5:6.789' YEAR TO SECOND, " +
			"RANGE<DATE> '[2020-01-01, UNBOUNDED)', RANGE<TIMESTAMP> '[2020-01-01 00:00:00, 2020-01-02 00:00:00)', true, 'x')",
		"CREATE TABLE `" + ds.DatasetID + ".nested` (a INT64, tags ARRAY<STRING>, r STRUCT<s STRING, n INT64>)",
		"INSERT `" + ds.DatasetID + ".nested` VALUES (1, ['p', 'q'], STRUCT('x' AS s, NULL AS n)), (2, [], NULL)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	extract := func(table, name string, format bigquery.DataFormat) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		ref.DestinationFormat = format
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table(table).ExtractorTo(ref).Run(ectx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ectx)
		if err == nil {
			err = st.Err()
		}
		return err
	}
	read := func(name string) []byte {
		t.Helper()
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return b
	}

	// CSV: the header, then each value's text.
	if err := extract("flat", "flat.csv", bigquery.CSV); err != nil {
		t.Fatalf("a CSV extract of every type: %v", err)
	}
	recs, err := csv.NewReader(bytes.NewReader(read("flat.csv"))).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("flat.csv: %v %v", recs, err)
	}
	wantCSV := []string{"1.5", "2020-01-02 03:04:05.123456 UTC", "2020-01-02T03:04:05.5", "03:04:05.25", `{"a":[1,2],"b":"<x>"}`,
		"POINT(1 2)", "1-2 3 4:5:6.789", "[2020-01-01, UNBOUNDED)", "[2020-01-01 00:00:00 UTC, 2020-01-02 00:00:00 UTC)", "true", "x"}
	if !reflect.DeepEqual(recs[1], wantCSV) {
		t.Errorf("flat.csv row %q,\nwant %q", recs[1], wantCSV)
	}

	// NEWLINE_DELIMITED_JSON: the same text, FLOAT64 a number, BOOL a
	// boolean, the JSON column its value.
	if err := extract("flat", "flat.json", bigquery.JSON); err != nil {
		t.Fatalf("a JSON extract of every type: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(read("flat.json"), &row); err != nil {
		t.Fatalf("flat.json: %v", err)
	}
	wantJSON := map[string]any{"f": 1.5, "ts": wantCSV[1], "dt": wantCSV[2], "tm": wantCSV[3],
		"j": map[string]any{"a": []any{1.0, 2.0}, "b": "<x>"}, "g": "POINT(1 2)", "i": wantCSV[6], "r": wantCSV[7], "rt": wantCSV[8],
		"b": true, "s": "x"}
	if !reflect.DeepEqual(row, wantJSON) {
		t.Errorf("flat.json %v,\nwant %v", row, wantJSON)
	}

	// Nested JSON: RECORDs as objects, REPEATED as arrays, NULLs and an
	// empty array left out.
	if err := extract("nested", "nested.json", bigquery.JSON); err != nil {
		t.Fatalf("a JSON extract of a nested table: %v", err)
	}
	var lines []string
	sc2 := bufio.NewScanner(bytes.NewReader(read("nested.json")))
	for sc2.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc2.Bytes(), &v); err != nil {
			t.Fatalf("nested.json line %q: %v", sc2.Text(), err)
		}
		b, _ := json.Marshal(v)
		lines = append(lines, string(b))
	}
	if strings.Join(lines, "\n") != `{"a":"1","r":{"s":"x"},"tags":["p","q"]}`+"\n"+`{"a":"2"}` {
		t.Errorf("nested.json read back %q", lines)
	}

	// CSV of a nested table: BigQuery's refusal, nothing written.
	err = extract("nested", "nested.csv", bigquery.CSV)
	if err == nil || !strings.Contains(err.Error(), "Operation cannot be performed on a nested schema. Field: tags") {
		t.Errorf("a CSV extract of a nested table = %v, want BigQuery's refusal", err)
	}
	if _, err := bucket.Object("nested.csv").Attrs(ctx); err == nil {
		t.Errorf("nested.csv was written")
	}

	// AVRO of an INTERVAL (with no RANGE, which Avro refuses).
	for _, sql := range []string{
		"CREATE TABLE `" + ds.DatasetID + ".iv` (i INTERVAL, g GEOGRAPHY, r RANGE<TIMESTAMP>)",
		"INSERT `" + ds.DatasetID + ".iv` VALUES (INTERVAL '1-2 3 4:5:6.789' YEAR TO SECOND, ST_GEOGPOINT(1, 2), " +
			"RANGE<TIMESTAMP> '[2020-01-01 00:00:00, UNBOUNDED)')",
		"CREATE TABLE `" + ds.DatasetID + ".ivonly` AS SELECT i FROM `" + ds.DatasetID + ".iv`",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := extract("ivonly", "flat.avro", bigquery.Avro); err != nil {
		t.Fatalf("an AVRO extract of an INTERVAL: %v", err)
	}
	ocf, err := goavro.NewOCFReader(bytes.NewReader(read("flat.avro")))
	if err != nil {
		t.Fatal(err)
	}
	if s := ocf.Codec().Schema(); !strings.Contains(s, `"sqlType":"INTERVAL"`) {
		t.Errorf("AVRO writer schema %s, want a string of sqlType INTERVAL", s)
	}
	if !ocf.Scan() {
		t.Fatal("flat.avro has no rows")
	}
	if v, err := ocf.Read(); err != nil || v.(map[string]any)["i"].(map[string]any)["string"] != "1-2 3 4:5:6.789" {
		t.Errorf("AVRO row %v, %v", v, err)
	}

	// PARQUET: INTERVAL a STRING, GEOGRAPHY a WKT STRING, RANGE a group
	// of start and end.
	if err := extract("iv", "flat.parquet", bigquery.Parquet); err != nil {
		t.Fatalf("a PARQUET extract of an INTERVAL, a GEOGRAPHY and a RANGE: %v", err)
	}
	pf, err := file.NewParquetReader(bytes.NewReader(read("flat.parquet")))
	if err != nil {
		t.Fatal(err)
	}
	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := fr.ReadTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Release()
	var got []string
	for i := 0; i < int(tbl.NumCols()); i++ {
		got = append(got, tbl.Schema().Field(i).Name+" "+tbl.Schema().Field(i).Type.String()+" "+tbl.Column(i).Data().Chunk(0).String())
	}
	want := []string{`i utf8 ["1-2 3 4:5:6.789"]`, `g utf8 ["POINT(1 2)"]`,
		`r struct<start: timestamp[us, tz=UTC], end: timestamp[us, tz=UTC]> {[1577836800000000] [(null)]}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PARQUET read back %q,\nwant %q", got, want)
	}
}
