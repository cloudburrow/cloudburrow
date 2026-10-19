//go:build browser

package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBigtableChangeStreamThroughThePage (#1591), in the emulators shard.
// On a table's page, Edit change stream with a retention of 1 day enables
// the stream, as the admin client's TableInfo reads it; a row written
// after it with the official client is listed in the table's Change
// stream section with its SetCell mutation; Edit change stream with 0
// disables it again.
func TestBigtableChangeStreamThroughThePage(t *testing.T) {
	needService(t, "bigtable")
	project := uniqueProject(t)
	admin, client, table := bigtableClients(t, project)
	p := open(t)
	q := "?project=" + project
	ctx := context.Background()
	edit := func(retention string) {
		t.Helper()
		p.navigate("/bigtable/" + table + q)
		p.clickText("#view .page-actions button", "Edit change stream")
		p.waitFor(`document.querySelector(".modal #f-retention") !== null`)
		p.setField(".modal #f-retention", retention)
		p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal") === null`)
	}

	edit("1")
	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if d, _ := info.ChangeStreamRetention.(time.Duration); d != 24*time.Hour {
		t.Fatalf("after Edit change stream 1, TableInfo reads retention %v, want 24h", info.ChangeStreamRetention)
	}
	bigtableWrite(t, client, table, "streamed-row", map[string][]byte{"cf:v": []byte("x")})

	var section string
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		p.navigate("/bigtable/" + table + q)
		p.waitFor(`document.querySelector("#tab-changestream") !== null`)
		p.clickText("#tab-changestream", "Change stream")
		p.eval(`[...document.querySelectorAll("#view [role=tabpanel]")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &section)
		if strings.Contains(section, "streamed-row") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Change stream section reads %q; want the row written after the stream was enabled", section)
		}
	}
	if !strings.Contains(section, "SetCell") {
		t.Errorf("the Change stream section reads %q; want the row's SetCell mutation", section)
	}

	edit("0")
	if info, err = admin.TableInfo(ctx, table); err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if d, _ := info.ChangeStreamRetention.(time.Duration); d != 0 {
		t.Errorf("after Edit change stream 0, TableInfo reads retention %v, want none", info.ChangeStreamRetention)
	}
}
