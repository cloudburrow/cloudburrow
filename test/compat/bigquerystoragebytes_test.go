//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryPartitionsBytesAndWriteTimes (#1458): INFORMATION_SCHEMA
// PARTITIONS' total_logical_bytes and total_billable_bytes, and region
// TABLE_STORAGE's total_logical_bytes, active_logical_bytes and
// long_term_logical_bytes, are the rows' logical bytes by BigQuery's
// documented data type sizes (INT64 and DATE 8, STRING 2 + its UTF-8
// bytes, an ARRAY the sum of its elements, a STRUCT the sum of its
// fields, GEOGRAPHY 16 + 24 per vertex, JSON its text, NULL 0), streamed
// rows counted; PARTITIONS' last_modified_time is "the most recent time
// that data was written to the partition" and TABLE_STORAGE's
// storage_last_modified_time the table's, the engine's transaction time
// (bounded by its CURRENT_TIMESTAMP before and after), and a write to one partition moves only its time. JOBS
// has bi_engine_statistics, session_info, continuous_query_info and
// ml_statistics, NULL for a query job outside a session.
// Measured first: PARTITIONS and TABLE_STORAGE had no byte columns or
// write times, and JOBS none of the four.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tabledata.insertAll
func TestBigQueryPartitionsBytesAndWriteTimes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	exec := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	at := func(v bigquery.Value) time.Time {
		ts, _ := v.(time.Time)
		return ts
	}
	// The write times are the engine's transaction times, which do not
	// follow the front's clock (clock:advance moves the front's only), so
	// the bounds are the engine's CURRENT_TIMESTAMP, in microseconds.
	engineNow := func() time.Time {
		t.Helper()
		_, rows, err := infoSchemaRead(ctx, c, "SELECT CURRENT_TIMESTAMP()", false)
		if err != nil || len(rows) != 1 {
			t.Fatalf("CURRENT_TIMESTAMP(): %v, %v", rows, err)
		}
		return at(rows[0][0])
	}
	within := func(what string, got, from, to time.Time) {
		t.Helper()
		if got.IsZero() || got.Before(from) || got.After(to) {
			t.Errorf("%s: %v, want between %v and %v", what, got, from, to)
		}
	}

	exec("CREATE TABLE " + d + ".bp (n INT64, d DATE, s STRING, a ARRAY<INT64>, r STRUCT<x INT64, y STRING>, g GEOGRAPHY, j JSON) PARTITION BY d")
	before := engineNow()
	exec("INSERT INTO " + d + ".bp VALUES (1, DATE '2024-01-01', 'abc', [1, 2, 3], STRUCT(1, 'xy'), ST_GEOGPOINT(1, 2), JSON '{\"a\":1}'), " +
		"(2, DATE '2024-01-02', NULL, [], NULL, NULL, NULL)")
	after := engineNow()

	partitions := func(insert bool) map[string][]bigquery.Value {
		t.Helper()
		_, rows, err := infoSchemaRead(ctx, c, "SELECT partition_id, total_rows, total_logical_bytes, total_billable_bytes, "+
			"last_modified_time FROM "+d+".INFORMATION_SCHEMA.PARTITIONS WHERE table_name = 'bp'", insert)
		if err != nil {
			t.Fatalf("insert=%v: PARTITIONS: %v", insert, err)
		}
		out := map[string][]bigquery.Value{}
		for _, r := range rows {
			out[r[0].(string)] = r
		}
		return out
	}
	for _, insert := range []bool{false, true} {
		got := partitions(insert)
		// 8 + 8 + (2 + 3) + 3 * 8 + (8 + 2 + 2) + (16 + 24) + 7, and 8 + 8.
		p1, p2 := got["20240101"], got["20240102"]
		if len(got) != 2 || p1 == nil || p2 == nil || p1[1] != int64(1) || p1[2] != int64(104) || p1[3] != int64(104) ||
			p2[2] != int64(16) || p2[3] != int64(16) {
			t.Fatalf("insert=%v: PARTITIONS of bp: %v", insert, got)
		}
		within("last_modified_time of 20240101", at(p1[4]), before, after)
		within("last_modified_time of 20240102", at(p2[4]), before, after)
	}

	// A later write to one partition moves its time only.
	first := partitions(false)["20240101"][4]
	before = engineNow()
	exec("INSERT INTO " + d + ".bp (n, d, s) VALUES (3, DATE '2024-01-02', 'é')")
	after = engineNow()
	got := partitions(false)
	if got["20240101"][4] != first {
		t.Errorf("last_modified_time of 20240101 after a write to 20240102: %v, want %v", got["20240101"][4], first)
	}
	within("last_modified_time of 20240102 after a write to it", at(got["20240102"][4]), before, after)
	if p2 := got["20240102"]; p2[1] != int64(2) || p2[2] != int64(16+8+8+4) {
		t.Errorf("PARTITIONS 20240102 after the write: %v, want 2 rows of 36 bytes", p2)
	}

	// Streamed rows count, and the table's write time is the stream's.
	exec("CREATE TABLE " + d + ".bs (k STRING, v FLOAT64)")
	before = engineNow()
	type row struct {
		K string  `bigquery:"k"`
		V float64 `bigquery:"v"`
	}
	if err := ds.Table("bs").Inserter().Put(ctx, []row{{"ab", 1}, {"", 2}}); err != nil {
		t.Fatalf("insertAll into bs: %v", err)
	}
	after = engineNow()
	for _, insert := range []bool{false, true} {
		_, rows, err := infoSchemaRead(ctx, c, "SELECT table_name, total_logical_bytes, active_logical_bytes, long_term_logical_bytes, "+
			"storage_last_modified_time FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE WHERE table_schema = '"+d+"' ORDER BY table_name", insert)
		if err != nil {
			t.Fatalf("insert=%v: TABLE_STORAGE: %v", insert, err)
		}
		if len(rows) != 2 || rows[0][0] != "bp" || rows[0][1] != int64(140) || rows[0][2] != int64(140) || rows[0][3] != int64(0) ||
			rows[1][0] != "bs" || rows[1][1] != int64(4+8+2+8) || rows[1][2] != int64(22) || rows[1][3] != int64(0) {
			t.Fatalf("insert=%v: TABLE_STORAGE: %v", insert, rows)
		}
		within("storage_last_modified_time of bs", at(rows[1][4]), before, after)
	}

	// JOBS' four RECORD columns, NULL for this query job.
	for _, insert := range []bool{false, true} {
		_, rows, err := infoSchemaRead(ctx, c, "SELECT bi_engine_statistics IS NULL, session_info IS NULL, continuous_query_info IS NULL, "+
			"ml_statistics IS NULL, COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_type = 'QUERY' GROUP BY 1, 2, 3, 4", insert)
		if err != nil || len(rows) != 1 || rows[0][0] != true || rows[0][1] != true || rows[0][2] != true || rows[0][3] != true {
			t.Errorf("insert=%v: JOBS' RECORD columns: %v, %v, want NULL", insert, rows, err)
		}
	}
}
