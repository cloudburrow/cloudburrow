//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryIngestionPartitionHistory (#1458): an ingestion-time
// partitioned table's change table keeps each change's _PARTITIONTIME
// (googlesqlite patch 0050), so INFORMATION_SCHEMA.PARTITIONS'
// last_modified_time of each of its partitions is "the most recent time
// that data was written to the partition" (bounded by the instance clock
// before and after), and a write to one partition moves only its time;
// FOR SYSTEM_TIME AS OF a past time of the table reads its rows of then,
// with their _PARTITIONTIME and _PARTITIONDATE when the query names them,
// and with a partition decorator the partition's rows of then, as of a
// column-partitioned table. JOBS has the nine columns of statistics the
// emulator never makes, empty or NULL. A table made by CREATE TABLE ... AS
// SELECT holds a streamed row from DML past its streaming buffer
// (googlesqlite patch 0056). Measured first: last_modified_time was NULL
// for each partition of such a table, the reads were 501, JOBS had none of
// the nine, and a row streamed to a CREATE TABLE ... AS SELECT table was
// modifiable once out of the buffer.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tabledata.insertAll
func TestBigQueryIngestionPartitionHistory(t *testing.T) {
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
	now := func() time.Time { t.Helper(); return advanceBigQueryClock(t, h, 0) }
	at := func(v bigquery.Value) time.Time { ts, _ := v.(time.Time); return ts }
	within := func(what string, got, from, to time.Time) {
		t.Helper()
		if got.IsZero() || got.Before(from) || got.After(to) {
			t.Errorf("%s: %v, want between %v and %v", what, got, from, to)
		}
	}
	read := func(sql string, insert bool) []string {
		t.Helper()
		_, got, err := infoSchemaRead(ctx, c, sql, insert)
		if err != nil {
			t.Fatalf("insert=%v: %s: %v", insert, sql, err)
		}
		var out []string
		for _, r := range got {
			var cells []string
			for _, v := range r {
				if ts, ok := v.(time.Time); ok {
					v = ts.UTC().Format("2006-01-02")
				}
				cells = append(cells, fmt.Sprint(v))
			}
			out = append(out, strings.Join(cells, " "))
		}
		return out
	}

	p := "`" + d + ".ing`"
	exec("CREATE TABLE " + p + " (n INT64) PARTITION BY _PARTITIONDATE")
	before := now()
	exec("INSERT INTO " + p + " (_PARTITIONTIME, n) VALUES (TIMESTAMP '2024-01-01', 1), (TIMESTAMP '2024-01-02', 2)")
	after := now()
	partitions := func(insert bool) map[string]bigquery.Value {
		t.Helper()
		_, rows, err := infoSchemaRead(ctx, c, "SELECT partition_id, last_modified_time FROM "+d+
			".INFORMATION_SCHEMA.PARTITIONS WHERE table_name = 'ing'", insert)
		if err != nil {
			t.Fatalf("insert=%v: PARTITIONS: %v", insert, err)
		}
		out := map[string]bigquery.Value{}
		for _, r := range rows {
			out[r[0].(string)] = r[1]
		}
		return out
	}
	for _, insert := range []bool{false, true} {
		got := partitions(insert)
		if len(got) != 2 {
			t.Fatalf("insert=%v: PARTITIONS of ing: %v", insert, got)
		}
		within("last_modified_time of 20240101", at(got["20240101"]), before, after)
		within("last_modified_time of 20240102", at(got["20240102"]), before, after)
	}
	mid := advanceBigQueryClock(t, h, time.Second)
	first := partitions(false)["20240101"]
	before = now()
	exec("INSERT INTO " + p + " (_PARTITIONTIME, n) VALUES (TIMESTAMP '2024-01-02', 3)")
	after = now()
	got := partitions(false)
	if got["20240101"] != first {
		t.Errorf("last_modified_time of 20240101 after a write to 20240102: %v, want %v", got["20240101"], first)
	}
	within("last_modified_time of 20240102 after a write to it", at(got["20240102"]), before, after)

	// Time travel, before the third row.
	asOf := " FOR SYSTEM_TIME AS OF TIMESTAMP '" + mid.Format(time.RFC3339Nano) + "'"
	for _, insert := range []bool{false, true} {
		if g := read("SELECT n FROM "+p+asOf+" ORDER BY n", insert); strings.Join(g, ",") != "1,2" {
			t.Errorf("insert=%v: rows of then: %v, want 1, 2", insert, g)
		}
		if g := read("SELECT * FROM "+p+asOf+" ORDER BY n", insert); strings.Join(g, ",") != "1,2" {
			t.Errorf("insert=%v: SELECT * of then: %v, want 1, 2 (no pseudo-columns)", insert, g)
		}
		if g := read("SELECT n, _PARTITIONTIME, _PARTITIONDATE FROM "+p+asOf+" WHERE _PARTITIONDATE = '2024-01-02'", insert); strings.Join(g, ",") != "2 2024-01-02 2024-01-02" {
			t.Errorf("insert=%v: _PARTITIONTIME of then: %v, want 2 of 2024-01-02", insert, g)
		}
		if g := read("SELECT n FROM `"+d+".ing$20240102`"+asOf, insert); strings.Join(g, ",") != "2" {
			t.Errorf("insert=%v: ing$20240102 of then: %v, want 2", insert, g)
		}
		if g := read("SELECT n FROM `"+d+".ing$20240102` ORDER BY n", insert); strings.Join(g, ",") != "2,3" {
			t.Errorf("insert=%v: ing$20240102 now: %v, want 2, 3", insert, g)
		}
	}
	// A decorator of a column-partitioned table's past.
	exec("CREATE TABLE `" + d + ".col` (n INT64, dt DATE) PARTITION BY dt")
	exec("INSERT INTO `" + d + ".col` VALUES (1, '2024-01-01'), (2, '2024-01-02')")
	mid = advanceBigQueryClock(t, h, time.Second)
	exec("INSERT INTO `" + d + ".col` VALUES (3, '2024-01-02')")
	if g := read("SELECT n FROM `"+d+".col$20240102` FOR SYSTEM_TIME AS OF TIMESTAMP '"+mid.Format(time.RFC3339Nano)+"'", false); strings.Join(g, ",") != "2" {
		t.Errorf("col$20240102 of then: %v, want 2", g)
	}

	// JOBS' nine columns of statistics, empty or NULL.
	for _, insert := range []bool{false, true} {
		g := read("SELECT ARRAY_LENGTH(job_stages), ARRAY_LENGTH(object_storage_stats), ARRAY_LENGTH(timeline), "+
			"query_info IS NULL, materialized_view_statistics IS NULL, metadata_cache_statistics IS NULL, "+
			"search_statistics IS NULL, vector_search_statistics IS NULL, ARRAY_LENGTH(external_service_costs), COUNT(*) > 0 "+
			"FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_type = 'QUERY' GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9", insert)
		if strings.Join(g, ",") != "0 0 0 true true true true true 0 true" {
			t.Errorf("insert=%v: JOBS' statistics columns: %v, want empty and NULL", insert, g)
		}
	}

	// A CREATE TABLE ... AS SELECT table holds a streamed row past its
	// streaming buffer.
	exec("CREATE TABLE `" + d + ".ctas` AS SELECT 1 AS n")
	type row struct {
		N int64 `bigquery:"n"`
	}
	if err := ds.Table("ctas").Inserter().Put(ctx, []*row{{N: 2}}); err != nil {
		t.Fatal(err)
	}
	advanceBigQueryClock(t, h, time.Minute)
	if err := bqRun(ctx, c, "DELETE FROM `"+d+".ctas` WHERE n = 2", false); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
		t.Errorf("DELETE of a row streamed to a CREATE TABLE ... AS SELECT table a minute ago: %v, want the streaming buffer's error", err)
	}
	exec("DELETE FROM `" + d + ".ctas` WHERE n = 1")
}
