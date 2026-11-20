//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// Streamed rows and DML inside a multi-statement transaction (#2012):
// "Rows that were recently written using the BigQuery Storage Write API
// (REST) can't be modified with data manipulation language (DML), such as
// UPDATE, DELETE, MERGE, or TRUNCATE statements"
// (https://docs.cloud.google.com/bigquery/docs/data-manipulation-language);
// https://docs.cloud.google.com/bigquery/docs/transactions makes no
// exception for DML inside a transaction, where an error goes to the
// exception handler with the transaction open, or, with none, fails the
// script and rolls the transaction back.

// TestBigQueryStreamingBufferDMLInTransaction: inside BEGIN TRANSACTION,
// UPDATE, DELETE, MERGE and TRUNCATE TABLE of a row tabledata.insertAll
// just wrote fail with BigQuery's streaming buffer error, through
// jobs.query and a query job, and the transaction is rolled back (its
// earlier INSERT is not kept); DML of the table's other rows commits; an
// exception handler sees the error and rolls back.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tabledata.insertAll
func TestBigQueryStreamingBufferDMLInTransaction(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	tbl := ds.Table("txnbuffer")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "name", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	full := "`" + project + "." + ds.DatasetID + ".txnbuffer`"
	if _, err := scriptRows(ctx, c, "INSERT INTO "+full+" (id, name) VALUES (1, 'n1')", false); err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID   int64  `bigquery:"id"`
		Name string `bigquery:"name"`
	}
	if err := tbl.Inserter().Put(ctx, []*row{{ID: 2, Name: "n2"}}); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		return strings.Join(queryRows(t, ctx, c, "", "", "SELECT FORMAT('%d=%s', id, name) FROM "+full+" ORDER BY id"), ",")
	}
	if got := read(); got != "1=n1,2=n2" {
		t.Fatalf("at the start: %s", got)
	}
	const bufferMsg = "would affect rows in the streaming buffer, which is not supported"
	for _, insert := range []bool{false, true} {
		for _, stmt := range []string{
			"UPDATE " + full + " SET name = 'x' WHERE id = 2",
			"DELETE FROM " + full + " WHERE id >= 2",
			"MERGE " + full + " t USING (SELECT 2 AS id) s ON t.id = s.id WHEN MATCHED THEN UPDATE SET name = 'm'",
			"TRUNCATE TABLE " + full,
		} {
			_, err := scriptRows(ctx, c, "BEGIN TRANSACTION;\nINSERT INTO "+full+" (id, name) VALUES (9, 'n9');\n"+
				stmt+";\nCOMMIT TRANSACTION", insert)
			var gerr *googleapi.Error
			if err == nil || !strings.Contains(err.Error(), bufferMsg) {
				t.Errorf("%s in a transaction (insert %v): %v, want the streaming buffer error", stmt, insert, err)
			} else if !insert && (!errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest ||
				len(gerr.Errors) == 0 || gerr.Errors[0].Reason != "badRequest") {
				t.Errorf("%s in a transaction: %v, want 400 badRequest", stmt, err)
			}
			if got := read(); got != "1=n1,2=n2" {
				t.Errorf("after %s in a transaction (insert %v): %s, want 1=n1,2=n2 (rolled back)", stmt, insert, got)
			}
		}
	}

	// The handler sees the error with the transaction open.
	got, err := scriptRows(ctx, c, "BEGIN\n  BEGIN TRANSACTION;\n  DELETE FROM "+full+" WHERE id = 2;\n  COMMIT TRANSACTION;\n"+
		"EXCEPTION WHEN ERROR THEN\n  ROLLBACK TRANSACTION;\n  SELECT @@error.message AS m;\nEND;", false)
	if err != nil || len(got) != 1 || !strings.Contains(fmt.Sprint(got[0][0]), bufferMsg) {
		t.Errorf("the handler's @@error.message: %v %v, want the streaming buffer error", got, err)
	}

	// DML of the rows not in the buffer commits.
	if _, err := scriptRows(ctx, c, "BEGIN TRANSACTION;\nUPDATE "+full+" SET name = 'u' WHERE id = 1;\n"+
		"INSERT INTO "+full+" (id, name) VALUES (3, 'n3');\nDELETE FROM "+full+" WHERE id = 3;\nCOMMIT TRANSACTION", false); err != nil {
		t.Errorf("DML of unbuffered rows in a transaction: %v", err)
	}
	if got := read(); got != "1=u,2=n2" {
		t.Errorf("after the committed transaction: %s, want 1=u,2=n2", got)
	}
}

// The 10,000-column limit on queries and DDL (#2012): "Each table, query
// result, or view definition can have up to 10,000 columns. This includes
// nested and repeated columns." (https://docs.cloud.google.com/bigquery/quotas).

// TestBigQueryTenThousandColumnsQueriesAndDDL: a query result, CREATE
// TABLE ... AS SELECT and CREATE TABLE of 10,000 columns are taken; of
// 10,001 (nested columns counted) each fails 400, through jobs.query and a
// query job, and no table is made.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryTenThousandColumnsQueriesAndDDL(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	path := func(name string) string { return "`" + project + "." + ds.DatasetID + "." + name + "`" }
	list := func(n int, def bool) string {
		parts := make([]string, n)
		for i := range parts {
			if def {
				parts[i] = fmt.Sprintf("c%d INT64", i)
			} else {
				parts[i] = fmt.Sprintf("%d AS c%d", i, i)
			}
		}
		return strings.Join(parts, ", ")
	}
	const limitMsg = "can have up to 10000 columns"
	ok := func(sql string) {
		t.Helper()
		if _, err := scriptRows(ctx, c, sql, false); err != nil {
			t.Fatalf("%.70s...: %v", sql, err)
		}
	}
	refused := func(sql string) {
		t.Helper()
		for _, insert := range []bool{false, true} {
			_, err := scriptRows(ctx, c, sql, insert)
			var gerr *googleapi.Error
			switch {
			case err == nil || !strings.Contains(err.Error(), limitMsg):
				t.Errorf("%.70s... (insert %v): %v, want the 10,000-column error", sql, insert, err)
			case !insert && (!errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest ||
				len(gerr.Errors) == 0 || gerr.Errors[0].Reason != "invalidQuery"):
				t.Errorf("%.70s...: %v, want 400 invalidQuery", sql, err)
			}
		}
	}

	// 10,000 columns are taken.
	it, err := c.Query("SELECT " + list(10000, false)).Read(ctx)
	if err != nil {
		t.Fatalf("a query of 10,000 columns: %v", err)
	}
	var vals []bigquery.Value
	if err := it.Next(&vals); err != nil || len(vals) != 10000 || vals[9999] != int64(9999) {
		t.Fatalf("a query of 10,000 columns: %d values, %v", len(vals), err)
	}
	ok("CREATE TABLE " + path("ddl10000") + " (" + list(10000, true) + ")")
	ok("CREATE TABLE " + path("ctas10000") + " AS SELECT " + list(10000, false))
	md, err := ds.Table("ctas10000").Metadata(ctx)
	if err != nil || len(md.Schema) != 10000 {
		t.Fatalf("the CTAS table of 10,000 columns: %v", err)
	}

	// 10,001 are not.
	refused("SELECT " + list(10001, false))
	refused("SELECT *, 1 AS extra FROM " + path("ddl10000"))
	refused("SELECT t FROM " + path("ddl10000") + " AS t") // a STRUCT of 10,000 fields: 10,001 columns
	refused("CREATE TABLE " + path("ctas10001") + " AS SELECT " + list(10001, false))
	refused("CREATE TABLE " + path("ddl10001") + " (" + list(10001, true) + ")")
	refused("CREATE TABLE " + path("nested10001") + " (" + list(9999, true) + ", r STRUCT<x INT64>)")
	refused("CREATE TABLE " + path("arraynested10001") + " (" + list(9999, true) + ", r ARRAY<STRUCT<x INT64>>)")
	for _, name := range []string{"ctas10001", "ddl10001", "nested10001", "arraynested10001"} {
		if _, err := ds.Table(name).Metadata(ctx); err == nil {
			t.Errorf("the refused table %s was made", name)
		}
	}
	// An ARRAY of a scalar is one column.
	ok("CREATE TABLE " + path("array10000") + " (" + list(9999, true) + ", r ARRAY<INT64>)")
	if got, err := scriptRows(ctx, c, "SELECT COUNT(*) FROM "+path("ctas10000"), false); err != nil ||
		!reflect.DeepEqual(got, [][]bigquery.Value{{int64(1)}}) {
		t.Errorf("the CTAS table's rows: %v %v", got, err)
	}
}
