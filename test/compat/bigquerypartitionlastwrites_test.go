//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryPartitionLastWritesKept (#1458): a partition's last write
// outlives the engine's seven days of change records (googlesqlite patch
// 0090 keeps what the change table lets go of), so
// INFORMATION_SCHEMA.PARTITIONS' last_modified_time stays "the most recent
// time that data was written to the partition" eight and 99 days on;
// storage_tier and TABLE_STORAGE's active and long-term logical bytes of a
// table made more than 90 days ago split its partitions by whether they
// went 90 days unmodified; fail_safe_physical_bytes counts the rows
// deleted in the seven days after the time travel window ("deleted or
// changed data retained after the time-travel window"), not in
// total_physical_bytes; a copy job writes its destination's partitions,
// and tables.delete of a partition is a write the deleted rows' bytes
// count for. Before the patch the front read only the change table's
// seven days, so (docs/gaps.md, as of branch feat/1458-bq-partitions-rest)
// last_modified_time was NULL eight days after a partition's last write,
// storage_tier, active_logical_bytes and long_term_logical_bytes NULL for
// a table older than 90 days, and fail_safe_physical_bytes always 0.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.delete
func TestBigQueryPartitionLastWritesKept(t *testing.T) {
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
	within := func(what string, v bigquery.Value, from, to time.Time) {
		t.Helper()
		got, _ := v.(time.Time)
		if got.IsZero() || got.Before(from) || got.After(to) {
			t.Errorf("%s: %v, want between %v and %v", what, v, from, to)
		}
	}
	// partitions are table's PARTITIONS rows by partition_id:
	// last_modified_time and storage_tier.
	partitions := func(table string, insert bool) map[string][]bigquery.Value {
		t.Helper()
		_, rows, err := infoSchemaRead(ctx, c, "SELECT partition_id, last_modified_time, storage_tier FROM "+d+
			".INFORMATION_SCHEMA.PARTITIONS WHERE table_name = '"+table+"'", insert)
		if err != nil {
			t.Fatalf("insert=%v: PARTITIONS: %v", insert, err)
		}
		out := map[string][]bigquery.Value{}
		for _, r := range rows {
			out[fmt.Sprint(r[0])] = r[1:]
		}
		return out
	}
	storage := func(insert bool) string {
		t.Helper()
		_, rows, err := infoSchemaRead(ctx, c, "SELECT total_logical_bytes, active_logical_bytes, long_term_logical_bytes, "+
			"current_physical_bytes, total_physical_bytes, active_physical_bytes, long_term_physical_bytes, "+
			"time_travel_physical_bytes, fail_safe_physical_bytes FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE "+
			"WHERE table_schema = '"+d+"' AND table_name = 'lw'", insert)
		if err != nil || len(rows) != 1 {
			t.Fatalf("insert=%v: TABLE_STORAGE: %v %v", insert, rows, err)
		}
		return fmt.Sprint(rows[0])
	}

	// Each row is 19 logical bytes: INT64 8, a STRING of one byte 2 + 1,
	// DATE 8.
	exec("CREATE TABLE `" + d + ".lw` (n INT64, s STRING, dt DATE) PARTITION BY dt")
	w0 := now()
	exec("INSERT INTO `" + d + ".lw` VALUES (1, 'a', '2024-01-01'), (2, 'b', '2024-01-02'), (3, 'c', '2024-01-03')")
	if err := ds.Table("lw$20240103").Delete(ctx); err != nil {
		t.Fatalf("tables.delete lw$20240103: %v", err)
	}
	w0end := now()

	// Eight days on, a write to 20240102: the engine's change table lets
	// go of the first writes, and 20240101 keeps its time.
	advanceBigQueryClock(t, h, 8*24*time.Hour)
	w1 := now()
	exec("INSERT INTO `" + d + ".lw` VALUES (4, 'd', '2024-01-02')")
	w1end := now()
	for _, insert := range []bool{false, true} {
		got := partitions("lw", insert)
		if len(got) != 2 {
			t.Fatalf("insert=%v: PARTITIONS of lw eight days on: %v, want 20240101 and 20240102", insert, got)
		}
		within("last_modified_time of 20240101 eight days on", got["20240101"][0], w0, w0end)
		within("last_modified_time of 20240102 eight days on", got["20240102"][0], w1, w1end)
		// Row 3, deleted eight days ago, is past the seven days of time
		// travel and in fail-safe, which total_physical_bytes leaves out.
		if g, want := storage(insert), "[57 57 0 57 57 57 0 0 19]"; g != want {
			t.Errorf("insert=%v: TABLE_STORAGE eight days on: %s, want %s", insert, g, want)
		}
	}

	// 91 days more: lw is 99 days old, 20240101 unmodified for 99 days
	// and 20240102 just written.
	advanceBigQueryClock(t, h, 91*24*time.Hour)
	w2 := now()
	exec("INSERT INTO `" + d + ".lw` VALUES (5, 'e', '2024-01-02')")
	w2end := now()
	for _, insert := range []bool{false, true} {
		got := partitions("lw", insert)
		if len(got) != 2 {
			t.Fatalf("insert=%v: PARTITIONS of lw 99 days on: %v", insert, got)
		}
		within("last_modified_time of 20240101 99 days on", got["20240101"][0], w0, w0end)
		within("last_modified_time of 20240102 99 days on", got["20240102"][0], w2, w2end)
		if got["20240101"][1] != "LONG_TERM" || got["20240102"][1] != "ACTIVE" {
			t.Errorf("insert=%v: storage_tier 99 days on: %v, %v, want LONG_TERM, ACTIVE", insert, got["20240101"][1], got["20240102"][1])
		}
		if g, want := storage(insert), "[76 57 19 76 76 57 19 0 0]"; g != want {
			t.Errorf("insert=%v: TABLE_STORAGE 99 days on: %s, want %s", insert, g, want)
		}
	}

	// A copy job writes every partition of its destination.
	exec("CREATE TABLE `" + d + ".lwcopy` (n INT64, s STRING, dt DATE) PARTITION BY dt")
	w3 := now()
	cp := ds.Table("lwcopy").CopierFrom(ds.Table("lw"))
	cp.WriteDisposition = bigquery.WriteAppend
	job, err := cp.Run(ctx)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("copy: %v %v", err, st)
	}
	w3end := now()
	got := partitions("lwcopy", false)
	if len(got) != 2 {
		t.Fatalf("PARTITIONS of lwcopy: %v", got)
	}
	for _, id := range []string{"20240101", "20240102"} {
		within("last_modified_time of lwcopy's "+id, got[id][0], w3, w3end)
		if got[id][1] != "ACTIVE" {
			t.Errorf("storage_tier of lwcopy's %s: %v, want ACTIVE", id, got[id][1])
		}
	}
}
