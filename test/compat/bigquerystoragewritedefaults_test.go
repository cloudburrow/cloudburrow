//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"cloud.google.com/go/civil"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The Storage Write API's missing values and RANGE columns (#1391).

// TestBigQueryStorageWriteMissingValueDefaults: with the managed
// writer's WithDefaultMissingValueInterpretation(DEFAULT_VALUE), a field
// of the writer schema a row leaves unset is written the column's
// default ("Missing value is interpreted as column default value if
// declared in the table schema, NULL otherwise", storage.proto);
// WithMissingValueInterpretations names columns one by one, NULL_VALUE
// overriding the default for its column; with neither the field is NULL.
// A value given is kept. A REQUIRED column with a default may be left
// unset. In arrow_rows, a NULL with DEFAULT_VALUE is written the default.
//
// covers: bigquery.tabledata.list
func TestBigQueryStorageWriteMissingValueDefaults(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, _ := twoDatasets(t, h, c)
	tbl := ds.Table("dv")
	if err := tbl.Create(h.Context(), &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "i", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "s", Type: bigquery.StringFieldType, DefaultValueExpression: "'dflt'"},
		{Name: "n", Type: bigquery.IntegerFieldType, Required: true, DefaultValueExpression: "42"},
		{Name: "z", Type: bigquery.StringFieldType},
	}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	m, dp := writerSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	row := func(i int64, s string) []byte {
		msg := dynamicpb.NewMessage(m)
		msg.Set(m.Fields().ByName("i"), protoreflect.ValueOfInt64(i))
		if s != "" {
			msg.Set(m.Fields().ByName("s"), protoreflect.ValueOfString(s))
		}
		// n is a required field of the proto2 schema adapt makes.
		b, err := proto.MarshalOptions{AllowPartial: true}.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	write := func(what string, opts []managedwriter.WriterOption, rows ...[]byte) error {
		t.Helper()
		ms, err := mw.NewManagedStream(h.Context(), append([]managedwriter.WriterOption{managedwriter.WithDestinationTable(writeTablePath(tbl)),
			managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp)}, opts...)...)
		if err != nil {
			t.Fatalf("%s: NewManagedStream: %v", what, err)
		}
		defer ms.Close()
		_, err = appendResult(t, h, ms, rows)
		return err
	}
	dv := storagepb.AppendRowsRequest_DEFAULT_VALUE
	nv := storagepb.AppendRowsRequest_NULL_VALUE
	if err := write("DEFAULT_VALUE", []managedwriter.WriterOption{managedwriter.WithDefaultMissingValueInterpretation(dv)},
		row(1, ""), row(2, "given")); err != nil {
		t.Fatalf("default_missing_value_interpretation DEFAULT_VALUE: %v", err)
	}
	if err := write("the map", []managedwriter.WriterOption{managedwriter.WithDefaultMissingValueInterpretation(dv),
		managedwriter.WithMissingValueInterpretations(map[string]storagepb.AppendRowsRequest_MissingValueInterpretation{"s": nv})},
		row(3, "")); err != nil {
		t.Fatalf("missing_value_interpretations s NULL_VALUE: %v", err)
	}
	if err := write("one column", []managedwriter.WriterOption{
		managedwriter.WithMissingValueInterpretations(map[string]storagepb.AppendRowsRequest_MissingValueInterpretation{"s": dv, "n": dv})},
		row(4, "")); err != nil {
		t.Fatalf("missing_value_interpretations DEFAULT_VALUE: %v", err)
	}
	// With none, n is NULL, which its NOT NULL refuses: a row error.
	if err := write("none", nil, row(5, "")); err == nil {
		t.Errorf("a NULL of a REQUIRED column, no interpretation: no error")
	}
	// arrow_rows: a NULL is the missing value; n, which the Arrow schema
	// lacks, is written its default either way.
	b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int64}, {Name: "s", Type: arrow.BinaryTypes.String, Nullable: true}}, nil))
	b.Field(0).(*array.Int64Builder).Append(6)
	b.Field(1).(*array.StringBuilder).AppendNull()
	schema, batch := arrowMessagesOf(t, b.NewRecord())
	raw, err := bqstorage.NewBigQueryWriteClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()
	ar, err := raw.AppendRows(h.Context())
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	defer ar.CloseSend()
	req := arrowAppend(writeTablePath(tbl)+"/streams/_default", schema, batch, nil)
	req.DefaultMissingValueInterpretation = dv
	if err := ar.Send(req); err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp, err := ar.Recv(); err != nil || resp.GetError() != nil {
		t.Fatalf("Arrow append with DEFAULT_VALUE: %v, %v", resp, err)
	}
	got := readRows(t, tbl.Read(h.Context()), "Table.Read")
	want := []string{"1|dflt|42|<nil>", "2|given|42|<nil>", "3|<nil>|42|<nil>", "4|dflt|42|<nil>", "6|dflt|42|<nil>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestBigQueryStorageWriteRanges: RANGE<DATE>, RANGE<DATETIME> and
// RANGE<TIMESTAMP> columns written by the managed writer in proto_rows (a
// message of start and end, as adapt makes it) and by the raw client in
// arrow_rows (a Struct of start and end); a bound left unset is
// UNBOUNDED, a RANGE left unset NULL; Table.Read reads each back.
//
// covers: bigquery.tabledata.list
func TestBigQueryStorageWriteRanges(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, _ := twoDatasets(t, h, c)
	if err := bqRun(h.Context(), c, "CREATE TABLE "+ds.DatasetID+".rg (i INT64, d RANGE<DATE>, dt RANGE<DATETIME>, ts RANGE<TIMESTAMP>)", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	tbl := ds.Table("rg")
	m, dp := writerSchema(t, h, tbl)
	msg := dynamicpb.NewMessage(m)
	msg.Set(m.Fields().ByName("i"), protoreflect.ValueOfInt64(1))
	set := func(msg *dynamicpb.Message, name string, start, end any) {
		f := m.Fields().ByName(protoreflect.Name(name))
		r := dynamicpb.NewMessage(f.Message())
		if start != nil {
			r.Set(f.Message().Fields().ByName("start"), protoreflect.ValueOf(start))
		}
		if end != nil {
			r.Set(f.Message().Fields().ByName("end"), protoreflect.ValueOf(end))
		}
		msg.Set(f, protoreflect.ValueOfMessage(r))
	}
	set(msg, "d", int32(18262), int32(18293))                        // [2020-01-01, 2020-02-01)
	set(msg, "dt", packedDateTime(2020, 1, 1, 3, 4, 5, 250000), nil) // [2020-01-01T03:04:05.25, UNBOUNDED)
	set(msg, "ts", int64(1577836800000000), int64(1577923200500000)) // [2020-01-01, 2020-01-02 00:00:00.5) UTC
	full, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	empty := dynamicpb.NewMessage(m)
	empty.Set(m.Fields().ByName("i"), protoreflect.ValueOfInt64(2))
	set(empty, "d", nil, nil) // [UNBOUNDED, UNBOUNDED)
	nulls, err := proto.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	mw := managedWriter(t, h, project)
	ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	defer ms.Close()
	if _, err := appendResult(t, h, ms, [][]byte{full, nulls}); err != nil {
		t.Fatalf("AppendRows of RANGEs: %v", err)
	}

	// The same rows in arrow_rows, i 3 and 4.
	st := func(elem arrow.DataType) arrow.DataType {
		return arrow.StructOf(arrow.Field{Name: "start", Type: elem, Nullable: true}, arrow.Field{Name: "end", Type: elem, Nullable: true})
	}
	dtType := &arrow.TimestampType{Unit: arrow.Microsecond}
	tsType := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
	b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int64},
		{Name: "d", Type: st(arrow.FixedWidthTypes.Date32), Nullable: true},
		{Name: "dt", Type: st(dtType), Nullable: true},
		{Name: "ts", Type: st(tsType), Nullable: true},
	}, nil))
	b.Field(0).(*array.Int64Builder).AppendValues([]int64{3, 4}, nil)
	d := b.Field(1).(*array.StructBuilder)
	d.Append(true)
	d.FieldBuilder(0).(*array.Date32Builder).Append(18262)
	d.FieldBuilder(1).(*array.Date32Builder).Append(18293)
	d.Append(true)
	d.FieldBuilder(0).(*array.Date32Builder).AppendNull()
	d.FieldBuilder(1).(*array.Date32Builder).AppendNull()
	dtb := b.Field(2).(*array.StructBuilder)
	dtb.Append(true)
	dtb.FieldBuilder(0).(*array.TimestampBuilder).Append(arrow.Timestamp(1577847845250000)) // 2020-01-01 03:04:05.25
	dtb.FieldBuilder(1).(*array.TimestampBuilder).AppendNull()
	dtb.AppendNull()
	dtb.FieldBuilder(0).(*array.TimestampBuilder).AppendNull()
	dtb.FieldBuilder(1).(*array.TimestampBuilder).AppendNull()
	tsb := b.Field(3).(*array.StructBuilder)
	tsb.Append(true)
	tsb.FieldBuilder(0).(*array.TimestampBuilder).Append(arrow.Timestamp(1577836800000000))
	tsb.FieldBuilder(1).(*array.TimestampBuilder).Append(arrow.Timestamp(1577923200500000))
	tsb.AppendNull()
	tsb.FieldBuilder(0).(*array.TimestampBuilder).AppendNull()
	tsb.FieldBuilder(1).(*array.TimestampBuilder).AppendNull()
	schema, batch := arrowMessagesOf(t, b.NewRecord())
	raw, err := bqstorage.NewBigQueryWriteClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()
	ar, err := raw.AppendRows(h.Context())
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	defer ar.CloseSend()
	if err := ar.Send(arrowAppend(writeTablePath(tbl)+"/streams/_default", schema, batch, nil)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp, err := ar.Recv(); err != nil || resp.GetError() != nil {
		t.Fatalf("append of Arrow RANGEs: %v, %v", resp, err)
	}

	rows := map[int64][]bigquery.Value{}
	it := tbl.Read(h.Context())
	for {
		var row []bigquery.Value
		if err := it.Next(&row); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatalf("Table.Read: %v", err)
		}
		rows[row[0].(int64)] = row[1:]
	}
	jan1, feb1 := civil.Date{Year: 2020, Month: 1, Day: 1}, civil.Date{Year: 2020, Month: 2, Day: 1}
	dt := civil.DateTime{Date: jan1, Time: civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 250000000}}
	tsStart, tsEnd := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 1, 2, 0, 0, 0, 500000000, time.UTC)
	wantFull := []bigquery.Value{&bigquery.RangeValue{Start: jan1, End: feb1}, &bigquery.RangeValue{Start: dt},
		&bigquery.RangeValue{Start: tsStart, End: tsEnd}}
	wantNulls := []bigquery.Value{&bigquery.RangeValue{}, nil, nil}
	for _, i := range []int64{1, 3} {
		if !reflect.DeepEqual(rows[i], wantFull) {
			t.Errorf("row %d: %v, want %v", i, fmtRanges(rows[i]), fmtRanges(wantFull))
		}
	}
	for _, i := range []int64{2, 4} {
		if !reflect.DeepEqual(rows[i], wantNulls) {
			t.Errorf("row %d: %v, want %v", i, fmtRanges(rows[i]), fmtRanges(wantNulls))
		}
	}
}
