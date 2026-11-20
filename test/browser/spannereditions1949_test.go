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
)

// TestSpannerEditionsThroughThePage (#1949), in a shard whose instance
// serves Spanner: Google's Create instance page selects the Enterprise
// edition and Autoscaling, whose minimum, maximum and targets it sends with
// default backup schedules on; the instance's page shows the edition, the
// autoscaling limits and targets, its compute capacity at the minimum, and
// default backup schedules Enabled. A database made in it has the default
// backup schedule on its Backup schedules tab, and Create backup schedule
// makes an incremental schedule (Backup type) the tab lists as Incremental.
func TestSpannerEditionsThroughThePage(t *testing.T) {
	needService(t, "spanner")
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-ed-%d", time.Now().UnixNano()%1e8)
	const db = "eddb"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})

	p := open(t)
	p.navigate("/spanner" + q)
	p.clickText("#view button", "Create instance")
	p.waitFor(`location.pathname === "/spanner/create" && document.querySelector("#view #f-displayName") !== null`)
	var disabled []string
	p.eval(`[...document.querySelectorAll("#view input[type=radio]:disabled")].map((r) => r.value)`, &disabled)
	if strings.Contains(strings.Join(disabled, ","), "Enterprise") || strings.Contains(strings.Join(disabled, ","), "Autoscaling") {
		t.Errorf("the Enterprise editions or Autoscaling are drawn disabled: %v", disabled)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view #f-edition-1`))) // Enterprise
	setField(p, "#view", "f-displayName", "Editions in browser")
	setField(p, "#view", "f-instance", inst)
	p.run(chromedp.Click(chromedp.CSS(`#view #f-scaling-1`))) // Autoscaling
	p.waitFor(`(() => { const m = document.querySelector("#view #f-minimum"); return m !== null && m.offsetParent !== null })()`)
	setField(p, "#view", "f-minimum", "2")
	setField(p, "#view", "f-maximum", "8")
	setField(p, "#view", "f-highPriorityCPU", "60")
	setField(p, "#view", "f-storageTarget", "90")
	var checked bool
	p.eval(`document.querySelector("#view #f-defaultBackups").checked`, &checked)
	if !checked {
		t.Error("Enable default backup schedules is not checked by default, as Google's console has it")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname !== "/spanner/create"`)

	page := readDataPage(t, "spanner", project, inst)
	if got := page.summary("Edition"); got != "Enterprise" {
		t.Errorf("the instance's Edition = %q, want Enterprise", got)
	}
	if got := page.summary("Autoscaling"); !strings.Contains(got, "2 to 8 nodes") || !strings.Contains(got, "high priority CPU target 60%") || !strings.Contains(got, "storage target 90%") {
		t.Errorf("the instance's Autoscaling = %q, want 2 to 8 nodes and its targets", got)
	}
	if got := page.summary("Compute capacity"); !strings.Contains(got, "2 nodes") {
		t.Errorf("the instance's Compute capacity = %q, want its 2-node minimum", got)
	}
	if got := page.summary("Default backup schedules"); got != "Enabled" {
		t.Errorf("the instance's Default backup schedules = %q, want Enabled", got)
	}

	body, _ := json.Marshal(map[string]string{"instance": inst, "database": db,
		"ddl": "CREATE TABLE Orders (Id INT64 NOT NULL) PRIMARY KEY (Id)"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a database through the console API = %d: %s", code, out)
	}
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.clickText("#view tbody a", db)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Create backup schedule")
	p.waitFor(`document.querySelector(".modal.is-open #f-schedule") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-schedule`), "sixhourly"))
	setField(p, ".modal.is-open", "f-cron", "0 */6 * * *")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-backupType-1`))) // Incremental backup
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#tab-backup-schedules") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backup-schedules`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("sixhourly") && r.textContent.includes("Incremental"))`)
	rows := readDataPage(t, "spanner", project, inst, db).rows("backup-schedules")
	if s := rows["sixhourly"]; s == nil || s["Type"] != "Incremental" || s["Cron"] != "0 */6 * * *" {
		t.Errorf("the console API lists sixhourly as %v, want an incremental schedule", s)
	}
	if s := rows["default-backup-schedule"]; s == nil || s["Type"] != "Full" || s["Retention"] != "7 days" {
		t.Errorf("the default backup schedule is %v, want full backups kept 7 days (rows %v)", s, rows)
	}
}
