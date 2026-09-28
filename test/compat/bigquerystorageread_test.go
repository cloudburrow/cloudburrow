//go:build compat

package compat

import (
	"bytes"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// storageReadTypesDDL is a table of every column type the Storage Read
// tests read, with REPEATED columns and RECORDs at depth (#1095, #1098).
const storageReadTypesDDL = `(i INT64 NOT NULL, f FLOAT64, n NUMERIC, bn BIGNUMERIC, b BOOL, s STRING, y BYTES, d DATE,
 dt DATETIME, tm TIME, ts TIMESTAMP, j JSON,
 rec STRUCT<a INT64, ts TIMESTAMP, r ARRAY<STRING>>, r ARRAY<INT64>, rts ARRAY<TIMESTAMP>,
 rr ARRAY<STRUCT<x INT64, y STRING, z ARRAY<FLOAT64>>>)`

// storageReadTypesRows are its rows: every value in the first, NULL or an
// empty array in the second (the REPEATED RECORD with one empty array, as
// #1095 measured), and the third is #1095's r INTEGER REPEATED [9].
const storageReadTypesRows = `(i, f, n, bn, b, s, y, d, dt, tm, ts, j, rec, r, rts, rr) VALUES
 (1, 1.5, NUMERIC '123.456789012', BIGNUMERIC '-2.5', true, 'x', b'\x00\xffab', DATE '2024-01-02',
  DATETIME '2024-01-02 03:04:05.123456', TIME '03:04:05.5', TIMESTAMP '2024-01-02 03:04:05.123456 UTC', JSON '{"k":1}',
  STRUCT(3, TIMESTAMP '2020-01-01 00:00:00 UTC', ['p', 'q']), [7, 8], [TIMESTAMP '2021-01-01 00:00:01.5 UTC'],
  [STRUCT(1 AS x, 'a' AS y, [0.5, -1.0] AS z), STRUCT(2 AS x, NULL AS y, CAST([] AS ARRAY<FLOAT64>) AS z)]),
 (2, -2.25, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, [9], [], []),
 (3, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, [9], [], [])`

// storageReadTypesTable makes the table in a dataset of its own and
// returns it, with a Storage Read client.
func storageReadTypesTable(t *testing.T, h *Harness) (*bigquery.Client, string, *bigquery.Table, *bqstorage.BigQueryReadClient) {
	t.Helper()
	c, project := bigqueryClient(t, h)
	one, _ := twoDatasets(t, h, c)
	if err := bqRun(h.Context(), c, "CREATE TABLE "+one.DatasetID+".types "+storageReadTypesDDL, false); err != nil {
		t.Fatalf("create %s.types: %v", one.DatasetID, err)
	}
	if err := bqRun(h.Context(), c, "INSERT INTO "+one.DatasetID+".types "+storageReadTypesRows, false); err != nil {
		t.Fatalf("insert into %s.types: %v", one.DatasetID, err)
	}
	rc, err := bqstorage.NewBigQueryReadClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewBigQueryReadClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	return c, project, one.Table("types"), rc
}

// readSession makes a read session of tbl in format, and returns it with
// every ReadRows answer of its streams.
func readSession(t *testing.T, h *Harness, rc *bqstorage.BigQueryReadClient, project string, tbl *bigquery.Table,
	format storagepb.DataFormat, opts *storagepb.ReadSession_TableReadOptions) (*storagepb.ReadSession, []*storagepb.ReadRowsResponse) {
	t.Helper()
	s, err := rc.CreateReadSession(h.Context(), &storagepb.CreateReadSessionRequest{
		Parent: "projects/" + project,
		ReadSession: &storagepb.ReadSession{Table: "projects/" + project + "/datasets/" + tbl.DatasetID + "/tables/" + tbl.TableID,
			DataFormat: format, ReadOptions: opts},
		MaxStreamCount: 1,
	})
	if err != nil {
		t.Fatalf("CreateReadSession of %s in %s: %v", tbl.TableID, format, err)
	}
	var out []*storagepb.ReadRowsResponse
	for _, st := range s.GetStreams() {
		stream, err := rc.ReadRows(h.Context(), &storagepb.ReadRowsRequest{ReadStream: st.Name})
		if err != nil {
			t.Fatalf("ReadRows in %s: %v", format, err)
		}
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("ReadRows in %s: %v", format, err)
			}
			out = append(out, resp)
		}
	}
	return s, out
}

// arrowRowValues reads each answer's batch after the session's schema, as
// BigQuery frames them, and returns each row's values joined by "|" (a
// decimal exactly; arrow prints one through a float).
func arrowRowValues(t *testing.T, s *storagepb.ReadSession, resps []*storagepb.ReadRowsResponse) []string {
	t.Helper()
	var out []string
	for _, resp := range resps {
		r, err := ipc.NewReader(io.MultiReader(bytes.NewReader(s.GetArrowSchema().GetSerializedSchema()),
			bytes.NewReader(resp.GetArrowRecordBatch().GetSerializedRecordBatch())))
		if err != nil {
			t.Fatalf("decode the Arrow rows: %v", err)
		}
		for r.Next() {
			rec := r.Record()
			for i := 0; i < int(rec.NumRows()); i++ {
				var vals []string
				for j := 0; j < int(rec.NumCols()); j++ {
					col := rec.Column(j)
					v := col.ValueStr(i)
					if d, ok := col.(*array.Decimal128); ok && !col.IsNull(i) {
						v = exactDecimal(d.Value(i).BigInt(), d.DataType().(*arrow.Decimal128Type).Scale)
					}
					if d, ok := col.(*array.Decimal256); ok && !col.IsNull(i) {
						v = exactDecimal(d.Value(i).BigInt(), d.DataType().(*arrow.Decimal256Type).Scale)
					}
					vals = append(vals, v)
				}
				out = append(out, strings.Join(vals, "|"))
			}
		}
		if err := r.Err(); err != nil {
			t.Fatalf("decode the Arrow rows: %v", err)
		}
		r.Release()
	}
	return out
}

func exactDecimal(n *big.Int, scale int32) string {
	r := new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	return strings.TrimSuffix(strings.TrimRight(r.FloatString(int(scale)), "0"), ".")
}

// TestBigQueryStorageReadOfRepeatedAndRecordColumnsInArrow (#1095),
// through the official Storage Read client: an Arrow read session of a
// table with REPEATED columns (of INT64, of TIMESTAMP, of a RECORD, one
// row's empty), a RECORD holding a REPEATED column, and a column of every
// other type streams each row with its values, a REPEATED column as an
// Arrow list and a RECORD as a struct; with selected fields and a row
// restriction, those; and the official Go client's Table.Read through the
// Storage Read API (Client.EnableStorageReadClient) reads every value, a
// DATETIME as a civil.DateTime. The instance keeps its data: the table is
// there after.
// Measured first against the pinned emulator: ReadRows of (i INT64,
// r ARRAY<INT64>) with rows (1, [7,8]) and (2, [9]) killed the emulator's
// process ("panic: arrow/array: field 1 has 3 rows. want=2"), the client
// got UNAVAILABLE "error reading from server: EOF", and the emulator
// restarted with every dataset gone; a DATETIME column failed "invalid
// timestamp string".
func TestBigQueryStorageReadOfRepeatedAndRecordColumnsInArrow(t *testing.T) {
	h := New(t)
	c, project, tbl, rc := storageReadTypesTable(t, h)

	s, resps := readSession(t, h, rc, project, tbl, storagepb.DataFormat_ARROW, nil)
	// The emulator's schema (TableToARROW, in its types/arrow.go), with a
	// DATETIME a timestamp with no zone, as BigQuery's.
	wantSchema := []string{
		"i: type=int64, nullable", "f: type=float64, nullable", "n: type=decimal(38, 9), nullable",
		"bn: type=decimal256(76, 38), nullable", "b: type=bool, nullable", "s: type=utf8, nullable", "y: type=binary, nullable",
		"d: type=date32, nullable", "dt: type=timestamp[us], nullable", "tm: type=time64[us], nullable",
		"ts: type=timestamp[us, tz=UTC], nullable", "j: type=utf8, nullable",
		"rec: type=struct<a: int64, ts: timestamp[us, tz=UTC], r: list<r: utf8>>, nullable",
		"r: type=list<r: int64>", "rts: type=list<rts: timestamp[us, tz=UTC]>",
		"rr: type=list<rr: struct<x: int64, y: utf8, z: list<z: float64>>>",
	}
	r, err := ipc.NewReader(bytes.NewReader(s.GetArrowSchema().GetSerializedSchema()))
	if err != nil {
		t.Fatalf("read the session's Arrow schema: %v", err)
	}
	for _, w := range wantSchema {
		if !strings.Contains(r.Schema().String(), "- "+w+"\n") && !strings.HasSuffix(r.Schema().String(), "- "+w) {
			t.Errorf("the session's Arrow schema lacks %q:\n%s", w, r.Schema())
		}
	}
	r.Release()
	got := arrowRowValues(t, s, resps)
	want := []string{
		`1|1.5|123.456789012|-2.5|true|x|AP9hYg==|2024-01-02|2024-01-02 03:04:05.123456Z|03:04:05.500000|2024-01-02 03:04:05.123456Z|{"k":1}|` +
			`{"a":3,"r":["p","q"],"ts":"2020-01-01 00:00:00"}|[7,8]|["2021-01-01 00:00:01.5"]|` +
			`[{"x":1,"y":"a","z":[0.5,-1]},{"x":2,"y":null,"z":[]}]`,
		`2|-2.25|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|[9]|[]|[]`,
		`3|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|[9]|[]|[]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the Arrow rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Selected fields and a row restriction.
	s, resps = readSession(t, h, rc, project, tbl, storagepb.DataFormat_ARROW,
		&storagepb.ReadSession_TableReadOptions{SelectedFields: []string{"i", "r", "rr"}, RowRestriction: "i >= 2"})
	if got := arrowRowValues(t, s, resps); !reflect.DeepEqual(got, []string{"2|[9]|[]", "3|[9]|[]"}) {
		t.Errorf("i, r, rr WHERE i >= 2: %q", got)
	}

	// The Go client, through the Storage Read API and through REST.
	sc, _ := bigqueryClient(t, h)
	if err := sc.EnableStorageReadClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))); err != nil {
		t.Fatalf("EnableStorageReadClient: %v", err)
	}
	it := sc.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context())
	fast := readRows(t, it, "Table.Read through the Storage Read API")
	if !it.IsAccelerated() {
		t.Error("Table.Read was not read through the Storage Read API")
	}
	wantGo := []string{
		`1|1.5|30864197253/250000000|-5/2|true|x|[0 255 97 98]|2024-01-02|2024-01-02T03:04:05.123456000|03:04:05.500000000|` +
			`2024-01-02 03:04:05.123456 +0000 UTC|{"k":1}|[3 2020-01-01 00:00:00 +0000 UTC [p q]]|[7 8]|` +
			`[2021-01-01 00:00:01.5 +0000 UTC]|[[1 a [0.5 -1]] [2 <nil> []]]`,
		`2|-2.25|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|[9]|[]|[]`,
		`3|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|[9]|[]|[]`,
	}
	if !reflect.DeepEqual(fast, wantGo) {
		t.Errorf("Table.Read through the Storage Read API:\n%s\nwant\n%s", strings.Join(fast, "\n"), strings.Join(wantGo, "\n"))
	}
	// The emulator kept its data.
	if _, err := c.Dataset(tbl.DatasetID).Table(tbl.TableID).Metadata(h.Context()); err != nil {
		t.Errorf("the table after the reads: %v", err)
	}
}

// TestBigQueryStorageReadInAvroOnAHyphenatedProject (#1098), through the
// official Storage Read client on the instance's project, whose ID has a
// hyphen as every CloudBurrow project ID does: an Avro read session of
// the table of the Arrow test streams its rows, which goavro decodes with
// the session's schema, every value as it was written. Measured first
// against the pinned emulator: ReadRows failed "failed to create avro
// codec ...: Record ought to have valid name: schema name ought to have
// second and remaining characters contain only [A-Za-z0-9_]: <project>",
// the emulator having named the schema's record by the namespace
// "<project>.<dataset>".
func TestBigQueryStorageReadInAvroOnAHyphenatedProject(t *testing.T) {
	h := New(t)
	_, project, tbl, rc := storageReadTypesTable(t, h)
	if !strings.Contains(project, "-") {
		t.Fatalf("the project %q has no hyphen; the test is of one that has", project)
	}
	s, resps := readSession(t, h, rc, project, tbl, storagepb.DataFormat_AVRO, nil)
	codec, err := goavro.NewCodec(s.GetAvroSchema().GetSchema())
	if err != nil {
		t.Fatalf("goavro refuses the session's Avro schema %s: %v", s.GetAvroSchema().GetSchema(), err)
	}
	var rows []map[string]any
	for _, resp := range resps {
		buf := resp.GetAvroRows().GetSerializedBinaryRows()
		for len(buf) > 0 {
			v, rest, err := codec.NativeFromBinary(buf)
			if err != nil {
				t.Fatalf("decode the Avro rows: %v", err)
			}
			rows = append(rows, v.(map[string]any))
			buf = rest
		}
	}
	if len(rows) != 3 {
		t.Fatalf("%d Avro rows, want 3", len(rows))
	}
	// unwrap is the value of a union ({"type": value}), or v.
	unwrap := func(v any) any {
		if m, ok := v.(map[string]any); ok && len(m) == 1 {
			for _, x := range m {
				return x
			}
		}
		return v
	}
	row := rows[0]
	for col, want := range map[string]any{
		"i": int64(1), "f": 1.5, "b": true, "s": "x", "y": []byte("\x00\xffab"), "dt": "2024-01-02T03:04:05.123456",
		"tm": 3*time.Hour + 4*time.Minute + 5500*time.Millisecond, "j": `{"k":1}`,
		"n": big.NewRat(123456789012, 1e9), "bn": big.NewRat(-5, 2),
		"d": time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "ts": time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC),
		"r":   []any{int64(7), int64(8)},
		"rts": []any{time.Date(2021, 1, 1, 0, 0, 1, 500000000, time.UTC)},
		"rec": map[string]any{"a": map[string]any{"long": int64(3)},
			"ts": map[string]any{"long.timestamp-micros": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, "r": []any{"p", "q"}},
		"rr": []any{
			map[string]any{"x": map[string]any{"long": int64(1)}, "y": map[string]any{"string": "a"}, "z": []any{0.5, -1.0}},
			map[string]any{"x": map[string]any{"long": int64(2)}, "y": nil, "z": []any{}},
		},
	} {
		got := unwrap(row[col])
		if !sameAvro(got, want) {
			t.Errorf("Avro %s: %#v, want %#v", col, got, want)
		}
	}
	for k := range row["rec"].(map[string]any) {
		if strings.Contains(k, "-") || !strings.HasSuffix(k, ".rec") {
			t.Errorf("the nullable RECORD's union branch is %q", k)
		}
	}
	for i, want := range []map[string]any{
		{"i": int64(2), "f": -2.25, "r": []any{int64(9)}, "rts": []any{}, "rr": []any{}},
		{"i": int64(3), "f": nil, "r": []any{int64(9)}, "rts": []any{}, "rr": []any{}},
	} {
		for col, w := range want {
			if got := unwrap(rows[i+1][col]); !sameAvro(got, w) {
				t.Errorf("Avro row %d, %s: %#v, want %#v", i+2, col, got, w)
			}
		}
		for _, col := range []string{"n", "bn", "b", "s", "y", "d", "dt", "tm", "ts", "j", "rec"} {
			if rows[i+1][col] != nil {
				t.Errorf("Avro row %d, %s: %#v, want nil", i+2, col, rows[i+1][col])
			}
		}
	}
}

// sameAvro compares goavro's native values: times by instant, decimals by
// value, the rest deeply.
func sameAvro(got, want any) bool {
	switch w := want.(type) {
	case time.Time:
		g, ok := got.(time.Time)
		return ok && g.Equal(w)
	case *big.Rat:
		g, ok := got.(*big.Rat)
		return ok && g.Cmp(w) == 0
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !sameAvro(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k := range w {
			if !sameAvro(g[k], w[k]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}
