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
)

// studioAssets is Studio's Files listing through the console API (#2094).
func studioAssets(t *testing.T, project string) []struct{ Kind, Name, DisplayName, Parent string } {
	t.Helper()
	code, body := consoleDo(t, http.MethodPost, "/api/studio/dataform?call=files&project="+url.QueryEscape(project), "{}")
	if code != http.StatusOK {
		t.Fatalf("Studio's files = %d: %s", code, body)
	}
	var out struct {
		Entries []struct{ Kind, Name, DisplayName, Parent string }
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Entries
}

// studioContent is a code asset's content once its first save is
// committed (the repository is listed before its commit lands).
func studioContent(t *testing.T, project, name, want string) string {
	t.Helper()
	df := "/api/studio/dataform?project=" + url.QueryEscape(project) + "&call=read"
	var body string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		var code int
		code, body = consoleDo(t, http.MethodPost, df, `{"name":"`+name+`"}`)
		if code == http.StatusOK && strings.Contains(body, want) {
			break
		}
	}
	return body
}

func studioAsset(t *testing.T, project, display string) (kind, name, parent string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		for _, e := range studioAssets(t, project) {
			if e.DisplayName == display {
				return e.Kind, e.Name, e.Parent
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no code asset named %q in Studio's files", display)
		}
	}
}

// TestDataformBigQueryStudioCodeAssetsThroughThePage (#2094), in a shard whose
// instance serves Dataform beside BigQuery. From Studio's Home: a notebook
// (Create new > Notebook, + SQL, Run cell against BigQuery, a Python cell
// kept, Save, Version history), a data canvas
// (Search, Add to canvas, Join, a SQL node's Run, Query these results,
// Visualize, Ask in natural language through the local model or its
// reason, Save), a saved query kept in Dataform (Save query, Version
// history, Share > Manage permissions with Code Viewer), the Files pane's
// Create folder and Move to folder, Source control over a Dataform
// repository's workspace file, Recent tabs reopening a closed tab, the
// Map of a GEOGRAPHY result and the BigQuery notebook template; and the
// Dataform pages for folders, settings and a repository's Code and
// Permissions. The console API reads each result back. Every screen meets
// WCAG 2.1 AA.
func TestDataformBigQueryStudioCodeAssetsThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	stamp := uniqueSuffix()
	ds := fmt.Sprintf("studio_assets_%d", stamp)
	fixture(t, "bigquery", project, `{"datasetId":"`+ds+`"}`, ds)
	bq := "/api/studio/bigquery?project=" + url.QueryEscape(project) + "&call=query"
	for _, stmt := range []string{
		fmt.Sprintf("CREATE TABLE `%s.%s.customers` AS SELECT 1 AS id, 'ann' AS name", project, ds),
		fmt.Sprintf("CREATE TABLE `%s.%s.orders` AS SELECT 1 AS customer_id, 30 AS total", project, ds),
	} {
		b, _ := json.Marshal(map[string]any{"query": stmt, "write": true})
		if code, body := consoleDo(t, http.MethodPost, bq, string(b)); code != http.StatusOK {
			t.Fatalf("%s = %d: %s", stmt, code, body)
		}
	}
	df := "/api/studio/dataform?project=" + url.QueryEscape(project) + "&call="
	t.Cleanup(func() {
		for _, e := range studioAssets(t, project) {
			if strings.Contains(e.DisplayName, fmt.Sprint(stamp)) {
				call := "delete"
				if e.Kind == "folder" {
					call = "deletefolder"
				}
				consoleDo(t, http.MethodPost, df+call, `{"name":"`+e.Name+`"}`)
			}
		}
	})

	p := open(t)
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	var whatsNew int
	p.eval(`document.querySelectorAll(".bq-home-whatsnew li").length`, &whatsNew)
	if whatsNew == 0 {
		t.Error("Check out what's new in Studio lists nothing")
	}

	// A notebook: a SQL cell runs on BigQuery, a Python cell says why not.
	nbName := fmt.Sprintf("browser notebook %d", stamp)
	p.clickText(".bq-home-action", "Notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null`)
	p.clickText(".bq-notebook .bq-toolbar button", "+ SQL")
	p.setField(".bq-nb-cell:last-child .bq-nb-source", "%%bigquery\nSELECT 41 + 1 AS answer")
	p.clickText(".bq-nb-cell:last-child button", "Run cell")
	p.waitFor(`[...document.querySelectorAll(".bq-nb-cell:last-child .bq-nb-table td")].some((c) => c.textContent === "42")`)
	// A Python cell is kept (it runs on a connected runtime:
	// TestBigQueryStudioNotebookRuntimeThroughThePage).
	p.clickText(".bq-notebook .bq-toolbar button", "+ Code")
	p.setField(".bq-nb-cell:last-child .bq-nb-source", "print('hi')")
	p.clickText(".bq-notebook .bq-toolbar button", "Save")
	p.waitFor(`document.querySelector("#bq-asset-name") !== null`)
	p.setField("#bq-asset-name", nbName)
	p.clickText(".modal .modal-actions button", "Save")
	if kind, name, _ := studioAsset(t, project, nbName); kind != "notebook" {
		t.Errorf("the notebook is kept as %q", kind)
	} else {
		body := studioContent(t, project, name, "41 + 1")
		if !strings.Contains(body, "41 + 1") || !strings.Contains(body, "nbformat") {
			t.Errorf("the notebook reads back as %s", body)
		}
	}
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText(".bq-notebook .bq-toolbar button", "Version history")
	p.waitFor(`document.querySelectorAll(".bq-version-list tbody tr").length === 1`)
	p.clickText(".modal .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	// A data canvas: Search, Add to canvas, Join, SQL, Visualize.
	cvName := fmt.Sprintf("browser canvas %d", stamp)
	p.clickText(".bq-tabs .tab", "Home")
	p.clickText(".bq-home-action", "Data canvas")
	p.waitFor(`document.querySelector(".bq-canvas") !== null`)
	p.clickText(".bq-canvas .bq-toolbar button", "Search")
	p.setField(".bq-canvas-search input[type=search]", ds+" customers")
	p.clickText(".bq-canvas-search button", "Search")
	p.clickText(".bq-canvas-found button", "Add to canvas")
	p.setField(".bq-canvas-search input[type=search]", ds+" orders")
	p.clickText(".bq-canvas-search button", "Search")
	p.clickText(".bq-canvas-found button", "Add to canvas")
	p.waitFor(`document.querySelectorAll(".bq-canvas-table").length === 2`)
	p.clickText(".bq-canvas-table .bq-canvas-actions button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-table td")].some((c) => c.textContent === "ann")`)
	p.clickText(".bq-canvas-table .bq-canvas-actions button", "Join")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql textarea")].some((a) => a.value.includes("JOIN"))`)
	p.clickText(".bq-canvas .bq-toolbar button", "SQL")
	p.setField(".bq-canvas-sql:last-child textarea", "SELECT 'a' AS k, 1 AS v UNION ALL SELECT 'b', 2")
	p.clickText(".bq-canvas-sql:last-child .bq-canvas-actions button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql:last-child td")].some((c) => c.textContent === "b")`)
	p.clickText(".bq-canvas-sql:last-child .bq-canvas-actions button", "Query these results")
	p.waitFor(`document.querySelectorAll(".bq-canvas-sql").length === 3`)
	p.clickText(".bq-canvas-sql:nth-last-child(2) .bq-canvas-actions button", "Visualize")
	p.waitFor(`document.querySelector(".bq-canvas-viz svg.bq-chart") !== null`)
	// Natural language: the local model's answer, or why there is none.
	p.clickText(".bq-canvas-search button", "Ask in natural language")
	p.waitForWithin(`(() => { const l = document.querySelector(".bq-canvas-found"); return l && !l.querySelector(".spinner") && l.textContent.length > 0; })()`, 3*time.Minute)
	var nl string
	p.eval(`document.querySelector(".bq-canvas-found").textContent`, &nl)
	if !hasService(t, "playground") && !strings.Contains(nl, "local model") {
		t.Errorf("Ask in natural language without a local model says %q", nl)
	}
	// The canvas is long: its toolbar is clicked where it is, not scrolled
	// under the console's header.
	p.eval(`[...document.querySelectorAll(".bq-canvas .bq-toolbar button")].find((b) => b.textContent === "Save").click()`, nil)
	p.waitFor(`document.querySelector("#bq-asset-name") !== null`)
	p.setField("#bq-asset-name", cvName)
	p.clickText(".modal .modal-actions button", "Save")
	if kind, name, _ := studioAsset(t, project, cvName); kind != "data_canvas" {
		t.Errorf("the canvas is kept as %q", kind)
	} else if body := studioContent(t, project, name, "viz"); !strings.Contains(body, `\"type\":\"viz\"`) || !strings.Contains(body, ds+".orders") {
		t.Errorf("the canvas reads back as %s", body)
	}
	p.waitFor(`document.querySelector(".modal") === null`)

	// A saved query in Dataform: Save query, Version history, Manage
	// permissions.
	sqName := fmt.Sprintf("browser query %d", stamp)
	p.eval(`window.scrollTo(0, 0); document.querySelectorAll(".bq-workspace, #main, main").forEach((n) => { n.scrollTop = 0; }); true`, nil)
	p.clickText(".bq-new-query", "SQL query")
	p.setField(".bq-editor", "SELECT 1 AS one")
	p.clickText(".bq-toolbar .bq-menu-button", "Save ▾")
	p.clickText(".bq-menu-item", "Save query")
	p.setField("#bq-save-name", sqName)
	p.clickText(".modal .modal-actions button", "Save")
	_, sqRepo, _ := studioAsset(t, project, sqName)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.setField(".bq-editor", "SELECT 2 AS two")
	p.clickText(".bq-toolbar .bq-menu-button", "Save ▾")
	p.clickText(".bq-menu-item", "Save query")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		_, body := consoleDo(t, http.MethodPost, df+"history", `{"name":"`+sqRepo+`"}`)
		if strings.Count(body, `"commit"`) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second Save query made no second version: %s", body)
		}
	}
	p.clickText(".bq-toolbar .bq-menu-button", "Save ▾")
	p.clickText(".bq-menu-item", "Version history")
	p.waitFor(`document.querySelectorAll(".bq-version-list tbody tr").length === 2`)
	p.clickText(".modal .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText(".bq-toolbar .bq-menu-button", "Share ▾")
	p.clickText(".bq-menu-item", "Manage permissions")
	p.waitFor(`document.querySelector("#bq-perm-member") !== null && document.querySelector(".bq-perm-list .spinner") === null`)
	p.setField("#bq-perm-member", "user:ana@example.com")
	p.setField("#bq-perm-role", "roles/dataform.codeViewer")
	p.clickText(".modal .modal-actions button", "Save")
	p.waitFor(`[...document.querySelectorAll(".bq-perm-list td")].some((c) => c.textContent === "user:ana@example.com")`)
	if code, body := consoleDo(t, http.MethodPost, df+"getiam", `{"name":"`+sqRepo+`"}`); code != http.StatusOK || !strings.Contains(body, "roles/dataform.codeViewer") {
		t.Errorf("the saved query's policy is %d: %s", code, body)
	}
	p.clickText(".modal .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Files: Create folder, then Move to folder.
	folder := fmt.Sprintf("browser folder %d", stamp)
	p.clickText(".bq-pane-tabs .tab", "Files")
	p.clickText(".bq-files-tools button", "+ Create folder")
	p.setField("#bq-asset-name", folder)
	p.clickText(".modal .modal-actions button", "Save")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".bq-files .bq-node-name")].some((n) => n.textContent === %q)`, folder))
	_, folderName, _ := studioAsset(t, project, folder)
	p.eval(fmt.Sprintf(`document.querySelector('[data-file=%q] .bq-node-menu').click()`, sqRepo), nil)
	p.clickText(fmt.Sprintf(`[data-file=%q] .bq-menu-item`, sqRepo), "Move to folder")
	p.setField("#bq-move-folder", folderName)
	p.clickText(".modal .modal-actions button", "Move")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		if _, _, parent := studioAsset(t, project, sqName); parent == folderName {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Move to folder did not move the saved query")
		}
	}

	// Source control: a Dataform repository's workspace file in a tab.
	repoID := fmt.Sprintf("studio-src-%d", stamp)
	repo := "projects/" + project + "/locations/us-central1/repositories/" + repoID
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/dataform"+q, `{"repositoryId":"`+repoID+`","region":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create repository = %d: %s", code, body)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{repo}, "Action": "delete", "Values": map[string]string{}})
		consoleDo(t, http.MethodPost, "/api/actions/dataform"+q, string(b))
	})
	act := func(path []string, action string, values map[string]string) {
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/dataform"+q, string(b)); code != http.StatusOK {
			t.Fatalf("%s = %d: %s", action, code, body)
		}
	}
	act([]string{repo}, "createworkspace", map[string]string{"workspaceId": "dev"})
	act([]string{repo, "workspaces", "dev"}, "createfile", map[string]string{"path": "definitions/a.sqlx", "contents": "config { type: \"view\" }\nselect 1 as a\n"})
	p.clickText(".bq-pane-tabs .tab", "Source control")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".bq-source .bq-node-name")].some((n) => n.textContent === %q)`, repoID))
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(".bq-source li")].find((l) => l.textContent.startsWith(%q)).querySelector("button[aria-label='Expand dev']").click()`, repoID), nil)
	p.clickText(".bq-source .bq-node-name", "definitions/a.sqlx")
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === "a.sqlx" && document.querySelector("textarea[aria-label='Edit definitions/a.sqlx']").value.includes("select 1")`)

	// Recent tabs: a closed tab opens again.
	p.eval(`document.querySelector(".bq-tabs .tab.is-selected").parentElement.querySelector(".bq-tab-close").click()`, nil)
	p.waitFor(`![...document.querySelectorAll(".bq-tabs .tab")].some((b) => b.textContent === "a.sqlx")`)
	p.clickText(".bq-recent-tabs .bq-menu-button", "Recent tabs ▾")
	p.clickText(".bq-recent-tabs .bq-menu-item", "a.sqlx")
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === "a.sqlx"`)

	// Map: a GEOGRAPHY result on the Visualization tab.
	p.eval(`window.scrollTo(0, 0); document.querySelectorAll(".bq-workspace, #main, main").forEach((n) => { n.scrollTop = 0; }); true`, nil)
	p.clickText(".bq-new-query", "SQL query")
	p.setField(".bq-editor", "SELECT 'here' AS name, ST_GEOGPOINT(-122.08, 37.42) AS loc UNION ALL SELECT 'there', ST_GEOGPOINT(-73.98, 40.75)")
	p.clickText(".bq-toolbar button", "Run")
	p.waitForWithin(`document.querySelector(".bq-results-tabs") !== null || document.querySelector(".bq-results .form-error") !== null`, time.Minute)
	p.clickText(".bq-results-tabs .tab", "Visualization")
	p.waitFor(`document.querySelector("svg.bq-map") !== null`)
	var points int
	p.eval(`document.querySelectorAll("svg.bq-map circle").length`, &points)
	if points != 2 {
		t.Errorf("the Map draws %d points, want 2", points)
	}

	// The BigQuery notebook template.
	p.clickText(".bq-tabs .tab", "Home")
	p.clickText(".bq-home-templates button", "Notebook: BigQuery template")
	p.waitFor(`document.querySelectorAll(".bq-nb-cell").length === 4`)

	// The Dataform pages: Folders, Settings, a repository's Code and
	// Permissions.
	p.navigate("/bigquery-dataform-folders" + q)
	p.waitFor(rowReady(folderName))
	p.navigate("/bigquery-dataform-settings" + q)
	p.waitFor(rowReady("projects/" + project + "/locations/asia-east1/config"))
	p.navigate("/bigquery-dataform" + q)
	p.waitFor(rowReady(repo))
	var listed string
	p.eval(`document.querySelector("#view").textContent`, &listed)
	if strings.Contains(listed, sqRepo) {
		t.Error("the Dataform page lists a Studio code asset, which Google's hides")
	}
	repoPath := "/bigquery-dataform/" + url.PathEscape(repo) + q
	p.navigate(repoPath)
	p.waitFor(`["Code", "Permissions"].every((l) => [...document.querySelectorAll("#view [role=tab]")].some((t) => t.textContent.trim() === l))`)

	studio := "/bigquery-studio" + q
	tree := `document.querySelector(".bq-tree") !== null`
	pane := func(label string) string {
		return `[...document.querySelectorAll(".bq-pane-tabs .tab")].find((b) => b.textContent === "` + label + `").click()`
	}
	homeAction := func(label string) string {
		return `[...document.querySelectorAll(".bq-tabs .tab")].find((b) => b.textContent === "Home").click();
			[...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent === "` + label + `").click()`
	}
	auditScreens(t, []a11yPage{
		{"Studio notebook", studio, tree, homeAction("Notebook")},
		{"Studio data canvas", studio, tree, homeAction("Data canvas") + `; [...document.querySelectorAll(".bq-canvas .bq-toolbar button")].find((b) => b.textContent === "Search").click()`},
		{"Studio Files over Dataform", studio, tree, pane("Files")},
		{"Studio Source control over Dataform", studio, tree, pane("Source control")},
		screen("Dataform folders", "/bigquery-dataform-folders", project),
		screen("Dataform settings", "/bigquery-dataform-settings", project),
		{"Dataform repository code", repoPath, settled, `[...document.querySelectorAll("#view [role=tab]")].find((t) => t.textContent.trim() === "Code").click()`},
	})
}
