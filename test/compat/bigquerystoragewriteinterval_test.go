//go:build compat

package compat

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"cloud.google.com/go/bigquery/storage/managedwriter/adapt"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// INTERVAL values through the Storage Write API's proto_rows (#1424). The
// supported-data-types page gives an INTERVAL column the protocol buffer
// types string ("The string value must be an INTERVAL literal") and
// google.protobuf.Duration; the Go client's adapt package maps INTERVAL to
// a string, and to a Duration with adapt.WithProtoMapping (its own example
// is WithIntervalAsDuration).

const intervalWriteDDL = `(i INT64 NOT NULL, s INTERVAL, d INTERVAL, r ARRAY<INTERVAL>, rec STRUCT<x INTERVAL>)`

// intervalWriterSchema is the table's row message as adapt makes it from
// the table schema the Write API gives (GetWriteStream, FULL view): the
// pinned client's BQSchemaToStorageTableSchema fails on an INTERVAL ("unknown
// type value: INTERVAL"), and its StorageSchemaToProto2Descriptor maps
// INTERVAL to no type, so every INTERVAL field is given one with
// adapt.WithProtoMapping: opts, by default a string, and d, by path,
// google.protobuf.Duration. (Two path mappings to Duration fail in the
// pinned client: "file appears multiple times: google/protobuf/duration.proto".)
// The descriptor is normalized.
func intervalWriterSchema(t *testing.T, h *Harness, mw *managedwriter.Client, tbl *bigquery.Table,
	opts ...adapt.ProtoConversionOption) (protoreflect.MessageDescriptor, *descriptorpb.DescriptorProto) {
	t.Helper()
	ws, err := mw.GetWriteStream(h.Context(), &storagepb.GetWriteStreamRequest{Name: writeTablePath(tbl) + "/streams/_default",
		View: storagepb.WriteStreamView_FULL})
	if err != nil {
		t.Fatalf("GetWriteStream: %v", err)
	}
	if len(opts) == 0 {
		opts = []adapt.ProtoConversionOption{
			adapt.WithProtoMapping(adapt.ProtoMapping{FieldType: storagepb.TableFieldSchema_INTERVAL, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING}),
			adapt.WithProtoMapping(adapt.ProtoMapping{FieldPath: "d", TypeName: "google.protobuf.Duration", Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE}),
		}
	}
	d, err := adapt.StorageSchemaToProtoDescriptorWithOptions(ws.GetTableSchema(), "root", opts...)
	if err != nil {
		t.Fatal(err)
	}
	m := d.(protoreflect.MessageDescriptor)
	if k := m.Fields().ByName("d").Kind(); k != protoreflect.MessageKind {
		t.Fatalf("adapt made d a %s, want a google.protobuf.Duration", k)
	}
	dp, err := adapt.NormalizeDescriptor(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, dp
}

// intervalRow is a row of i, the string s, the Duration (secs, nanos) d,
// the strings r, the Durations rd (of ivd's table) and the string rec.x;
// an empty string or a nil leaves the field unset.
type intervalRow struct {
	i    int64
	s    string
	d    *[2]int64
	r    []string
	rd   [][2]int64
	recX string
}

func (ir intervalRow) bytes(t *testing.T, m protoreflect.MessageDescriptor) []byte {
	t.Helper()
	msg := dynamicpb.NewMessage(m)
	f := func(name string) protoreflect.FieldDescriptor { return m.Fields().ByName(protoreflect.Name(name)) }
	duration := func(fd protoreflect.FieldDescriptor, d [2]int64) protoreflect.Value {
		dm := dynamicpb.NewMessage(fd.Message())
		dm.Set(fd.Message().Fields().ByName("seconds"), protoreflect.ValueOfInt64(d[0]))
		dm.Set(fd.Message().Fields().ByName("nanos"), protoreflect.ValueOfInt32(int32(d[1])))
		return protoreflect.ValueOfMessage(dm)
	}
	msg.Set(f("i"), protoreflect.ValueOfInt64(ir.i))
	if ir.s != "" {
		msg.Set(f("s"), protoreflect.ValueOfString(ir.s))
	}
	if ir.d != nil {
		msg.Set(f("d"), duration(f("d"), *ir.d))
	}
	for _, s := range ir.r {
		msg.Mutable(f("r")).List().Append(protoreflect.ValueOfString(s))
	}
	for _, d := range ir.rd {
		msg.Mutable(f("rd")).List().Append(duration(f("rd"), d))
	}
	if ir.recX != "" {
		rec := dynamicpb.NewMessage(f("rec").Message())
		rec.Set(f("rec").Message().Fields().ByName("x"), protoreflect.ValueOfString(ir.recX))
		msg.Set(f("rec"), protoreflect.ValueOfMessage(rec))
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// intervalRows are the rows each stream writes: the canonical format and
// the literal syntax, Durations of a day and more, negative, and below a
// microsecond (cut), REPEATED and in a RECORD, and a row of NULLs. (No
// negative time part is under an hour or has a fraction: the Go client's
// IntervalValue reads -0:1:1 as positive and prints a negative fraction
// as ".-5".)
func intervalRows(t *testing.T, m protoreflect.MessageDescriptor, base int64) [][]byte {
	return [][]byte{
		intervalRow{i: base + 1, s: "1-2 3 4:5:6.789", d: &[2]int64{90061, 500_000_000}, r: []string{"0-0 0 0:0:1", "INTERVAL 5 DAY"},
			recX: "INTERVAL '1-2' YEAR TO MONTH"}.bytes(t, m),
		intervalRow{i: base + 2, s: "-10-0 -3 -4:5:6", d: &[2]int64{-3661, 0}, recX: "interval -25 month"}.bytes(t, m),
		intervalRow{i: base + 3}.bytes(t, m),
	}
}

// intervalRowsRead are those rows, as Table.Read gives them.
func intervalRowsRead(base int64) []string {
	n := func(k int64) string { return strconv.FormatInt(base+k, 10) }
	return []string{
		n(1) + "|1-2 3 4:5:6.789|0-0 0 25:1:1.5|[0-0 0 0:0:1 0-0 5 0:0:0]|[1-2 0 0:0:0]",
		n(2) + "|-10-0 -3 -4:5:6|0-0 0 -1:1:1|[]|[-2-1 0 0:0:0]",
		n(3) + "|<nil>|<nil>|[]|<nil>",
	}
}

func intervalTable(t *testing.T, h *Harness, c *bigquery.Client, ds *bigquery.Dataset) *bigquery.Table {
	t.Helper()
	if err := bqRun(h.Context(), c, "CREATE TABLE "+ds.DatasetID+".iv "+intervalWriteDDL, false); err != nil {
		t.Fatalf("create %s.iv: %v", ds.DatasetID, err)
	}
	return ds.Table("iv")
}

// writeIntervalsToDefaultStream: the managed writer's default stream
// writes intervalRows; Table.Read reads each value back as an INTERVAL.
// TestBigQueryStorageWriteDefaultStreamOfEveryType runs it.
func writeIntervalsToDefaultStream(t *testing.T, h *Harness, c *bigquery.Client, project string, ds *bigquery.Dataset) {
	tbl := intervalTable(t, h, c, ds)
	mw := managedWriter(t, h, project)
	m, dp := intervalWriterSchema(t, h, mw, tbl)
	ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	defer ms.Close()
	if _, err := appendResult(t, h, ms, intervalRows(t, m, 0)); err != nil {
		t.Fatalf("AppendRows of INTERVALs: %v", err)
	}
	if got, want := readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read"), intervalRowsRead(0); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read of the INTERVALs:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestBigQueryStorageWriteIntervals: COMMITTED, PENDING and BUFFERED
// streams write intervalRows, as strings and google.protobuf.Durations;
// the table holds them as INTERVALs: Table.Read (tabledata.list), a
// jobs.query that casts and computes with them, and the Storage Read API
// read each back as written. A row whose INTERVAL string is not an
// interval, or whose Duration is not valid or out of INTERVAL's range,
// fails the append with row_errors naming it, and nothing of that append
// is written. Measured first: a Duration was UNIMPLEMENTED, and a string
// that is not an interval was passed to the emulator.
//
// covers: bigquery.tabledata.list, bigquery.jobs.query
func TestBigQueryStorageWriteIntervals(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, _ := twoDatasets(t, h, c)
	tbl := intervalTable(t, h, c, ds)
	mw := managedWriter(t, h, project)
	m, dp := intervalWriterSchema(t, h, mw, tbl)
	stream := func(typ managedwriter.StreamType) *managedwriter.ManagedStream {
		ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
			managedwriter.WithType(typ), managedwriter.WithSchemaDescriptor(dp))
		if err != nil {
			t.Fatalf("NewManagedStream(%s): %v", typ, err)
		}
		t.Cleanup(func() { _ = ms.Close() })
		return ms
	}
	read := func() []string {
		return readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read")
	}

	committed := stream(managedwriter.CommittedStream)
	if off, err := appendResult(t, h, committed, intervalRows(t, m, 10), managedwriter.WithOffset(0)); err != nil || off != 0 {
		t.Fatalf("COMMITTED append: offset %d, %v", off, err)
	}
	pending := stream(managedwriter.PendingStream)
	if _, err := appendResult(t, h, pending, intervalRows(t, m, 20)); err != nil {
		t.Fatalf("PENDING append: %v", err)
	}
	buffered := stream(managedwriter.BufferedStream)
	if _, err := appendResult(t, h, buffered, intervalRows(t, m, 30)); err != nil {
		t.Fatalf("BUFFERED append: %v", err)
	}
	if got, want := read(), intervalRowsRead(10); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read before the commit and the flush:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := pending.Finalize(h.Context()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if resp, err := mw.BatchCommitWriteStreams(h.Context(), &storagepb.BatchCommitWriteStreamsRequest{
		Parent: writeTablePath(tbl), WriteStreams: []string{pending.StreamName()}}); err != nil || len(resp.GetStreamErrors()) != 0 {
		t.Fatalf("BatchCommitWriteStreams: %v, %v", resp, err)
	}
	if _, err := buffered.FlushRows(h.Context(), 2); err != nil {
		t.Fatalf("FlushRows: %v", err)
	}
	want := append(append(intervalRowsRead(10), intervalRowsRead(20)...), intervalRowsRead(30)...)
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// jobs.query: the values are INTERVALs.
	q := "SELECT i, CAST(s AS STRING), EXTRACT(HOUR FROM d), DATETIME '2024-01-01 00:00:00' + d, ARRAY_LENGTH(r), rec.x FROM " +
		tbl.DatasetID + ".iv WHERE i BETWEEN 11 AND 13"
	wantQ := []string{"11|1-2 3 4:5:6.789|25|2024-01-02T01:01:01.500000000|2|1-2 0 0:0:0", "12|-10-0 -3 -4:5:6|-1|2023-12-31T22:58:59|0|-2-1 0 0:0:0",
		"13|<nil>|<nil>|<nil>|0|<nil>"}
	if got := queryRows(t, h.Context(), c, project, "", q); !reflect.DeepEqual(got, wantQ) {
		t.Errorf("%s:\n%s\nwant\n%s", q, strings.Join(got, "\n"), strings.Join(wantQ, "\n"))
	}

	// The Storage Read API, through the Go client's accelerated Table.Read.
	sc, _ := bigqueryClient(t, h)
	if err := sc.EnableStorageReadClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))); err != nil {
		t.Fatalf("EnableStorageReadClient: %v", err)
	}
	it := sc.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context())
	if got := readRows(t, it, "Table.Read through the Storage Read API"); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read through the Storage Read API:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !it.IsAccelerated() {
		t.Error("Table.Read was not read through the Storage Read API")
	}

	// Values that are not INTERVALs: each its row's error.
	def := stream(managedwriter.DefaultStream)
	bad := [][]byte{
		intervalRow{i: 41, s: "1-2 3 4:5:6"}.bytes(t, m),
		intervalRow{i: 42, s: "not an interval"}.bytes(t, m),
		intervalRow{i: 43, d: &[2]int64{1, -5}}.bytes(t, m),
		intervalRow{i: 44, r: []string{"0-0 0 0:60:0"}}.bytes(t, m),
		intervalRow{i: 45, recX: "10001-0 0 0:0:0"}.bytes(t, m),
	}
	r, err := def.AppendRows(h.Context(), bad)
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	if _, err := r.GetResult(h.Context()); err == nil {
		t.Error("an append of values that are not INTERVALs succeeded")
	}
	full, _ := r.FullResponse(h.Context())
	var idx []int64
	for _, re := range full.GetRowErrors() {
		idx = append(idx, re.GetIndex())
		if re.GetCode() != storagepb.RowError_FIELDS_ERROR {
			t.Errorf("row error %d: %s, want FIELDS_ERROR", re.GetIndex(), re.GetCode())
		}
	}
	if !reflect.DeepEqual(idx, []int64{1, 2, 3, 4}) {
		t.Errorf("row_errors of the rows: %v (%v), want rows 1 to 4", idx, full.GetRowErrors())
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read after the refused append:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Every INTERVAL a Duration, as the client's documented
	// WithIntervalAsDuration maps them, REPEATED among them.
	if err := bqRun(h.Context(), c, "CREATE TABLE "+ds.DatasetID+".ivd (i INT64, d INTERVAL, rd ARRAY<INTERVAL>)", false); err != nil {
		t.Fatalf("create ivd: %v", err)
	}
	dtbl := ds.Table("ivd")
	dm, ddp := intervalWriterSchema(t, h, mw, dtbl, adapt.WithProtoMapping(adapt.ProtoMapping{FieldType: storagepb.TableFieldSchema_INTERVAL,
		TypeName: "google.protobuf.Duration", Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE}))
	dms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(dtbl)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(ddp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	defer dms.Close()
	if _, err := appendResult(t, h, dms, [][]byte{
		intervalRow{i: 1, d: &[2]int64{0, 0}, rd: [][2]int64{{-3600, 0}, {0, 1500}, {86400 * 400, 0}}}.bytes(t, dm),
		intervalRow{i: 2}.bytes(t, dm),
	}); err != nil {
		t.Fatalf("append of Durations: %v", err)
	}
	r, err = dms.AppendRows(h.Context(), [][]byte{intervalRow{i: 3, rd: [][2]int64{{0, 1_000_000_000}}}.bytes(t, dm)})
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	if full, _ := r.FullResponse(h.Context()); len(full.GetRowErrors()) != 1 {
		t.Errorf("a Duration of a second's nanos: %v, want a row error", full)
	}
	wantD := []string{"1|0-0 0 0:0:0|[0-0 0 -1:0:0 0-0 0 0:0:0.000001 0-0 0 9600:0:0]", "2|<nil>|[]"}
	if got := readRows(t, dtbl.Read(h.Context()), "Table.Read"); !reflect.DeepEqual(got, wantD) {
		t.Errorf("Table.Read of the Durations: %q, want %q", got, wantD)
	}
}
