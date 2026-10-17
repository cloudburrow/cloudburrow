package bigtablefront

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSQL runs GoogleSQL for Bigtable through the official client's
// PrepareStatement, Bind and Execute over the emulator's rows.
func TestSQL(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opt := option.WithGRPCConn(dial(t, addr))
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "users"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "users", "info"); err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{"u#1", "u#2", "x#1"} {
		m := bigtable.NewMutation()
		m.Set("info", "name", bigtable.Timestamp(1000), []byte("old"))
		m.Set("info", "name", bigtable.Timestamp(2000), []byte("name-"+k))
		n := make([]byte, 8)
		binary.BigEndian.PutUint64(n, uint64(i+1))
		m.Set("info", "n", bigtable.Timestamp(1000), n)
		if err := c.Open("users").Apply(ctx, k, m); err != nil {
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
	rows := run("SELECT _key, CAST(info['name'] AS STRING) AS name, TO_INT64(info['n']) AS n FROM users WHERE STARTS_WITH(_key, @p)",
		map[string]bigtable.SQLType{"p": bigtable.BytesSQLType{}}, map[string]any{"p": []byte("u#")})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	var key []byte
	var name string
	var n int64
	if err := rows[1].GetByName("_key", &key); err != nil || string(key) != "u#2" {
		t.Errorf("_key = %q, %v", key, err)
	}
	if err := rows[1].GetByName("name", &name); err != nil || name != "name-u#2" {
		t.Errorf("name = %q, %v", name, err)
	}
	if err := rows[1].GetByName("n", &n); err != nil || n != 2 {
		t.Errorf("n = %d, %v", n, err)
	}
	all := run("SELECT * FROM users LIMIT 2", nil, nil)
	if len(all) != 2 {
		t.Fatalf("SELECT * LIMIT 2 = %d rows", len(all))
	}
	// The client keys a BYTES-keyed map by the keys' base64.
	var fam map[string][]byte
	if err := all[0].GetByName("info", &fam); err != nil || string(fam[base64.StdEncoding.EncodeToString([]byte("name"))]) != "name-u#1" || len(fam) != 2 {
		t.Errorf("info = %v, %v", fam, err)
	}
	if got := run("SELECT _key FROM users WHERE _key = 'x#1'", nil, nil); len(got) != 1 {
		t.Errorf("_key = 'x#1' = %d rows", len(got))
	}
	if _, err := c.PrepareStatement(ctx, "SELECT COUNT(*) FROM users", nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an aggregate = %v, want InvalidArgument", err)
	}
	if _, err := c.PrepareStatement(ctx, "SELECT nope['x'] FROM users", nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an unknown family = %v, want InvalidArgument", err)
	}
	if _, err := c.PrepareStatement(ctx, "SELECT _key FROM missing", nil); status.Code(err) != codes.NotFound {
		t.Errorf("a missing table = %v, want NotFound", err)
	}
}
