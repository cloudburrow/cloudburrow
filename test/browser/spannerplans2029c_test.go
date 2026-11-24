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

// TestSpannerStudioRunOptionsAndQueryStatistics (#2029), in the emulators
// shard. Google's Spanner Studio
// (https://docs.cloud.google.com/spanner/docs/tune-query-with-visualizer):
// Run produces the results and the explanation, and its drop-down changes
// that to Results only or Explanation only; the Explanation's information
// panel has the Query summary (rows returned, the time the query took,
// prominent operators) and the Query execution timeline (how long each
// machine group ran its part of the query). Here: the default Run fills
// both tabs, with every operator's statistics, the Query summary and a
// timeline whose Root group ran the query; Results only fills Results
// and leaves the Explanation empty; Explanation only draws the plan with
// no statistics (PLAN mode) and no rows. The drop-down is reached and
// used with the keyboard, and the screen passes the WCAG checks.
func TestSpannerStudioRunOptionsAndQueryStatistics(t *testing.T) {
	needService(t, "spanner")
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-runopt-%d", time.Now().UnixNano()%1e8)
	body, _ := json.Marshal(map[string]string{"displayName": "Run options", "instance": inst, "edition": "Standard",
		"configType": "Regional", "regionalConfig": "regional-us-central1", "unit": "Nodes", "nodes": "1",
		"scaling": "Manual allocation"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create instance through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	mk, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "createdatabase", "Values": map[string]string{"database": "rundb",
		"ddl": "CREATE TABLE Singers (SingerId INT64 NOT NULL, FirstName STRING(1024)) PRIMARY KEY (SingerId);"}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(mk)); code != http.StatusOK {
		t.Fatalf("Create database through the console API = %d: %s", code, out)
	}
	ins, _ := json.Marshal(map[string]any{"instance": inst, "database": "rundb", "write": true,
		"sql": "INSERT INTO Singers (SingerId, FirstName) VALUES (1, 'Marc'), (2, 'Catalina'), (3, 'Alice'), (4, 'Lea')"})
	if code, out := consoleDo(t, http.MethodPost, "/api/studio/spanner"+q+"&call=run", string(ins)); code != http.StatusOK || !strings.Contains(out, "4 row") {
		t.Fatalf("insert through Spanner Studio = %d: %s", code, out)
	}
	// The API: Results only is NORMAL mode, with no plan and no statistics.
	ro, _ := json.Marshal(map[string]any{"instance": inst, "database": "rundb", "mode": "results", "sql": "SELECT FirstName FROM Singers"})
	if code, out := consoleDo(t, http.MethodPost, "/api/studio/spanner"+q+"&call=run", string(ro)); code != http.StatusOK ||
		!strings.Contains(out, "Catalina") || strings.Contains(out, `"plan"`) || strings.Contains(out, `"stats"`) {
		t.Fatalf("Results only through the console API = %d: %s; want the rows, no plan and no statistics", code, out)
	}

	p := open(t)
	p.navigate("/spanner/" + inst + "/rundb" + q)
	p.waitFor(`document.querySelector("#view .side-nav #tab-studio") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	p.waitFor(`document.querySelector("#spanner-explorer").textContent.includes("Singers")`)
	p.eval(`(() => { try { sessionStorage.clear(); } catch {} return true })()`, nil)
	setSQL := func(sql string) {
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#spanner-editor"); e.value = %q;
			e.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sql), nil)
	}
	pick := func(label string) {
		p.run(chromedp.Click(chromedp.CSS(`#spanner-run-options`)))
		p.waitFor(`!document.querySelector("#spanner-run-options").nextElementSibling.hidden`)
		p.eval(fmt.Sprintf(`(() => { [...document.querySelector("#spanner-run-options").nextElementSibling.querySelectorAll("[role=menuitemcheckbox]")]
			.find((b) => b.textContent.includes(%q)).click(); return true })()`, label), nil)
	}
	setSQL("SELECT SingerId, FirstName FROM Singers WHERE SingerId > 1")

	// Run, by default: the results and the explanation, with statistics.
	var runText string
	p.eval(`document.querySelector("#spanner-run").textContent`, &runText)
	if strings.TrimSpace(runText) != "Run" {
		t.Errorf("the Run button reads %q; want Run", runText)
	}
	p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
	p.waitFor(`(document.querySelector("#spanner-results") || {}).textContent?.includes("Catalina")`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-explanation`)))
	p.waitFor(`document.querySelectorAll(".spanner-plan-card").length > 0`)
	p.waitFor(`[...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Table scan") && c.textContent.includes("Rows returned: 3"))`)
	var summary map[string]string
	p.eval(`Object.fromEntries([...document.querySelectorAll("#spanner-query-summary dt")].map((d) => [d.textContent, d.nextElementSibling.textContent]))`, &summary)
	if summary["Rows returned"] != "3" || summary["Total elapsed time"] == "" {
		t.Errorf("the Query summary is %v; want 3 rows returned and the elapsed time", summary)
	}
	var timeline []string
	p.eval(`[...document.querySelectorAll("#spanner-query-timeline .spanner-timeline-row")].map((r) => r.textContent)`, &timeline)
	if len(timeline) != 1 || !strings.HasPrefix(timeline[0], "Root") || !strings.Contains(timeline[0], " to ") {
		t.Errorf("the Query execution timeline is %q; want one row, the Root machine group's start and end", timeline)
	}
	p.waitFor(`document.querySelector("#spanner-query-timeline h4").textContent === "Query execution timeline"`)

	t.Run("WCAG", func(t *testing.T) {
		for _, u := range p.unnamedControls() {
			t.Errorf("Spanner Studio's Explanation: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("Spanner Studio's Explanation: %s", c)
		}
	})

	// Results only: the rows, and no explanation.
	pick("Results only")
	p.waitFor(`document.querySelector("#spanner-run").textContent.trim() === "Run (results only)"`)
	setSQL("SELECT FirstName FROM Singers WHERE SingerId = 4")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
	p.waitFor(`(document.querySelector("#spanner-results") || {}).textContent?.includes("Lea")`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-explanation`)))
	p.waitFor(`document.querySelectorAll(".spanner-plan-card").length === 0 && document.querySelector("#spanner-results").textContent.includes("Explain a query")`)

	// Explanation only, chosen with the keyboard: the plan, no statistics.
	t.Run("keyboard", func(t *testing.T) {
		p.eval(`(() => { document.querySelector("#spanner-editor").focus(); return true })()`, nil)
		p.tabTo(`a.id === "spanner-run-options"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`!document.querySelector("#spanner-run-options").nextElementSibling.hidden`)
		p.waitFor(`document.activeElement && document.activeElement.getAttribute("role") === "menuitemcheckbox"`)
		for i := 0; i < 3; i++ {
			var label string
			p.eval(`document.activeElement.textContent`, &label)
			if strings.Contains(label, "Explanation only") {
				break
			}
			p.run(chromedp.KeyEvent(kb.ArrowDown))
		}
		p.waitFor(`document.activeElement.textContent.includes("Explanation only")`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector("#spanner-run").textContent.trim() === "Run (explanation only)"`)
		p.waitFor(`document.activeElement && document.activeElement.id === "spanner-run-options"`)
	})
	p.waitFor(`document.querySelector("#spanner-run").textContent.trim() === "Run (explanation only)"`)
	setSQL("SELECT FirstName FROM Singers WHERE SingerId = 2")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
	p.waitFor(`document.querySelector("#spanner-tab-explanation").getAttribute("aria-selected") === "true"`)
	p.waitFor(`[...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Table scan"))`)
	p.waitFor(`![...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Rows returned"))`)
	p.waitFor(`document.querySelector("#spanner-query-timeline") === null`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-results`)))
	p.waitFor(`document.querySelector("#spanner-results").textContent.includes("Run a statement to see its results.")`)

	// The choice is kept for the page, as Google's drop-down keeps it.
	p.navigate("/spanner/" + inst + "/rundb" + q)
	p.waitFor(`document.querySelector("#view .side-nav #tab-studio") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	p.waitFor(`(document.querySelector("#spanner-run") || {}).textContent?.trim() === "Run (explanation only)"`)
	pick("Results and explanation")
	p.waitFor(`document.querySelector("#spanner-run").textContent.trim() === "Run"`)
}
