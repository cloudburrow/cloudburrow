//go:build compat

package compat

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryPastIngestionTimeCopies (#1458): FOR SYSTEM_TIME AS OF a
// past time of an ingestion-time partitioned table, in a snapshot, a
// clone and a copy job of table@time, makes a table partitioned as the
// table is, each row in its partition of then (its _PARTITIONTIME kept,
// as a copy of the table keeps it); and a query whose select list has a *
// and that names _PARTITIONTIME or _PARTITIONDATE reads the past rows
// with * giving the table's columns only, as of the table itself
// (https://cloud.google.com/bigquery/docs/querying-partitioned-tables:
// the pseudo-columns are not in SELECT *). Times are the instance clock.
// Measured first: each of these was 501 ("keeps no _PARTITIONTIME", "a
// query whose select list has a *").
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryPastIngestionTimeCopies(t *testing.T) {
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
				switch x := v.(type) {
				case time.Time:
					v = x.UTC().Format("2006-01-02")
				case nil:
					v = "NULL"
				}
				cells = append(cells, fmt.Sprint(v))
			}
			out = append(out, strings.Join(cells, " "))
		}
		return out
	}

	p := "`" + d + ".pi`"
	exec("CREATE TABLE " + p + " (n INT64, s STRING) PARTITION BY _PARTITIONDATE")
	exec("INSERT INTO " + p + " (_PARTITIONTIME, n, s) VALUES (TIMESTAMP '2024-01-01', 1, 'a'), (TIMESTAMP '2024-01-02', 2, 'b')")
	mid := advanceBigQueryClock(t, h, time.Second)
	advanceBigQueryClock(t, h, time.Second)
	exec("INSERT INTO " + p + " (_PARTITIONTIME, n, s) VALUES (TIMESTAMP '2024-01-02', 3, 'c')")
	exec("UPDATE " + p + " SET s = 'z' WHERE n = 1")
	asOf := " FOR SYSTEM_TIME AS OF TIMESTAMP '" + mid.Format(time.RFC3339Nano) + "'"

	// A query with a * that names the pseudo-columns.
	for _, insert := range []bool{false, true} {
		if g := read("SELECT *, _PARTITIONDATE AS pd FROM "+p+asOf+" ORDER BY n", insert); strings.Join(g, ",") != "1 a 2024-01-01,2 b 2024-01-02" {
			t.Errorf("insert=%v: SELECT *, _PARTITIONDATE of then: %v, want 1 a 2024-01-01, 2 b 2024-01-02", insert, g)
		}
		if g := read("SELECT * FROM "+p+asOf+" WHERE _PARTITIONTIME = TIMESTAMP '2024-01-02'", insert); strings.Join(g, ",") != "2 b" {
			t.Errorf("insert=%v: SELECT * WHERE _PARTITIONTIME of then: %v, want 2 b", insert, g)
		}
		if g := read("SELECT t.*, t._PARTITIONTIME FROM "+p+" AS t"+asOf+" WHERE t.n = 1", insert); strings.Join(g, ",") != "1 a 2024-01-01" {
			t.Errorf("insert=%v: SELECT t.*, t._PARTITIONTIME of then: %v, want 1 a 2024-01-01", insert, g)
		}
	}

	// A snapshot, a clone and a copy job of the table as it was.
	exec("CREATE SNAPSHOT TABLE `" + d + ".pi_snap` CLONE " + p + asOf)
	exec("CREATE TABLE `" + d + ".pi_clone` CLONE " + p + asOf)
	cp := ds.Table("pi_copy").CopierFrom(ds.Table("pi@" + strconv.FormatInt(mid.UnixMilli(), 10)))
	job, err := cp.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("copy of pi@%d: %v", mid.UnixMilli(), err)
	}
	for _, table := range []string{"pi_snap", "pi_clone", "pi_copy"} {
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if tp := md.TimePartitioning; tp == nil || tp.Field != "" || tp.Type != bigquery.DayPartitioningType {
			t.Errorf("%s: timePartitioning %+v, want by day of ingestion time", table, md.TimePartitioning)
		}
		for _, insert := range []bool{false, true} {
			if g := read("SELECT n, s, _PARTITIONTIME, _PARTITIONDATE FROM `"+d+"."+table+"` ORDER BY n", insert); strings.Join(g, ",") != "1 a 2024-01-01 2024-01-01,2 b 2024-01-02 2024-01-02" {
				t.Errorf("insert=%v: %s: %v, want 1 a and 2 b in their partitions of then", insert, table, g)
			}
		}
		if g := read("SELECT n FROM `"+d+"."+table+"$20240102`", false); strings.Join(g, ",") != "2" {
			t.Errorf("%s$20240102: %v, want 2", table, g)
		}
	}
}
