//go:build compat

package compat

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBigtableSQLLanguage (#1511) runs GoogleSQL for Bigtable beyond the
// key-value subset through the official client: conditions on columns,
// scalar, map, JSON and timestamp functions, aggregates with GROUP BY,
// COUNT(DISTINCT), ORDER BY _key, with_history and the as_of, before and
// latest_n filters, parameters of each documented type; a logical view
// run as a query and a materialized view serving its rows, up to date
// after a write; and the refusals, INVALID_ARGUMENT for what the language
// does not have and UNIMPLEMENTED for what the emulator's SQL does not
// build.
//
// covers: google.bigtable.v2.Bigtable/PrepareQuery, google.bigtable.v2.Bigtable/ExecuteQuery
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateLogicalView, google.bigtable.admin.v2.BigtableInstanceAdmin/CreateMaterializedView
func TestBigtableSQLLanguage(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // asks for the Bigtable endpoint first
	ctx := h.Context()
	const id = "cb-inst-sql"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "SQL language",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	c, err := bigtable.NewClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := ac.CreateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"o", "empty"} {
		if err := ac.CreateColumnFamily(ctx, "orders", f); err != nil {
			t.Fatal(err)
		}
	}
	be := func(n int64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, uint64(n)); return b }
	for i, o := range []struct {
		region string
		qty    int64
	}{{"eu", 3}, {"us", 5}, {"eu", 7}, {"us", 1}, {"ap", 2}} {
		m := bigtable.NewMutation()
		m.Set("o", "region", bigtable.Timestamp(1_000_000), []byte(o.region))
		m.Set("o", "qty", bigtable.Timestamp(1_000_000), be(o.qty))
		m.Set("o", "status", bigtable.Timestamp(1_000_000), []byte("new"))
		m.Set("o", "status", bigtable.Timestamp(2_000_000), []byte("paid"))
		m.Set("o", "json", bigtable.Timestamp(1_000_000), []byte(fmt.Sprintf(`{"a":{"b":%d}}`, i)))
		if err := c.Open("orders").Apply(ctx, fmt.Sprintf("o#%d", i+1), m); err != nil {
			t.Fatal(err)
		}
	}
	run := func(sql string, params map[string]bigtable.SQLType, bind map[string]any) []bigtable.ResultRow {
		t.Helper()
		ps, err := c.PrepareStatement(ctx, sql, params)
		if err != nil {
			t.Fatalf("PrepareStatement(%s): %v", sql, err)
		}
		bs, err := ps.Bind(bind)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var out []bigtable.ResultRow
		if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool { out = append(out, r); return true }); err != nil {
			t.Fatalf("Execute(%s): %v", sql, err)
		}
		return out
	}
	get := func(r bigtable.ResultRow, name string, dst any) {
		t.Helper()
		if err := r.GetByName(name, dst); err != nil {
			t.Fatalf("GetByName(%s): %v", name, err)
		}
	}
	// Conditions on columns, and scalar functions.
	rows := run("SELECT _key, UPPER(CAST(o['region'] AS STRING)) AS r, TO_INT64(o['qty']) * 2 AS q2 FROM orders "+
		"WHERE CAST(o['region'] AS STRING) = 'eu' AND TO_INT64(o['qty']) > @min ORDER BY _key", map[string]bigtable.SQLType{"min": bigtable.Int64SQLType{}}, map[string]any{"min": int64(4)})
	if len(rows) != 1 {
		t.Fatalf("column condition = %d rows, want 1", len(rows))
	}
	var key []byte
	var r string
	var q2 int64
	get(rows[0], "_key", &key)
	get(rows[0], "r", &r)
	get(rows[0], "q2", &q2)
	if string(key) != "o#3" || r != "EU" || q2 != 14 {
		t.Errorf("row = %s %s %d", key, r, q2)
	}
	// Aggregates with GROUP BY, ordinal and alias.
	rows = run("SELECT CAST(o['region'] AS STRING) AS region, COUNT(*) AS n, SUM(TO_INT64(o['qty'])) AS total, AVG(TO_INT64(o['qty'])) AS mean "+
		"FROM orders GROUP BY region", nil, nil)
	got := map[string][2]int64{}
	for _, row := range rows {
		var region string
		var n, total int64
		var mean float64
		get(row, "region", &region)
		get(row, "n", &n)
		get(row, "total", &total)
		get(row, "mean", &mean)
		got[region] = [2]int64{n, total}
	}
	if fmt.Sprint(got) != "map[ap:[1 2] eu:[2 10] us:[2 6]]" {
		t.Errorf("GROUP BY = %v", got)
	}
	rows = run("SELECT COUNT(*) AS n, MAX(_key) AS last, COUNT(DISTINCT o['region']) AS regions FROM orders", nil, nil)
	var n, regions int64
	var last []byte
	get(rows[0], "n", &n)
	get(rows[0], "last", &last)
	get(rows[0], "regions", &regions)
	if n != 5 || string(last) != "o#5" || regions != 3 {
		t.Errorf("aggregates = %d %s %d", n, last, regions)
	}
	// Map functions, JSON, an unused family as NULL, LIKE, IN.
	rows = run("SELECT ARRAY_LENGTH(MAP_KEYS(o)) AS k, MAP_CONTAINS_KEY(o, b'qty') AS has, JSON_VALUE(CAST(o['json'] AS STRING), '$.a.b') AS j, "+
		"empty IS NULL AS e FROM orders WHERE _key LIKE 'o#%' AND _key IN ('o#2', 'o#4')", nil, nil)
	if len(rows) != 2 {
		t.Fatalf("map functions = %d rows", len(rows))
	}
	var k int64
	var has, e bool
	var j string
	get(rows[0], "k", &k)
	get(rows[0], "has", &has)
	get(rows[0], "j", &j)
	get(rows[0], "e", &e)
	if k != 4 || !has || j != "1" || !e {
		t.Errorf("map functions = %d %v %q %v", k, has, j, e)
	}
	// WITH_HISTORY: both cells, newest first; the filters cut them.
	rows = run("SELECT o['status'] AS s FROM orders(with_history => TRUE) WHERE _key = 'o#1'", nil, nil)
	var hist []any
	get(rows[0], "s", &hist)
	if len(hist) != 2 {
		t.Errorf("with_history = %v", hist)
	}
	rows = run("SELECT ARRAY_LENGTH(o['status']) AS n, o['status'][OFFSET(0)].value AS v, UNIX_MICROS(o['status'][OFFSET(0)].timestamp) AS ts "+
		"FROM orders(with_history => TRUE, before => @t) WHERE _key = 'o#1'", map[string]bigtable.SQLType{"t": bigtable.TimestampSQLType{}}, map[string]any{"t": time.UnixMicro(2_000_000)})
	var v []byte
	var ts int64
	get(rows[0], "n", &n)
	get(rows[0], "v", &v)
	get(rows[0], "ts", &ts)
	if n != 1 || string(v) != "new" || ts != 1_000_000 {
		t.Errorf("before = %d %s %d", n, v, ts)
	}
	rows = run("SELECT o['status'] AS s FROM orders(as_of => TIMESTAMP_MICROS(1500000)) WHERE _key = 'o#1'", nil, nil)
	get(rows[0], "s", &v)
	if string(v) != "new" {
		t.Errorf("as_of = %s", v)
	}
	rows = run("SELECT ARRAY_LENGTH(o['status']) AS n FROM orders(with_history => TRUE, latest_n => 1) LIMIT 1", nil, nil)
	get(rows[0], "n", &n)
	if n != 1 {
		t.Errorf("latest_n = %d", n)
	}
	// Parameters of the other types.
	rows = run("SELECT @s AS s, @f AS f, @b AS b, @a AS a, @d AS d", map[string]bigtable.SQLType{
		"s": bigtable.StringSQLType{}, "f": bigtable.Float64SQLType{}, "b": bigtable.BoolSQLType{},
		"a": bigtable.ArraySQLType{ElemType: bigtable.Int64SQLType{}}, "d": bigtable.DateSQLType{}},
		map[string]any{"s": "x", "f": 1.5, "b": true, "a": []any{int64(1), int64(2)}, "d": nil})
	var f float64
	var b bool
	var a []any
	get(rows[0], "s", &r)
	get(rows[0], "f", &f)
	get(rows[0], "b", &b)
	get(rows[0], "a", &a)
	if r != "x" || f != 1.5 || !b || len(a) != 2 {
		t.Errorf("params = %q %v %v %v", r, f, b, a)
	}
	// Views: a logical view runs as a query; a materialized view serves its rows.
	if err := iac.CreateLogicalView(ctx, id, &bigtable.LogicalViewInfo{LogicalViewID: "eu_orders",
		Query: "SELECT _key, CAST(o['region'] AS STRING) AS region FROM orders WHERE CAST(o['region'] AS STRING) = 'eu'"}); err != nil {
		t.Fatal(err)
	}
	rows = run("SELECT _key, region FROM eu_orders ORDER BY _key", nil, nil)
	if len(rows) != 2 {
		t.Errorf("logical view = %d rows, want 2", len(rows))
	}
	if err := iac.CreateMaterializedView(ctx, id, &bigtable.MaterializedViewInfo{MaterializedViewID: "by_region",
		Query: "SELECT CAST(o['region'] AS STRING) AS region, COUNT(*) AS n FROM orders GROUP BY region"}); err != nil {
		t.Fatal(err)
	}
	m := bigtable.NewMutation()
	m.Set("o", "region", bigtable.Timestamp(1_000_000), []byte("sa"))
	if err := c.Open("orders").Apply(ctx, "o#6", m); err != nil {
		t.Fatal(err)
	}
	if rows = run("SELECT region, n FROM by_region", nil, nil); len(rows) != 4 {
		t.Errorf("materialized view = %d rows, want 4 (kept up to date)", len(rows))
	}
	if err := iac.CreateLogicalView(ctx, id, &bigtable.LogicalViewInfo{LogicalViewID: "bad", Query: "SELECT nope FROM orders"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a view over a missing column = %v, want InvalidArgument", err)
	}
	// Refusals.
	for sql, want := range map[string]codes.Code{
		"SELECT _key FROM orders ORDER BY o":                    codes.InvalidArgument,
		"SELECT _key FROM orders ORDER BY _key DESC":            codes.InvalidArgument,
		"SELECT * FROM UNPACK((SELECT * FROM orders))":          codes.Unimplemented,
		"SELECT SOUNDEX('x')":                                   codes.Unimplemented,
		"SELECT NOPE(1)":                                        codes.InvalidArgument,
		"SELECT _key, COUNT(*) FROM orders":                     codes.InvalidArgument,
		"SELECT _key FROM orders(after => CURRENT_TIMESTAMP())": codes.InvalidArgument,
		"SELECT CAST(o['qty'] AS INT64) FROM orders":            codes.InvalidArgument,
		"SELECT a._key FROM orders a JOIN orders b":             codes.InvalidArgument,
		"DELETE FROM orders":                                    codes.InvalidArgument,
	} {
		if _, err := c.PrepareStatement(ctx, sql, nil); status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
}

// TestBigtableUndeleteTable (#1511): a table deleted through CloudBurrow is
// kept for the seven days Google documents, and UndeleteTable brings it
// back with its families and cells; a table not deleted is NOT_FOUND.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/UndeleteTable
func TestBigtableUndeleteTable(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "undeleted") // asks for the Bigtable endpoint first
	ctx := h.Context()
	tbl := fmt.Sprintf("projects/%s/instances/%s/tables/undeleted", h.Project(), bigtableInstance)
	a := admin.TableAdminClientV2()
	if _, err := a.UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl}); status.Code(err) != codes.NotFound {
		t.Errorf("UndeleteTable of a live table = %v, want NotFound", err)
	}
	m := bigtable.NewMutation()
	m.Set("info", "name", bigtable.Timestamp(7000), []byte("kept"))
	if err := c.Open("undeleted").Apply(ctx, "r1", m); err != nil {
		t.Fatal(err)
	}
	if err := admin.DeleteTable(ctx, "undeleted"); err != nil {
		t.Fatal(err)
	}
	op, err := a.UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl})
	if err != nil {
		t.Fatalf("UndeleteTable: %v", err)
	}
	if got, err := op.Wait(ctx); err != nil || got.GetName() != tbl {
		t.Fatalf("UndeleteTable.Wait = %v, %v", got, err)
	}
	r, err := c.Open("undeleted").ReadRow(ctx, "r1")
	if err != nil || len(r["info"]) != 1 || string(r["info"][0].Value) != "kept" || r["info"][0].Timestamp != 7000 {
		t.Errorf("the undeleted row = %v, %v", r, err)
	}
}
