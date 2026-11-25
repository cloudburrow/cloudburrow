package bigtablefront

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/bigtable/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

func TestOrderedCodeInt64RoundTripsInOrder(t *testing.T) {
	vals := []int64{math.MinInt64, math.MinInt64 + 1, -1 << 40, -65536, -257, -256, -65, -64, -1, 0, 1, 63, 64, 127, 128, 255, 256, 65535, 1 << 40, math.MaxInt64 - 1, math.MaxInt64}
	var encs [][]byte
	for _, v := range vals {
		b := appendOrderedInt64(nil, v)
		if len(b) > 10 {
			t.Errorf("%d encodes to %d bytes", v, len(b))
		}
		got, n, err := parseOrderedInt64(b)
		if err != nil || got != v || n != len(b) {
			t.Errorf("parse(append(%d)) = %d, %d, %v", v, got, n, err)
		}
		encs = append(encs, b)
	}
	if !sort.SliceIsSorted(encs, func(i, j int) bool { return bytes.Compare(encs[i], encs[j]) < 0 }) {
		t.Errorf("the encodings do not sort as the values do")
	}
}

// TestSplitOrderedCodeStructs decodes the reference's examples of the
// struct's OrderedCodeBytes encoding (types.proto).
func TestSplitOrderedCodeStructs(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want []string
	}{
		{"\x00\x00", 1, []string{""}},
		{"\x00\x00", 2, []string{"", ""}},
		{"\x00\x00\x00\x01B", 2, []string{"", "B"}},
		{"A", 2, []string{"A", ""}},
		{"\x00\x00\x00\x01B", 3, []string{"", "B", ""}},
		{"A\x00\x01\x00\x00\x00\x01C", 3, []string{"A", "", "C"}},
		{"a\x00\xffb", 1, []string{"a\x00b"}},
	} {
		parts, err := splitOrderedCode([]byte(c.in), c.n)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		var got []string
		for _, p := range parts {
			got = append(got, string(p))
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := splitOrderedCode([]byte("a\x00\x01b\x00\x01c"), 2); err == nil {
		t.Error("three fields for a schema of two: no error")
	}
	if _, err := splitOrderedCode([]byte("a\x00"), 1); err == nil {
		t.Error("a trailing null byte: no error")
	}
}

func rowKeyClients(t *testing.T, addr string) (context.Context, *bigtable.AdminClient, *bigtable.Client, btapb.BigtableTableAdminClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	conn := dial(t, addr)
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	c, err := localclient.Bigtable(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return ctx, ac, c, btapb.NewBigtableTableAdminClient(conn)
}

var delimitedKeys = bigtable.StructType{
	Fields: []bigtable.StructField{
		{FieldName: "myfield1", FieldType: bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}},
		{FieldName: "myfield2", FieldType: bigtable.Int64Type{Encoding: bigtable.BigEndianBytesEncoding{}}},
	},
	Encoding: bigtable.StructDelimitedBytesEncoding{Delimiter: []byte("#")},
}

// TestRowKeySchemaAdmin (#1900) runs the official client's own row key
// schema cases (integration_test.go, TestIntegration_CreateTableWithRowKeySchema
// and TestIntegration_UpdateRowKeySchemaInTable), which the emulator skips.
func TestRowKeySchemaAdmin(t *testing.T) {
	addr := serve(t)
	ctx, ac, _, raw := rowKeyClients(t, addr)
	str := bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}
	i64 := bigtable.Int64Type{Encoding: bigtable.BigEndianBytesEncoding{}}
	fams := map[string]bigtable.GCPolicy{"fam1": bigtable.MaxVersionsPolicy(1), "fam2": bigtable.MaxVersionsPolicy(2)}
	for i, c := range []struct {
		desc string
		rks  bigtable.StructType
		ok   bool
	}{
		{"conflict family and row key column", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "fam1", FieldType: i64}}, Encoding: bigtable.StructOrderedCodeBytesEncoding{}}, false},
		{"missing encoding in struct type", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield", FieldType: i64}}}, false},
		{"DelimitedBytes missing delimiter", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield", FieldType: str}}, Encoding: bigtable.StructDelimitedBytesEncoding{}}, false},
		{"Singleton with more than 1 field", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "a", FieldType: str}, {FieldName: "b", FieldType: i64}}, Encoding: bigtable.StructSingletonEncoding{}}, false},
		{"reserved name", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "_key", FieldType: str}}, Encoding: bigtable.StructSingletonEncoding{}}, false},
		{"no field name", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "", FieldType: str}}, Encoding: bigtable.StructSingletonEncoding{}}, false},
		{"a non-scalar field", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "a", FieldType: bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: bigtable.SumAggregator{}}}}, Encoding: bigtable.StructSingletonEncoding{}}, false},
		{"a timestamp without its encoding", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "a", FieldType: bigtable.TimestampType{}}}, Encoding: bigtable.StructSingletonEncoding{}}, false},
		{"Singleton ok", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield1", FieldType: str}}, Encoding: bigtable.StructSingletonEncoding{}}, true},
		{"OrderedCode ok", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield1", FieldType: str}, {FieldName: "myfield2", FieldType: i64}}, Encoding: bigtable.StructOrderedCodeBytesEncoding{}}, true},
		{"DelimitedBytes ok", delimitedKeys, true},
	} {
		id := "create" + string(rune('a'+i))
		rks := c.rks
		err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: id, Families: fams, RowKeySchema: &rks})
		if c.ok != (err == nil) {
			t.Errorf("%s: CreateTable = %v, want ok %v", c.desc, err, c.ok)
			continue
		}
		if !c.ok {
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: %v, want INVALID_ARGUMENT", c.desc, err)
			}
			continue
		}
		ti, err := ac.TableInfo(ctx, id)
		if err != nil || ti.RowKeySchema == nil || len(ti.RowKeySchema.Fields) != len(c.rks.Fields) {
			t.Errorf("%s: TableInfo = %+v, %v", c.desc, ti, err)
		}
	}

	// Update: set on a table without one, refused on one with one, and
	// cleared (UpdateTableRemoveRowKeySchema sets ignore_warnings).
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "upd", Families: map[string]bigtable.GCPolicy{"fam1": bigtable.MaxVersionsPolicy(1)}}); err != nil {
		t.Fatal(err)
	}
	if err := ac.UpdateTableWithRowKeySchema(ctx, "upd", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "fam1", FieldType: i64}}, Encoding: bigtable.StructSingletonEncoding{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("update with a family's name = %v", err)
	}
	if err := ac.UpdateTableWithRowKeySchema(ctx, "upd", delimitedKeys); err != nil {
		t.Fatalf("UpdateTableWithRowKeySchema: %v", err)
	}
	if ti, err := ac.TableInfo(ctx, "upd"); err != nil || ti.RowKeySchema == nil || ti.RowKeySchema.Fields[1].FieldName != "myfield2" {
		t.Fatalf("TableInfo after the update = %+v, %v", ti, err)
	}
	if err := ac.UpdateTableWithRowKeySchema(ctx, "upd", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "mycol", FieldType: i64}}, Encoding: bigtable.StructSingletonEncoding{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("update over an existing schema = %v", err)
	}
	name := "projects/p/instances/i/tables/upd"
	if _, err := raw.UpdateTable(ctx, &btapb.UpdateTableRequest{Table: &btapb.Table{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"row_key_schema"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("clearing without ignore_warnings = %v", err)
	}
	// A family named as a field cannot be added.
	if err := ac.CreateColumnFamily(ctx, "upd", "myfield1"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateColumnFamily(myfield1) = %v", err)
	}
	// ListTables carries it in SCHEMA_VIEW, not in NAME_ONLY.
	for view, want := range map[btapb.Table_View]bool{btapb.Table_SCHEMA_VIEW: true, btapb.Table_NAME_ONLY: false} {
		resp, err := raw.ListTables(ctx, &btapb.ListTablesRequest{Parent: "projects/p/instances/i", View: view})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tb := range resp.GetTables() {
			if tb.GetName() == name {
				found = tb.GetRowKeySchema() != nil
			}
		}
		if found != want {
			t.Errorf("ListTables(%v): schema %v, want %v", view, found, want)
		}
	}
	if err := ac.UpdateTableRemoveRowKeySchema(ctx, "upd"); err != nil {
		t.Fatalf("UpdateTableRemoveRowKeySchema: %v", err)
	}
	if ti, err := ac.TableInfo(ctx, "upd"); err != nil || ti.RowKeySchema != nil {
		t.Fatalf("TableInfo after the clear = %+v, %v", ti, err)
	}
	// A deleted table's schema goes with it.
	if err := ac.DeleteTable(ctx, "createk"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "createk"); err != nil {
		t.Fatal(err)
	}
	if ti, err := ac.TableInfo(ctx, "createk"); err != nil || ti.RowKeySchema != nil {
		t.Fatalf("a new table under a deleted one's name = %+v, %v", ti, err)
	}
}

// TestRowKeySchemaSQL (#1900): the reference's sales example of structured
// row key queries, and a key of OrderedCodeBytes with an INT64 and a
// TIMESTAMP.
func TestRowKeySchemaSQL(t *testing.T) {
	addr := serve(t)
	ctx, ac, c, _ := rowKeyClients(t, addr)
	str := bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "sales",
		Families: map[string]bigtable.GCPolicy{"product": bigtable.NoGcPolicy()},
		RowKeySchema: &bigtable.StructType{Fields: []bigtable.StructField{
			{FieldName: "user_id", FieldType: bigtable.BytesType{Encoding: bigtable.RawBytesEncoding{}}},
			{FieldName: "purchase_date", FieldType: str},
			{FieldName: "order_number", FieldType: str},
		}, Encoding: bigtable.StructDelimitedBytesEncoding{Delimiter: []byte("#")}}}); err != nil {
		t.Fatal(err)
	}
	for key, prod := range map[string][2]string{
		"user1#2025-05-20#abcd1233": {"phone", "iPhone16"},
		"user1#2025-05-20#abcd1235": {"tablet", "Pixel"},
		"user2#2025-05-24#defg456":  {"phone", "Pixel9"},
	} {
		m := bigtable.NewMutation()
		m.Set("product", "product_type", bigtable.Timestamp(1000), []byte(prod[0]))
		m.Set("product", "product_name", bigtable.Timestamp(1000), []byte(prod[1]))
		if err := c.Open("sales").Apply(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}
	run := func(sql string) ([]string, []string, error) {
		ps, err := c.PrepareStatement(ctx, sql, nil)
		if err != nil {
			return nil, nil, err
		}
		bs, err := ps.Bind(nil)
		if err != nil {
			return nil, nil, err
		}
		var cols, rows []string
		err = bs.Execute(ctx, func(r bigtable.ResultRow) bool {
			cols = cols[:0]
			var vals []string
			for i, col := range r.Metadata.Columns {
				cols = append(cols, col.Name)
				var v any
				if err := r.GetByIndex(i, &v); err != nil {
					t.Fatalf("%s: GetByIndex(%d): %v", sql, i, err)
				}
				vals = append(vals, show(v))
			}
			rows = append(rows, strings.Join(vals, " "))
			return true
		})
		return cols, rows, err
	}
	expect := func(sql, wantCols, wantRows string) {
		t.Helper()
		cols, rows, err := run(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if strings.Join(cols, ",") != wantCols || strings.Join(rows, "\n") != wantRows {
			t.Errorf("%s = %v\n%s\nwant %s\n%s", sql, cols, strings.Join(rows, "\n"), wantCols, wantRows)
		}
	}
	expect("SELECT * FROM sales", "user_id,purchase_date,order_number,product",
		"user1 2025-05-20 abcd1233 {product_name:iPhone16,product_type:phone}\n"+
			"user1 2025-05-20 abcd1235 {product_name:Pixel,product_type:tablet}\n"+
			"user2 2025-05-24 defg456 {product_name:Pixel9,product_type:phone}")
	expect(`SELECT product['product_type'] AS product_type, product['product_name'] AS product_name FROM sales WHERE user_id = b"user1"`,
		"product_type,product_name", "phone iPhone16\ntablet Pixel")
	expect(`SELECT user_id, product['product_type'] AS product_type, count(*) AS count FROM sales GROUP BY 1, 2`,
		"user_id,product_type,count", "user1 phone 1\nuser1 tablet 1\nuser2 phone 1")
	expect("SELECT _key, user_id FROM sales", "_key,user_id",
		"user1#2025-05-20#abcd1233 user1\nuser1#2025-05-20#abcd1235 user1\nuser2#2025-05-24#defg456 user2")

	// A key that does not conform fails the queries that read a field,
	// not those that read _key alone.
	m := bigtable.NewMutation()
	m.Set("product", "product_type", bigtable.Timestamp(1000), []byte("x"))
	if err := c.Open("sales").Apply(ctx, "nodelimiters", m); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("SELECT user_id FROM sales"); err == nil || !strings.Contains(err.Error(), "row key schema") {
		t.Errorf("a field of a key that does not decode: %v", err)
	}
	if _, rows, err := run("SELECT _key FROM sales"); err != nil || len(rows) != 4 {
		t.Errorf("SELECT _key = %v, %v", rows, err)
	}

	// OrderedCodeBytes of an INT64 (ordered code) and a TIMESTAMP (micros,
	// big-endian).
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "events",
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy()},
		RowKeySchema: &bigtable.StructType{Fields: []bigtable.StructField{
			{FieldName: "shard", FieldType: bigtable.Int64Type{Encoding: bigtable.Int64OrderedCodeBytesEncoding{}}},
			{FieldName: "at", FieldType: bigtable.TimestampType{Encoding: bigtable.TimestampUnixMicrosInt64Encoding{UnixMicrosInt64Encoding: bigtable.BigEndianBytesEncoding{}}}},
		}, Encoding: bigtable.StructOrderedCodeBytesEncoding{}}}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 5, 20, 12, 0, 0, 0, time.UTC)
	for _, shard := range []int64{-300, 7} {
		var ts [8]byte
		binary.BigEndian.PutUint64(ts[:], uint64(at.UnixMicro()))
		key := escapeOrdered(appendOrderedInt64(nil, shard))
		key = append(append(key, 0x00, 0x01), escapeOrdered(ts[:])...)
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte("v"))
		if err := c.Open("events").Apply(ctx, string(key), m); err != nil {
			t.Fatal(err)
		}
	}
	expect("SELECT shard, at FROM events WHERE shard < 0", "shard,at", "-300 2025-05-20T12:00:00Z")
}

func escapeOrdered(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0}, []byte{0, 0xff})
}

// TestRowKeySchemaSurvivesARestart: the schema is kept with the front's
// state, and a backup restores its table with it.
func TestRowKeySchemaSurvivesARestart(t *testing.T) {
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	dir := t.TempDir()
	addr, stop := serveKept(t, emu.Addr, dir)
	ctx, ac, _, _ := rowKeyClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "kept", RowKeySchema: &delimitedKeys}); err != nil {
		t.Fatal(err)
	}
	stop()
	time.Sleep(200 * time.Millisecond)
	addr, _ = serveKept(t, emu.Addr, dir)
	ctx, ac, _, _ = rowKeyClients(t, addr)
	if ti, err := ac.TableInfo(ctx, "kept"); err != nil || ti.RowKeySchema == nil || len(ti.RowKeySchema.Fields) != 2 {
		t.Fatalf("TableInfo after the restart = %+v, %v", ti, err)
	}
}
