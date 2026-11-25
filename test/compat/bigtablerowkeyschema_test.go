//go:build compat

package compat

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

// TestBigtableRowKeySchemas (#1900) runs, through the official admin
// client, the client's own row key schema cases
// (TestIntegration_CreateTableWithRowKeySchema and
// TestIntegration_UpdateRowKeySchemaInTable, which skip on the emulator):
// the invalid schemas refused, the valid ones kept and read back by
// TableInfo and ListTables in SCHEMA_VIEW, a schema set by UpdateTable on a
// table without one, refused over one, and cleared only with
// ignore_warnings. Then the reference's structured row key queries run
// over a keyed table through the data client's PrepareStatement.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateTable, google.bigtable.admin.v2.BigtableTableAdmin/GetTable, google.bigtable.admin.v2.BigtableTableAdmin/ListTables, google.bigtable.admin.v2.BigtableTableAdmin/UpdateTable, google.bigtable.admin.v2.BigtableTableAdmin/ModifyColumnFamilies
// covers: google.bigtable.v2.Bigtable/PrepareQuery, google.bigtable.v2.Bigtable/ExecuteQuery
func TestBigtableRowKeySchemas(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // asks for the Bigtable endpoint first
	ctx := h.Context()
	const id = "cb-inst-rks1900"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Row key schemas",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	c, err := localclient.Bigtable(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	str := bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}
	i64 := bigtable.Int64Type{Encoding: bigtable.BigEndianBytesEncoding{}}
	delimited := bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield1", FieldType: str}, {FieldName: "myfield2", FieldType: i64}},
		Encoding: bigtable.StructDelimitedBytesEncoding{Delimiter: []byte{'#'}}}
	for i, tc := range []struct {
		desc string
		rks  bigtable.StructType
		ok   bool
	}{
		{"conflict family and row key column", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "fam1", FieldType: i64}}, Encoding: bigtable.StructOrderedCodeBytesEncoding{}}, false},
		{"missing encoding in struct type", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield", FieldType: i64}}}, false},
		{"DelimitedBytes missing delimiter", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield", FieldType: str}}, Encoding: bigtable.StructDelimitedBytesEncoding{}}, false},
		{"Singleton with more than 1 field", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield1", FieldType: str}, {FieldName: "myfield2", FieldType: i64}}, Encoding: bigtable.StructSingletonEncoding{}}, false},
		{"Singleton ok", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield1", FieldType: str}}, Encoding: bigtable.StructSingletonEncoding{}}, true},
		{"OrderedCode ok", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "myfield1", FieldType: str}, {FieldName: "myfield2", FieldType: i64}}, Encoding: bigtable.StructOrderedCodeBytesEncoding{}}, true},
		{"DelimitedBytes ok", delimited, true},
	} {
		table := "create" + string(rune('a'+i))
		rks := tc.rks
		err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: table, RowKeySchema: &rks,
			Families: map[string]bigtable.GCPolicy{"fam1": bigtable.MaxVersionsPolicy(1), "fam2": bigtable.MaxVersionsPolicy(2)}})
		if !tc.ok {
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: CreateTable = %v, want INVALID_ARGUMENT", tc.desc, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: CreateTable: %v", tc.desc, err)
		}
		if ti, err := ac.TableInfo(ctx, table); err != nil || ti.RowKeySchema == nil || len(ti.RowKeySchema.Fields) != len(tc.rks.Fields) {
			t.Errorf("%s: TableInfo = %+v, %v", tc.desc, ti, err)
		}
	}

	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "upd", Families: map[string]bigtable.GCPolicy{"fam1": bigtable.MaxVersionsPolicy(1)}}); err != nil {
		t.Fatal(err)
	}
	if err := ac.UpdateTableWithRowKeySchema(ctx, "upd", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "fam1", FieldType: i64}}, Encoding: bigtable.StructSingletonEncoding{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateTableWithRowKeySchema naming a family = %v, want INVALID_ARGUMENT", err)
	}
	if err := ac.UpdateTableWithRowKeySchema(ctx, "upd", delimited); err != nil {
		t.Fatalf("UpdateTableWithRowKeySchema: %v", err)
	}
	if ti, err := ac.TableInfo(ctx, "upd"); err != nil || ti.RowKeySchema == nil {
		t.Fatalf("TableInfo after UpdateTableWithRowKeySchema = %+v, %v", ti, err)
	}
	if err := ac.UpdateTableWithRowKeySchema(ctx, "upd", bigtable.StructType{Fields: []bigtable.StructField{{FieldName: "mycol", FieldType: i64}}, Encoding: bigtable.StructSingletonEncoding{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateTableWithRowKeySchema over a schema = %v, want INVALID_ARGUMENT", err)
	}
	raw := ac.TableAdminClientV2()
	name := "projects/" + h.Project() + "/instances/" + id + "/tables/upd"
	if _, err := raw.UpdateTable(ctx, &btapb.UpdateTableRequest{Table: &btapb.Table{Name: name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"row_key_schema"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("clearing the schema without ignore_warnings = %v, want INVALID_ARGUMENT", err)
	}
	if err := ac.CreateColumnFamily(ctx, "upd", "myfield1"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateColumnFamily named as a key field = %v, want INVALID_ARGUMENT", err)
	}
	it := raw.ListTables(ctx, &btapb.ListTablesRequest{Parent: "projects/" + h.Project() + "/instances/" + id, View: btapb.Table_SCHEMA_VIEW})
	listed := false
	for {
		tb, err := it.Next()
		if err != nil {
			break
		}
		if tb.GetName() == name {
			listed = tb.GetRowKeySchema() != nil
		}
	}
	if !listed {
		t.Errorf("ListTables(SCHEMA_VIEW) has no row key schema for %s", name)
	}
	if err := ac.UpdateTableRemoveRowKeySchema(ctx, "upd"); err != nil {
		t.Fatalf("UpdateTableRemoveRowKeySchema: %v", err)
	}
	if ti, err := ac.TableInfo(ctx, "upd"); err != nil || ti.RowKeySchema != nil {
		t.Errorf("TableInfo after UpdateTableRemoveRowKeySchema = %+v, %v", ti, err)
	}

	// The reference's structured row key queries (structured-row-key-queries).
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
	run := func(sql string) (string, string, error) {
		ps, err := c.PrepareStatement(ctx, sql, nil)
		if err != nil {
			return "", "", err
		}
		bs, err := ps.Bind(nil)
		if err != nil {
			return "", "", err
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
				switch x := v.(type) {
				case []byte:
					vals = append(vals, string(x))
				case string:
					vals = append(vals, x)
				case map[string][]byte:
					vals = append(vals, string(x["product_name"]))
				default:
					vals = append(vals, fmt.Sprint(v))
				}
			}
			rows = append(rows, strings.Join(vals, " "))
			return true
		})
		return strings.Join(cols, ","), strings.Join(rows, "\n"), err
	}
	expect := func(sql, wantCols, wantRows string) {
		t.Helper()
		cols, rows, err := run(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if cols != wantCols || rows != wantRows {
			t.Errorf("%s = %s\n%s\nwant %s\n%s", sql, cols, rows, wantCols, wantRows)
		}
	}
	// SELECT * reads the segments, then the family; _key is a pseudocolumn.
	cols, _, err := run("SELECT * FROM sales")
	if err != nil || cols != "user_id,purchase_date,order_number,product" {
		t.Errorf("SELECT * FROM sales: columns %q, %v", cols, err)
	}
	expect(`SELECT product['product_type'] AS product_type, product['product_name'] AS product_name FROM sales WHERE user_id = b"user1"`,
		"product_type,product_name", "phone iPhone16\ntablet Pixel")
	expect(`SELECT user_id, product['product_type'] AS product_type, count(*) AS count FROM sales GROUP BY 1, 2`,
		"user_id,product_type,count", "user1 phone 1\nuser1 tablet 1\nuser2 phone 1")
	expect("SELECT _key, user_id FROM sales", "_key,user_id",
		"user1#2025-05-20#abcd1233 user1\nuser1#2025-05-20#abcd1235 user1\nuser2#2025-05-24#defg456 user2")
	// A key that does not conform fails the queries that select a segment.
	m := bigtable.NewMutation()
	m.Set("product", "product_type", bigtable.Timestamp(1000), []byte("x"))
	if err := c.Open("sales").Apply(ctx, "nodelimiters", m); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("SELECT user_id FROM sales"); err == nil {
		t.Error("SELECT user_id over a key that does not conform: no error")
	}
	if _, rows, err := run("SELECT _key FROM sales"); err != nil || !bytes.Contains([]byte(rows), []byte("nodelimiters")) {
		t.Errorf("SELECT _key = %q, %v", rows, err)
	}
}
