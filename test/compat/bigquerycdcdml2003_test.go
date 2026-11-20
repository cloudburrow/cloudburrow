//go:build compat

package compat

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Change data capture's DML window and its remaining refusals (#2003), to
// https://docs.cloud.google.com/bigquery/docs/change-data-capture,
// Limitations: "Mutating DML statements are blocked while a table is
// actively receiving CDC streams or while unmerged changes remain on the
// table. When there are no active upsert streams and all pending changes
// are merged into the baseline table, mutating DML statements are
// supported." CloudBurrow merges each append's changes before answering
// it, so the window is an AppendRows connection's: from its first change
// applied until it ends. Google does not document the refusal's wording.

// TestBigQueryChangeDataCaptureBlocksDML: while a managed writer's default
// stream that has applied changes is open, UPDATE, DELETE, MERGE and
// TRUNCATE TABLE on the table fail (jobs.query and jobs.insert), nothing
// changed; INSERT, not a mutating statement, runs; DML on another table
// runs. Once the writer is closed, DELETE runs: after the front's clock is
// advanced 31 minutes, as CloudBurrow holds rows the Write API appended
// from DML for 30 minutes, which BigQuery does not for gRPC writes (#2007).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryChangeDataCaptureBlocksDML(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	tbl := cdcTable(t, h, ds, "cdcdml", true)
	cdcTable(t, h, ds, "cdcother", true)
	m, dp := cdcSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	full := "`" + project + "." + ds.DatasetID + ".cdcdml`"
	otherFull := "`" + project + "." + ds.DatasetID + ".cdcother`"

	ms, err := mw.NewManagedStream(ctx, managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			ms.Close()
		}
	}()
	res, err := ms.AppendRows(ctx, cdcRows(t, m,
		cdcChange{id: 1, name: "a", typ: "UPSERT", seq: "1"},
		cdcChange{id: 2, name: "b", typ: "UPSERT", seq: "1"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.GetResult(ctx); err != nil {
		t.Fatalf("append: %v", err)
	}
	want := map[int64]string{1: "a", 2: "b"}
	if got := cdcTableRows(t, h, c, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the upserts: %v, want %v", got, want)
	}

	refused := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s ran on a table receiving CDC", what)
			return
		}
		if !strings.Contains(err.Error(), "change data capture") || !strings.Contains(err.Error(), "actively receiving CDC streams") {
			t.Errorf("%s: %v, want the CDC refusal", what, err)
		}
	}
	for _, q := range []string{
		"UPDATE " + full + " SET name = 'u' WHERE id = 1",
		"DELETE FROM " + full + " WHERE id = 2",
		"MERGE " + full + " t USING (SELECT 1 AS id) s ON t.id = s.id WHEN MATCHED THEN DELETE",
		"TRUNCATE TABLE " + full,
	} {
		_, err := c.Query(q).Read(ctx)
		refused("jobs.query "+q, err)
	}
	// jobs.insert: the job fails.
	job, err := c.Query("DELETE FROM " + full + " WHERE id = 1").Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	refused("jobs.insert DELETE", err)
	if got := cdcTableRows(t, h, c, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the refused DML: %v, want %v", got, want)
	}
	// INSERT is not a mutating statement.
	if _, err := c.Query("INSERT INTO " + full + " (id, name) VALUES (3, 'c')").Read(ctx); err != nil {
		t.Errorf("INSERT on a table receiving CDC: %v", err)
	}
	want[3] = "c"
	// Another table is not receiving CDC.
	if _, err := c.Query("INSERT INTO " + otherFull + " (id, name) VALUES (1, 'x')").Read(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query("DELETE FROM " + otherFull + " WHERE id = 1").Read(ctx); err != nil {
		t.Errorf("DELETE on a table not receiving CDC: %v", err)
	}

	// The stream ends: no active upsert stream, nothing unmerged.
	ms.Close()
	closed = true
	advanceBigQueryClock(t, h, 31*time.Minute)
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err = c.Query("DELETE FROM " + full + " WHERE id = 2").Read(ctx)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("DELETE after the CDC stream closed: %v", err)
	}
	delete(want, 2)
	if got := cdcTableRows(t, h, c, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the DELETE: %v, want %v", got, want)
	}
}

// TestBigQueryChangeDataCaptureArrowRefused: "You must use the protobuf
// format as the ingestion format. The Apache Arrow format isn't
// supported." arrow_rows with _CHANGE_TYPE, through the official generated
// BigQueryWriteClient (the pinned managed writer writes proto_rows alone,
// bigquerystoragewritearrow_test.go), are INVALID_ARGUMENT, nothing
// written.
//
// covers: bigquery.tabledata.list
func TestBigQueryChangeDataCaptureArrowRefused(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	tbl := cdcTable(t, h, ds, "cdcarrow", true)
	raw, err := bqstorage.NewBigQueryWriteClient(ctx, storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()

	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "_CHANGE_TYPE", Type: arrow.BinaryTypes.String, Nullable: true},
	}, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	b.Field(0).(*array.Int64Builder).Append(1)
	b.Field(1).(*array.StringBuilder).Append("a")
	b.Field(2).(*array.StringBuilder).Append("UPSERT")
	rec := b.NewRecord()
	defer rec.Release()
	ser, batch := arrowMessagesOf(t, rec)

	ar, err := raw.AppendRows(ctx)
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	defer ar.CloseSend()
	if err := ar.Send(arrowAppend(writeTablePath(tbl)+"/streams/_default", ser, batch, nil)); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := ar.Recv()
	var st *status.Status
	if err != nil {
		st = status.Convert(err)
	} else if resp.GetError() != nil {
		st = status.FromProto(resp.GetError())
	}
	if st == nil || st.Code() != codes.InvalidArgument || !strings.Contains(st.Message(), "Apache Arrow") {
		t.Fatalf("Arrow rows with _CHANGE_TYPE: %v %v, want INVALID_ARGUMENT naming the Apache Arrow format", resp, err)
	}
	if got := cdcTableRows(t, h, c, "`"+project+"."+ds.DatasetID+".cdcarrow`"); len(got) != 0 {
		t.Errorf("the refused Arrow rows were written: %v", got)
	}
}

// TestBigQueryChangeDataCaptureTableLimits: "Primary keys cannot exceed 16
// columns" and "Tables with active CDC cannot have more than 2,000
// top-level columns defined by the table's schema". Through the managed
// writer, an UPSERT to a table whose key has 17 columns is
// INVALID_ARGUMENT, nothing written; a key of 16 columns is written, and so
// is a table of 1,990 columns, near the most the engine's tables hold (a
// table of more than 2,000 cannot be made, so that refusal is not
// reachable, #2008).
//
// covers: bigquery.tables.insert
func TestBigQueryChangeDataCaptureTableLimits(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	mw := managedWriter(t, h, project)

	// mk is a table of n INT64 columns c0..c(n-1), its key the first k.
	mk := func(name string, n, k int) *bigquery.Table {
		t.Helper()
		var schema bigquery.Schema
		var key []string
		for i := 0; i < n; i++ {
			col := fmt.Sprintf("c%d", i)
			schema = append(schema, &bigquery.FieldSchema{Name: col, Type: bigquery.IntegerFieldType, Required: i < k})
			if i < k {
				key = append(key, col)
			}
		}
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema,
			TableConstraints: &bigquery.TableConstraints{PrimaryKey: &bigquery.PrimaryKey{Columns: key}}}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return tbl
	}
	// upsert writes one UPSERT of the key columns' values (1..k).
	upsert := func(tbl *bigquery.Table, k int) error {
		t.Helper()
		m, dp := cdcSchema(t, h, tbl)
		msg := dynamicpb.NewMessage(m)
		for i := 0; i < k; i++ {
			msg.Set(m.Fields().ByName(protoreflect.Name(fmt.Sprintf("c%d", i))), protoreflect.ValueOfInt64(int64(i+1)))
		}
		msg.Set(m.Fields().ByName("_CHANGE_TYPE"), protoreflect.ValueOfString("UPSERT"))
		row, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, [][]byte{row})
		return err
	}
	count := func(tbl *bigquery.Table) int64 {
		t.Helper()
		it, err := c.Query("SELECT COUNT(*) FROM `" + project + "." + ds.DatasetID + "." + tbl.TableID + "`").Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			t.Fatal(err)
		}
		return row[0].(int64)
	}
	check := func(what string, tbl *bigquery.Table, k int, refuse bool, text string) {
		t.Helper()
		err := upsert(tbl, k)
		switch {
		case refuse && (status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), text)):
			t.Errorf("%s: %v, want INVALID_ARGUMENT saying %q", what, err, text)
		case !refuse && err != nil:
			t.Errorf("%s: %v", what, err)
		}
		want := int64(1)
		if refuse {
			want = 0
		}
		if n := count(tbl); n != want {
			t.Errorf("%s: %d rows, want %d", what, n, want)
		}
	}
	check("a key of 16 columns", mk("key16", 17, 16), 16, false, "")
	check("a key of 17 columns", mk("key17", 17, 17), 17, true, "up to 16 columns")
	// The engine's tables hold fewer than 2,000 columns (SQLite's 2,000
	// include the stream time and, in the change history table, the
	// change columns), so the 2,000-column refusal cannot be reached: a
	// table of 1,990 columns is written.
	check("1,990 columns", mk("cols1990", 1990, 1), 1, false, "")
}
