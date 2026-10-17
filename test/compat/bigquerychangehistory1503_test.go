//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryChangeHistoryCommittedTransaction (#1503): BigQuery
// documents that "you can't use the CHANGES function on tables that have
// had multi-statement transactions committed to them within the requested
// time window"
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/time-series-functions).
// CHANGES over a range that holds the commit of a transaction that wrote
// the table is refused 400, through jobs.query and jobs.insert; a range
// that ends before the commit, or starts after it, is read; a transaction
// rolled back is no commit; APPENDS has no such limit and reads the
// transaction's rows. Before, CHANGES over the commit read its rows (the
// front kept no record of which tables a transaction wrote).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryChangeHistoryCommittedTransaction(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	advanceBigQueryClock(t, h, 0)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".txn`"
	other := "`" + ds.DatasetID + ".untouched`"
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
	run("CREATE TABLE " + ref + " (product STRING, inventory INT64) OPTIONS(enable_change_history = TRUE)")
	run("CREATE TABLE " + other + " (product STRING) OPTIONS(enable_change_history = TRUE)")
	run("INSERT INTO " + ref + " VALUES ('apples', 10)")
	run("INSERT INTO " + other + " VALUES ('kiwis')")
	before := currentMicros(t, h, c)
	advanceBigQueryClock(t, h, time.Second)
	run("BEGIN TRANSACTION; INSERT INTO " + ref + " VALUES ('pears', 20); UPDATE " + ref +
		" SET inventory = 11 WHERE product = 'apples'; COMMIT TRANSACTION;")
	advanceBigQueryClock(t, h, time.Second)
	after := currentMicros(t, h, c)
	advanceBigQueryClock(t, h, time.Second)
	run("BEGIN TRANSACTION; INSERT INTO " + ref + " VALUES ('plums', 1); ROLLBACK TRANSACTION;")
	run("INSERT INTO " + ref + " VALUES ('figs', 3)")

	for _, insert := range []bool{false, true} {
		err := bqRun(ctx, c, "SELECT * FROM CHANGES(TABLE "+ref+", NULL, NULL)", insert)
		var e *googleapi.Error
		switch {
		case err == nil:
			t.Errorf("insert %v: CHANGES over the commit: no error", insert)
		case !strings.Contains(err.Error(), "multi-statement transaction was committed"):
			t.Errorf("insert %v: CHANGES over the commit: %v", insert, err)
		case !insert && (!errors.As(err, &e) || e.Code != http.StatusBadRequest):
			t.Errorf("CHANGES over the commit: %v, want 400", err)
		}
	}
	if got := read("SELECT product, inventory, _CHANGE_TYPE FROM CHANGES(TABLE " + ref + ", NULL, " + before + ")"); got != "apples 10 INSERT" {
		t.Errorf("CHANGES before the commit: %q", got)
	}
	if got := read("SELECT product, inventory, _CHANGE_TYPE FROM CHANGES(TABLE " + ref + ", " + after + ", NULL)"); got != "figs 3 INSERT" {
		t.Errorf("CHANGES after the commit: %q", got)
	}
	if got := read("SELECT product, _CHANGE_TYPE FROM CHANGES(TABLE " + other + ", NULL, NULL)"); got != "kiwis INSERT" {
		t.Errorf("CHANGES of a table the transaction did not write: %q", got)
	}
	if got := read("SELECT product, inventory FROM APPENDS(TABLE " + ref + ", NULL, NULL) ORDER BY _CHANGE_TIMESTAMP, product"); got != "apples 10; pears 20; figs 3" {
		t.Errorf("APPENDS: %q", got)
	}
}

// TestBigQueryChangeHistoryRetention (#1503): the history of a table is
// kept for its dataset's time travel window, max_time_travel_hours
// (https://cloud.google.com/bigquery/docs/time-travel): "The data returned
// by the CHANGES [APPENDS] function is limited to the time travel window
// of the table". In a dataset of a 48-hour window, after 49 hours (the
// instance's clock advanced) and the window then made 168 hours, APPENDS
// and CHANGES from before the 48 hours read nothing of it (a longer window
// does not bring back what the shorter let go), while the row inserted
// then and still there is read by FOR SYSTEM_TIME AS OF, and a later
// change is read. Before, the engine kept seven days of changes whatever
// the window (googlesqlite patch 0036), so the lengthened window read the
// old inserts and the deletion again.
//
// covers: bigquery.datasets.insert, bigquery.datasets.patch, bigquery.jobs.query
func TestBigQueryChangeHistoryRetention(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	advanceBigQueryClock(t, h, 0)
	ctx := h.Context()
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_retain")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{MaxTimeTravel: 48 * time.Hour}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	ref := "`" + ds.DatasetID + ".kept`"
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
	run("CREATE TABLE " + ref + " (product STRING, inventory INT64) OPTIONS(enable_change_history = TRUE)")
	start := currentMicros(t, h, c)
	run("INSERT INTO " + ref + " VALUES ('apples', 10), ('pears', 20)")
	run("UPDATE " + ref + " SET inventory = 11 WHERE product = 'apples'")
	run("DELETE " + ref + " WHERE product = 'pears'")
	if got := read("SELECT product, inventory, _CHANGE_TYPE FROM APPENDS(TABLE " + ref + ", NULL, NULL) ORDER BY product"); got != "apples 10 INSERT; pears 20 INSERT" {
		t.Fatalf("APPENDS within the window: %q", got)
	}

	advanceBigQueryClock(t, h, 49*time.Hour)
	mid := currentMicros(t, h, c)
	run("INSERT INTO " + ref + " VALUES ('figs', 3)")
	if _, err := ds.Update(ctx, bigquery.DatasetMetadataToUpdate{MaxTimeTravel: 168 * time.Hour}, ""); err != nil {
		t.Fatalf("update the window: %v", err)
	}
	if got := read("SELECT product, inventory, _CHANGE_TYPE FROM APPENDS(TABLE " + ref + ", " + start + ", NULL) ORDER BY product"); got != "figs 3 INSERT" {
		t.Errorf("APPENDS from before the 48-hour window, after it was made 168 hours: %q", got)
	}
	if got := read("SELECT product, inventory, _CHANGE_TYPE FROM CHANGES(TABLE " + ref + ", " + start +
		", TIMESTAMP_ADD(" + start + ", INTERVAL 1 DAY))"); got != "" {
		t.Errorf("CHANGES from before the 48-hour window: %q", got)
	}
	if got := read("SELECT product, inventory FROM " + ref + " FOR SYSTEM_TIME AS OF " + mid + " ORDER BY product"); got != "apples 11" {
		t.Errorf("FOR SYSTEM_TIME AS OF before the new insert: %q", got)
	}
	if got := read("SELECT product, inventory FROM " + ref + " ORDER BY product"); got != "apples 11; figs 3" {
		t.Errorf("the table: %q", got)
	}
}
