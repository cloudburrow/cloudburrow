//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestBigQueryStudioConnectToARuntimeThroughThePage (#2094, 2026-11-23
// round): a notebook's Connect has Google's drop-down, Additional
// connection options > Connect to a runtime
// (https://docs.cloud.google.com/bigquery/docs/create-notebooks), which
// opens the Connect to Agent Platform runtime dialog. Create new runtime
// assigns a Colab Enterprise runtime from a runtime template (Runtime
// Template, Runtime name) and connects the notebook to it: a cell sees the
// template's environment variable, Restart session keeps it, and
// Disconnect and delete runtime deletes the runtime. Connect to an
// existing runtime (Runtimes) connects to one made through the API. WCAG
// 2.1 AA for the dialog; the drop-down, its item and the dialog reached by
// keyboard.
func TestBigQueryStudioConnectToARuntimeThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	needService(t, "colab-templates")
	needService(t, "colab-runtimes")
	if os.Getenv("CLOUDBURROW_TEST_NOTEBOOK_RUNTIME") == "" {
		t.Skip("CLOUDBURROW_TEST_NOTEBOOK_RUNTIME is not set: the notebook runtime image is built on the first runtime")
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	rq := q + "&region=us-central1"
	stamp := uniqueSuffix()
	display := fmt.Sprintf("Studio template %d", stamp)
	values, _ := json.Marshal(map[string]string{"displayName": display, "region": "us-central1",
		"machineType": "e2-standard-4", "diskType": "pd-standard", "diskSize": "100", "idleShutdown": "true", "idleMinutes": "30",
		"environment": "latest", "env": `{"STUDIO_STAGE":"from-template"}`, "internet": "true", "euc": "true"})
	code, body := consoleDo(t, http.MethodPost, "/api/resources/colab-templates"+q, string(values))
	if code != http.StatusOK {
		t.Fatalf("create a runtime template = %d: %s", code, body)
	}
	var made struct{ Name string }
	if err := json.Unmarshal([]byte(body), &made); err != nil || made.Name == "" {
		t.Fatalf("create a runtime template answered %s", body)
	}
	tpl := made.Name
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-templates"+rq+"&name="+url.QueryEscape(tpl), "")
	})
	type listing struct {
		Items []struct {
			Name, Status string
			Fields       map[string]string
		}
	}
	runtimeNamed := func(name string) (id, status string) {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/colab-runtimes"+rq, "")
		if code != http.StatusOK {
			t.Fatalf("list runtimes = %d: %s", code, body)
		}
		var l listing
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		for _, it := range l.Items {
			if it.Fields["Runtime name"] == name {
				return it.Name, it.Status
			}
		}
		return "", ""
	}
	newName := fmt.Sprintf("studio-new-%d", stamp)
	t.Cleanup(func() {
		if id, _ := runtimeNamed(newName); id != "" {
			consoleDo(t, http.MethodDelete, "/api/resources/colab-runtimes"+rq+"&name="+url.QueryEscape(id), "")
		}
	})

	p := openWithin(t, 30*time.Minute)
	press := func(sel, text string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(%q)].some((b) => b.textContent.trim() === %q && !b.disabled && !b.hidden)`, sel, text))
		p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q && !b.disabled && !b.hidden).click(); true`, sel, text), nil)
	}
	cell := func(code string) string {
		t.Helper()
		press(".bq-notebook .bq-toolbar button", "+ Code")
		p.setField(".bq-nb-cell:last-child .bq-nb-source", code)
		press(".bq-nb-cell:last-child button", "Run cell")
		p.waitForWithin(`(() => { const c = document.querySelector(".bq-nb-cell:last-child");
			return !c.querySelector(".bq-nb-output .spinner") && (/\[\d+\]/.test(c.querySelector(".bq-nb-count").textContent) || c.querySelector(".bq-nb-output .form-error") !== null); })()`, 3*time.Minute)
		var out string
		p.eval(`document.querySelector(".bq-nb-cell:last-child .bq-nb-output").textContent`, &out)
		return out
	}
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null && document.querySelector(".bq-nb-runtime").textContent !== ""`)

	// By keyboard: the drop-down beside Connect opens on Enter with focus
	// on Connect to a runtime; Enter opens the dialog.
	more := `.bq-notebook .bq-toolbar button[aria-label="Additional connection options"]`
	p.run(chromedp.Focus(chromedp.CSS(more)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Connect to a runtime"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#bq-rt-title") !== null && document.activeElement && document.activeElement.id === "bq-rt-region"`)
	var title string
	p.eval(`document.querySelector("#bq-rt-title").textContent`, &title)
	if title != "Connect to Agent Platform runtime" {
		t.Errorf("the dialog is %q, want Connect to Agent Platform runtime", title)
	}
	var labels string
	p.eval(`[...document.querySelectorAll(".modal label, .modal legend")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, want := range []string{"Region", "Select a runtime", "Connect to an existing runtime", "Create new runtime", "Runtimes", "Runtime Template", "Runtime name"} {
		if !strings.Contains("|"+labels+"|", "|"+want+"|") {
			t.Errorf("Connect to a runtime has no %q: %s", want, labels)
		}
	}
	for _, fl := range p.keyboardFailures() {
		t.Errorf("Connect to Agent Platform runtime: %s", fl)
	}

	// Create new runtime from the template.
	p.eval(`document.querySelector('input[name="bq-rt-mode"][value="new"]').click(); true`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#bq-rt-template option")].some((o) => o.value === %q)`, tpl))
	p.setField("#bq-rt-template", tpl)
	p.setField("#bq-rt-name", newName)
	p.clickText(".modal button", "Connect")
	p.waitForWithin(`document.querySelector(".modal") === null && (document.querySelector(".bq-nb-runtime").textContent.startsWith("Connected") || document.querySelector(".bq-nb-runtime").className.includes("form-error"))`, 25*time.Minute)
	id, status := runtimeNamed(newName)
	var line string
	p.eval(`document.querySelector(".bq-nb-runtime").textContent`, &line)
	if id == "" || status != "Running" || !strings.HasPrefix(line, "Connected to the runtime "+id+":") {
		t.Fatalf("Create new runtime: runtime %q is %q; the notebook says %q", id, status, line)
	}
	if out := cell("import os\nprint(os.environ.get('STUDIO_STAGE'))"); !strings.Contains(out, "from-template") {
		t.Errorf("a cell on the template's runtime prints %q, want its STUDIO_STAGE", out)
	}
	press(".bq-notebook .bq-toolbar button", "Restart session")
	p.waitForWithin(`document.querySelector(".bq-nb-runtime").textContent.startsWith("Connected to the runtime")`, 5*time.Minute)
	if out := cell("import os\nprint(os.environ.get('STUDIO_STAGE'))"); !strings.Contains(out, "from-template") {
		t.Errorf("after Restart session a cell prints %q, want the template's STUDIO_STAGE still", out)
	}
	press(".bq-notebook .bq-toolbar button", "Disconnect and delete runtime")
	p.waitForWithin(`document.querySelector(".bq-nb-runtime").textContent === "Not connected."`, 5*time.Minute)
	if id, _ := runtimeNamed(newName); id != "" {
		t.Errorf("Disconnect and delete runtime left the runtime %s", id)
	}

	// Connect to an existing runtime, made through the API.
	existing := fmt.Sprintf("studio-existing-%d", stamp)
	code, body = consoleDo(t, http.MethodPost, "/api/resources/colab-runtimes"+rq, fmt.Sprintf(`{"notebookRuntimeTemplate":%q,"displayName":%q}`, tpl, existing))
	if code != http.StatusOK {
		t.Fatalf("create a runtime = %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &made); err != nil || made.Name == "" {
		t.Fatalf("create a runtime answered %s", body)
	}
	rt := made.Name
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-runtimes"+rq+"&name="+url.QueryEscape(rt), "")
	})
	p.clickExpr(`document.querySelector(` + fmt.Sprintf("%q", more) + `)`)
	p.clickText(".bq-menu:not([hidden]) .bq-menu-item", "Connect to a runtime")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#bq-rt-runtimes option")].some((o) => o.value === %q)`, rt))
	p.setField("#bq-rt-runtimes", rt)
	p.clickText(".modal button", "Connect")
	p.waitForWithin(`document.querySelector(".bq-nb-runtime").textContent.startsWith("Connected to the runtime")`, 5*time.Minute)
	if out := cell("21 * 2"); !strings.Contains(out, "42") {
		t.Errorf("a cell on the existing runtime shows %q", out)
	}

	auditScreens(t, []a11yPage{
		{"Studio Connect to Agent Platform runtime", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`,
			`[...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent.trim() === "Notebook").click();
			 setTimeout(() => { document.querySelector(` + fmt.Sprintf("%q", more) + `).click();
			   [...document.querySelectorAll(".bq-menu-item")].find((x) => x.textContent === "Connect to a runtime" && x.offsetParent !== null).click();
			   setTimeout(() => document.querySelector('input[name="bq-rt-mode"][value="new"]').click(), 300); }, 300)`},
	})
}

// TestDataformBigQueryStudioNotebookMenuBarThroughThePage (#2094,
// 2026-11-23 round; named for the shard that serves BigQuery and Dataform
// together): a notebook's Toggle header visibility shows Google's menu bar
// (https://docs.cloud.google.com/bigquery/docs/manage-notebooks). Edit >
// Notebook settings' Omit code cell output when saving this notebook
// leaves a run cell's output out of the saved notebook; File > Download
// gives the .ipynb and .py files; File > Rename (the Rename notebook
// dialog) renames the saved notebook. WCAG 2.1 AA with the menu bar open,
// and the toggle and the menus by keyboard.
func TestDataformBigQueryStudioNotebookMenuBarThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	stamp := uniqueSuffix()
	nbName := fmt.Sprintf("menu bar notebook %d", stamp)
	renamed := nbName + " renamed"
	t.Cleanup(func() {
		for _, e := range studioAssets(t, project) {
			if e.DisplayName == nbName || e.DisplayName == renamed {
				consoleDo(t, http.MethodPost, "/api/studio/dataform?call=delete&project="+url.QueryEscape(project), `{"name":"`+e.Name+`"}`)
			}
		}
	})
	p := open(t)
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null`)
	// Downloads are caught as the blobs the page hands the browser.
	p.eval(`window.cbBlobs = []; const make = URL.createObjectURL; URL.createObjectURL = (b) => { b.text().then((t) => window.cbBlobs.push(t)); return make(b); }; true`, nil)

	// The menu bar is hidden until Toggle header visibility, by keyboard.
	var hidden bool
	p.eval(`document.querySelector(".bq-nb-menubar").hidden`, &hidden)
	if !hidden {
		t.Error("the notebook's menu bar is shown before Toggle header visibility")
	}
	p.run(chromedp.Focus(chromedp.CSS(`.bq-notebook button[aria-label="Toggle header visibility"]`)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".bq-nb-menubar").hidden && document.querySelector(".bq-nb-header-toggle").getAttribute("aria-expanded") === "true"`)
	var menus []string
	p.eval(`[...document.querySelectorAll(".bq-nb-menubar .bq-menu-button")].map((b) => b.getAttribute("aria-label"))`, &menus)
	if strings.Join(menus, "|") != "File|Edit" {
		t.Errorf("the menu bar has %v, want File and Edit", menus)
	}
	for _, fl := range p.keyboardFailures() {
		t.Errorf("notebook menu bar: %s", fl)
	}

	// A %%bigquery cell run here keeps its output in the tab.
	p.clickText(".bq-notebook .bq-toolbar button", "+ Code")
	p.setField(".bq-nb-cell:last-child .bq-nb-source", "%%bigquery\nSELECT 41 + 1 AS answer")
	p.clickText(".bq-nb-cell:last-child button", "Run cell")
	p.waitFor(`[...document.querySelectorAll(".bq-nb-cell:last-child .bq-nb-table td")].some((c) => c.textContent === "42")`)

	// Edit > Notebook settings, by keyboard: Enter opens Edit on its item.
	p.run(chromedp.Focus(chromedp.CSS(`.bq-nb-menubar .bq-menu-button[aria-label=Edit]`)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Notebook settings"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#bq-nb-settings-title") !== null && document.activeElement && document.activeElement.id === "bq-nb-omit"`)
	var omitLabel string
	p.eval(`document.querySelector(".modal label[for=bq-nb-omit]").textContent.trim()`, &omitLabel)
	if omitLabel != "Omit code cell output when saving this notebook" {
		t.Errorf("Notebook settings' option is %q", omitLabel)
	}
	p.run(chromedp.KeyEvent(" "))
	p.clickText(".modal .modal-actions button", "Save")
	p.waitFor(`document.querySelector(".modal") === null`)

	// File > Download > Download .ipynb and Download .py.
	p.clickExpr(`document.querySelector(".bq-nb-menubar .bq-menu-button[aria-label=File]")`)
	p.clickText(".bq-menu:not([hidden]) .bq-menu-parent", "Download ▸")
	p.clickText(".bq-submenu:not([hidden]) .bq-menu-item", "Download .ipynb")
	p.clickExpr(`document.querySelector(".bq-nb-menubar .bq-menu-button[aria-label=File]")`)
	p.clickText(".bq-menu:not([hidden]) .bq-menu-parent", "Download ▸")
	p.clickText(".bq-submenu:not([hidden]) .bq-menu-item", "Download .py")
	p.waitFor(`window.cbBlobs.length === 2`)
	var blobs []string
	p.eval(`window.cbBlobs`, &blobs)
	if !strings.Contains(blobs[0], `"nbformat"`) || !strings.Contains(blobs[0], "41 + 1") || strings.Contains(blobs[0], "execute_result") {
		t.Errorf("Download .ipynb with outputs omitted gave %.400s", blobs[0])
	}
	if !strings.HasPrefix(blobs[1], "#!/usr/bin/env python") || !strings.Contains(blobs[1], "# In[ ]:") || !strings.Contains(blobs[1], "SELECT 41 + 1 AS answer") {
		t.Errorf("Download .py gave %.400s", blobs[1])
	}

	// Saved, the notebook has no output; File > Rename renames it.
	p.clickText(".bq-notebook .bq-toolbar button", "Save")
	p.waitFor(`document.querySelector("#bq-asset-name") !== null`)
	p.setField("#bq-asset-name", nbName)
	p.clickText(".modal .modal-actions button", "Save")
	p.waitFor(`document.querySelector(".modal") === null`)
	_, name, _ := studioAsset(t, project, nbName)
	if body := studioContent(t, project, name, "41 + 1"); !strings.Contains(body, "41 + 1") || strings.Contains(body, "execute_result") {
		t.Errorf("with Omit code cell output the saved notebook reads %.400s", body)
	}
	p.clickExpr(`document.querySelector(".bq-nb-menubar .bq-menu-button[aria-label=File]")`)
	p.clickText(".bq-menu:not([hidden]) .bq-menu-item", "Rename")
	p.waitFor(`document.querySelector("#bq-nb-rename-title") !== null && document.activeElement && document.activeElement.id === "bq-nb-rename-name"`)
	p.setField("#bq-nb-rename-name", renamed)
	p.clickText(".modal .modal-actions button", "Rename")
	p.waitFor(`document.querySelector(".modal") === null`)
	found := false
	for deadline := time.Now().Add(20 * time.Second); !found && time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		for _, e := range studioAssets(t, project) {
			found = found || (e.Name == name && e.DisplayName == renamed)
		}
	}
	if !found {
		t.Errorf("File > Rename did not rename %s to %q", name, renamed)
	}

	// nbOpen opens a new notebook from Home (the Home tab first: Studio
	// restores the tabs left open) and shows its menu bar.
	nbOpen := `const until = (f) => new Promise((ok) => { const look = () => (f() ? ok(f()) : setTimeout(look, 50)); look(); });
		[...document.querySelectorAll(".bq-tabs .tab")].find((b) => b.textContent.trim().startsWith("Home")).click();
		(await until(() => [...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent.trim() === "Notebook" && b.offsetParent !== null))).click();
		const nb = await until(() => [...document.querySelectorAll(".bq-notebook")].find((n) => n.offsetParent !== null && !n.querySelector(".bq-nb-cell")));
		if (nb.querySelector(".bq-nb-menubar").hidden) nb.querySelector(".bq-nb-header-toggle").click();`
	auditScreens(t, []a11yPage{
		{"Studio notebook menu bar", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`, nbOpen + `
			nb.querySelector(".bq-nb-menubar .bq-menu-button[aria-label=File]").click();`},
		{"Studio Notebook settings", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`, nbOpen + `
			nb.querySelector(".bq-nb-menubar .bq-menu-button[aria-label=Edit]").click();
			[...nb.querySelectorAll(".bq-menu-item")].find((x) => x.textContent === "Notebook settings").click();`},
	})
}
