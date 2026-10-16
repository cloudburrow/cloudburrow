package bigqueryfront

import (
	"net/http"
	"strings"
	"testing"
)

// TestIcebergCreateChecksColumnTypes (#1427): the CREATE TABLE form of a
// table for Apache Iceberg checks its column list's types as tables.insert
// does, at any position, ARRAY and STRUCT included.
func TestIcebergCreateChecksColumnTypes(t *testing.T) {
	opts := " WITH CONNECTION `p.us.c` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = 'gs://b/t')"
	for cols, bad := range map[string]string{
		"(i INT64 NOT NULL, s STRING, n NUMERIC(10, 2), ts TIMESTAMP)": "",
		"(i INT64, j JSON)":                        "j (JSON)",
		"(i INT64, a ARRAY<INT64>)":                "a (ARRAY<INTEGER>)",
		"(r STRUCT<x INT64>, i INT64)":             "r (RECORD)",
		"(i INT64, b BIGNUMERIC)":                  "b (BIGNUMERIC)",
		"(g GEOGRAPHY OPTIONS (description = ''))": "g (GEOGRAPHY)",
	} {
		d, msg, ok := parseDDLOptions("CREATE TABLE ds.t " + cols + opts)
		if !ok || msg != "" || d.invalid != "" || !d.iceberg() {
			t.Fatalf("%s: parsed %v %q %q iceberg %v", cols, ok, msg, d.invalid, d.iceberg())
		}
		got := front{}.icebergColumnsOf(nil, queryOptions{}, d)
		if bad == "" && got != "" || bad != "" && !strings.Contains(got, "the column "+bad+" ") {
			t.Errorf("%s: %q, want the column %q", cols, got, bad)
		}
	}
	if got := ctasQuery("CREATE TABLE t WITH CONNECTION c OPTIONS (table_format = 'ICEBERG') AS SELECT (1) AS i;"); got != " SELECT (1) AS i" {
		t.Errorf("ctasQuery: %q", got)
	}
	if d, _, _ := parseDDLOptions("CREATE TABLE ds.t (j JSON) OPTIONS (description = 'x')"); d.iceberg() {
		t.Error("a table of no table_format is a table for Apache Iceberg")
	}
}

// TestIcebergWriteError (#1427): a write whose files could not be written
// is answered by Cloud Storage's status where BigQuery has its reason.
func TestIcebergWriteError(t *testing.T) {
	it := icebergTable{Dataset: "ds", Table: "t", Location: "gs://b/t"}
	for msg, want := range map[string]int{
		"writing gs://b/t/data/x.parquet: Cloud Storage answered 404: no bucket": http.StatusNotFound,
		"writing gs://b/t/data/x.parquet: Cloud Storage answered 403: denied":    http.StatusForbidden,
		"writing gs://b/t/data/x.parquet: dial tcp: refused":                     http.StatusBadRequest,
	} {
		status, why := icebergWriteError(it, msg)
		if status != want || !strings.Contains(why.Message, "ds.t") || !strings.Contains(why.Message, msg) {
			t.Errorf("%s: %d %+v, want %d", msg, status, why, want)
		}
	}
}
