//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// currentMicros reads the engine's CURRENT_TIMESTAMP() as an expression
// of that time, for FOR SYSTEM_TIME AS OF.
func currentMicros(t *testing.T, h *Harness, c *bigquery.Client) string {
	t.Helper()
	got := rows(t, h.Context(), c, "SELECT UNIX_MICROS(CURRENT_TIMESTAMP())")
	if len(got) != 1 {
		t.Fatalf("CURRENT_TIMESTAMP(): %v", got)
	}
	return fmt.Sprintf("TIMESTAMP_MICROS(%d)", got[0][0].(int64))
}

// TestBigQueryChangeHistoryRebuilds (#1466): a table's change history
// goes on across ALTER TABLE ADD COLUMN, RENAME COLUMN, DROP COLUMN, ALTER
// COLUMN SET DATA TYPE and RENAME TO, which CloudBurrow carries out by
// making the table again: FOR SYSTEM_TIME AS OF a time before them reads
// the rows as they were, in the current schema; CHANGES and APPENDS read
// the changes made before them, and none for the rebuilds themselves.
//
// covers: bigquery.jobs.query
func TestBigQueryChangeHistoryRebuilds(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".rebuilt`"
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func(sql string) string {
		t.Helper()
		return strings.Join(changeRows(t, rows(t, ctx, c, sql)), "; ")
	}
	run("CREATE TABLE " + ref + " (product STRING, inventory INT64, note STRING) OPTIONS(enable_change_history = TRUE)")
	run("INSERT INTO " + ref + " VALUES ('apples', 10, 'x'), ('pears', 20, 'y')")
	t1 := currentMicros(t, h, c)
	run("UPDATE " + ref + " SET inventory = 11 WHERE product = 'apples'")
	run("ALTER TABLE " + ref + " ADD COLUMN color STRING")
	run("ALTER TABLE " + ref + " RENAME COLUMN product TO name")
	run("ALTER TABLE " + ref + " DROP COLUMN note")
	run("ALTER TABLE " + ref + " ALTER COLUMN inventory SET DATA TYPE NUMERIC")
	run("INSERT INTO " + ref + " VALUES ('kiwis', 5, 'green')")
	t2 := currentMicros(t, h, c)
	run("DELETE " + ref + " WHERE name = 'pears'")

	if got := read("SELECT name, CAST(inventory AS INT64), color FROM " + ref + " FOR SYSTEM_TIME AS OF " + t1 +
		" ORDER BY name"); got != "apples 10 <nil>; pears 20 <nil>" {
		t.Errorf("as of before the rebuilds: %q", got)
	}
	if got := read("SELECT name, CAST(inventory AS INT64), color FROM " + ref + " FOR SYSTEM_TIME AS OF " + t2 +
		" ORDER BY name"); got != "apples 11 <nil>; kiwis 5 green; pears 20 <nil>" {
		t.Errorf("as of after the rebuilds: %q", got)
	}
	want := "apples 10 INSERT false; pears 20 INSERT false; apples 11 UPDATE false; apples 10 DELETE true; kiwis 5 INSERT false; pears 20 DELETE false"
	if got := read("SELECT name, CAST(inventory AS INT64), _CHANGE_TYPE, _CHANGE_IS_FOR_UPDATE FROM CHANGES(TABLE " + ref +
		", NULL, NULL) ORDER BY _CHANGE_TIMESTAMP, _CHANGE_TYPE DESC, name"); got != want {
		t.Errorf("CHANGES across the rebuilds:\n got %s\nwant %s", got, want)
	}

	run("ALTER TABLE " + ref + " RENAME TO renamed")
	moved := "`" + ds.DatasetID + ".renamed`"
	if got := read("SELECT name, CAST(inventory AS INT64), _CHANGE_TYPE FROM APPENDS(TABLE " + moved +
		", NULL, NULL) ORDER BY _CHANGE_TIMESTAMP, name"); got != "apples 10 INSERT; pears 20 INSERT; kiwis 5 INSERT" {
		t.Errorf("APPENDS after RENAME TO: %q", got)
	}
	if got := read("SELECT name, CAST(inventory AS INT64) FROM " + moved + " FOR SYSTEM_TIME AS OF " + t1 +
		" ORDER BY name"); got != "apples 10; pears 20" {
		t.Errorf("as of before the rebuilds, after RENAME TO: %q", got)
	}
	if got := read("SELECT name, CAST(inventory AS INT64) FROM " + moved + " ORDER BY name"); got != "apples 11; kiwis 5" {
		t.Errorf("the table now: %q", got)
	}
}

// TestBigQueryChangeHistoryReplace (#1466): CREATE OR REPLACE TABLE keeps
// the replaced contents for time travel ("the data that is replaced or
// removed can be recovered through the time travel window",
// https://cloud.google.com/bigquery/docs/time-travel), and the history of
// the table goes on: APPENDS reads the rows inserted before the replace,
// and the replace records the rows it replaced as deleted, as a
// WRITE_TRUNCATE does.
//
// covers: bigquery.jobs.query
func TestBigQueryChangeHistoryReplace(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".replaced`"
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func(sql string) string {
		t.Helper()
		return strings.Join(changeRows(t, rows(t, ctx, c, sql)), "; ")
	}
	run("CREATE TABLE " + ref + " (product STRING, inventory INT64)")
	run("INSERT INTO " + ref + " VALUES ('apples', 10), ('pears', 20)")
	t1 := currentMicros(t, h, c)
	run("CREATE OR REPLACE TABLE " + ref + " OPTIONS(enable_change_history = TRUE) AS SELECT 'kiwis' AS product, 5 AS inventory")
	if got := read("SELECT product, inventory FROM " + ref + " FOR SYSTEM_TIME AS OF " + t1 +
		" ORDER BY product"); got != "apples 10; pears 20" {
		t.Errorf("as of before the replace: %q", got)
	}
	if got := read("SELECT product, inventory FROM " + ref + " ORDER BY product"); got != "kiwis 5" {
		t.Errorf("the table now: %q", got)
	}
	got := changeRows(t, rows(t, ctx, c, "SELECT product, inventory, _CHANGE_TYPE FROM CHANGES(TABLE "+ref+", NULL, NULL)"))
	sort.Strings(got)
	if want := "apples 10 DELETE; apples 10 INSERT; kiwis 5 INSERT; pears 20 DELETE; pears 20 INSERT"; strings.Join(got, "; ") != want {
		t.Errorf("CHANGES across the replace:\n got %v\nwant %s", got, want)
	}
}

// TestBigQueryChangeHistoryLimits (#1466): the documented refusals the
// earlier tests leave out. APPENDS and CHANGES "can't" be called in a
// multi-statement transaction; CHANGES "can't" read a table with change
// data capture (a primary key and max_staleness); neither reads a
// materialized view or a snapshot; CHANGES's range is at most one day,
// as given; and the engine's clock follows the front's (#1370's
// clock:advance). 400 each; the wording is CloudBurrow's, UNVERIFIED.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryChangeHistoryLimits(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".limits`"
	path := func(n string) string { return "`" + ds.DatasetID + "." + n + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + ref + " (product STRING, inventory INT64) OPTIONS(enable_change_history = TRUE)",
		"INSERT INTO " + ref + " VALUES ('apples', 10)",
		"CREATE TABLE " + path("cdc") + " (id INT64) OPTIONS(max_staleness = INTERVAL 15 MINUTE, enable_change_history = TRUE)",
		"ALTER TABLE " + path("cdc") + " ADD PRIMARY KEY (id) NOT ENFORCED",
		"CREATE MATERIALIZED VIEW " + path("mv") + " AS SELECT product, SUM(inventory) AS total FROM " + ref + " GROUP BY product",
		"CREATE SNAPSHOT TABLE " + path("snap") + " CLONE " + ref,
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	refused := func(sql, want string) {
		t.Helper()
		for _, insert := range []bool{false, true} {
			err := bqRun(ctx, c, sql, insert)
			var e *googleapi.Error
			switch {
			case err == nil:
				t.Errorf("insert %v: %s: no error", insert, sql)
			case !strings.Contains(err.Error(), want):
				t.Errorf("insert %v: %s: %v, want %q", insert, sql, err, want)
			case !insert && (!errors.As(err, &e) || e.Code != http.StatusBadRequest):
				t.Errorf("%s: %v, want 400", sql, err)
			}
		}
	}
	refused("BEGIN TRANSACTION; SELECT * FROM APPENDS(TABLE "+ref+", NULL, NULL); COMMIT TRANSACTION;", "multi-statement transaction")
	refused("BEGIN TRANSACTION; SELECT * FROM CHANGES(TABLE "+ref+", NULL, NULL); COMMIT TRANSACTION;", "multi-statement transaction")
	refused("SELECT * FROM CHANGES(TABLE "+path("cdc")+", NULL, NULL)", "change data capture")
	refused("SELECT * FROM APPENDS(TABLE "+path("mv")+", NULL, NULL)", "regular table")
	refused("SELECT * FROM CHANGES(TABLE "+path("mv")+", NULL, NULL)", "regular table")
	refused("SELECT * FROM APPENDS(TABLE "+path("snap")+", NULL, NULL)", "regular table")
	refused("SELECT * FROM CHANGES(TABLE "+path("snap")+", NULL, NULL)", "regular table")
	refused("SELECT * FROM CHANGES(TABLE "+ref+", TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 49 HOUR), "+
		"TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR))", "more than one day")
	// After a transaction, APPENDS reads as before.
	if got := changeRows(t, rows(t, ctx, c, "BEGIN TRANSACTION; COMMIT TRANSACTION; SELECT product, _CHANGE_TYPE FROM APPENDS(TABLE "+ref+", NULL, NULL)")); strings.Join(got, "; ") != "apples INSERT" {
		t.Errorf("APPENDS after a transaction: %v", got)
	}

	// The front's clock: the engine's CURRENT_TIMESTAMP() and change
	// times follow it (a minute here), as every time BigQuery reports
	// does (#1487).
	before := advanceBigQueryClock(t, h, 0)
	after := advanceBigQueryClock(t, h, time.Minute)
	got := rows(t, ctx, c, "SELECT UNIX_SECONDS(CURRENT_TIMESTAMP())")
	if len(got) != 1 || got[0][0].(int64) < before.Add(time.Minute).Unix() || got[0][0].(int64) > after.Add(time.Minute).Unix() {
		t.Errorf("CURRENT_TIMESTAMP() after the front's clock moved a minute: %v, want about %v", got, after.Unix())
	}
	if err := bqRun(ctx, c, "UPDATE "+ref+" SET inventory = 12 WHERE TRUE", false); err != nil {
		t.Fatal(err)
	}
	if got := changeRows(t, rows(t, ctx, c, "SELECT product, inventory, _CHANGE_TYPE FROM CHANGES(TABLE "+ref+
		", TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR), NULL) ORDER BY _CHANGE_TYPE DESC")); strings.Join(got, "; ") != "apples 12 UPDATE; apples 10 INSERT; apples 10 DELETE" {
		t.Errorf("CHANGES of the last hour by the front's clock: %v", got)
	}
}
