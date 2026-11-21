//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"google.golang.org/api/iterator"
)

// TestBigQueryChangeDataCaptureWatermark (#2104): a table receiving change
// data capture reports upsert_stream_apply_watermark in
// INFORMATION_SCHEMA.TABLES, "the time at which the last apply job
// occurred" (https://docs.cloud.google.com/bigquery/docs/change-data-capture,
// read 2026-11-21), queried as Google's page does. CloudBurrow applies an
// append's changes before it answers it, so the watermark is set by the
// append and moves with each later one; a table with no change applied
// reads NULL. The rows read are the changes applied, whatever
// max_staleness, as no change is left pending.
//
// covers: bigquery.jobs.query
func TestBigQueryChangeDataCaptureWatermark(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	tbl := cdcTable(t, h, ds, "wm", true)
	cdcTable(t, h, ds, "plain", true)
	m, dp := cdcSchema(t, h, tbl)
	mw := managedWriter(t, h, project)

	watermark := func(table string) (time.Time, bool) {
		t.Helper()
		q := c.Query("SELECT upsert_stream_apply_watermark FROM `" + project + "." + ds.DatasetID +
			".INFORMATION_SCHEMA.TABLES` WHERE table_name = '" + table + "'")
		it, err := q.Read(ctx)
		if err != nil {
			t.Fatalf("watermark of %s: %v", table, err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			t.Fatalf("watermark of %s: %v", table, err)
		}
		if err := it.Next(&row); err != iterator.Done {
			t.Fatalf("watermark of %s: more than one row (%v)", table, err)
		}
		if row[0] == nil {
			return time.Time{}, false
		}
		ts, ok := row[0].(time.Time)
		if !ok {
			t.Fatalf("watermark of %s: a %T", table, row[0])
		}
		return ts, true
	}

	if _, ok := watermark("wm"); ok {
		t.Fatalf("a table with no change applied has a watermark")
	}
	before := time.Now().Add(-time.Minute)
	if _, err := cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m,
		cdcChange{id: 1, name: "a", typ: "UPSERT", seq: "1"},
		cdcChange{id: 2, name: "b", typ: "UPSERT", seq: "1"})); err != nil {
		t.Fatalf("first append: %v", err)
	}
	first, ok := watermark("wm")
	if !ok || first.Before(before) {
		t.Fatalf("after the first append: watermark %v (set %v), want one after %v", first, ok, before)
	}
	if _, ok := watermark("plain"); ok {
		t.Errorf("a table that received no change has a watermark")
	}
	full := "`" + project + "." + ds.DatasetID + ".wm`"
	if got := cdcTableRows(t, h, c, full); len(got) != 2 {
		t.Fatalf("rows before the watermark are applied: %v", got)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m,
		cdcChange{id: 1, typ: "DELETE", seq: "2"})); err != nil {
		t.Fatalf("second append: %v", err)
	}
	second, ok := watermark("wm")
	if !ok || !second.After(first) {
		t.Fatalf("after the second append: watermark %v, want after %v", second, first)
	}
	// "A CDC DELETE operation is considered to be applied only when the
	// upsert_stream_apply_watermark timestamp has passed": it has.
	if got := cdcTableRows(t, h, c, full); len(got) != 1 || got[2] != "b" {
		t.Fatalf("after the DELETE: %v", got)
	}
}
