//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// The tests in this file are #901: the DDL and loads the front (#881) did
// not check yet, each driven through the official Go client. Measured
// against the pinned emulator first: ALTER TABLE reported ADD COLUMN,
// RENAME TO, DROP COLUMN and SET OPTIONS done and changed nothing; CREATE
// TABLE ... AS SELECT 1 AS `x!` made a column "x!"; the statements inside
// IF, LOOP, WHILE, REPEAT, FOR and CASE blocks were not run, with success;
// and a CSV load with autodetect made a column "a b!" from its header.

// bqRun runs sql through jobs.query (Query.Read) or jobs.insert (Query.Run
// and Job.Wait), with a deadline of its own, so a 500 the client would
// retry shows as a timeout rather than hanging the test.
func bqRun(ctx context.Context, c *bigquery.Client, sql string, insert bool) error {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q := c.Query(sql)
	if !insert {
		_, err := q.Read(qctx)
		return err
	}
	job, err := q.Run(qctx)
	if err != nil {
		return err
	}
	st, err := job.Wait(qctx)
	if err != nil {
		return err
	}
	return st.Err()
}

func tableExists(t *testing.T, ctx context.Context, tbl *bigquery.Table) bool {
	t.Helper()
	_, err := tbl.Metadata(ctx)
	var e *googleapi.Error
	if errors.As(err, &e) && e.Code == http.StatusNotFound {
		return false
	}
	if err != nil {
		t.Fatalf("Metadata of %s: %v", tbl.TableID, err)
	}
	return true
}

// TestBigQueryAlterTableNamesAndNotImplemented (#901): ALTER TABLE's new
// names are held to the rules, 400 invalidQuery: an ADD COLUMN named a!,
// a STRUCT field named q?, a RENAME TO t!, a RENAME COLUMN to c!. An ALTER
// TABLE BigQuery would run is 501, through jobs.query and jobs.insert
// alike, since the emulator would report it done and leave the table as it
// was; the table's schema is unchanged and it is not renamed.
func TestBigQueryAlterTableNamesAndNotImplemented(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := ds.Table("altered")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".altered`"
	for _, insert := range []bool{false, true} {
		for sql, code := range map[string]int{
			"ALTER TABLE " + ref + " ADD COLUMN `b!` STRING":                  http.StatusBadRequest,
			"ALTER TABLE " + ref + " ADD COLUMN s STRUCT<`q?` INT64>":         http.StatusBadRequest,
			"ALTER TABLE " + ref + " RENAME TO `t!`":                          http.StatusBadRequest,
			"ALTER TABLE " + ref + " RENAME COLUMN a TO `c!`":                 http.StatusBadRequest,
			"ALTER TABLE " + ref + " ADD COLUMN `first name` STRING":          http.StatusNotImplemented,
			"ALTER TABLE " + ref + " RENAME TO renamed":                       http.StatusNotImplemented,
			"ALTER TABLE " + ref + " SET OPTIONS (description = 'described')": http.StatusNotImplemented,
		} {
			err := bqRun(ctx, c, sql, insert)
			wantHTTPStatus(t, sql, err, code)
			var e *googleapi.Error
			if errors.As(err, &e) && code == http.StatusNotImplemented && (len(e.Errors) != 1 || e.Errors[0].Reason != "notImplemented") {
				t.Errorf("%s: %+v, want reason notImplemented", sql, e.Errors)
			}
		}
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != 1 || md.Schema[0].Name != "a" || md.Description != "" {
		t.Errorf("the table changed: %d columns, description %q", len(md.Schema), md.Description)
	}
	for _, name := range []string{"t!", "renamed"} {
		if tableExists(t, ctx, ds.Table(name)) {
			t.Errorf("a table %s was made", name)
		}
	}
}

// TestBigQueryCreateTableAsSelectColumnsAreChecked (#901): the columns a
// CREATE TABLE ... AS SELECT's query gives are held to the column rules,
// top level and nested STRUCT fields alike, and through a WITH; a refused
// statement makes nothing. The front reads the columns by running the
// query alone with no rows. A table whose columns BigQuery takes is made
// with them.
func TestBigQueryCreateTableAsSelectColumnsAreChecked(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for table, sel := range map[string]string{
		"top":    "SELECT 1 AS `x!`",
		"nested": "SELECT 1 AS ok, STRUCT(2 AS `y?`) AS s",
		"with":   "WITH w AS (SELECT 3 AS `z/`) SELECT * FROM w",
	} {
		for _, insert := range []bool{false, true} {
			err := bqRun(ctx, c, "CREATE TABLE "+path(table)+" AS "+sel, insert)
			wantHTTPStatus(t, "CREATE TABLE "+table+" AS "+sel, err, http.StatusBadRequest)
			var e *googleapi.Error
			if errors.As(err, &e) && (len(e.Errors) != 1 || e.Errors[0].Reason != "invalidQuery") {
				t.Errorf("%s: %+v, want reason invalidQuery", table, e.Errors)
			}
		}
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("the refused CREATE TABLE ... AS SELECT made %s", table)
		}
	}
	if err := bqRun(ctx, c, "CREATE TABLE "+path("made")+" AS SELECT 1 AS `first name`, STRUCT(2 AS `n-1`) AS s", false); err != nil {
		t.Fatalf("CREATE TABLE ... AS SELECT with names BigQuery takes: %v", err)
	}
	md, err := ds.Table("made").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != 2 || md.Schema[0].Name != "first name" || len(md.Schema[1].Schema) != 1 || md.Schema[1].Schema[0].Name != "n-1" {
		t.Errorf("the table's columns: %+v", md.Schema)
	}
}

// TestBigQueryScriptBlocks (#901): the DDL inside a script's IF, LOOP and
// BEGIN ... EXCEPTION blocks is held to the rules, 400, and nothing is
// made. A control-flow block BigQuery would run is 501, since the emulator
// skips the statements inside it and reports success (measured): the
// table the IF would make is not there. A BEGIN ... END block, which the
// emulator runs, is run.
func TestBigQueryScriptBlocks(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for sql, code := range map[string]int{
		"IF TRUE THEN CREATE TABLE " + path("if!") + " (a INT64); END IF":                                http.StatusBadRequest,
		"LOOP CREATE TABLE " + path("loop!") + " (a INT64); LEAVE; END LOOP":                             http.StatusBadRequest,
		"BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN CREATE TABLE " + path("handler!") + " (a INT64); END": http.StatusBadRequest,
		"IF TRUE THEN CREATE TABLE " + path("ifok") + " (a INT64); END IF":                               http.StatusNotImplemented,
		"WHILE FALSE DO SELECT 1; END WHILE":                                                             http.StatusNotImplemented,
	} {
		wantHTTPStatus(t, sql, bqRun(ctx, c, sql, false), code)
	}
	for _, table := range []string{"if!", "loop!", "handler!", "ifok"} {
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("the refused script made %s", table)
		}
	}
	if err := bqRun(ctx, c, "BEGIN CREATE TABLE "+path("block")+" (a INT64); END", false); err != nil {
		t.Fatalf("a BEGIN ... END block: %v", err)
	}
	if !tableExists(t, ctx, ds.Table("block")) {
		t.Error("the BEGIN ... END block did not make its table")
	}
}

// TestBigQueryAutodetectLoadNames (#901): a CSV load with autodetect and
// no schema, into a new table, whose header gives a column a name BigQuery
// refuses fails as BigQuery fails it under columnNameCharacterMap STRICT,
// the default: the job is done with an "invalid" errorResult, which
// Job.Wait's status and a later Job.Status report, and there is no table.
// Under V2, which would rename the column, it is 501. A header BigQuery
// takes loads its row. A NEWLINE_DELIMITED_JSON load with autodetect into a
// new table, which the emulator fails with a 500, is 501.
func TestBigQueryAutodetectLoadNames(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table, data string, format bigquery.DataFormat, charMap bigquery.ColumnNameCharacterMap) (*bigquery.Job, error) {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.SourceFormat = format
		src.AutoDetect = true
		l := ds.Table(table).LoaderFrom(src)
		l.ColumnNameCharacterMap = charMap
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return l.Run(lctx)
	}

	job, err := load("bad_header", "a b!,c\n1,x\n", bigquery.CSV, "")
	if err != nil {
		t.Fatalf("the load was refused before it ran: %v", err)
	}
	st, err := job.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	var be *bigquery.Error
	if !errors.As(st.Err(), &be) || be.Reason != "invalid" || !strings.Contains(be.Message, `"a b!"`) {
		t.Errorf("the job's status error is %v, want reason invalid naming a b!", st.Err())
	}
	again, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if st, err := again.Status(ctx); err != nil || !errors.As(st.Err(), &be) || be.Reason != "invalid" {
		t.Errorf("jobs.get reports %v %v, want the job failed", st, err)
	}
	if tableExists(t, ctx, ds.Table("bad_header")) {
		t.Error("the failed load left its table")
	}

	_, err = load("v2_header", "a!,c\n1,x\n", bigquery.CSV, bigquery.V2ColumnNameCharacterMap)
	wantHTTPStatus(t, "an autodetect load under V2 with a header BigQuery would rename", err, http.StatusNotImplemented)
	if tableExists(t, ctx, ds.Table("v2_header")) {
		t.Error("the refused V2 load left its table")
	}

	_, err = load("json_new", `{"a":1}`+"\n", bigquery.JSON, "")
	wantHTTPStatus(t, "an autodetect JSON load into a new table", err, http.StatusNotImplemented)

	job, err = load("good_header", "first name,n\nbob,3\n", bigquery.CSV, "")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("a load whose header BigQuery takes: %v %v", err, st.Err())
	}
	md, err := ds.Table("good_header").Metadata(ctx)
	if err != nil || len(md.Schema) != 2 || md.Schema[0].Name != "first name" {
		t.Fatalf("the loaded table's schema: %+v %v", md, err)
	}
	if n := countRows(t, h, ds.Table("good_header")); n != 1 {
		t.Errorf("the load wrote %d rows, want 1", n)
	}
}
