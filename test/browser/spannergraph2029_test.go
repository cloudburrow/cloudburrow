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

// TestSpannerStudioGraphQueryPlans (#2029), in the emulators shard: on an
// Enterprise instance with Google's FinGraph, Spanner Studio runs a graph
// query (a label expression over two element tables) and shows its rows,
// and its Explanation draws the plan: a Union all of each table's Table
// scan. Explain of an OPTIONAL MATCH draws its Outer apply, and of a
// quantified path its Recursive union over a Recursive spool scan. The
// plan view passes the WCAG checks, and a card is reached and opened with
// the keyboard.
func TestSpannerStudioGraphQueryPlans(t *testing.T) {
	needService(t, "spanner")
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-graph-%d", time.Now().UnixNano()%1e8)
	body, _ := json.Marshal(map[string]string{"displayName": "Graph instance", "instance": inst, "edition": "Enterprise",
		"configType": "Regional", "regionalConfig": "regional-us-central1", "unit": "Nodes", "nodes": "1",
		"scaling": "Manual allocation"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create instance (Enterprise) through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	ddl := strings.Join([]string{
		"CREATE TABLE Person (id INT64 NOT NULL, name STRING(MAX)) PRIMARY KEY (id)",
		"CREATE TABLE Account (id INT64 NOT NULL, is_blocked BOOL, nick_name STRING(MAX)) PRIMARY KEY (id)",
		"CREATE TABLE PersonOwnAccount (id INT64 NOT NULL, account_id INT64 NOT NULL, FOREIGN KEY (account_id) REFERENCES Account (id)) PRIMARY KEY (id, account_id), INTERLEAVE IN PARENT Person ON DELETE CASCADE",
		"CREATE TABLE AccountTransferAccount (id INT64 NOT NULL, to_id INT64 NOT NULL, amount FLOAT64, FOREIGN KEY (to_id) REFERENCES Account (id)) PRIMARY KEY (id, to_id), INTERLEAVE IN PARENT Account ON DELETE CASCADE",
		"CREATE PROPERTY GRAPH FinGraph NODE TABLES (Account, Person) EDGE TABLES (" +
			"PersonOwnAccount SOURCE KEY (id) REFERENCES Person (id) DESTINATION KEY (account_id) REFERENCES Account (id) LABEL Owns, " +
			"AccountTransferAccount SOURCE KEY (id) REFERENCES Account (id) DESTINATION KEY (to_id) REFERENCES Account (id) LABEL Transfers)",
	}, ";\n") + ";"
	mk, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "createdatabase", "Values": map[string]string{"database": "fingraph", "ddl": ddl}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(mk)); code != http.StatusOK {
		t.Fatalf("Create database with FinGraph through the console API = %d: %s", code, out)
	}
	for _, sql := range []string{
		"INSERT INTO Account (id, is_blocked, nick_name) VALUES (7, false, 'Vacation Fund'), (16, true, 'Vacation Fund'), (20, false, 'Rainy Day Fund')",
		"INSERT INTO Person (id, name) VALUES (1, 'Alex'), (2, 'Dana'), (3, 'Lee')",
		"INSERT INTO PersonOwnAccount (id, account_id) VALUES (1, 7), (2, 20), (3, 16)",
		"INSERT INTO AccountTransferAccount (id, to_id, amount) VALUES (7, 16, 300), (16, 20, 300), (20, 7, 500), (20, 16, 200)",
	} {
		ins, _ := json.Marshal(map[string]any{"instance": inst, "database": "fingraph", "write": true, "sql": sql})
		if code, out := consoleDo(t, http.MethodPost, "/api/studio/spanner"+q+"&call=run", string(ins)); code != http.StatusOK || !strings.Contains(out, "row") {
			t.Fatalf("%s through Spanner Studio = %d: %s", sql, code, out)
		}
	}

	p := open(t)
	p.navigate("/spanner/" + inst + "/fingraph" + q)
	p.waitFor(`document.querySelector("#view .side-nav #tab-studio") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	p.waitFor(`document.querySelector("#spanner-explorer").textContent.includes("Person")`)
	setSQL := func(sql string) {
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#spanner-editor"); e.value = %q;
			e.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sql), nil)
	}
	cardNames := `[...document.querySelectorAll(".spanner-plan-card .spanner-plan-name")].map((e) => e.textContent)`

	// Run: the rows of both element tables, and the plan's Union all
	setSQL("GRAPH FinGraph MATCH (n:Person|Account) RETURN n.id AS id, n.name AS name, n.nick_name AS nick_name")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
	p.waitFor(`(() => { const r = (document.querySelector("#spanner-results") || {}).textContent || "";
		return r.includes("Dana") && r.includes("Rainy Day Fund") })()`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-explanation`)))
	p.waitFor(`document.querySelectorAll(".spanner-plan-card").length > 0`)
	var names []string
	p.eval(cardNames, &names)
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "Union all") || strings.Count(joined, "Table scan") != 2 || !strings.Contains(joined, "Serialize result") {
		t.Errorf("the plan's cards are %v; want a Union all of two Table scans under Serialize result", names)
	}
	var scans []string
	p.eval(`[...document.querySelectorAll(".spanner-plan-card")].filter((c) => c.textContent.includes("Table scan")).map((c) => c.textContent)`, &scans)
	for _, want := range []string{"on Person", "on Account"} {
		if !strings.Contains(strings.Join(scans, " | "), want) {
			t.Errorf("the Table scan cards read %q; want one %s", scans, want)
		}
	}
	if !strings.Contains(strings.Join(scans, " | "), "Rows returned: 3") {
		t.Errorf("the Table scan cards read %q; want each with Rows returned: 3", scans)
	}

	t.Run("WCAG", func(t *testing.T) {
		for _, u := range p.unnamedControls() {
			t.Errorf("Spanner Studio's plan of a graph query: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("Spanner Studio's plan of a graph query: %s", c)
		}
	})

	t.Run("keyboard", func(t *testing.T) {
		// Tab reaches a Table scan card; Enter opens its information
		p.tabTo(`a.classList.contains("spanner-plan-card") && a.textContent.includes("Table scan")`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`(document.querySelector("#spanner-iterator-info") || {}).textContent?.includes("Table scan") ||
			(document.querySelector("#spanner-plan-info") || {}).textContent?.includes("Table scan")`)
	})

	// Explain: OPTIONAL MATCH's Outer apply, a quantified path's Recursive
	// union over a Recursive spool scan
	setSQL("GRAPH FinGraph MATCH (n:Person) OPTIONAL MATCH (n)-[:Owns]->(a:Account {is_blocked: TRUE}) RETURN n.name AS name, a.id AS blocked")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-explain`)))
	p.waitFor(fmt.Sprintf(`%s.includes("Outer apply")`, cardNames))
	setSQL("GRAPH FinGraph MATCH (s:Account {id: 7})-[e:Transfers]->{1,3}(d:Account) RETURN ARRAY_LENGTH(e) AS hops, d.id AS dst")
	p.run(chromedp.Click(chromedp.CSS(`#spanner-explain`)))
	p.waitFor(fmt.Sprintf(`(() => { const n = %s; return n.includes("Recursive union") })()`, cardNames))
	p.waitFor(`[...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Recursive spool scan"))`)
}
