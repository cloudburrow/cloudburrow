package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"github.com/apache/arrow/go/v15/arrow/memory"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// fakeRows is the emulator's REST API for the Storage Read front: each
// jobs.query is recorded and answered with the rows of the table it names
// (FROM `ds.t`), as the pinned emulator writes them, or 404.
type fakeRows struct {
	mu      sync.Mutex
	rows    map[string]string // "ds.t" -> the "rows" JSON
	queries []string
}

func (f *fakeRows) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if parts := strings.Split(r.URL.Path, "/"); r.Method == http.MethodGet && len(parts) == 9 && parts[5] == "datasets" && parts[7] == "tables" {
		// tables.get, which CreateReadSession checks the table with.
		if _, ok := f.rows[parts[6]+"."+parts[8]]; ok && parts[4] == "p-1" {
			writeJSON(w, 200, map[string]any{"id": parts[6] + "." + parts[8]})
			return
		}
		writeError(w, 404, "notFound", "Not found: Table "+parts[6]+"."+parts[8])
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/bigquery/v2/projects/p-1/queries" {
		writeError(w, 404, "notFound", "not found: "+r.URL.Path)
		return
	}
	var q struct {
		Query         string          `json:"query"`
		FormatOptions map[string]bool `json:"formatOptions"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &q)
	f.queries = append(f.queries, q.Query)
	if !q.FormatOptions["useInt64Timestamp"] {
		writeError(w, 400, "invalid", "no useInt64Timestamp")
		return
	}
	for name, rows := range f.rows {
		if strings.Contains(q.Query, " FROM `"+name+"`") {
			writeJSON(w, 200, map[string]any{"jobReference": map[string]string{"projectId": "p-1", "jobId": fmt.Sprint("job", len(f.queries))},
				"rows": json.RawMessage(rows), "jobComplete": true})
			return
		}
	}
	writeError(w, 404, "notFound", "Not found: Table")
}

// fakeReadServer is the emulator's Storage Read API: CreateReadSession
// answers as the pinned emulator does (one stream; the Arrow schema a
// whole IPC stream, the Avro schema named by the project and dataset);
// ReadRows must never be called (storageread.go).
type fakeReadServer struct {
	storagepb.UnimplementedBigQueryReadServer
	arrow    []byte
	avro     string
	mu       sync.Mutex
	sessions int
	reads    int
}

func (s *fakeReadServer) CreateReadSession(_ context.Context, req *storagepb.CreateReadSessionRequest) (*storagepb.ReadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions++
	table := req.GetReadSession().GetTable()
	sess := &storagepb.ReadSession{Name: "sess", Table: table, DataFormat: req.GetReadSession().GetDataFormat(),
		Streams: []*storagepb.ReadStream{{Name: fmt.Sprintf("%s/streams/%d", table, s.sessions)}}}
	if sess.DataFormat == storagepb.DataFormat_AVRO {
		sess.Schema = &storagepb.ReadSession_AvroSchema{AvroSchema: &storagepb.AvroSchema{Schema: s.avro}}
	} else {
		sess.Schema = &storagepb.ReadSession_ArrowSchema{ArrowSchema: &storagepb.ArrowSchema{SerializedSchema: s.arrow}}
	}
	return sess, nil
}

func (s *fakeReadServer) ReadRows(*storagepb.ReadRowsRequest, storagepb.BigQueryRead_ReadRowsServer) error {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	return status.Error(codes.Internal, "the emulator's ReadRows was called")
}

// emulatorArrowSchema is the Arrow schema the emulator writes for the
// table of testRows (types/arrow.go, TableToARROW), as it writes it: a
// whole IPC stream, with an empty batch.
func emulatorArrowSchema(t *testing.T) []byte {
	t.Helper()
	datetime := arrow.MetadataFrom(map[string]string{"ARROW:extension:name": "google:sqlType:datetime"})
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int64},
		{Name: "f", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "n", Type: &arrow.Decimal128Type{Precision: 38, Scale: 9}, Nullable: true},
		{Name: "bn", Type: &arrow.Decimal256Type{Precision: 76, Scale: 38}, Nullable: true},
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
		{Name: "s", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "y", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "d", Type: arrow.PrimitiveTypes.Date32, Nullable: true},
		{Name: "dt", Type: arrow.FixedWidthTypes.Timestamp_us, Nullable: true, Metadata: datetime},
		{Name: "tm", Type: arrow.FixedWidthTypes.Time64us, Nullable: true},
		{Name: "ts", Type: arrow.FixedWidthTypes.Timestamp_us, Nullable: true},
		{Name: "rec", Nullable: true, Type: arrow.StructOf(
			arrow.Field{Name: "a", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
			arrow.Field{Name: "ts", Type: arrow.FixedWidthTypes.Timestamp_us, Nullable: true},
			arrow.Field{Name: "r", Type: arrow.ListOfField(arrow.Field{Name: "r", Type: arrow.BinaryTypes.String})})},
		{Name: "r", Type: arrow.ListOfField(arrow.Field{Name: "r", Type: arrow.PrimitiveTypes.Int64})},
		{Name: "rr", Type: arrow.ListOfField(arrow.Field{Name: "rr", Type: arrow.StructOf(
			arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
			arrow.Field{Name: "y", Type: arrow.BinaryTypes.String, Nullable: true})})},
	}, nil)
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema))
	b := array.NewRecordBuilder(memory.NewGoAllocator(), schema)
	defer b.Release()
	rec := b.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emulatorAvroSchema is the Avro schema the emulator writes for the table
// of testRows (types/avro.go), measured against the pinned image.
const emulatorAvroSchema = `{"namespace":"p-1.ds","name":"t","type":"record","fields":[` +
	`{"type":"long","name":"i"},{"type":["null","double"],"name":"f"},` +
	`{"type":["null",{"logicalType":"decimal","precision":38,"scale":9,"type":"bytes"}],"name":"n"},` +
	`{"type":["null",{"logicalType":"decimal","precision":77,"scale":38,"type":"bytes"}],"name":"bn"},` +
	`{"type":["null","boolean"],"name":"b"},{"type":["null","string"],"name":"s"},{"type":["null","bytes"],"name":"y"},` +
	`{"type":["null",{"logicalType":"date","type":"int"}],"name":"d"},` +
	`{"type":["null",{"logicalType":"datetime","type":"string"}],"name":"dt"},` +
	`{"type":["null",{"logicalType":"time-micros","type":"long"}],"name":"tm"},` +
	`{"type":["null",{"logicalType":"timestamp-micros","type":"long"}],"name":"ts"},` +
	`{"type":["null",{"fields":[{"name":"a","type":["null","long"]},{"name":"ts","type":["null",{"logicalType":"timestamp-micros","type":"long"}]},` +
	`{"name":"r","type":{"items":"string","type":"array"}}],"name":"rec","type":"record"}],"name":"rec"},` +
	`{"type":{"items":"long","type":"array"},"name":"r"},` +
	`{"type":{"items":{"fields":[{"name":"x","type":["null","long"]},{"name":"y","type":["null","string"]}],"name":"rr","type":"record"},"type":"array"},"name":"rr"}]}`

// testRows are the rows of ds.t as the pinned emulator's jobs.query gives
// them with useInt64Timestamp (measured): a TIMESTAMP in a RECORD or a
// REPEATED value is the engine's text.
const testRows = `[
 {"f":[{"v":"1"},{"v":"1.5"},{"v":"0.123456789"},{"v":"-2.5"},{"v":"true"},{"v":"x"},{"v":"AP9hYg=="},{"v":"2024-01-02"},
  {"v":"2024-01-02T03:04:05.123456"},{"v":"03:04:05.5"},{"v":"1704164645123456"},
  {"v":{"f":[{"v":"3"},{"v":"2020-01-01 00:00:00+00"},{"v":[{"v":"p"},{"v":"q"}]}]}},
  {"v":[{"v":"7"},{"v":"8"}]},{"v":[{"v":{"f":[{"v":"1"},{"v":"a"}]}}]}]},
 {"f":[{"v":"2"},{"v":"+Inf"},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},
  {"v":null},{"v":[{"v":"9"}]},{"v":[]}]}
]`

// storageReadFront starts the front on a fake emulator; it returns a
// client of the front and the fakes.
func storageReadFront(t *testing.T, ctx context.Context, records *jobRecords) (storagepb.BigQueryReadClient, *fakeReadServer, *fakeRows) {
	t.Helper()
	rest := &fakeRows{rows: map[string]string{"ds.t": testRows}}
	emu := &fakeReadServer{arrow: emulatorArrowSchema(t), avro: emulatorAvroSchema}
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	storagepb.RegisterBigQueryReadServer(gs, emu)
	go func() { _ = gs.Serve(up) }()
	t.Cleanup(gs.Stop)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	go func() { served <- serveStorageRead(ctx, l, up.Addr().String(), rest, records, nil, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serveStorageRead: %v", err)
		}
	})
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return storagepb.NewBigQueryReadClient(conn), emu, rest
}

// readAll reads a stream from offset: its answers, or the error.
func readAll(ctx context.Context, c storagepb.BigQueryReadClient, stream string, offset int64) ([]*storagepb.ReadRowsResponse, error) {
	rs, err := c.ReadRows(ctx, &storagepb.ReadRowsRequest{ReadStream: stream, Offset: offset})
	if err != nil {
		return nil, err
	}
	var out []*storagepb.ReadRowsResponse
	for {
		resp, err := rs.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, resp)
	}
}

// arrowValues reads the batch after the schema, as BigQuery frames them,
// and returns each row's values as arrow prints them, joined by "|".
func arrowValues(t *testing.T, schema, batch []byte) []string {
	t.Helper()
	r, err := ipc.NewReader(io.MultiReader(bytes.NewReader(schema), bytes.NewReader(batch)))
	if err != nil {
		t.Fatalf("read the Arrow rows: %v", err)
	}
	defer r.Release()
	var out []string
	for r.Next() {
		rec := r.Record()
		for i := 0; i < int(rec.NumRows()); i++ {
			var vals []string
			for j := 0; j < int(rec.NumCols()); j++ {
				vals = append(vals, arrowValue(rec.Column(j), i))
			}
			out = append(out, strings.Join(vals, "|"))
		}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read the Arrow rows: %v", err)
	}
	return out
}

// arrowValue is the value at i of col as arrow prints it, but a decimal's
// exactly (arrow prints it through a float).
func arrowValue(col arrow.Array, i int) string {
	var n *big.Int
	var scale int32
	switch c := col.(type) {
	case *array.Decimal128:
		n, scale = c.Value(i).BigInt(), c.DataType().(*arrow.Decimal128Type).Scale
	case *array.Decimal256:
		n, scale = c.Value(i).BigInt(), c.DataType().(*arrow.Decimal256Type).Scale
	default:
		return col.ValueStr(i)
	}
	if col.IsNull(i) {
		return col.ValueStr(i)
	}
	r := new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	return strings.TrimSuffix(strings.TrimRight(r.FloatString(int(scale)), "0"), ".")
}

// TestStorageReadRowsAreWrittenByTheFront (#1095, #1098): ReadRows is
// answered by the front, from a query of the table's whole name, never by
// the emulator (whose ReadRows panics on a REPEATED column in Arrow and
// fails an Avro schema named by a project with a hyphen): in Arrow, a
// REPEATED column as a list and a RECORD as a struct, at any depth, every
// type read from the REST API's text; in Avro, with valid names, decoded
// by goavro. The schema is sent in the first answer only; offset skips
// rows; the query is left out of jobs.list.
func TestStorageReadRowsAreWrittenByTheFront(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	records := &jobRecords{}
	c, emu, rest := storageReadFront(t, ctx, records)
	session := func(format storagepb.DataFormat, restriction string) *storagepb.ReadSession {
		t.Helper()
		s, err := c.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p-1",
			ReadSession: &storagepb.ReadSession{Table: "projects/p-1/datasets/ds/tables/t", DataFormat: format,
				ReadOptions: &storagepb.ReadSession_TableReadOptions{RowRestriction: restriction}}})
		if err != nil || len(s.GetStreams()) != 1 {
			t.Fatalf("CreateReadSession: %v %v", s, err)
		}
		return s
	}

	// Arrow.
	s := session(storagepb.DataFormat_ARROW, "i > 0")
	schema := s.GetArrowSchema().GetSerializedSchema()
	if msgs, _, ok := arrowMessages(schema); !ok || len(msgs) != 1 {
		t.Fatalf("the session's Arrow schema is %d messages, want the schema's alone", len(msgs))
	}
	sr, err := ipc.NewReader(bytes.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dt", "ts"} {
		f, _ := sr.Schema().FieldsByName(name)
		zone := f[0].Type.(*arrow.TimestampType).TimeZone
		if name == "dt" && zone != "" || name == "ts" && zone != "UTC" {
			t.Errorf("the session's %s is a timestamp of the zone %q", name, zone)
		}
	}
	sr.Release()
	resps, err := readAll(ctx, c, s.Streams[0].Name, 0)
	if err != nil || len(resps) != 1 {
		t.Fatalf("ReadRows: %d answers, %v", len(resps), err)
	}
	want := []string{
		`1|1.5|0.123456789|-2.5|true|x|AP9hYg==|2024-01-02|2024-01-02 03:04:05.123456Z|03:04:05.500000|2024-01-02 03:04:05.123456Z|` +
			`{"a":3,"r":["p","q"],"ts":"2020-01-01 00:00:00"}|[7,8]|[{"x":1,"y":"a"}]`,
		`2|+Inf|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|[9]|[]`,
	}
	got := arrowValues(t, schema, resps[0].GetArrowRecordBatch().GetSerializedRecordBatch())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the Arrow rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if resps[0].GetRowCount() != 2 || resps[0].GetArrowRecordBatch().GetRowCount() != 2 ||
		!bytes.Equal(resps[0].GetArrowSchema().GetSerializedSchema(), schema) {
		t.Errorf("the answer: %d rows, schema sent %v", resps[0].GetRowCount(), resps[0].GetArrowSchema() != nil)
	}
	if want := "SELECT `i`, `f`, `n`, `bn`, `b`, `s`, `y`, `d`, `dt`, `tm`, `ts`, `rec`, `r`, `rr` FROM `ds.t` WHERE (i > 0)"; rest.queries[0] != want {
		t.Errorf("the query: %s\nwant %s", rest.queries[0], want)
	}
	if !records.isInternal("p-1", "job1") {
		t.Error("the front's query is not left out of jobs.list")
	}

	// Offset.
	resps, err = readAll(ctx, c, s.Streams[0].Name, 1)
	if err != nil || len(resps) != 1 || resps[0].GetRowCount() != 1 {
		t.Fatalf("ReadRows from 1: %v %v", resps, err)
	}
	if got := arrowValues(t, schema, resps[0].GetArrowRecordBatch().GetSerializedRecordBatch()); len(got) != 1 || got[0] != want[1] {
		t.Errorf("from 1: %q", got)
	}
	if _, err := readAll(ctx, c, s.Streams[0].Name, 3); status.Code(err) != codes.OutOfRange {
		t.Errorf("from 3 of 2 rows: %v, want OUT_OF_RANGE", err)
	}

	// Avro.
	s = session(storagepb.DataFormat_AVRO, "")
	avro := s.GetAvroSchema().GetSchema()
	codec, err := goavro.NewCodec(avro)
	if err != nil {
		t.Fatalf("goavro refuses the session's schema %s: %v", avro, err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(avro), &m)
	if m["namespace"] != "p_1.ds" || m["name"] != "t" {
		t.Errorf("the Avro schema is named %v.%v", m["namespace"], m["name"])
	}
	resps, err = readAll(ctx, c, s.Streams[0].Name, 0)
	if err != nil || len(resps) != 1 || resps[0].GetAvroSchema().GetSchema() != avro {
		t.Fatalf("ReadRows in Avro: %v %v", resps, err)
	}
	var rows []map[string]any
	buf := resps[0].GetAvroRows().GetSerializedBinaryRows()
	for len(buf) > 0 {
		v, rest, err := codec.NativeFromBinary(buf)
		if err != nil {
			t.Fatalf("decode the Avro rows: %v", err)
		}
		rows = append(rows, v.(map[string]any))
		buf = rest
	}
	if len(rows) != 2 {
		t.Fatalf("%d Avro rows, want 2", len(rows))
	}
	r := rows[0]
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	for k, want := range map[string]any{
		"i": int64(1), "f": 1.5, "b": true, "s": "x", "y": "\x00\xffab", "dt": "2024-01-02T03:04:05.123456",
		"tm": 3*time.Hour + 4*time.Minute + 5500*time.Millisecond,
	} {
		v := r[k]
		if u, ok := v.(map[string]any); ok {
			for _, x := range u {
				v = x
			}
		}
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		if v != want {
			t.Errorf("Avro %s: %#v, want %#v", k, v, want)
		}
	}
	if n := r["n"].(map[string]any)["bytes.decimal"].(*big.Rat); n.Cmp(big.NewRat(123456789, 1e9)) != 0 {
		t.Errorf("Avro n: %v", n)
	}
	if n := r["bn"].(map[string]any)["bytes.decimal"].(*big.Rat); n.Cmp(big.NewRat(-5, 2)) != 0 {
		t.Errorf("Avro bn: %v", n)
	}
	if d := r["d"].(map[string]any)["int.date"].(time.Time); !d.Equal(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Avro d: %v", d)
	}
	if got := r["ts"].(map[string]any)["long.timestamp-micros"].(time.Time); !got.Equal(ts) {
		t.Errorf("Avro ts: %v", got)
	}
	rec := r["rec"].(map[string]any)["p_1.ds.rec"].(map[string]any)
	if rec["a"].(map[string]any)["long"] != int64(3) || fmt.Sprint(rec["r"]) != "[p q]" ||
		!rec["ts"].(map[string]any)["long.timestamp-micros"].(time.Time).Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Avro rec: %v", rec)
	}
	if fmt.Sprint(r["r"]) != "[7 8]" || fmt.Sprint(rows[1]["r"]) != "[9]" || fmt.Sprint(rows[1]["rr"]) != "[]" {
		t.Errorf("Avro r: %v, %v; rr of row 2: %v", r["r"], rows[1]["r"], rows[1]["rr"])
	}
	if rr := r["rr"].([]any); len(rr) != 1 || fmt.Sprint(rr[0]) != "map[x:map[long:1] y:map[string:a]]" {
		t.Errorf("Avro rr: %v", r["rr"])
	}
	if rows[1]["f"].(map[string]any)["double"].(float64) <= 1e308 || rows[1]["s"] != nil {
		t.Errorf("Avro row 2: %v", rows[1])
	}

	if emu.reads != 0 {
		t.Errorf("the emulator's ReadRows was called %d times", emu.reads)
	}
}

// TestStorageReadRowsInBatches: a read of more than readRowsBatch rows is
// sent in several answers, the schema in the first alone; a read of no
// rows is one answer with none.
func TestStorageReadRowsInBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _, rest := storageReadFront(t, ctx, nil)
	row := `{"f":[{"v":"1"},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":null},{"v":[]},{"v":[]}]}`
	rest.rows["ds.big"] = "[" + strings.TrimSuffix(strings.Repeat(row+",", readRowsBatch+5), ",") + "]"
	rest.rows["ds.none"] = "[]"
	for _, tc := range []struct {
		table   string
		answers []int64
	}{{"big", []int64{readRowsBatch, 5}}, {"none", []int64{0}}} {
		for _, format := range []storagepb.DataFormat{storagepb.DataFormat_ARROW, storagepb.DataFormat_AVRO} {
			s, err := c.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p-1",
				ReadSession: &storagepb.ReadSession{Table: "projects/p-1/datasets/ds/tables/" + tc.table, DataFormat: format}})
			if err != nil {
				t.Fatal(err)
			}
			resps, err := readAll(ctx, c, s.Streams[0].Name, 0)
			if err != nil || len(resps) != len(tc.answers) {
				t.Fatalf("%s in %s: %d answers, %v; want %d", tc.table, format, len(resps), err, len(tc.answers))
			}
			for i, resp := range resps {
				if resp.GetRowCount() != tc.answers[i] || (resp.GetSchema() != nil) != (i == 0) {
					t.Errorf("%s in %s, answer %d: %d rows, schema %v", tc.table, format, i, resp.GetRowCount(), resp.GetSchema() != nil)
				}
				if format == storagepb.DataFormat_ARROW {
					if got := arrowValues(t, s.GetArrowSchema().GetSerializedSchema(), resp.GetArrowRecordBatch().GetSerializedRecordBatch()); int64(len(got)) != tc.answers[i] {
						t.Errorf("%s answer %d: %d Arrow rows", tc.table, i, len(got))
					}
				}
			}
		}
	}
}

// TestStorageReadRefusesWhatWouldCrashTheEmulator: a CreateReadSession the
// emulator's handler would panic on (no read_session) or cannot serve is
// INVALID_ARGUMENT before it is sent; a stream the front did not see made
// is UNIMPLEMENTED; a table gone is NOT_FOUND; another method is passed
// through, with the emulator's answer.
func TestStorageReadRefusesWhatWouldCrashTheEmulator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, emu, rest := storageReadFront(t, ctx, nil)
	for name, req := range map[string]*storagepb.CreateReadSessionRequest{
		"no read_session": {Parent: "projects/p-1"},
		"no table":        {Parent: "projects/p-1", ReadSession: &storagepb.ReadSession{DataFormat: storagepb.DataFormat_ARROW}},
		"no format":       {Parent: "projects/p-1", ReadSession: &storagepb.ReadSession{Table: "projects/p-1/datasets/ds/tables/t"}},
	} {
		if _, err := c.CreateReadSession(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if emu.sessions != 0 {
		t.Errorf("the emulator was sent %d of them", emu.sessions)
	}

	_, err := readAll(ctx, c, "projects/p-1/locations/l/sessions/x/streams/y", 0)
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "did not see made") {
		t.Errorf("ReadRows of an unknown stream: %v, want UNIMPLEMENTED", err)
	}

	// A table the REST API does not find, in a project the emulator has
	// or not, is NOT_FOUND before the emulator sees it (#1102): the
	// emulator's handler panicked on a project it does not have.
	for _, table := range []string{"projects/p-1/datasets/ds/tables/gone", "projects/nosuch/datasets/ds/tables/t"} {
		_, err := c.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p-1",
			ReadSession: &storagepb.ReadSession{Table: table, DataFormat: storagepb.DataFormat_ARROW}})
		if status.Code(err) != codes.NotFound {
			t.Errorf("CreateReadSession of %s: %v, want NOT_FOUND", table, err)
		}
	}
	if emu.sessions != 0 {
		t.Errorf("the emulator was sent %d sessions of tables not found", emu.sessions)
	}

	// A table gone after its session was made: NOT_FOUND from ReadRows.
	s, err := c.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p-1",
		ReadSession: &storagepb.ReadSession{Table: "projects/p-1/datasets/ds/tables/t", DataFormat: storagepb.DataFormat_ARROW}})
	if err != nil {
		t.Fatal(err)
	}
	rest.mu.Lock()
	delete(rest.rows, "ds.t")
	rest.mu.Unlock()
	if _, err := readAll(ctx, c, s.Streams[0].Name, 0); status.Code(err) != codes.NotFound {
		t.Errorf("ReadRows of a table gone: %v, want NOT_FOUND", err)
	}

	_, err = c.SplitReadStream(ctx, &storagepb.SplitReadStreamRequest{Name: "x"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "SplitReadStream not implemented") {
		t.Errorf("SplitReadStream: %v, want the emulator's own answer", err)
	}
	if emu.reads != 0 {
		t.Errorf("the emulator's ReadRows was called %d times", emu.reads)
	}
}

// TestBigQueryAvroSchemaNames (#1098): the emulator's names are made valid
// Avro names, and records of one name made distinct; the rest is kept.
func TestBigQueryAvroSchemaNames(t *testing.T) {
	// Two RECORD columns named rec, one inside a REPEATED RECORD.
	emu := `{"namespace":"p-1.ds","name":"t-1","type":"record","fields":[` +
		`{"type":["null",{"fields":[{"name":"a","type":["null","long"]}],"name":"rec","type":"record"}],"name":"rec"},` +
		`{"type":{"items":{"fields":[{"name":"rec","type":["null",{"fields":[{"name":"z","type":"long"}],"name":"rec","type":"record"}]}],` +
		`"name":"rr","type":"record"},"type":"array"},"name":"rr"}]}`
	got := bigQueryAvroSchema(emu)
	if _, err := goavro.NewCodec(emu); err == nil {
		t.Fatal("goavro takes the emulator's schema; the test expects it refused, as measured")
	}
	if _, err := goavro.NewCodec(got); err != nil {
		t.Fatalf("goavro refuses %s: %v", got, err)
	}
	for _, want := range []string{`"namespace":"p_1.ds"`, `"name":"t_1"`, `"name":"rec","type":"record"`, `"name":"rec_2","type":"record"`, `"name":"rr","type":"record"`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %s", got, want)
		}
	}
	for in, want := range map[string]string{"a-b": "a_b", "9lives": "_9lives", "": "_", "ok_1": "ok_1", "é": "_"} {
		if got := avroName(in); got != want {
			t.Errorf("avroName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := bigQueryAvroSchema("not json"); got != "not json" {
		t.Errorf("a schema that is not JSON: %q", got)
	}
}

func TestDecimalBytes(t *testing.T) {
	for in, want := range map[string][]byte{
		"0": {0}, "1.25": {0x7d}, "-0.01": {0xff}, "-1": {0x9c}, "-1.28": {0xff, 0x80}, "1.28": {0x00, 0x80}, "-1.29": {0xff, 0x7f},
		"12345678.9": {0x49, 0x96, 0x02, 0xd2},
	} {
		got, err := decimalBytes(in, 2)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("decimalBytes(%q, 2) = %x %v, want %x", in, got, err, want)
		}
	}
}

func TestParseReadTable(t *testing.T) {
	for path, want := range map[string]bool{
		"projects/p/datasets/d/tables/t": true,
		"projects/p/datasets/d/tables/":  false,
		"projects/p/datasets/d":          false,
		"projects/p/tables/t/datasets/d": false,
	} {
		if _, ok := parseReadTable(path); ok != want {
			t.Errorf("parseReadTable(%q) = %v, want %v", path, ok, want)
		}
	}
}
