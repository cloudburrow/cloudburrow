//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryPlainDMLThroughTheFront (#1008): INSERT, UPDATE, DELETE,
// MERGE and TRUNCATE TABLE change the rows as BigQuery does, and each job
// reports its statement type and how many rows it changed, through the
// official Go client (Query.Run and Job.Wait: jobs.insert, jobs.get), in
// jobs.list (Client.Jobs), and in jobs.query's answer (the generated
// client). Measured first: every DML job reported statementType SELECT
// and 0 affected rows, and a MERGE from a subquery failed 400 "MERGE:
// source must be a single-table reference".
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.list, bigquery.jobs.query
func TestBigQueryPlainDMLThroughTheFront(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	type want struct {
		typ                        string
		affected                   int64
		inserted, updated, deleted int64
	}
	jobs := map[string]want{}
	run := func(sql string, w want) {
		t.Helper()
		q := c.Query(sql)
		q.DefaultDatasetID = ds.DatasetID
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st, err := job.Wait(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if st.Err() != nil {
			t.Fatalf("%s: %v", sql, st.Err())
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		if qs == nil {
			t.Fatalf("%s: the job reports no query statistics", sql)
		}
		got := want{typ: qs.StatementType, affected: qs.NumDMLAffectedRows}
		if qs.DMLStats != nil {
			got.inserted, got.updated, got.deleted = qs.DMLStats.InsertedRowCount, qs.DMLStats.UpdatedRowCount, qs.DMLStats.DeletedRowCount
		}
		if got != w {
			t.Errorf("%s: the job reports %+v, want %+v", sql, got, w)
		}
		jobs[job.ID()] = w
	}
	run("INSERT INTO orders (id, region, amount) VALUES (5, 'ap', 2), (6, 'ap', 3)", want{"INSERT", 2, 2, 0, 0})
	run("UPDATE orders SET region = 'eu2' WHERE region = 'eu'", want{"UPDATE", 2, 0, 2, 0})
	run("UPDATE orders o SET amount = o.amount WHERE o.region = 'us'", want{"UPDATE", 2, 0, 2, 0})
	run("DELETE FROM orders WHERE id = 6", want{"DELETE", 1, 0, 0, 1})
	run("MERGE orders o USING (SELECT 5 AS id, 'sa' AS region) s ON o.id = s.id "+
		"WHEN MATCHED THEN UPDATE SET region = s.region", want{"MERGE", 1, 0, 1, 0})
	run("MERGE orders o USING (SELECT 4 AS id, 'x' AS region UNION ALL SELECT 7, 'nz') s ON o.id = s.id "+
		"WHEN MATCHED AND o.region = 'none' THEN DELETE "+
		"WHEN MATCHED THEN UPDATE SET region = s.region "+
		"WHEN NOT MATCHED THEN INSERT (id, region, amount) VALUES (s.id, s.region, 0)", want{"MERGE", 2, 1, 1, 0})
	run("DELETE FROM orders WHERE id = 42", want{"DELETE", 0, 0, 0, 0})

	rows := func() string {
		t.Helper()
		it, err := c.Query("SELECT id, region FROM `" + ds.DatasetID + ".orders` ORDER BY id").Read(ctx)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		var out []string
		for {
			var r []bigquery.Value
			err := it.Next(&r)
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			out = append(out, fmt.Sprint(r[0], " ", r[1]))
		}
		return strings.Join(out, ", ")
	}
	if got, w := rows(), "1 eu2, 2 eu2, 3 us, 4 x, 5 sa, 7 nz"; got != w {
		t.Errorf("the table holds %s, want %s", got, w)
	}

	// jobs.list gives each job's statistics as jobs.get does.
	it := c.Jobs(ctx)
	it.AllUsers = true
	seen := 0
	for seen < len(jobs) {
		j, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Client.Jobs: %v", err)
		}
		w, ok := jobs[j.ID()]
		if !ok {
			continue
		}
		seen++
		qs, _ := j.LastStatus().Statistics.Details.(*bigquery.QueryStatistics)
		if qs == nil || qs.StatementType != w.typ || qs.NumDMLAffectedRows != w.affected {
			t.Errorf("jobs.list gives job %s %+v, want %+v", j.ID(), qs, w)
		}
	}
	if seen != len(jobs) {
		t.Errorf("jobs.list gave %d of the %d DML jobs", seen, len(jobs))
	}

	// jobs.query answers with the count and the DmlStats, and TRUNCATE
	// TABLE deletes every row.
	svc := generatedBigQuery(t, h)
	legacy := false
	resp, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: "TRUNCATE TABLE orders", UseLegacySql: &legacy,
		DefaultDataset: &bq.DatasetReference{ProjectId: project, DatasetId: ds.DatasetID}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.query TRUNCATE TABLE: %v", err)
	}
	if resp.NumDmlAffectedRows != 6 || resp.DmlStats == nil || resp.DmlStats.DeletedRowCount != 6 {
		t.Errorf("jobs.query TRUNCATE TABLE answered numDmlAffectedRows %v, dmlStats %+v; want 6 deleted", resp.NumDmlAffectedRows, resp.DmlStats)
	}
	if got := rows(); got != "" {
		t.Errorf("after TRUNCATE TABLE the table holds %s", got)
	}
	job, err := svc.Jobs.Get(project, resp.JobReference.JobId).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	if job.Statistics == nil || job.Statistics.Query == nil || job.Statistics.Query.StatementType != "TRUNCATE_TABLE" {
		t.Errorf("jobs.get of the TRUNCATE TABLE: %+v", job.Statistics)
	}
}

// TestBigQueryMergeFromASubqueryInAScript (#1008): the front runs a MERGE
// from a subquery from a table of its own only when it is run alone;
// inside a script of several statements it is 501, and nothing is run.
//
// covers: bigquery.jobs.query
func TestBigQueryMergeFromASubqueryInAScript(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	q := c.Query("INSERT INTO orders (id, region, amount) VALUES (9, 'x', 1); " +
		"MERGE orders o USING (SELECT 9 AS id) s ON o.id = s.id WHEN MATCHED THEN DELETE")
	q.DefaultDatasetID = ds.DatasetID
	_, err := q.Read(ctx)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotImplemented || !strings.Contains(gerr.Message, "MERGE") {
		t.Fatalf("a script with a MERGE from a subquery: %v, want 501 naming it", err)
	}
	it, err := c.Query("SELECT COUNT(*) FROM `" + ds.DatasetID + ".orders`").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n []bigquery.Value
	if err := it.Next(&n); err != nil || n[0] != int64(4) {
		t.Errorf("after the refused script the table has %v rows (%v), want 4", n, err)
	}
}

// TestBigQueryTablePatchLabelsAndDescription (#1009), through the official
// Go client's Table.Update (tables.patch): a label set to a new value, a
// label deleted (DeleteLabel, sent as null) and one added are merged
// into the table's labels, a label not named is kept, and an empty
// description clears it. Measured first: the deleted label read back as
// "", a patch of some labels dropped the others, and an empty description
// was ignored.
//
// covers: bigquery.tables.patch, bigquery.tables.get
func TestBigQueryTablePatchLabelsAndDescription(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	_, tbl := seedOrders(t, h, c)
	var u bigquery.TableMetadataToUpdate
	u.Description = "orders"
	u.SetLabel("a", "1")
	u.SetLabel("b", "2")
	u.SetLabel("keep", "k")
	if _, err := tbl.Update(ctx, u, ""); err != nil {
		t.Fatalf("Table.Update: %v", err)
	}
	u = bigquery.TableMetadataToUpdate{}
	u.SetLabel("a", "9")
	u.DeleteLabel("b")
	u.SetLabel("c", "3")
	u.Description = ""
	if _, err := tbl.Update(ctx, u, ""); err != nil {
		t.Fatalf("Table.Update: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatalf("Table.Metadata: %v", err)
	}
	var labels []string
	for k, v := range md.Labels {
		labels = append(labels, k+"="+v)
	}
	sort.Strings(labels)
	if got := strings.Join(labels, ","); got != "a=9,c=3,keep=k" {
		t.Errorf("the table's labels are %s, want a=9,c=3,keep=k", got)
	}
	if md.Description != "" {
		t.Errorf("the description is %q after it was cleared", md.Description)
	}
	if len(md.Schema) != 3 {
		t.Errorf("the patch changed the schema: %v", schemaText(md.Schema))
	}

	// Every label deleted leaves none.
	u = bigquery.TableMetadataToUpdate{}
	for _, k := range []string{"a", "c", "keep"} {
		u.DeleteLabel(k)
	}
	if _, err := tbl.Update(ctx, u, ""); err != nil {
		t.Fatalf("Table.Update: %v", err)
	}
	if md, err = tbl.Metadata(ctx); err != nil || len(md.Labels) != 0 {
		t.Errorf("after deleting every label: %v %v", md.Labels, err)
	}
}

// TestBigQueryTablePatchRefusesSchemaChanges (#1009), through Table.Update
// (tables.patch): a schema that adds a REQUIRED column, drops a column or
// changes a column's type is refused 400, as BigQuery refuses it, and the
// table is left as it was. Measured first: the emulator took each.
//
// covers: bigquery.tables.patch
func TestBigQueryTablePatchRefusesSchemaChanges(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	_, tbl := seedOrders(t, h, c)
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := schemaText(md.Schema)
	withRequired := append(append(bigquery.Schema{}, md.Schema...), &bigquery.FieldSchema{Name: "must", Type: bigquery.StringFieldType, Required: true})
	dropped := append(bigquery.Schema{}, md.Schema[:2]...)
	retyped := bigquery.Schema{{Name: md.Schema[0].Name, Type: bigquery.StringFieldType}}
	retyped = append(retyped, md.Schema[1:]...)
	for name, s := range map[string]bigquery.Schema{"a REQUIRED column added": withRequired, "a column dropped": dropped, "a column retyped": retyped} {
		_, err := tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: s}, "")
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest || !strings.Contains(gerr.Message, "Provided Schema does not match Table") {
			t.Errorf("%s: %v, want 400 invalid", name, err)
		}
	}
	if md, err = tbl.Metadata(ctx); err != nil || schemaText(md.Schema) != before {
		t.Errorf("after the refused patches the table reads %v (%v), want %s", schemaText(md.Schema), err, before)
	}
}

// TestBigQueryTableUpdateAddsAColumnAndLabelsAtOnce (#1054), through the
// official Go client: one Table.Update (tables.patch) that adds a column,
// sets and deletes labels and sets the description does all of it: the
// table reads the new column (NULL in its rows, which it keeps), the
// labels merged, and the new description. tables.patch and tables.update
// take one path in the front, which reads the table once.
//
// covers: bigquery.tables.patch
func TestBigQueryTableUpdateAddsAColumnAndLabelsAtOnce(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	_, tbl := seedOrders(t, h, c)
	var u bigquery.TableMetadataToUpdate
	u.SetLabel("a", "1")
	u.SetLabel("b", "2")
	if _, err := tbl.Update(ctx, u, ""); err != nil {
		t.Fatalf("Table.Update: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u = bigquery.TableMetadataToUpdate{Schema: append(append(bigquery.Schema{}, md.Schema...), &bigquery.FieldSchema{Name: "note", Type: bigquery.StringFieldType})}
	u.SetLabel("a", "9")
	u.DeleteLabel("b")
	u.Description = "with a note"
	if _, err := tbl.Update(ctx, u, md.ETag); err != nil {
		t.Fatalf("Table.Update adding a column and labels: %v", err)
	}
	if md, err = tbl.Metadata(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := schemaText(md.Schema), schemaText(u.Schema); got != want {
		t.Errorf("the table reads %s, want %s", got, want)
	}
	if !reflect.DeepEqual(md.Labels, map[string]string{"a": "9"}) || md.Description != "with a note" {
		t.Errorf("the table's labels %v and description %q, want a=9 and %q", md.Labels, md.Description, "with a note")
	}
	it, err := c.Query("SELECT COUNT(*) FROM " + tbl.DatasetID + ".orders WHERE note IS NULL").Read(ctx)
	if err != nil {
		t.Fatalf("SELECT of the new column: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || len(row) != 1 || row[0] != int64(4) {
		t.Errorf("the rows with note NULL: %v %v, want 4", row, err)
	}
}

// TestBigQueryCreateViewReadsBackAsWritten (#1014): a view made with
// CREATE VIEW through jobs.query (Query.Read), and one replaced with
// CREATE OR REPLACE VIEW through a query job (Query.Run), read back
// through Table.Metadata (tables.get) with their query as written, and
// give their rows. Measured first: tables.get gave the emulator's
// rewritten SQL, which names its own table and functions.
//
// covers: bigquery.tables.get, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryCreateViewReadsBackAsWritten(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	d := ds.DatasetID
	view := "SELECT id FROM `" + d + ".orders` WHERE amount > 5"
	if _, err := c.Query("CREATE VIEW `" + d + ".big` AS " + view).Read(ctx); err != nil {
		t.Fatalf("CREATE VIEW: %v", err)
	}
	md, err := ds.Table("big").Metadata(ctx)
	if err != nil {
		t.Fatalf("Table.Metadata: %v", err)
	}
	if md.ViewQuery != view {
		t.Errorf("the view's query reads back %q, want %q", md.ViewQuery, view)
	}
	// Created again with IF NOT EXISTS and another query: nothing changes.
	if _, err := c.Query("CREATE VIEW IF NOT EXISTS `" + d + ".big` AS SELECT 1 AS one").Read(ctx); err != nil {
		t.Fatalf("CREATE VIEW IF NOT EXISTS: %v", err)
	}
	if md, err = ds.Table("big").Metadata(ctx); err != nil || md.ViewQuery != view {
		t.Errorf("after CREATE VIEW IF NOT EXISTS the query reads %q (%v), want %q", md.ViewQuery, err, view)
	}

	replaced := "SELECT id, region\nFROM `" + d + ".orders`\nWHERE region = 'us'"
	job, err := c.Query("CREATE OR REPLACE VIEW `" + d + ".big` AS " + replaced).Run(ctx)
	if err != nil {
		t.Fatalf("CREATE OR REPLACE VIEW: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("CREATE OR REPLACE VIEW: %v %v", err, st.Err())
	}
	if md, err = ds.Table("big").Metadata(ctx); err != nil || md.ViewQuery != replaced {
		t.Errorf("the replaced view's query reads back %q (%v), want %q", md.ViewQuery, err, replaced)
	}
	it, err := c.Query("SELECT COUNT(*) FROM `" + d + ".big`").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n []bigquery.Value
	if err := it.Next(&n); err != nil || n[0] != int64(2) {
		t.Errorf("the view gives %v rows (%v), want 2", n, err)
	}
}

// TestBigQueryUpdateFrom (#1027): UPDATE ... FROM a subquery or a table
// runs, as BigQuery runs it (https://cloud.google.com/bigquery/docs/reference/standard-sql/dml-syntax#update_statement),
// and reports its rows; one with several FROM items is 501. Measured
// first through the front: 400 "failed to analyze: Update with joins not
// supported [at 1:1]".
func TestBigQueryUpdateFrom(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	run := func(sql string) *bigquery.QueryStatistics {
		t.Helper()
		q := c.Query(sql)
		q.DefaultDatasetID = ds.DatasetID
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st, err := job.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		if qs == nil {
			t.Fatalf("%s: no query statistics", sql)
		}
		return qs
	}
	qs := run("UPDATE orders o SET region = s.region FROM (SELECT 3 AS id, 'x' AS region UNION ALL SELECT 4, 'y') s WHERE o.id = s.id")
	if qs.StatementType != "UPDATE" || qs.NumDMLAffectedRows != 2 {
		t.Errorf("UPDATE ... FROM a subquery: %s %d, want UPDATE 2", qs.StatementType, qs.NumDMLAffectedRows)
	}
	if err := runIn(ctx, c, project, ds.DatasetID, "CREATE TABLE ids AS SELECT 1 AS id, 'z' AS region", false); err != nil {
		t.Fatal(err)
	}
	if qs := run("UPDATE orders SET region = ids.region FROM ids WHERE orders.id = ids.id"); qs.NumDMLAffectedRows != 1 {
		t.Errorf("UPDATE ... FROM a table: %d rows, want 1", qs.NumDMLAffectedRows)
	}
	got := queryRows(t, ctx, c, project, ds.DatasetID, "SELECT STRING_AGG(region, ',' ORDER BY id) FROM orders")
	if want := []string{"z,eu,x,y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("read back %v, want %v", got, want)
	}
	err := runIn(ctx, c, project, ds.DatasetID, "UPDATE orders o SET region = 'q' FROM ids, ids AS j WHERE o.id = ids.id", false)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != 501 {
		t.Errorf("UPDATE ... FROM two items: %v, want 501", err)
	}
}
