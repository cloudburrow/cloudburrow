//go:build compat

package compat

import (
	"context"
	"regexp"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryTableStoragePhysicalAndJobColumns (#1458): region
// TABLE_STORAGE has every column the TABLE_STORAGE page documents, the
// physical byte columns estimated from the logical bytes (UNVERIFIED,
// docs re-read 2026-11-17: BigQuery's are compressed sizes): current_physical_bytes
// the table's logical bytes, time_travel_physical_bytes those of the rows
// a DELETE or an UPDATE replaced within the time travel window,
// total_physical_bytes and active_physical_bytes the two added,
// long_term_physical_bytes 0 for a new table and fail_safe_physical_bytes
// 0. JOBS has query_info.query_hashes.normalized_literals, the same for
// two queries alike but for a literal and a comment and a 64-digit hex
// string, global_query_info NULL, and copy_job_statistics.copied_rows of
// a copy job. Measured first: TABLE_STORAGE had no physical byte columns
// ("Unrecognized name"), and JOBS no global_query_info, copy_job_statistics
// or query_info.query_hashes.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryTableStoragePhysicalAndJobColumns(t *testing.T) {
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

	// Three rows of 8 + 2 + 3 bytes; one deleted, one updated to 'abcd'
	// (its old row, 13 bytes, held for time travel).
	exec("CREATE TABLE " + d + ".ph (n INT64, s STRING)")
	exec("INSERT INTO " + d + ".ph VALUES (1, 'abc'), (2, 'abc'), (3, 'abc')")
	exec("DELETE FROM " + d + ".ph WHERE n = 1")
	exec("UPDATE " + d + ".ph SET s = 'abcd' WHERE n = 2")
	for _, insert := range []bool{false, true} {
		_, rows, err := infoSchemaRead(ctx, c, "SELECT total_logical_bytes, current_physical_bytes, total_physical_bytes, "+
			"active_physical_bytes, long_term_physical_bytes, time_travel_physical_bytes, fail_safe_physical_bytes "+
			"FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE WHERE table_schema = '"+d+"' AND table_name = 'ph'", insert)
		if err != nil {
			t.Fatalf("insert=%v: TABLE_STORAGE: %v", insert, err)
		}
		// Now 13 + 14 = 27; held 13 (deleted) + 13 (replaced) = 26.
		want := []bigquery.Value{int64(27), int64(27), int64(53), int64(53), int64(0), int64(26), int64(0)}
		if len(rows) != 1 || len(rows[0]) != len(want) {
			t.Fatalf("insert=%v: TABLE_STORAGE: %v", insert, rows)
		}
		for i, w := range want {
			if rows[0][i] != w {
				t.Errorf("insert=%v: TABLE_STORAGE column %d: %v, want %v (row %v)", insert, i, rows[0][i], w, rows[0])
			}
		}
	}

	// JOBS: query hashes, global_query_info, copy_job_statistics.
	run := func(sql string) string {
		t.Helper()
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		j, err := c.Query(sql).Run(qctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if st, err := j.Wait(qctx); err != nil || st.Err() != nil {
			t.Fatalf("%s: %v, %v", sql, err, st)
		}
		return j.ID()
	}
	a := run("SELECT n FROM " + d + ".ph WHERE n > 1 /* first */")
	b := run("SELECT n FROM " + d + ".ph WHERE n > 2")
	other := run("SELECT s FROM " + d + ".ph WHERE n > 1")
	cp, err := ds.Table("ph_copy").CopierFrom(ds.Table("ph")).Run(ctx)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if st, err := cp.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("copy: %v, %v", err, st)
	}
	hex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, insert := range []bool{false, true} {
		_, rows, err := infoSchemaRead(ctx, c, "SELECT job_id, query_info.query_hashes.normalized_literals, global_query_info IS NULL, "+
			"copy_job_statistics.copied_rows FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id IN ('"+a+"', '"+b+"', '"+other+"', '"+cp.ID()+"')", insert)
		if err != nil {
			t.Fatalf("insert=%v: JOBS: %v", insert, err)
		}
		got := map[string][]bigquery.Value{}
		for _, r := range rows {
			got[r[0].(string)] = r
		}
		if len(got) != 4 {
			t.Fatalf("insert=%v: JOBS: %v", insert, rows)
		}
		ha, _ := got[a][1].(string)
		if !hex.MatchString(ha) || got[b][1] != ha || got[other][1] == ha || got[other][1] == nil {
			t.Errorf("insert=%v: normalized_literals: %v, %v, %v", insert, got[a][1], got[b][1], got[other][1])
		}
		for _, id := range []string{a, cp.ID()} {
			if got[id][2] != true {
				t.Errorf("insert=%v: global_query_info of %s: not NULL", insert, id)
			}
		}
		if got[a][3] != nil || got[cp.ID()][3] != int64(2) || got[cp.ID()][1] != nil {
			t.Errorf("insert=%v: copy_job_statistics.copied_rows: query %v, copy %v (hash %v)", insert, got[a][3], got[cp.ID()][3], got[cp.ID()][1])
		}
	}
}
