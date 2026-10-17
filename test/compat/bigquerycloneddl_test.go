//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
//
// TestBigQueryCloneAndSnapshotDDL (#1002), through the official Go client,
// by jobs.query and by jobs.insert: CREATE TABLE ... CLONE makes a writable
// table with the source's schema (REQUIRED kept) and rows and a
// cloneDefinition; CREATE SNAPSHOT TABLE ... CLONE makes a read-only
// SNAPSHOT; CREATE TABLE ... CLONE of the snapshot restores it into a
// standard table; IF NOT EXISTS of a table that exists does nothing, and
// without it the statement fails duplicate; a missing source fails notFound;
// DROP SNAPSHOT TABLE deletes the snapshot, and of a standard table is 501.
// The job reports statementType, ddlOperationPerformed and ddlTargetTable.
// Measured first: the emulator refuses every one of these statements (400
// "not supported", #937).
func TestBigQueryCloneAndSnapshotDDL(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	path := func(table string) string { return "`" + d + "." + table + "`" }
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType, Required: true, Description: "key"},
		{Name: "name", Type: bigquery.StringFieldType}}
	if err := ds.Table("base").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+path("base")+" VALUES (1, 'one'), (2, 'two')", false); err != nil {
		t.Fatal(err)
	}
	// run runs sql as a query job and returns its query statistics.
	run := func(sql string) (*bigquery.QueryStatistics, error) {
		t.Helper()
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		job, err := c.Query(sql).Run(qctx)
		if err != nil {
			return nil, err
		}
		st, err := job.Wait(qctx)
		if err != nil {
			return nil, err
		}
		if err := st.Err(); err != nil {
			return nil, err
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		return qs, nil
	}
	wantDDL := func(what string, qs *bigquery.QueryStatistics, statement, op, table string) {
		t.Helper()
		if qs == nil || qs.StatementType != statement || qs.DDLOperationPerformed != op || qs.DDLTargetTable == nil ||
			qs.DDLTargetTable.DatasetID != d || qs.DDLTargetTable.TableID != table {
			t.Errorf("%s: the job's statistics %+v, want %s %s of %s", what, qs, statement, op, table)
		}
	}
	count := func(table string) string {
		t.Helper()
		return fmt.Sprint(rows(t, ctx, c, "SELECT COUNT(*) FROM "+path(table)))
	}

	before := advanceBigQueryClock(t, h, 0).Add(-time.Minute) // the instance's clock, #1487
	// CREATE TABLE ... CLONE, through jobs.query and jobs.insert.
	if err := bqRun(ctx, c, "CREATE TABLE "+path("clone_q")+" CLONE "+path("base"), false); err != nil {
		t.Fatalf("CLONE through jobs.query: %v", err)
	}
	qs, err := run("CREATE TABLE IF NOT EXISTS " + path("clone_j") + " CLONE " + path("base") + ";")
	if err != nil {
		t.Fatalf("CLONE through jobs.insert: %v", err)
	}
	wantDDL("CLONE", qs, "CREATE_TABLE", "CREATE", "clone_j")
	for _, table := range []string{"clone_q", "clone_j"} {
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if md.Type != bigquery.RegularTable || schemaText(md.Schema) != schemaText(schema) || !md.Schema[0].Required {
			t.Errorf("%s: type %q, schema %s, want TABLE and the base's schema", table, md.Type, schemaText(md.Schema))
		}
		if cd := md.CloneDefinition; cd == nil || cd.BaseTableReference == nil || cd.BaseTableReference.TableID != "base" ||
			cd.CloneTime.Before(before) || cd.CloneTime.After(advanceBigQueryClock(t, h, 0).Add(time.Minute)) {
			t.Errorf("%s: the cloneDefinition %+v", table, md.CloneDefinition)
		}
		if got := count(table); got != "[[2]]" {
			t.Errorf("%s holds %s rows, want 2", table, got)
		}
	}
	// The clone is writable, and its base is left as it was.
	if err := bqRun(ctx, c, "INSERT INTO "+path("clone_q")+" VALUES (3, 'three')", false); err != nil {
		t.Errorf("INSERT into the clone: %v", err)
	}
	if got := count("base"); got != "[[2]]" {
		t.Errorf("the base holds %s rows after the clone's insert, want 2", got)
	}
	// IF NOT EXISTS of a table that exists does nothing; without it, the
	// statement fails duplicate, and a missing source notFound.
	qs, err = run("CREATE TABLE IF NOT EXISTS " + path("clone_q") + " CLONE " + path("base"))
	if err != nil {
		t.Fatalf("IF NOT EXISTS over the clone: %v", err)
	}
	wantDDL("IF NOT EXISTS", qs, "CREATE_TABLE", "SKIP", "clone_q")
	if got := count("clone_q"); got != "[[3]]" {
		t.Errorf("IF NOT EXISTS changed the clone: %s rows, want 3", got)
	}
	err = bqRun(ctx, c, "CREATE TABLE "+path("clone_q")+" CLONE "+path("base"), false)
	wantReason(t, "CLONE over a table", err, http.StatusConflict, "duplicate")
	err = bqRun(ctx, c, "CREATE TABLE "+path("nothing")+" CLONE "+path("missing"), false)
	wantReason(t, "CLONE of a missing table", err, http.StatusNotFound, "notFound")
	if tableExists(t, ctx, ds.Table("nothing")) {
		t.Error("a failed CLONE made its table")
	}

	// CREATE SNAPSHOT TABLE ... CLONE makes a read-only snapshot.
	qs, err = run("CREATE SNAPSHOT TABLE " + path("snap") + " CLONE " + path("base"))
	if err != nil {
		t.Fatalf("CREATE SNAPSHOT TABLE: %v", err)
	}
	wantDDL("CREATE SNAPSHOT TABLE", qs, "CREATE_SNAPSHOT_TABLE", "CREATE", "snap")
	md, err := ds.Table("snap").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sd := md.SnapshotDefinition; md.Type != bigquery.Snapshot || sd == nil || sd.BaseTableReference == nil ||
		sd.BaseTableReference.TableID != "base" || sd.SnapshotTime.Before(before) {
		t.Errorf("the snapshot: type %q, %+v", md.Type, md.SnapshotDefinition)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+path("snap")+" VALUES (9, 'nine')", false); err == nil {
		t.Error("an INSERT into the snapshot ran")
	}
	if got := count("snap"); got != "[[2]]" {
		t.Errorf("the snapshot holds %s rows, want 2", got)
	}
	err = bqRun(ctx, c, "CREATE SNAPSHOT TABLE "+path("snap2")+" CLONE "+path("snap"), false)
	wantReason(t, "a snapshot of a snapshot", err, http.StatusBadRequest, "invalidQuery")

	// CREATE TABLE ... CLONE of the snapshot restores it.
	if err := bqRun(ctx, c, "CREATE TABLE "+path("restored")+" CLONE "+path("snap"), true); err != nil {
		t.Fatalf("CLONE of the snapshot: %v", err)
	}
	md, err = ds.Table("restored").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Type != bigquery.RegularTable || md.SnapshotDefinition != nil {
		t.Errorf("the restored table: type %q, snapshotDefinition %+v", md.Type, md.SnapshotDefinition)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+path("restored")+" VALUES (3, 'three')", false); err != nil {
		t.Errorf("INSERT into the restored table: %v", err)
	}

	// DROP SNAPSHOT TABLE deletes a snapshot; of a standard table it is 501.
	err = bqRun(ctx, c, "DROP SNAPSHOT TABLE "+path("clone_j"), false)
	wantReason(t, "DROP SNAPSHOT TABLE of a table", err, http.StatusNotImplemented, "notImplemented")
	if !tableExists(t, ctx, ds.Table("clone_j")) {
		t.Error("a refused DROP SNAPSHOT TABLE deleted the table")
	}
	qs, err = run("DROP SNAPSHOT TABLE " + path("snap"))
	if err != nil {
		t.Fatalf("DROP SNAPSHOT TABLE: %v", err)
	}
	wantDDL("DROP SNAPSHOT TABLE", qs, "DROP_SNAPSHOT_TABLE", "DROP", "snap")
	if tableExists(t, ctx, ds.Table("snap")) {
		t.Error("DROP SNAPSHOT TABLE left the snapshot")
	}
	if err := bqRun(ctx, c, "DROP SNAPSHOT TABLE IF EXISTS "+path("snap"), false); err != nil {
		t.Errorf("DROP SNAPSHOT TABLE IF EXISTS of a dropped snapshot: %v", err)
	}
	err = bqRun(ctx, c, "DROP SNAPSHOT TABLE "+path("snap"), false)
	wantReason(t, "DROP SNAPSHOT TABLE of a dropped snapshot", err, http.StatusNotFound, "notFound")
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
//
// TestBigQueryCloneDDLOptionsAndSystemTime (#1002), through the official Go
// client: CREATE SNAPSHOT TABLE ... CLONE ... FOR SYSTEM_TIME AS OF
// CURRENT_TIMESTAMP() OPTIONS(expiration_timestamp, description, labels)
// makes a SNAPSHOT that tables.get reads back with each option, and
// CREATE TABLE ... CLONE ... OPTIONS(friendly_name) a clone with it. FOR
// SYSTEM_TIME AS OF a time before the table's time travel window fails
// invalidQuery, as https://cloud.google.com/bigquery/docs/time-travel
// documents; a time in the past within it copies the table as it was
// then (#1460: the base's two rows, inserted before it); an option
// CloudBurrow does not set is 501. A refused statement makes no table.
func TestBigQueryCloneDDLOptionsAndSystemTime(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType, Required: true}}
	if err := ds.Table("base").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+path("base")+" VALUES (1), (2)", false); err != nil {
		t.Fatal(err)
	}

	sql := "CREATE SNAPSHOT TABLE " + path("snap") + " CLONE " + path("base") +
		" FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP() OPTIONS(expiration_timestamp = " +
		"TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 48 HOUR), description = 'two rows', labels = [('team', 'data')])"
	if err := bqRun(ctx, c, sql, true); err != nil {
		t.Fatalf("CREATE SNAPSHOT TABLE with OPTIONS: %v", err)
	}
	md, err := ds.Table("snap").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Type != bigquery.Snapshot || md.SnapshotDefinition == nil || md.SnapshotDefinition.BaseTableReference.TableID != "base" {
		t.Errorf("the snapshot reads back type %q, %+v", md.Type, md.SnapshotDefinition)
	}
	if want := advanceBigQueryClock(t, h, 0).Add(48 * time.Hour); md.ExpirationTime.Before(want.Add(-5*time.Minute)) || md.ExpirationTime.After(want.Add(5*time.Minute)) {
		t.Errorf("the snapshot expires %v, want about %v", md.ExpirationTime, want)
	}
	if md.Description != "two rows" || md.Labels["team"] != "data" {
		t.Errorf("the snapshot's description %q, labels %v", md.Description, md.Labels)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT COUNT(*) FROM "+path("snap"))); got != "[[2]]" {
		t.Errorf("the snapshot holds %s rows", got)
	}

	if err := bqRun(ctx, c, "CREATE TABLE "+path("cl")+" CLONE "+path("base")+" OPTIONS(friendly_name = 'the clone')", false); err != nil {
		t.Fatalf("CREATE TABLE CLONE with OPTIONS: %v", err)
	}
	if md, err := ds.Table("cl").Metadata(ctx); err != nil || md.Name != "the clone" || md.CloneDefinition == nil || md.Type != bigquery.RegularTable {
		t.Errorf("the clone reads back %+v, %v", md, err)
	}

	err = bqRun(ctx, c, "CREATE TABLE "+path("old")+" CLONE "+path("base")+
		" FOR SYSTEM_TIME AS OF TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 30 DAY)", false)
	wantReason(t, "AS OF before the time travel window", err, http.StatusBadRequest, "invalidQuery")
	time.Sleep(2 * time.Second) // the base is older than the time read below
	err = bqRun(ctx, c, "CREATE SNAPSHOT TABLE "+path("past")+" CLONE "+path("base")+
		" FOR SYSTEM_TIME AS OF TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 SECOND)", false)
	if err != nil {
		t.Errorf("AS OF a time in the past: %v", err)
	} else if got := fmt.Sprint(rows(t, ctx, c, "SELECT COUNT(*) FROM "+path("past"))); got != "[[2]]" {
		t.Errorf("the snapshot of a second ago holds %s rows, want 2", got)
	}
	err = bqRun(ctx, c, "CREATE TABLE "+path("kms")+" CLONE "+path("base")+" OPTIONS(not_an_option = TRUE)", false)
	wantReason(t, "an option CloudBurrow does not set", err, http.StatusNotImplemented, "notImplemented")
	for _, name := range []string{"old", "kms"} {
		if tableExists(t, ctx, ds.Table(name)) {
			t.Errorf("a refused statement made %s", name)
		}
	}
}

// TestBigQueryCloneAndSnapshotInAScript (#1459, #1567): CREATE TABLE …
// CLONE, CREATE SNAPSHOT TABLE and DROP SNAPSHOT TABLE inside a script of
// several statements with no TEMP table or function, which the front
// carries out a statement at a time: the clone is a TABLE with the base's
// rows, a later statement of the script reads it, the snapshot is a
// SNAPSHOT, and a second script drops it.
func TestBigQueryCloneAndSnapshotInAScript(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("base")+" (id INT64)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+path("base")+" VALUES (1), (2)", false); err != nil {
		t.Fatal(err)
	}
	got := rows(t, ctx, c, "DECLARE n INT64 DEFAULT 1;\n"+
		"CREATE TABLE "+path("cloned")+" CLONE "+path("base")+";\n"+
		"CREATE SNAPSHOT TABLE "+path("snap")+" CLONE "+path("base")+";\n"+
		"INSERT INTO "+path("cloned")+" VALUES (3);\n"+
		"SELECT COUNT(*) FROM "+path("cloned")+" WHERE id > n;")
	if fmt.Sprint(got) != "[[2]]" {
		t.Errorf("the script's last statement read %v of the clone, want [[2]]", got)
	}
	for table, want := range map[string]bigquery.TableType{"cloned": bigquery.RegularTable, "snap": bigquery.Snapshot} {
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || md.Type != want {
			t.Errorf("the script's %s is %+v (%v), want a %s", table, md, err, want)
		}
	}
	if n := fmt.Sprint(rows(t, ctx, c, "SELECT COUNT(*) FROM "+path("snap"))); n != "[[2]]" {
		t.Errorf("the script's snapshot has %s rows, want the base's 2", n)
	}
	if err := bqRun(ctx, c, "DECLARE x INT64 DEFAULT 0;\nDROP SNAPSHOT TABLE "+path("snap")+";\nSELECT x;", false); err != nil {
		t.Fatalf("DROP SNAPSHOT TABLE in a script: %v", err)
	}
	if _, err := ds.Table("snap").Metadata(ctx); !isNotFound(err) {
		t.Errorf("after the script's DROP SNAPSHOT TABLE the snapshot reads %v, want NOT_FOUND", err)
	}
}
