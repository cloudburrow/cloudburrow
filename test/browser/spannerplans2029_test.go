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

// TestSpannerQueryPlanView (#2029), in the emulators shard, whose instance
// serves Spanner. Spanner Studio's Explanation tab draws a query's plan as
// Google's query plan visualizer does
// (https://docs.cloud.google.com/spanner/docs/tune-query-with-visualizer):
// after Run (PROFILE mode), a card per operator, top-down from the
// Distributed union through Serialize result to the Table scan "on
// Singers" marked "Full scan" with its warning, the local distributed
// union hidden; each card's Rows returned, Latency and CPU time; the
// Highest row count chip; the Query summary (Total elapsed time, CPU time,
// Rows returned, Rows scanned) and Prominent operators; Expanded and
// Compact; and Download JSON. Explain (PLAN mode) of a query through an
// index draws its Index scan, and selecting a card shows its Iterator
// information, with the Seek Condition. An INTERSECT and a TABLESAMPLE
// query draw their operators too.
func TestSpannerQueryPlanView(t *testing.T) {
	needService(t, "spanner")
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-plan-%d", time.Now().UnixNano()%1e8)
	body, _ := json.Marshal(map[string]string{"displayName": "Plan instance", "instance": inst, "edition": "Standard",
		"configType": "Regional", "regionalConfig": "regional-us-central1", "unit": "Nodes", "nodes": "1",
		"scaling": "Manual allocation"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create instance through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	mk, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "createdatabase", "Values": map[string]string{"database": "plandb",
		"ddl": "CREATE TABLE Singers (SingerId INT64 NOT NULL, FirstName STRING(1024), LastName STRING(1024)) PRIMARY KEY (SingerId);\nCREATE INDEX SingersByLastName ON Singers(LastName);"}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(mk)); code != http.StatusOK {
		t.Fatalf("Create database through the console API = %d: %s", code, out)
	}
	ins, _ := json.Marshal(map[string]any{"instance": inst, "database": "plandb", "write": true,
		"sql": "INSERT INTO Singers (SingerId, FirstName, LastName) VALUES (1, 'Marc', 'Richards'), (2, 'Catalina', 'Smith'), (3, 'Alice', 'Trentor')"})
	if code, out := consoleDo(t, http.MethodPost, "/api/studio/spanner"+q+"&call=run", string(ins)); code != http.StatusOK || !strings.Contains(out, "3 row") {
		t.Fatalf("insert through Spanner Studio = %d: %s", code, out)
	}

	p := open(t)
	p.navigate("/spanner/" + inst + "/plandb" + q)
	p.waitFor(`document.querySelector("#view .side-nav #tab-studio") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	p.waitFor(`document.querySelector("#spanner-explorer").textContent.includes("Singers")`)
	setSQL := func(sql string) {
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#spanner-editor"); e.value = %q;
			e.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sql), nil)
	}
	setSQL("SELECT SingerId, FirstName FROM Singers")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
	p.waitFor(`(document.querySelector("#spanner-results") || {}).textContent?.includes("Catalina")`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-explanation`)))
	p.waitFor(`document.querySelectorAll(".spanner-plan-card").length > 0`)

	var cards []struct {
		Name, Text string
		Depth      int
	}
	p.eval(`[...document.querySelectorAll(".spanner-plan-card")].map((c) => {
		let d = 0; for (let x = c.parentElement; x && !x.classList.contains("spanner-plan-graph"); x = x.parentElement) if (x.tagName === "UL") d++;
		return { Name: c.querySelector(".spanner-plan-name").textContent, Text: c.textContent, Depth: d } })`, &cards)
	var names []string
	for _, c := range cards {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "Distributed union,Serialize result,Table scan" {
		t.Fatalf("the plan's cards are %v; want Distributed union, Serialize result, Table scan, top-down (the local distributed union hidden)", names)
	}
	if !(cards[0].Depth < cards[1].Depth && cards[1].Depth < cards[2].Depth) {
		t.Errorf("the cards' depths are %+v; want each below the one before", cards)
	}
	scan := cards[2].Text
	for _, w := range []string{"on Singers", "Full scan", "Rows returned: 3", "Latency:", "CPU time:", "Highest row count"} {
		if !strings.Contains(scan, w) {
			t.Errorf("the Table scan card reads %q; want %q", scan, w)
		}
	}
	p.waitFor(`document.querySelector(".spanner-plan-card .spanner-plan-warning") !== null`)
	var summary map[string]string
	p.eval(`Object.fromEntries([...document.querySelectorAll("#spanner-query-summary dt")].map((d) => [d.textContent, d.nextElementSibling.textContent]))`, &summary)
	for _, k := range []string{"Total elapsed time", "CPU time", "Rows returned", "Rows scanned"} {
		if summary[k] == "" {
			t.Errorf("the Query summary is %v; want %s", summary, k)
		}
	}
	if summary["Rows returned"] != "3" || summary["Rows scanned"] != "3" {
		t.Errorf("the Query summary is %v; want 3 rows returned and 3 scanned", summary)
	}
	p.waitFor(`document.querySelector("#spanner-prominent") !== null && document.querySelector("#spanner-prominent").textContent.includes("Highest row count")`)
	p.waitFor(`document.querySelector("#spanner-plan-download") !== null`)

	// Compact hides the chips and the times.
	p.run(chromedp.Click(chromedp.CSS(`#spanner-plan-compact`)))
	p.waitFor(`document.querySelector(".spanner-plan-tree.is-compact") !== null && document.querySelector(".spanner-plan-card .spanner-plan-times") === null`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-plan-expanded`)))
	p.waitFor(`document.querySelector(".spanner-plan-card .spanner-plan-times") !== null`)

	// Explain through the index: its Index scan, and the Iterator information.
	setSQL("SELECT LastName FROM Singers@{FORCE_INDEX=SingersByLastName} WHERE LastName = 'Smith'")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-explain`)))
	p.waitFor(`[...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Index scan") && c.textContent.includes("on SingersByLastName"))`)
	p.waitFor(`![...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Full scan") || c.textContent.includes("Rows returned"))`)
	p.eval(`(() => { [...document.querySelectorAll(".spanner-plan-card")].find((c) => c.textContent.includes("Filter scan")).click(); return true })()`, nil)
	p.waitFor(`(document.querySelector("#spanner-iterator-info") || {}).textContent?.includes("Seek Condition")`)
	p.waitFor(`document.querySelector("#spanner-iterator-info").textContent.includes("$LastName = 'Smith'")`)

	// A set operation and a sample, which the first planner refused:
	// INTERSECT's Union all of Union inputs under a Hash aggregate and a
	// Filter, and TABLESAMPLE's Random id assign under a Sort limit.
	setSQL("SELECT FirstName FROM Singers INTERSECT DISTINCT SELECT FirstName FROM Singers WHERE SingerId > 1")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-explain`)))
	p.waitFor(`(() => { const n = [...document.querySelectorAll(".spanner-plan-card .spanner-plan-name")].map((e) => e.textContent);
		return ["Filter", "Hash aggregate", "Union all", "Union input"].every((w) => n.includes(w)) })()`)
	setSQL("SELECT FirstName FROM Singers TABLESAMPLE RESERVOIR (2 ROWS)")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-explanation`)))
	p.waitFor(`(() => { const c = [...document.querySelectorAll(".spanner-plan-card")];
		const s = c.find((e) => e.querySelector(".spanner-plan-name").textContent === "Sort limit");
		return s && s.textContent.includes("Rows returned: 2") && c.some((e) => e.querySelector(".spanner-plan-name").textContent === "Random id assign") })()`)
}
