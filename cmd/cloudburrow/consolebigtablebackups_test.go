package main

import (
	"context"
	"net/http"
	"testing"

	"cloud.google.com/go/bigtable"
)

// TestConsoleBigtableBackupsAndViews (#1499): a table page's Create backup,
// Restore backup to a new table and Create authorized view, over the
// Bigtable front, read back through the official clients.
func TestConsoleBigtableBackupsAndViews(t *testing.T) {
	srv, addr := bigtablePickerConsole(t)
	ctx := context.Background()
	admin, err := bigtable.NewAdminClient(ctx, "demo", bigtableInstance, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, "demo", bigtableInstance, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := admin.CreateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "orders", "cf"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"eu#1", "us#1"} {
		m := bigtable.NewMutation()
		m.Set("cf", "total", bigtable.Now(), []byte("9"))
		if err := client.Open("orders").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	act := func(action string, values map[string]string) (int, string) {
		return pickerDo(t, srv, http.MethodPost, "/api/actions/bigtable?project=demo",
			map[string]any{"Path": []string{"orders"}, "Action": action, "Values": values})
	}
	if code, out := act("backup", map[string]string{"cluster": "c1", "backup": "nightly", "days": "7"}); code != http.StatusOK {
		t.Fatalf("Create backup = %d: %s", code, out)
	}
	if info, err := admin.BackupInfo(ctx, "c1", "nightly"); err != nil || info.SourceTable != "orders" {
		t.Errorf("BackupInfo = %+v, %v", info, err)
	}
	if code, out := act("backup", map[string]string{"cluster": "c1", "backup": "x", "days": "400"}); code != http.StatusBadRequest {
		t.Errorf("a 400-day backup = %d: %s", code, out)
	}
	if code, out := act("restore", map[string]string{"cluster": "c1", "backup": "nightly", "table": "orders-restored"}); code != http.StatusOK {
		t.Fatalf("Restore backup = %d: %s", code, out)
	}
	n := 0
	if err := client.Open("orders-restored").ReadRows(ctx, bigtable.InfiniteRange(""), func(bigtable.Row) bool { n++; return true }); err != nil || n != 2 {
		t.Errorf("the restored table has %d rows, %v; want 2", n, err)
	}
	if code, out := act("authview", map[string]string{"view": "eu", "prefixes": "eu#", "columns": "cf:*"}); code != http.StatusOK {
		t.Fatalf("Create authorized view = %d: %s", code, out)
	}
	var keys []string
	if err := client.OpenAuthorizedView("orders", "eu").ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil || len(keys) != 1 || keys[0] != "eu#1" {
		t.Errorf("rows through the view = %v, %v; want [eu#1]", keys, err)
	}
	if code, out := act("authview", map[string]string{"view": "bad", "columns": "nofamily"}); code != http.StatusBadRequest {
		t.Errorf("a column with no family = %d: %s", code, out)
	}
}
