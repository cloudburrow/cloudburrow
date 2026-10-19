//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestSpannerSchemaAndPartitionsThroughThePage (#1207), in a shard whose
// instance serves Spanner. A database's page has Update schema: the CREATE
// TABLE typed into its form is confirmed by typing the database's name back,
// a wrong name is refused with nothing sent, and once confirmed the console
// API lists the new table. The instance's page shows the Instance
// configurations tab with the one in use; Create instance partition there
// adds a row to the Instance partitions tab, which opens the partition's
// page; the row's Delete instance partition, confirmed by its name, removes
// it, as the console API reads it.
func TestSpannerSchemaAndPartitionsThroughThePage(t *testing.T) {
	needService(t, "spanner")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-%d", time.Now().UnixNano()%1e9)
	const db = "schema"
	body, _ := json.Marshal(map[string]string{"instance": inst, "database": db,
		"ddl": "CREATE TABLE Widgets (Id INT64 NOT NULL) PRIMARY KEY (Id)"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a Spanner database through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	const actions = "/api/actions/spanner"

	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.clickText("#view tbody a", db)

	// Update schema, confirmed with the database's name.
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Update schema")
	p.waitFor(`document.querySelector(".modal.is-open #f-statements") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-statements`), "CREATE TABLE Gadgets (Id INT64 NOT NULL) PRIMARY KEY (Id);"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector("#confirm-input") !== null && document.activeElement === document.querySelector("#confirm-input")`)
	var confirm string
	p.eval(`document.querySelector("#confirm-input").closest("form").textContent`, &confirm)
	if !strings.Contains(confirm, "database "+db+" in instance "+inst) {
		t.Errorf("the Update schema confirmation reads %q; want it to name database %s in instance %s", confirm, db, inst)
	}
	p.run(chromedp.SendKeys(chromedp.CSS(`#confirm-input`), "wrong"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector("#confirm-input").closest("form").querySelector(".form-error").hidden`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 0 {
		t.Fatalf("Update schema confirmed with the wrong name was sent: %v", sent)
	}
	p.eval(`(() => { const f = document.querySelector("#confirm-input"); f.value = ""; return true; })()`, nil)
	p.run(chromedp.SendKeys(chromedp.CSS(`#confirm-input`), db), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Fatalf("the confirmed Update schema sent %d requests, want 1: %v", len(sent), sent)
	}
	if tables := readDataPage(t, "spanner", project, inst, db).rows("tables"); tables["Gadgets"] == nil {
		t.Errorf("after Update schema the database's tables are %v, want Gadgets among them", tables)
	}

	// The instance's page: its configurations, and a partition made, opened
	// and deleted.
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.waitFor(`document.querySelector("#tab-instance-configs") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-instance-configs`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("emulator-config") && r.textContent.includes("Yes"))`)

	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Create instance partition")
	p.waitFor(`document.querySelector(".modal.is-open #f-partition") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-partition`), "browser-part"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#tab-partitions") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-partitions`)))
	// A row inside a page has no Inspect label; its link and its actions
	// button name it.
	const listed = `[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "browser-part")`
	p.waitFor(listed)
	if parts := readDataPage(t, "spanner", project, inst).rows("partitions"); parts["browser-part"] == nil {
		t.Fatalf("the console API does not list the partition created in the browser: %v", parts)
	}

	p.clickText("#view tbody a", "browser-part")
	p.waitFor(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.includes("/instancePartitions/browser-part"))`)
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.waitFor(`document.querySelector("#tab-partitions") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-partitions`)))
	p.waitFor(listed)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for browser-part"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete instance partition")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "browser-part"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && !` + listed)
	if parts := readDataPage(t, "spanner", project, inst).rows("partitions"); len(parts) != 0 {
		t.Errorf("the console API still lists partitions %v after Delete", parts)
	}
}
