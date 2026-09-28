//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// bqValue runs sql through jobs.query and returns its first row, or the
// error.
func bqValue(ctx context.Context, c *bigquery.Client, sql string) string {
	it, err := c.Query(sql).Read(ctx)
	if err != nil {
		return "error: " + err.Error()
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprint(row)
}

// TestBigQueryCreateFunctionOfAnExistingFunction (#986), through the
// official Go client, jobs.query and a query job alike. Measured first:
// CREATE FUNCTION of a function that existed succeeded and kept the old
// body, and CREATE OR REPLACE FUNCTION did not replace it. Now CREATE
// FUNCTION of it fails "Already Exists" (409 duplicate; a failed job),
// CREATE OR REPLACE replaces it, one whose body does not analyse fails
// and keeps the old one, IF NOT EXISTS does nothing, and in a script after
// other statements it is 501 before anything runs. The same for a table
// function (#1047, #1061): 501 against the pinned v0.8.1, whose engine freed
// table functions (#1043) and could not drop one.
func TestBigQueryCreateFunctionOfAnExistingFunction(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	if err := bqRun(ctx, c, "CREATE FUNCTION "+d+".h(x INT64) AS (x + 1)", false); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	err := bqRun(ctx, c, "CREATE FUNCTION "+d+".h(x INT64) AS (x + 2)", false)
	wantReason(t, "CREATE FUNCTION of an existing function", err, http.StatusConflict, "duplicate")
	if _, jerr := jobError(t, ctx, c, "CREATE FUNCTION "+d+".h(x INT64) AS (x + 2)"); jerr == nil || jerr.Reason != "duplicate" {
		t.Errorf("CREATE FUNCTION of an existing function as a query job: %v", jerr)
	}
	if got := bqValue(ctx, c, "SELECT "+d+".h(1)"); got != "[2]" {
		t.Errorf("after the refused CREATE FUNCTION, h(1) = %s, want [2]", got)
	}
	if err := bqRun(ctx, c, "CREATE FUNCTION IF NOT EXISTS "+d+".h(x INT64) AS (x + 5)", false); err != nil {
		t.Errorf("IF NOT EXISTS: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT "+d+".h(1)"); got != "[2]" {
		t.Errorf("after IF NOT EXISTS, h(1) = %s, want [2]", got)
	}
	for i, insert := range []bool{false, true} {
		if err := bqRun(ctx, c, fmt.Sprintf("CREATE OR REPLACE FUNCTION %s.h(x INT64) AS (x + %d)", d, 10+i), insert); err != nil {
			t.Fatalf("CREATE OR REPLACE (query job %v): %v", insert, err)
		}
		if got, want := bqValue(ctx, c, "SELECT "+d+".h(1)"), fmt.Sprintf("[%d]", 11+i); got != want {
			t.Errorf("after CREATE OR REPLACE (query job %v), h(1) = %s, want %s", insert, got, want)
		}
	}
	if err := bqRun(ctx, c, "CREATE OR REPLACE FUNCTION "+d+".h(x INT64) AS (nope + 1)", false); err == nil {
		t.Errorf("CREATE OR REPLACE with a body that does not analyse succeeded")
	}
	if got := bqValue(ctx, c, "SELECT "+d+".h(1)"); got != "[12]" {
		t.Errorf("after the failed CREATE OR REPLACE, h(1) = %s, want [12]", got)
	}
	for _, sql := range []string{
		"SELECT 1; CREATE FUNCTION " + d + ".h(x INT64) AS (x + 3)",
		"CREATE OR REPLACE FUNCTION " + d + ".h(x INT64) AS (x + 3); SELECT 1",
	} {
		for _, insert := range []bool{false, true} {
			err := bqRun(ctx, c, sql, insert)
			wantReason(t, fmt.Sprintf("%s (query job %v)", sql, insert), err, http.StatusNotImplemented, "notImplemented")
		}
	}
	if got := bqValue(ctx, c, "SELECT "+d+".h(1)"); got != "[12]" {
		t.Errorf("after the 501s, h(1) = %s, want [12]", got)
	}

	// A table function (#1047): made, called, CREATE of it again 409, IF
	// NOT EXISTS nothing, CREATE OR REPLACE replaces it (and one whose body
	// does not analyse keeps it), through jobs.query and a query job.
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x AS y)", false); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+d+".tf(1)"); got != "[1]" {
		t.Errorf("tf(1) = %s, want [1]", got)
	}
	err = bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x + 2 AS y)", false)
	wantReason(t, "CREATE TABLE FUNCTION of an existing one", err, http.StatusConflict, "duplicate")
	if _, jerr := jobError(t, ctx, c, "CREATE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x + 2 AS y)"); jerr == nil || jerr.Reason != "duplicate" {
		t.Errorf("CREATE TABLE FUNCTION of an existing one as a query job: %v", jerr)
	}
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION IF NOT EXISTS "+d+".tf(x INT64) AS (SELECT x + 5 AS y)", false); err != nil {
		t.Errorf("IF NOT EXISTS: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+d+".tf(1)"); got != "[1]" {
		t.Errorf("after the refused CREATE and IF NOT EXISTS, tf(1) = %s, want [1]", got)
	}
	for i, insert := range []bool{false, true} {
		sql := fmt.Sprintf("CREATE OR REPLACE TABLE FUNCTION %s.tf(x INT64) AS (SELECT x + %d AS y)", d, 10+i)
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("CREATE OR REPLACE TABLE FUNCTION (query job %v): %v", insert, err)
		}
		if got, want := bqValue(ctx, c, "SELECT * FROM "+d+".tf(1)"), fmt.Sprintf("[%d]", 11+i); got != want {
			t.Errorf("after CREATE OR REPLACE TABLE FUNCTION (query job %v), tf(1) = %s, want %s", insert, got, want)
		}
	}
	if err := bqRun(ctx, c, "CREATE OR REPLACE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT nope AS y)", false); err == nil {
		t.Errorf("CREATE OR REPLACE TABLE FUNCTION with a body that does not analyse succeeded")
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+d+".tf(1)"); got != "[12]" {
		t.Errorf("after the failed CREATE OR REPLACE TABLE FUNCTION, tf(1) = %s, want [12]", got)
	}
	for _, insert := range []bool{false, true} {
		err := bqRun(ctx, c, "CREATE OR REPLACE TABLE FUNCTION "+d+".tf(x INT64) AS (SELECT x AS y); SELECT 1", insert)
		wantReason(t, fmt.Sprintf("CREATE OR REPLACE TABLE FUNCTION in a script (query job %v)", insert), err, http.StatusNotImplemented, "notImplemented")
	}
	if got := bqValue(ctx, c, "SELECT * FROM "+d+".tf(1)"); got != "[12]" {
		t.Errorf("after the 501s, tf(1) = %s, want [12]", got)
	}
}

// TestBigQueryDropSchema (#990), through the official Go client, jobs.query
// and a query job alike. Measured first: the emulator refused every DROP
// SCHEMA (400 "currently unsupported DROP SCHEMA statement"). Now it drops
// the dataset: RESTRICT (the default) of an empty one, CASCADE of one with
// a table and a function (which is then gone), in a script whose other
// statements do not name it; RESTRICT of one with a table fails
// resourceInUse and keeps it; one that does not exist fails notFound, or
// with IF EXISTS does nothing; a script whose other statements name it is
// 501 and nothing is run.
func TestBigQueryDropSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	exists := func(name string) bool {
		t.Helper()
		_, err := c.Dataset(name).Metadata(ctx)
		var e *googleapi.Error
		if errors.As(err, &e) && e.Code == http.StatusNotFound {
			return false
		}
		if err != nil {
			t.Fatalf("datasets.get %s: %v", name, err)
		}
		return true
	}
	mk := func(name string, withTable bool) {
		t.Helper()
		sql := "CREATE SCHEMA " + name
		if withTable {
			sql += "; CREATE TABLE " + name + ".t (a INT64); INSERT INTO " + name + ".t VALUES (1)"
		}
		if err := bqRun(ctx, c, sql, true); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		t.Cleanup(func() { _ = c.Dataset(name).DeleteWithContents(context.Background()) })
	}
	for _, insert := range []bool{false, true} {
		n := map[bool]string{false: "_q", true: "_j"}[insert]
		empty, full, script := d+n+"_empty", d+n+"_full", d+n+"_script"
		mk(empty, false)
		mk(full, true)
		mk(script, true)

		err := bqRun(ctx, c, "DROP SCHEMA "+full, insert)
		if insert {
			if got := errReason(err); got != "resourceInUse" {
				t.Errorf("DROP SCHEMA of a dataset with a table (query job): %v", err)
			}
		} else {
			wantReason(t, "DROP SCHEMA of a dataset with a table", err, http.StatusBadRequest, "resourceInUse")
		}
		if !exists(full) {
			t.Errorf("the refused DROP SCHEMA dropped %s", full)
		}
		if got := bqValue(ctx, c, "SELECT COUNT(*) FROM "+full+".t"); got != "[1]" {
			t.Errorf("after the refused DROP SCHEMA, %s.t has %s rows", full, got)
		}

		if err := bqRun(ctx, c, "DROP SCHEMA "+empty, insert); err != nil || exists(empty) {
			t.Errorf("DROP SCHEMA of an empty dataset (query job %v): %v, still there %v", insert, err, exists(empty))
		}
		if err := bqRun(ctx, c, "CREATE FUNCTION "+full+".f(x INT64) AS (x + 1)", insert); err != nil {
			t.Fatal(err)
		}
		if err := bqRun(ctx, c, "DROP SCHEMA IF EXISTS "+full+" CASCADE", insert); err != nil || exists(full) {
			t.Errorf("DROP SCHEMA CASCADE (query job %v): %v, still there %v", insert, err, exists(full))
		}
		if got := bqValue(ctx, c, "SELECT "+full+".f(1)"); !strings.Contains(got, "Function not found") {
			t.Errorf("after DROP SCHEMA CASCADE, %s.f(1) = %s, want not found", full, got)
		}
		// Made again, the dataset has none of its old tables.
		mk(full, false)
		if _, err := c.Dataset(full).Table("t").Metadata(ctx); err == nil {
			t.Errorf("%s.t came back with the dataset", full)
		}

		err = bqRun(ctx, c, "DROP SCHEMA "+d+n+"_nope", insert)
		if insert {
			if got := errReason(err); got != "notFound" {
				t.Errorf("DROP SCHEMA of a dataset that does not exist (query job): %v", err)
			}
		} else {
			wantReason(t, "DROP SCHEMA of a dataset that does not exist", err, http.StatusNotFound, "notFound")
		}
		if err := bqRun(ctx, c, "DROP SCHEMA IF EXISTS "+d+n+"_nope", insert); err != nil {
			t.Errorf("DROP SCHEMA IF EXISTS of a dataset that does not exist (query job %v): %v", insert, err)
		}

		// A dataset with only a function the front saw made is not empty.
		fn := d + n + "_fn"
		mk(fn, false)
		if err := bqRun(ctx, c, "CREATE FUNCTION "+fn+".g(x INT64) AS (x)", insert); err != nil {
			t.Fatal(err)
		}
		if got := errReason(bqRun(ctx, c, "DROP SCHEMA "+fn, insert)); got != "resourceInUse" || !exists(fn) {
			t.Errorf("DROP SCHEMA of a dataset with a function (query job %v): %s, still there %v", insert, got, exists(fn))
		}
		// A script that fails keeps the dataset.
		err = bqRun(ctx, c, "DROP SCHEMA "+fn+" CASCADE; SELECT * FROM nope.nope", insert)
		if got := errReason(err); got != "notImplemented" || !exists(fn) {
			t.Errorf("a failed script with DROP SCHEMA (query job %v): %v, still there %v", insert, err, exists(fn))
		}

		err = bqRun(ctx, c, "DROP SCHEMA "+script+" CASCADE; SELECT COUNT(*) FROM "+script+".t", insert)
		wantReason(t, fmt.Sprintf("a script that names the dataset (query job %v)", insert), err, http.StatusNotImplemented, "notImplemented")
		if !exists(script) {
			t.Errorf("the 501 dropped %s", script)
		}
		if err := bqRun(ctx, c, "SELECT 1; DROP SCHEMA "+script+" CASCADE; SELECT 2", insert); err != nil || exists(script) {
			t.Errorf("DROP SCHEMA CASCADE in a script (query job %v): %v, still there %v", insert, err, exists(script))
		}
	}
}

// TestBigQueryCopyJobs (#987), through the official Go client
// (Table.CopierFrom). Measured first: a copy job was answered 400
// "unspecified job configuration query". Now the front copies: a new
// table from two sources, with the source's schema (a REQUIRED column, a
// RECORD with a REPEATED field, FLOAT64, TIMESTAMP, NUMERIC) and every
// value read back; WRITE_EMPTY into a table with rows fails
// "duplicate"; WRITE_APPEND adds; WRITE_TRUNCATE replaces; CREATE_NEVER
// of a missing table fails "notFound", and so does a missing source. The
// job is the front's: jobs.get and jobs.list read it back.
func TestBigQueryCopyJobs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "name", Type: bigquery.StringFieldType, Description: "the name"},
		{Name: "rec", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "x", Type: bigquery.IntegerFieldType},
			{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		}},
		{Name: "f", Type: bigquery.FloatFieldType},
		{Name: "ts", Type: bigquery.TimestampFieldType},
		{Name: "n", Type: bigquery.NumericFieldType},
	}
	for _, name := range []string{"a", "b", "full"} {
		if err := ds.Table(name).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	d := ds.DatasetID
	for _, sql := range []string{
		"INSERT INTO " + d + ".a VALUES (1, 'one', STRUCT(10, ['p', 'q']), 1.5, TIMESTAMP '2020-01-02 03:04:05.123456 UTC', NUMERIC '1.25'), (2, NULL, NULL, NULL, NULL, NULL)",
		"INSERT INTO " + d + ".b VALUES (3, 'three', STRUCT(30, ARRAY<STRING>[]), -0.25, TIMESTAMP '2021-01-01 00:00:00 UTC', NUMERIC '3')",
		"INSERT INTO " + d + ".full (id) VALUES (9)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	copyTo := func(dst string, set func(*bigquery.Copier), srcs ...string) (*bigquery.Job, error) {
		var tables []*bigquery.Table
		for _, s := range srcs {
			tables = append(tables, ds.Table(s))
		}
		cp := ds.Table(dst).CopierFrom(tables...)
		if set != nil {
			set(cp)
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := cp.Run(cctx)
		if err != nil {
			return nil, err
		}
		st, err := job.Wait(cctx)
		if err == nil {
			err = st.Err()
		}
		return job, err
	}
	rows := func(table string) []string {
		t.Helper()
		it, err := c.Query("SELECT TO_JSON_STRING(t) FROM " + d + "." + table + " AS t").Read(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		var out []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("read %s: %v", table, err)
			}
			out = append(out, fmt.Sprint(row[0]))
		}
		sort.Strings(out)
		return out
	}
	wantJobReason := func(what string, err error, reason string) {
		t.Helper()
		var be *bigquery.Error
		if !errors.As(err, &be) || be.Reason != reason {
			t.Errorf("%s: %v, want reason %s", what, err, reason)
		}
	}

	job, err := copyTo("both", nil, "a", "b")
	if err != nil {
		t.Fatalf("copy a and b to a new table: %v", err)
	}
	want := append(rows("a"), rows("b")...)
	sort.Strings(want)
	if got := rows("both"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the copy holds %v, want %v", got, want)
	}
	meta, err := ds.Table("both").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := schemaText(meta.Schema), schemaText(schema); got != want {
		t.Errorf("the copy's schema is %s, want %s", got, want)
	}
	// The copy's FLOAT column takes a FLOAT64 value.
	if err := bqRun(ctx, c, "INSERT INTO "+d+".both (id, f) VALUES (4, CAST(0.5 AS FLOAT64))", false); err != nil {
		t.Errorf("INSERT into the copy: %v", err)
	}
	j, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatalf("jobs.get of the copy: %v", err)
	}
	if conf, err := j.Config(); err != nil || len(conf.(*bigquery.CopyConfig).Srcs) != 2 {
		t.Errorf("jobs.get of the copy: %v %v", conf, err)
	}
	if st := j.LastStatus(); st == nil || st.State != bigquery.Done || st.Err() != nil {
		t.Errorf("jobs.get of the copy: status %+v", st)
	}
	found := false
	it := c.Jobs(ctx)
	for {
		lj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || lj.ID() == job.ID()
	}
	if !found {
		t.Errorf("jobs.list does not list the copy %s", job.ID())
	}

	_, err = copyTo("full", nil, "a")
	wantJobReason("WRITE_EMPTY into a table with rows", err, "duplicate")
	if got := rows("full"); len(got) != 1 {
		t.Errorf("the refused copy changed full: %v", got)
	}
	if _, err := copyTo("full", func(cp *bigquery.Copier) { cp.WriteDisposition = bigquery.WriteAppend }, "b"); err != nil {
		t.Errorf("WRITE_APPEND: %v", err)
	}
	if got := rows("full"); len(got) != 2 {
		t.Errorf("after WRITE_APPEND, full holds %v", got)
	}
	if _, err := copyTo("full", func(cp *bigquery.Copier) { cp.WriteDisposition = bigquery.WriteTruncate }, "a"); err != nil {
		t.Errorf("WRITE_TRUNCATE: %v", err)
	}
	if got, want := rows("full"), rows("a"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after WRITE_TRUNCATE, full holds %v, want %v", got, want)
	}
	_, err = copyTo("none", func(cp *bigquery.Copier) { cp.CreateDisposition = bigquery.CreateNever }, "a")
	wantJobReason("CREATE_NEVER of a table that does not exist", err, "notFound")
	_, err = copyTo("none", nil, "nope")
	wantJobReason("a source that does not exist", err, "notFound")
	if _, err := ds.Table("none").Metadata(ctx); err == nil {
		t.Errorf("a failed copy made its table")
	}
	if err := bqRun(ctx, c, "CREATE VIEW "+d+".v AS SELECT id FROM "+d+".a", false); err != nil {
		t.Fatal(err)
	}
	_, err = copyTo("none", nil, "v")
	wantJobReason("a view as the source", err, "invalid")
	_, err = copyTo("snap", func(cp *bigquery.Copier) { cp.OperationType = bigquery.SnapshotOperation }, "a")
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
		t.Errorf("a snapshot: %v, want 501", err)
	}
}

// schemaText writes a schema's names, types, modes and descriptions.
func schemaText(s bigquery.Schema) string {
	var parts []string
	for _, f := range s {
		parts = append(parts, fmt.Sprintf("%s %s required=%v repeated=%v %q {%s}", f.Name, f.Type, f.Required, f.Repeated,
			f.Description, schemaText(f.Schema)))
	}
	return strings.Join(parts, ", ")
}

// errReason returns the reason of a job's error, as the Go client gives it:
// a *bigquery.Error from the job's status, or a *googleapi.Error from
// jobs.getQueryResults.
func errReason(err error) string {
	var be *bigquery.Error
	if errors.As(err, &be) {
		return be.Reason
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) && len(ge.Errors) > 0 {
		return ge.Errors[0].Reason
	}
	return ""
}
