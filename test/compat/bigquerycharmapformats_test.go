//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryColumnNameCharacterMapFormats (#1336): BigQuery documents
// columnNameCharacterMap as the "Character map supported for column names
// in CSV/Parquet loads", and "Using this option with unsupporting load
// formats will result in an error" (JobConfigurationLoad), so a
// NEWLINE_DELIMITED_JSON load that gives it is 400 invalid, through the
// official Go client's Loader.ColumnNameCharacterMap, and nothing is
// loaded. A Parquet file's column names are held to the map: a name the
// rules refuse ("!") is 400 under STRICT, the default ("Load jobs with
// unsupported characters in column names fail with an error message");
// under V1 or V2, where BigQuery replaces the unsupported characters with
// underscores ("except periods in Parquet file column names"), it loads
// as "_" (#1373, TestBigQueryParquetColumnNameCharacterMap). A name the
// rules take loads under any. Measured first: a JSON load under V2 loaded, and
// a Parquet column "!" was 501 under STRICT too.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryColumnNameCharacterMapFormats(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	run := func(table string, src bigquery.LoadSource, charMap bigquery.ColumnNameCharacterMap) error {
		l := ds.Table(table).LoaderFrom(src)
		l.ColumnNameCharacterMap = charMap
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		job, err := l.Run(lctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(lctx)
		if err != nil {
			return err
		}
		return st.Err()
	}

	for _, m := range []bigquery.ColumnNameCharacterMap{bigquery.V2ColumnNameCharacterMap, bigquery.StrictColumnNameCharacterMap} {
		src := bigquery.NewReaderSource(strings.NewReader(`{"a":1}` + "\n"))
		src.SourceFormat = bigquery.JSON
		src.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}
		err := run("json_"+strings.ToLower(string(m)), src, m)
		wantHTTPStatus(t, "a JSON load under columnNameCharacterMap "+string(m), err, http.StatusBadRequest)
		if err != nil && !strings.Contains(err.Error(), "CSV and Parquet") {
			t.Errorf("the JSON load's error %v, want it to name CSV and Parquet", err)
		}
		if tableExists(t, ctx, ds.Table("json_"+strings.ToLower(string(m)))) {
			t.Errorf("the refused JSON load under %s made its table", m)
		}
	}

	// The file of TestBigQueryLoadsReportCounts with its column b renamed
	// (the name's one byte replaced, in its schema element and its column
	// chunk's path).
	parquet, err := base64.StdEncoding.DecodeString(loadStatsParquet)
	if err != nil {
		t.Fatal(err)
	}
	named := func(name string) string {
		if strings.Count(string(parquet), "\x18\x01b") != 2 {
			t.Fatal("the Parquet file does not name its column b twice")
		}
		return strings.ReplaceAll(string(parquet), "\x18\x01b", "\x18\x01"+name)
	}
	for _, tc := range []struct {
		table, column, loaded string
		charMap               bigquery.ColumnNameCharacterMap
		want                  int
	}{
		{"pq_bang_strict", "!", "", "", http.StatusBadRequest},
		{"pq_bang_v1", "!", "_", bigquery.V1ColumnNameCharacterMap, 0},
		{"pq_bang_v2", "!", "_", bigquery.V2ColumnNameCharacterMap, 0},
		{"pq_b_v1", "b", "b", bigquery.V1ColumnNameCharacterMap, 0},
		{"pq_b_strict", "b", "b", bigquery.StrictColumnNameCharacterMap, 0},
	} {
		err := run(tc.table, parquetUpload(named(tc.column), nil), tc.charMap)
		if tc.want != 0 {
			wantHTTPStatus(t, "a Parquet load of a column "+tc.column+" under "+string(tc.charMap), err, tc.want)
			if tableExists(t, ctx, ds.Table(tc.table)) {
				t.Errorf("%s: the refused load made its table", tc.table)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.table, err)
			continue
		}
		md, err := ds.Table(tc.table).Metadata(ctx)
		if err != nil || len(md.Schema) != 2 || md.Schema[1].Name != tc.loaded {
			t.Errorf("%s: schema %v (%v), want a and %s", tc.table, md, err, tc.loaded)
		}
	}
}
