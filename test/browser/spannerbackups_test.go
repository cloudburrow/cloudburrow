//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestSpannerBackupsThroughThePage (#1498), in a shard whose instance serves
// Spanner. An instance's page has Create backup, whose database is chosen
// from the instance's: the backup made there is listed on the Backups tab,
// as the console API reads it, and its row opens the backup's page, whose
// Restore to a new database makes a database the instance's page lists. A
// database's page has Create backup schedule, which adds a row to its Backup
// schedules tab.
func TestSpannerBackupsThroughThePage(t *testing.T) {
	needService(t, "spanner")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-bk-%d", time.Now().UnixNano()%1e8)
	const db = "orders"
	body, _ := json.Marshal(map[string]string{"instance": inst, "database": db,
		"ddl": "CREATE TABLE Orders (Id INT64 NOT NULL, Item STRING(MAX)) PRIMARY KEY (Id)"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a Spanner database through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst, "Backups", "nightly"}, "Action": "deletebackup"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
		c, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(c))
	})

	// Create backup at the top of the instance's Backup/Restore tab (#1859).
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#tab-backups") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backups`)))
	p.clickText("#view #section-actions-backups button", "Create backup")
	p.waitFor(`document.querySelector(".modal.is-open #f-backup") !== null`)
	var dbChoice string
	p.eval(`document.querySelector(".modal.is-open #f-database").value`, &dbChoice)
	if dbChoice != db {
		t.Errorf("Create backup's database is %q, want %s chosen", dbChoice, db)
	}
	// Google's Expiration date is a date picker a week out, and Create
	// backup from offers an earlier point in time, its version time.
	var expiry struct {
		Type, Value string
		Earlier     bool
		VersionShow bool
	}
	p.eval(`(() => { const e = document.querySelector(".modal.is-open #f-expireDate");
		const v = document.querySelector(".modal.is-open #f-versionTime");
		return { Type: e.type, Value: e.value,
			Earlier: [...document.querySelectorAll(".modal.is-open input[type=radio]")].some((r) => r.value === "Earlier point in time"),
			VersionShow: !!v && !v.closest("[hidden]") } })()`, &expiry)
	if want := time.Now().UTC().AddDate(0, 0, 7).Format(time.DateOnly); expiry.Type != "date" || expiry.Value != want || !expiry.Earlier || expiry.VersionShow {
		t.Errorf("Create backup's expiration is %+v; want a date picker at %s, an Earlier point in time choice, and no version time shown for Current time", expiry, want)
	}
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-backup`), "nightly"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)

	p.waitFor(`document.querySelector("#tab-backups") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backups`)))
	const listed = `[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "nightly")`
	p.waitFor(listed)
	if b := readDataPage(t, "spanner", project, inst).rows("backups")["nightly"]; b == nil || b["Database"] != db {
		t.Fatalf("the console API lists backup nightly as %v, want it of database %s", b, db)
	}

	// The backup's page: Restore to a new database.
	// The create's snackbar can lie over the row: follow the link itself.
	var opened bool
	p.eval(`(() => { const a = [...document.querySelectorAll("#view tbody a")].find((a) => a.textContent === "nightly"); a.click(); return true })()`, &opened)
	p.waitFor(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.includes("/backups/nightly"))`)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Restore to a new database")
	p.waitFor(`document.querySelector(".modal.is-open #f-database") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-database`), "restored"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if dbs := readDataPage(t, "spanner", project, inst).rows("databases"); dbs["restored"] == nil {
		t.Fatalf("after Restore the instance's databases are %v, want restored among them", dbs)
	}

	// A database's page: Create backup schedule.
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.clickText("#view tbody a", db)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Create backup schedule")
	p.waitFor(`document.querySelector(".modal.is-open #f-schedule") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-schedule`), "daily"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#tab-backup-schedules") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backup-schedules`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("daily") && r.textContent.includes("0 2 * * *"))`)
	if s := readDataPage(t, "spanner", project, inst, db).rows("backup-schedules")["daily"]; s == nil || s["Cron"] != "0 2 * * *" {
		t.Errorf("the console API lists schedule daily as %v, want cron 0 2 * * *", s)
	}
}
