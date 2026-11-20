//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// DML of rows the Storage Write API just wrote (#2007), to
// https://docs.cloud.google.com/bigquery/docs/data-manipulation-language,
// Limitations: "Rows that were recently written using the BigQuery Storage
// Write API (REST) can't be modified with data manipulation language (DML),
// such as UPDATE, DELETE, MERGE, or TRUNCATE statements. The recent writes
// are those that occurred within the last 30 minutes." and "Alternatively,
// rows that were recently written using the Storage Write API (gRPC) can
// be modified using UPDATE, DELETE, or MERGE statements." (also
// https://docs.cloud.google.com/bigquery/docs/write-api, "Use data
// manipulation language (DML) with recently streamed data").

// TestBigQueryWriteAPIRowsModifiableAtOnce: rows appended through the
// managed writer, on the default stream and on a committed stream, are
// changed by UPDATE, MERGE and DELETE at once, with no wait; rows the same
// table got from tabledata.insertAll at the same time are still refused
// with BigQuery's streaming buffer error, and those of the gRPC writes
// beside them are not.
//
// covers: bigquery.jobs.query, bigquery.tabledata.insertAll
func TestBigQueryWriteAPIRowsModifiableAtOnce(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	tbl := ds.Table("grpcdml")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "name", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	full := "`" + project + "." + ds.DatasetID + ".grpcdml`"
	m, dp := writerSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	rowsOf := func(ids ...int64) [][]byte {
		t.Helper()
		var out [][]byte
		for _, id := range ids {
			msg := dynamicpb.NewMessage(m)
			msg.Set(m.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
			msg.Set(m.Fields().ByName("name"), protoreflect.ValueOfString(fmt.Sprintf("n%d", id)))
			b, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
		return out
	}
	appendRows := func(typ managedwriter.StreamType, ids ...int64) {
		t.Helper()
		ms, err := mw.NewManagedStream(ctx, managedwriter.WithDestinationTable(writeTablePath(tbl)),
			managedwriter.WithType(typ), managedwriter.WithSchemaDescriptor(dp))
		if err != nil {
			t.Fatal(err)
		}
		defer ms.Close()
		res, err := ms.AppendRows(ctx, rowsOf(ids...))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := res.GetResult(ctx); err != nil {
			t.Fatalf("%v append: %v", typ, err)
		}
		if typ == managedwriter.CommittedStream {
			if _, err := ms.Finalize(ctx); err != nil {
				t.Fatalf("finalize: %v", err)
			}
		}
	}
	appendRows(managedwriter.DefaultStream, 1, 2)
	appendRows(managedwriter.CommittedStream, 3, 4)
	run := func(sql string) error {
		_, err := c.Query(sql).Read(ctx)
		return err
	}
	read := func() string {
		t.Helper()
		return strings.Join(queryRows(t, ctx, c, "", "", "SELECT FORMAT('%d=%s', id, name) FROM "+full+" ORDER BY id"), ",")
	}
	if got := read(); got != "1=n1,2=n2,3=n3,4=n4" {
		t.Fatalf("after the appends: %s", got)
	}
	for _, sql := range []string{
		"UPDATE " + full + " SET name = 'u' WHERE id IN (1, 3)",
		"MERGE " + full + " t USING (SELECT 2 AS id) s ON t.id = s.id WHEN MATCHED THEN UPDATE SET name = 'm'",
		"DELETE FROM " + full + " WHERE id = 4",
	} {
		if err := run(sql); err != nil {
			t.Errorf("%s at once after a gRPC append: %v", sql, err)
		}
	}
	if got := read(); got != "1=u,2=m,3=u" {
		t.Errorf("after the DML: %s, want 1=u,2=m,3=u", got)
	}

	// tabledata.insertAll rows of the same table are held 30 minutes.
	type row struct {
		ID   int64  `bigquery:"id"`
		Name string `bigquery:"name"`
	}
	if err := tbl.Inserter().Put(ctx, []*row{{ID: 5, Name: "n5"}}); err != nil {
		t.Fatal(err)
	}
	err := run("UPDATE " + full + " SET name = 'x' WHERE id = 5")
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest ||
		!strings.Contains(gerr.Message, "would affect rows in the streaming buffer, which is not supported") {
		t.Errorf("UPDATE of a row insertAll just wrote: %v, want 400 naming the streaming buffer", err)
	}
	appendRows(managedwriter.DefaultStream, 6)
	if err := run("UPDATE " + full + " SET name = 'g' WHERE id IN (1, 6)"); err != nil {
		t.Errorf("UPDATE of gRPC rows beside an insertAll row: %v", err)
	}
	if got := read(); got != "1=g,2=m,3=u,5=n5,6=g" {
		t.Errorf("at the end: %s, want 1=g,2=m,3=u,5=n5,6=g", got)
	}
}

// Columns per table (#2008): "Each table, query result, or view
// definition can have up to 10,000 columns. This includes nested and
// repeated columns." (https://docs.cloud.google.com/bigquery/quotas,
// "Maximum number of columns in a table").

// TestBigQueryTenThousandColumns: a table of 10,000 columns is created,
// written by tabledata.insertAll and by an INSERT statement, and queried,
// its last column by name and every column by SELECT *; tables.insert of
// 10,001 columns, and of 9,999 columns and a RECORD of one field (10,001
// counting the nested one), is 400 invalid, nothing created.
//
// covers: bigquery.tables.insert, bigquery.tabledata.insertAll, bigquery.jobs.query
func TestBigQueryTenThousandColumns(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	wide := func(n int) bigquery.Schema {
		s := make(bigquery.Schema, n)
		for i := range s {
			s[i] = &bigquery.FieldSchema{Name: fmt.Sprintf("c%d", i), Type: bigquery.IntegerFieldType}
		}
		return s
	}
	tbl := ds.Table("cols10000")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: wide(10000)}); err != nil {
		t.Fatalf("create a table of 10,000 columns: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != 10000 {
		t.Fatalf("the table has %d columns, want 10,000", len(md.Schema))
	}
	row := bigquery.ValuesSaver{Schema: md.Schema, InsertID: "r1", Row: make([]bigquery.Value, 10000)}
	for i := range row.Row {
		row.Row[i] = int64(i)
	}
	if err := tbl.Inserter().Put(ctx, &row); err != nil {
		t.Fatalf("insertAll of a 10,000-column row: %v", err)
	}
	full := "`" + project + "." + ds.DatasetID + ".cols10000`"
	if _, err := c.Query("INSERT INTO " + full + " (c0, c9999) VALUES (-1, -2)").Read(ctx); err != nil {
		t.Fatalf("INSERT into the 10,000-column table: %v", err)
	}
	if got := strings.Join(queryRows(t, ctx, c, "", "", "SELECT FORMAT('%d:%d:%d', c0, IFNULL(c5000, 0), c9999) FROM "+full+" ORDER BY c0"), ","); got != "-1:0:-2,0:5000:9999" {
		t.Errorf("the rows: %s, want -1:0:-2,0:5000:9999", got)
	}
	it, err := c.Query("SELECT * FROM " + full + " WHERE c0 = 0").Read(ctx)
	if err != nil {
		t.Fatalf("SELECT * of 10,000 columns: %v", err)
	}
	var vals []bigquery.Value
	if err := it.Next(&vals); err != nil {
		t.Fatal(err)
	}
	if len(vals) != 10000 || vals[9999] != int64(9999) || vals[1234] != int64(1234) {
		t.Errorf("SELECT *: %d values, the last %v", len(vals), vals[len(vals)-1])
	}

	refused := func(name string, schema bigquery.Schema) {
		t.Helper()
		err := ds.Table(name).Create(ctx, &bigquery.TableMetadata{Schema: schema})
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest || !strings.Contains(gerr.Message, "up to 10000 columns") {
			t.Errorf("create %s: %v, want 400 saying a table can have up to 10000 columns", name, err)
		} else if len(gerr.Errors) == 0 || gerr.Errors[0].Reason != "invalid" {
			t.Errorf("create %s: reasons %+v, want invalid", name, gerr.Errors)
		}
		if _, err := ds.Table(name).Metadata(ctx); err == nil {
			t.Errorf("the refused table %s was created", name)
		}
	}
	refused("cols10001", wide(10001))
	nested := append(wide(9999), &bigquery.FieldSchema{Name: "r", Type: bigquery.RecordFieldType,
		Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}})
	refused("nested10001", nested)
}
