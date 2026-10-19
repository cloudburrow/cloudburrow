//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"github.com/chromedp/chromedp"
)

// TestBigtableBackupRestoreAndAuthorizedViewThroughTheBrowser (#1499). On a
// table's page, Create backup, then Restore backup to a new table: the
// official client reads the table's rows in the restored one. Create
// authorized view with the prefix a: the client reads a1 and a2 alone
// through it.
func TestBigtableBackupRestoreAndAuthorizedViewThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable")
	project := uniqueProject(t)
	admin, client, table := bigtableClients(t, project)
	p := open(t)
	q := "?project=" + project
	for _, r := range []string{"a1", "a2", "b1"} {
		bigtableWrite(t, client, table, r, map[string][]byte{"cf:v": []byte(r)})
	}
	ctx := context.Background()

	p.navigate("/bigtable/" + table + q)
	p.clickText("#view .page-actions button", "Create backup")
	p.waitFor(`document.querySelector(".modal #f-backup") !== null`)
	p.setField(".modal #f-backup", "b1")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if info, err := admin.BackupInfo(ctx, "cloudburrow-c1", "b1"); err != nil || info.SourceTable != table {
		t.Fatalf("BackupInfo = %+v, %v", info, err)
	}

	restored := table + "-r"
	p.clickText("#view .page-actions button", "Restore backup to a new table")
	p.waitFor(`document.querySelector(".modal #f-table") !== null`)
	p.setField(".modal #f-backup", "b1")
	p.setField(".modal #f-table", restored)
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), restored) })
	if keys := bigtableRowKeys(t, client, restored); strings.Join(keys, ",") != "a1,a2,b1" {
		t.Errorf("the restored table reads %v, want a1,a2,b1", keys)
	}

	p.clickText("#view .page-actions button", "Create authorized view")
	p.waitFor(`document.querySelector(".modal #f-view") !== null`)
	p.setField(".modal #f-view", "as")
	p.setField(".modal #f-prefixes", "a")
	p.setField(".modal #f-columns", "cf:*")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	var keys []string
	if err := client.OpenAuthorizedView(table, "as").ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil || strings.Join(keys, ",") != "a1,a2" {
		t.Errorf("through the view ReadRows reads %v, %v; want a1,a2", keys, err)
	}
}

// TestBigtableLogicalViewAndSnapshotThroughTheBrowser (#1499). On an
// instance's page, Create logical view: the instance admin client reads
// its query. On a table's page in that instance, Snapshot table: the admin
// client reads the snapshot.
func TestBigtableLogicalViewAndSnapshotThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable-instances")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the admin API itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	project := uniqueProject(t)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-views-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Browser views",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })
	admin, err := bigtable.NewAdminClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateTable(ctx, "items"); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/bigtable-instances/" + id + "?project=" + project)
	p.clickText("#view .page-actions button", "Create logical view")
	p.waitFor(`document.querySelector(".modal #f-view") !== null`)
	p.setField(".modal #f-view", "lv")
	p.setField(".modal #f-query", "SELECT _key FROM items")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if lv, err := iac.LogicalViewInfo(ctx, id, "lv"); err != nil || lv.Query != "SELECT _key FROM items" {
		t.Errorf("after Create logical view, LogicalViewInfo = %+v, %v", lv, err)
	}

	p.navigate("/bigtable/items?project=" + project + "&instance=" + id)
	p.clickText("#view .page-actions button", "Snapshot table")
	p.waitFor(`document.querySelector(".modal #f-snapshot") !== null`)
	p.setField(".modal #f-cluster", id+"-c1")
	p.setField(".modal #f-snapshot", "snap")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if s, err := admin.SnapshotInfo(ctx, id+"-c1", "snap"); err != nil || s.SourceTable != "items" {
		t.Errorf("after Snapshot table, SnapshotInfo = %+v, %v", s, err)
	}
}
