//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"
)

// TestBigQueryStreamTimeOfEveryTable (#1458): a row streamed by
// tabledata.insertAll a minute ago, past its streaming buffer, is held
// from DML ("within the last 30 minutes", data-manipulation-language) in
// a table made by CREATE TABLE IF NOT EXISTS, CREATE OR REPLACE TABLE
// and CREATE TABLE ... AS SELECT, with or without a column list, and the
// table's other rows are not.
//
// covers: bigquery.jobs.query, bigquery.tabledata.insertAll
func TestBigQueryStreamTimeOfEveryTable(t *testing.T) {
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
	type row struct {
		N int64 `bigquery:"n"`
	}
	made := map[string]string{
		"ine":      "CREATE TABLE IF NOT EXISTS `" + d + ".ine` (n INT64)",
		"ine_ctas": "CREATE TABLE IF NOT EXISTS `" + d + ".ine_ctas` AS SELECT 1 AS n",
		"repl":     "CREATE OR REPLACE TABLE `" + d + ".repl` (n INT64)",
		"repl_as":  "CREATE OR REPLACE TABLE `" + d + ".repl_as` AS SELECT 1 AS n",
		"part":     "CREATE TABLE IF NOT EXISTS `" + d + ".part` (n INT64, dt DATE) PARTITION BY dt",
	}
	for name, sql := range made {
		exec(sql)
		if !strings.Contains(sql, " AS SELECT") {
			exec("INSERT INTO `" + d + "." + name + "` (n) VALUES (1)")
		}
		if err := ds.Table(name).Inserter().Put(ctx, []*row{{N: 2}}); err != nil {
			t.Fatalf("insertAll into %s: %v", name, err)
		}
	}
	advanceBigQueryClock(t, h, time.Minute)
	for name := range made {
		if err := bqRun(ctx, c, "DELETE FROM `"+d+"."+name+"` WHERE n = 2", false); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
			t.Errorf("%s: DELETE of a row streamed a minute ago: %v, want the streaming buffer's error", name, err)
		}
		if err := bqRun(ctx, c, "DELETE FROM `"+d+"."+name+"` WHERE n = 1", false); err != nil {
			t.Errorf("%s: DELETE of a row not streamed: %v", name, err)
		}
	}
}
