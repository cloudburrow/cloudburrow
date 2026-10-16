//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryTimeTravelHistory (#1460): FOR SYSTEM_TIME AS OF a past time
// inside the time travel window reads the table as it was then
// (https://cloud.google.com/bigquery/docs/time-travel): after an INSERT,
// an UPDATE, a DELETE and another INSERT, each time between them reads the
// rows as they were, through jobs.query and jobs.insert, with the time as
// a literal and as a query parameter, with an alias and in a join with the
// table as it is. A time before the table was made still fails "Invalid
// snapshot time"; a time before an ALTER TABLE that rebuilt the table
// (ADD COLUMN) reads the rows as they were, the column added NULL (#1466;
// it was 501 until then). Measured first: every past time inside the
// window was 501 "keeps no history".
//
// The times are the engine's CURRENT_TIMESTAMP(), the clock the changes
// are recorded by, never this machine's.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryTimeTravelHistory(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := "`" + ds.DatasetID + ".history`"
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	now := func() time.Time {
		t.Helper()
		got := rows(t, ctx, c, "SELECT CURRENT_TIMESTAMP()")
		ts, ok := got[0][0].(time.Time)
		if !ok {
			t.Fatalf("CURRENT_TIMESTAMP(): %v", got)
		}
		return ts
	}
	read := func(sql string, insert bool, params ...bigquery.QueryParameter) []string {
		t.Helper()
		q := c.Query(sql)
		q.Parameters = params
		var it *bigquery.RowIterator
		var err error
		if insert {
			var job *bigquery.Job
			if job, err = q.Run(ctx); err == nil {
				it, err = job.Read(ctx)
			}
		} else {
			it, err = q.Read(ctx)
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return readRows(t, it, sql)
	}
	lit := func(ts time.Time) string {
		return fmt.Sprintf("TIMESTAMP_MICROS(%d)", ts.UnixMicro())
	}

	run("CREATE TABLE " + p + " (n INT64, s STRING)")
	run("INSERT INTO " + p + " VALUES (1, 'a'), (2, 'b')")
	t1 := now()
	run("UPDATE " + p + " SET s = 'c' WHERE n = 1")
	t2 := now()
	run("DELETE " + p + " WHERE n = 2")
	t3 := now()
	run("INSERT INTO " + p + " VALUES (3, 'd')")

	want := map[time.Time][]string{
		t1: {"1|a", "2|b"},
		t2: {"1|c", "2|b"},
		t3: {"1|c"},
	}
	for _, insert := range []bool{false, true} {
		for _, at := range []time.Time{t1, t2, t3} {
			sql := "SELECT n, s FROM " + p + " FOR SYSTEM_TIME AS OF " + lit(at) + " ORDER BY n"
			if got := read(sql, insert); !reflect.DeepEqual(got, want[at]) {
				t.Errorf("insert %v: %s: %v, want %v", insert, sql, got, want[at])
			}
		}
		sql := "SELECT h.n, h.s FROM " + p + " AS h FOR SYSTEM_TIME AS OF @at WHERE h.n > 0 ORDER BY h.n"
		if got := read(sql, insert, bigquery.QueryParameter{Name: "at", Value: t2}); !reflect.DeepEqual(got, want[t2]) {
			t.Errorf("insert %v: %s: %v, want %v", insert, sql, got, want[t2])
		}
	}
	// The table as it was, joined with the table as it is.
	sql := "SELECT old.n, old.s, cur.s FROM " + p + " AS old FOR SYSTEM_TIME AS OF " + lit(t1) + " JOIN " + p +
		" AS cur USING (n) ORDER BY n"
	if got := read(sql, false); !reflect.DeepEqual(got, []string{"1|a|c"}) {
		t.Errorf("%s: %v, want [1 a c]", sql, got)
	}
	// The job keeps the statement the client sent.
	job, err := c.Query("SELECT COUNT(*) FROM " + p + " FOR SYSTEM_TIME AS OF " + lit(t1)).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if cfg, err := job.Config(); err != nil || !strings.Contains(cfg.(*bigquery.QueryConfig).Q, "FOR SYSTEM_TIME AS OF") {
		t.Errorf("job's query: %+v, %v", cfg, err)
	}

	// Before the table was made: as BigQuery documents.
	wantAPIError(t, "before creation", bqRun(ctx, c, "SELECT n FROM "+p+" FOR SYSTEM_TIME AS OF "+
		lit(t1.Add(-time.Hour)), false), http.StatusBadRequest, "Invalid snapshot time")

	// An ALTER TABLE that rebuilds the table keeps its history (#1466):
	// the rows as they were, with the column added NULL.
	run("ALTER TABLE " + p + " ADD COLUMN extra STRING")
	if got := read("SELECT n, s, extra FROM "+p+" FOR SYSTEM_TIME AS OF "+lit(t2)+" ORDER BY n", false); !reflect.DeepEqual(got, []string{"1|c|<nil>", "2|b|<nil>"}) {
		t.Errorf("before a rebuild: %v, want [1 c NULL 2 b NULL]", got)
	}
	t4 := now()
	run("DELETE " + p + " WHERE n = 1")
	if got := read("SELECT n, s, extra FROM "+p+" FOR SYSTEM_TIME AS OF "+lit(t4)+" ORDER BY n", false); !reflect.DeepEqual(got, []string{"1|c|<nil>", "3|d|<nil>"}) {
		t.Errorf("after the rebuild: %v, want [1 c NULL 3 d NULL]", got)
	}
}
