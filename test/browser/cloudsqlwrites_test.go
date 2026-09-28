//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestCloudSQLReadWriteEditorsThroughThePage (#995), in the emulators shard,
// whose instance serves both Cloud SQL engines. On each engine's cloudburrow
// database page, the Query tab starts Read-only, and a CREATE TABLE there is
// the database's own refusal: PostgreSQL's read-only transaction, MySQL's
// command-denied error for the read-only account. Switched to Read-write,
// the Run button reads Run statement and the hint names the application's
// user; running the CREATE TABLE asks first, naming the database and the engine and showing
// the statement, and Cancel sends nothing. Confirmed, it answers in the
// database's words, and the database's Tables tab, read through the console
// API, lists the table; an INSERT of two rows answers 2 rows affected, and a
// read-only SELECT reads them back. The table is dropped the same way.
func TestCloudSQLReadWriteEditorsThroughThePage(t *testing.T) {
	for _, engine := range []struct {
		service, name, readOnly, created, inserted, dropped string
	}{
		{"cloudsql", "PostgreSQL", "read-only transaction", "Committed. PostgreSQL answered CREATE TABLE.",
			"Committed. PostgreSQL answered INSERT 0 2: 2 rows affected.", "Committed. PostgreSQL answered DROP TABLE."},
		{"cloudsql-mysql", "MySQL", "Error 1142", "Committed as cloudburrow. MySQL answered: 0 rows affected.",
			"Committed as cloudburrow. MySQL answered: 2 rows affected.", "Committed as cloudburrow. MySQL answered: 0 rows affected."},
	} {
		t.Run(engine.name, func(t *testing.T) {
			needService(t, engine.service)
			p := open(t)
			project := uniqueProject(t)
			q := "?project=" + url.QueryEscape(project)
			table := fmt.Sprintf("browser_rw_%d", time.Now().UnixNano()%1e12)
			t.Cleanup(func() {
				body, _ := json.Marshal(map[string]any{"Path": []string{"cloudburrow"}, "Mode": "read-write",
					"Statement": "DROP TABLE IF EXISTS " + table})
				consoleDo(t, http.MethodPost, "/api/query/"+engine.service+q, string(body))
			})

			p.navigate("/" + engine.service + "/cloudburrow" + q)
			p.waitFor(`document.querySelector("#tab-query") !== null`)
			p.run(chromedp.Click(`#tab-query`, chromedp.ByQuery))
			p.waitFor(`document.querySelector("#view .query-editor") !== null && document.querySelector("#view .query-mode") !== null`)
			statement := func(sql string) {
				p.eval(`(() => { const e = document.querySelector("#view .query-editor"); e.value = ""; return true; })()`, nil)
				p.run(chromedp.SendKeys(`#view .query-editor`, sql, chromedp.ByQuery))
			}
			alert := func() string {
				p.waitFor(`[...document.querySelectorAll("#view .form-error[role=alert]")].some((e) => !e.hidden)`)
				var text string
				p.eval(`[...document.querySelectorAll("#view .form-error[role=alert]")].map((e) => e.textContent).join(" ")`, &text)
				p.forgive()
				return text
			}
			confirmed := func(want string) {
				t.Helper()
				p.clickText("#view .query-pane button.primary", "Run statement")
				p.waitFor(`document.querySelector("#dml-confirm-title") !== null`)
				p.run(chromedp.Click(`.modal button[type=submit]`, chromedp.ByQuery))
				p.waitFor(fmt.Sprintf(`document.querySelector("#view .query-results").textContent.includes(%q)`, want))
			}

			create := "CREATE TABLE " + table + " (id integer PRIMARY KEY, name varchar(40))"
			statement(create)
			p.clickText("#view .query-pane button.primary", "Run")
			if got := alert(); !strings.Contains(got, engine.readOnly) {
				t.Errorf("a CREATE TABLE in Read-only was answered %q, want %q", got, engine.readOnly)
			}

			p.run(chromedp.Click(`#view .query-mode input[value="read-write"]`, chromedp.ByQuery))
			p.waitFor(`document.querySelector("#view .query-pane button.primary").textContent === "Run statement"`)
			var hint string
			p.eval(`document.querySelector("#view .query-pane .card > p.unavailable").textContent`, &hint)
			if !strings.Contains(hint, "runs as cloudburrow") {
				t.Errorf("the Read-write hint is %q, want it to name the application's user", hint)
			}
			statement(create)
			before := len(p.posts("/api/query/" + engine.service))
			p.clickText("#view .query-pane button.primary", "Run statement")
			p.waitFor(`document.querySelector("#dml-confirm-title") !== null`)
			var confirm struct{ Title, Statement, Button string }
			p.eval(`({ Title: document.querySelector("#dml-confirm-title").textContent,
				Statement: document.querySelector(".modal pre").textContent,
				Button: document.querySelector(".modal button[type=submit]").textContent })`, &confirm)
			if confirm.Title != "Write to database cloudburrow on the local "+engine.name+" server?" ||
				confirm.Statement != create || confirm.Button != "Run statement" {
				t.Errorf("the confirmation is %+v", confirm)
			}
			p.clickText(".modal button", "Cancel")
			p.waitFor(`document.querySelector(".modal") === null`)
			if n := len(p.posts("/api/query/" + engine.service)); n != before {
				t.Errorf("Cancel sent the statement (%d requests)", n-before)
			}
			confirmed(engine.created)
			if tables := readDataPage(t, engine.service, project, "cloudburrow").rows("tables"); tables[table] == nil {
				t.Errorf("after the confirmed CREATE TABLE the database lists %v", tables)
			}
			statement("INSERT INTO " + table + " (id, name) VALUES (1, 'one'), (2, 'two')")
			confirmed(engine.inserted)

			p.run(chromedp.Click(`#view .query-mode input[value="read-only"]`, chromedp.ByQuery))
			p.waitFor(`document.querySelector("#view .query-pane button.primary").textContent === "Run"`)
			statement("SELECT name FROM " + table + " ORDER BY id")
			p.clickText("#view .query-pane button.primary", "Run")
			p.waitFor(`document.querySelectorAll("#view .query-results tbody tr").length === 2`)
			var names []string
			p.eval(`[...document.querySelectorAll("#view .query-results tbody tr")].map((r) => r.textContent.trim())`, &names)
			if strings.Join(names, ",") != "one,two" {
				t.Errorf("the read-only SELECT reads %v, want one and two", names)
			}

			p.run(chromedp.Click(`#view .query-mode input[value="read-write"]`, chromedp.ByQuery))
			statement("DROP TABLE " + table)
			confirmed(engine.dropped)
			if tables := readDataPage(t, engine.service, project, "cloudburrow").rows("tables"); tables[table] != nil {
				t.Errorf("after the confirmed DROP TABLE the database still lists %s", table)
			}
		})
	}
}
