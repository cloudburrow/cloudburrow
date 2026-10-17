//go:build browser

package browser

import (
	"context"
	"strings"
	"testing"

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
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if info, err := admin.BackupInfo(ctx, "cloudburrow-c1", "b1"); err != nil || info.SourceTable != table {
		t.Fatalf("BackupInfo = %+v, %v", info, err)
	}

	restored := table + "-r"
	p.clickText("#view .page-actions button", "Restore backup to a new table")
	p.waitFor(`document.querySelector(".modal #f-table") !== null`)
	p.setField(".modal #f-backup", "b1")
	p.setField(".modal #f-table", restored)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
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
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	var keys []string
	if err := client.OpenAuthorizedView(table, "as").ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil || strings.Join(keys, ",") != "a1,a2" {
		t.Errorf("through the view ReadRows reads %v, %v; want a1,a2", keys, err)
	}
}
