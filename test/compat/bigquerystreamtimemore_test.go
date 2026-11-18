//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryStreamTimeOfMadeTables (#1458): a row streamed by
// tabledata.insertAll a minute ago, past its streaming buffer, is held
// from DML ("within the last 30 minutes", data-manipulation-language) in
// a table made by a copy job, a load job, a query job's new destination
// table and CREATE TABLE ... CLONE, LIKE and COPY, and the table's other
// rows are not.
//
// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.tabledata.insertAll
func TestBigQueryStreamTimeOfMadeTables(t *testing.T) {
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
	wait := func(what string, job *bigquery.Job, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: jobs.insert: %v", what, err)
		}
		wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if st, err := job.Wait(wctx); err != nil || st.Err() != nil {
			t.Fatalf("%s: %v %v", what, err, st)
		}
	}
	exec("CREATE TABLE `" + d + ".src` (n INT64)")
	exec("INSERT INTO `" + d + ".src` (n) VALUES (1)")
	var names []string

	cp := ds.Table("copied").CopierFrom(ds.Table("src"))
	job, err := cp.Run(ctx)
	wait("copy", job, err)
	names = append(names, "copied")

	src := bigquery.NewReaderSource(strings.NewReader("1\n"))
	src.Schema = bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}
	job, err = ds.Table("loaded").LoaderFrom(src).Run(ctx)
	wait("load", job, err)
	names = append(names, "loaded")

	q := c.Query("SELECT 1 AS n")
	q.Dst = ds.Table("queried")
	job, err = q.Run(ctx)
	wait("query destination", job, err)
	names = append(names, "queried")

	for name, sql := range map[string]string{
		"cloned": "CREATE TABLE `" + d + ".cloned` CLONE `" + d + ".src`",
		"liked":  "CREATE TABLE `" + d + ".liked` LIKE `" + d + ".src`",
		"copy":   "CREATE TABLE `" + d + ".copy` COPY `" + d + ".src`",
	} {
		exec(sql)
		if name == "liked" {
			exec("INSERT INTO `" + d + ".liked` (n) VALUES (1)")
		}
		names = append(names, name)
	}

	type row struct {
		N int64 `bigquery:"n"`
	}
	for _, name := range names {
		if err := ds.Table(name).Inserter().Put(ctx, []*row{{N: 2}}); err != nil {
			t.Fatalf("insertAll into %s: %v", name, err)
		}
	}
	advanceBigQueryClock(t, h, time.Minute)
	for _, name := range names {
		if err := bqRun(ctx, c, "DELETE FROM `"+d+"."+name+"` WHERE n = 2", false); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
			t.Errorf("%s: DELETE of a row streamed a minute ago: %v, want the streaming buffer's error", name, err)
		}
		if err := bqRun(ctx, c, "DELETE FROM `"+d+"."+name+"` WHERE n = 1", false); err != nil {
			t.Errorf("%s: DELETE of a row not streamed: %v", name, err)
		}
	}
}
