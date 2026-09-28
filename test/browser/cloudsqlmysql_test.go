//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestCloudSQLMySQLCreateQueryAndDropThroughThePage (#868), in the emulators
// shard, whose instance serves Cloud SQL for MySQL. Cloud SQL is one product
// whose pages are PostgreSQL and MySQL; the MySQL page lists the server's
// databases; Create database makes one, which the list
// shows. Its page's Query tab runs a SELECT and draws the rows, NULL as an em
// dash, and a CREATE TABLE there is refused with MySQL's own command-denied
// error and leaves the database with no tables, as the console API reads it.
// The row's Delete, confirmed by the database's name, drops it.
func TestCloudSQLMySQLCreateQueryAndDropThroughThePage(t *testing.T) {
	needService(t, "cloudsql-mysql")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	name := fmt.Sprintf("browser_%d", time.Now().UnixNano()%1e12)
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/cloudsql-mysql"+q+"&name="+name, "") })

	p.navigate("/cloudsql-mysql" + q)
	// One product, Cloud SQL, with a page per engine.
	p.waitFor(`document.querySelectorAll("#product-nav a").length > 0`)
	var nav struct {
		Title   string
		Pages   []string
		Current string
	}
	p.eval(`({ Title: document.querySelector("#product-nav .product-nav-title").textContent,
	           Pages: [...document.querySelectorAll("#product-nav a")].map((a) => a.textContent),
	           Current: document.querySelector("#product-nav a[aria-current=page]").textContent })`, &nav)
	if nav.Title != "Cloud SQL" || strings.Join(nav.Pages, ",") != "PostgreSQL,MySQL" || nav.Current != "MySQL" {
		t.Errorf("the product navigation is %+v, want Cloud SQL with PostgreSQL and MySQL, on MySQL", nav)
	}
	p.clickText("#view button.primary", "Create database")
	p.waitFor(`document.querySelector(".modal #f-database") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-database`, name, chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))

	// The database's page, on its Query tab.
	p.clickText("#view tbody a", name)
	p.waitFor(`document.querySelector("#tab-query") !== null`)
	p.run(chromedp.Click(`#tab-query`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view .query-editor") !== null`)
	var hint string
	p.eval(`document.querySelector("#view .query-pane .card > p.unavailable").textContent`, &hint)
	if !strings.Contains(hint, "START TRANSACTION READ ONLY") {
		t.Errorf("the editor's hint %q does not say how it stays read-only", hint)
	}

	runStatement := func(sql string) {
		p.eval(`(() => { const e = document.querySelector("#view .query-editor"); e.value = ""; return true; })()`, nil)
		p.run(chromedp.SendKeys(`#view .query-editor`, sql, chromedp.ByQuery))
		p.clickText("#view .query-pane button.primary", "Run")
	}
	runStatement("SELECT 41 + 1 AS answer, NULL AS nothing")
	p.waitFor(`document.querySelector("#view .query-results tbody tr") !== null`)
	var cells []string
	p.eval(`[...document.querySelectorAll("#view .query-results tbody tr:first-child td")].map((c) => c.textContent.trim())`, &cells)
	if !contains(cells, "42") || !contains(cells, "—") {
		t.Errorf("the SELECT drew the row %q, want 42 and an em dash for NULL", cells)
	}

	runStatement("CREATE TABLE made_in_the_editor (id INT)")
	p.waitFor(`[...document.querySelectorAll("#view .form-error[role=alert]")].some((e) => !e.hidden)`)
	var refusal string
	p.eval(`[...document.querySelectorAll("#view .form-error[role=alert]")].map((e) => e.textContent).join(" ")`, &refusal)
	if !strings.Contains(refusal, "Error 1142") || !strings.Contains(refusal, "CREATE command denied") {
		t.Errorf("CREATE TABLE in the editor was answered %q, want MySQL's command-denied error", refusal)
	}
	p.forgive()
	if tables := readDataPage(t, "cloudsql-mysql", project, name).rows("tables"); len(tables) != 0 {
		t.Errorf("the database has tables %v after a refused CREATE TABLE", tables)
	}

	// Back on the list, the row's Delete, confirmed by name.
	p.navigate("/cloudsql-mysql" + q)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name), chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, name, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, row))
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/cloudsql-mysql"+q, ""); code != http.StatusOK || strings.Contains(body, `"`+name+`"`) {
		t.Errorf("the console API still lists %s after Delete: %d %s", name, code, body)
	}
}
