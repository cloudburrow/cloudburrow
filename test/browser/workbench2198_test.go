//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestWorkbenchJupyterLabFromTheConsole (#2198): an instance's row's Open
// JupyterLab opens, in a new window, the instance's own JupyterLab on its
// proxy_uri host, through the console. In the real JupyterLab UI: the
// Launcher's Python 3 (ipykernel) notebook, a cell that creates a table in
// the instance's BigQuery and queries it with Google's client, run with
// Shift+Enter, whose output is checked; then the Launcher's Terminal, a
// shell in the instance, whose command's output is checked in the
// instance's files. The console's Workbench screens meet WCAG 2.1 AA and
// work by keyboard.
func TestWorkbenchJupyterLabFromTheConsole(t *testing.T) {
	needService(t, "workbench-instances")
	needService(t, "bigquery")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	name := "browser-lab-" + suffix
	dataset := "wb_lab_" + suffix
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+dataset+`"}`); code != http.StatusOK {
		t.Fatalf("create dataset = %d %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+url.QueryEscape(dataset)+"&force=true", "")
	})

	p := openWithin(t, 40*time.Minute)
	listPath := "/agent-platform/workbench/instances" + q
	p.navigate(listPath)
	p.waitFor(settled)
	p.clickText("#view button", "Create new")
	p.waitFor(`document.querySelector("#view #f-name") !== null`)
	p.submitForm(map[string]string{"name": name, "zone": "us-central1-a"}, "name")
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/workbench-instances"+q+"&name="+url.QueryEscape(name), "")
	})
	p.waitForWithin(listedRow("/agent-platform/workbench/instances", name), 30*time.Minute)
	p.navigate(listPath)
	p.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Active"))`, name), 10*time.Minute)

	// Open JupyterLab: a new window on the instance's proxy host.
	opened := chromedp.WaitNewTarget(p.ctx, func(i *target.Info) bool { return strings.Contains(i.URL, ".notebooks.localhost") })
	p.colabRowItem(name, "Open JupyterLab")
	var id target.ID
	select {
	case id = <-opened:
	case <-time.After(time.Minute):
		t.Fatal("Open JupyterLab opened no window on the instance's proxy host")
	}
	ctx, cancel := chromedp.NewContext(p.ctx, chromedp.WithTargetID(id))
	t.Cleanup(cancel)
	lab := &tab{t: t, ctx: ctx}
	lab.waitForWithin(`location.pathname.startsWith("/lab") && document.querySelector(".jp-Launcher") !== null`, 3*time.Minute)
	var host string
	lab.eval(`location.host`, &host)
	if !strings.Contains(host, "-dot-us-central1.notebooks.localhost:") {
		t.Errorf("JupyterLab opened at %s, want the instance's proxy_uri host", host)
	}

	// The Launcher's Python 3 notebook; a cell querying the instance's
	// BigQuery with Google's client, run with Shift+Enter.
	lab.run(chromedp.Click(chromedp.CSS(`.jp-LauncherCard[data-category="Notebook"]`)))
	lab.waitForWithin(`document.querySelector(".jp-NotebookPanel:not(.lm-mod-hidden) .jp-Cell .cm-content") !== null`, 2*time.Minute)
	// The kernel is idle before the cell is run: the toolbar's kernel
	// name shows it, and its status no longer reads starting or busy.
	lab.waitForWithin(`(() => { const s = document.querySelector(".jp-NotebookPanel:not(.lm-mod-hidden) .jp-Notebook-ExecutionIndicator");
		return !!s && s.getAttribute("data-status") === "idle"; })()`, 2*time.Minute)
	cell := fmt.Sprintf(`from google.cloud import bigquery; client = bigquery.Client(); client.query("CREATE TABLE %[1]s.answers AS SELECT 42 AS answer UNION ALL SELECT 958").result(); print("total", list(client.query("SELECT SUM(answer) AS s FROM %[1]s.answers").result())[0].s)`, dataset)
	lab.run(chromedp.Click(chromedp.CSS(`.jp-NotebookPanel:not(.lm-mod-hidden) .jp-Cell .cm-content`)))
	lab.run(chromedp.SendKeys(chromedp.CSS(`.jp-NotebookPanel:not(.lm-mod-hidden) .jp-Cell .cm-content`), cell))
	lab.run(chromedp.KeyEvent(kb.Enter, chromedp.KeyModifiers(chromedp.ModifierShift)))
	lab.waitForWithin(`[...document.querySelectorAll(".jp-NotebookPanel:not(.lm-mod-hidden) .jp-OutputArea-output")].some((o) => /total 1000/.test(o.textContent) || /Error/.test(o.textContent))`, 5*time.Minute)
	var output string
	lab.eval(`[...document.querySelectorAll(".jp-NotebookPanel:not(.lm-mod-hidden) .jp-OutputArea-output")].map((o) => o.textContent).join("\n")`, &output)
	if !strings.Contains(output, "total 1000") {
		t.Fatalf("the cell's output = %q, want total 1000", output)
	}

	// The Launcher's Terminal: a shell in the instance.
	lab.eval(`(() => { const b = document.querySelector('[data-command="launcher:create"], .jp-ToolbarButtonComponent[title="New Launcher"]');
		if (b) b.click(); return true; })()`, nil)
	lab.waitFor(`[...document.querySelectorAll(".jp-LauncherCard")].some((c) => c.textContent.trim() === "Terminal")`)
	lab.eval(`[...document.querySelectorAll(".jp-LauncherCard")].filter((c) => c.textContent.trim() === "Terminal").pop().click(), true`, nil)
	lab.waitForWithin(`document.querySelector(".jp-Terminal:not(.lm-mod-hidden) .xterm-helper-textarea") !== null`, time.Minute)
	time.Sleep(2 * time.Second) // the shell's prompt
	lab.run(chromedp.Focus(chromedp.CSS(`.jp-Terminal:not(.lm-mod-hidden) .xterm-helper-textarea`)))
	lab.run(chromedp.SendKeys(chromedp.CSS(`.jp-Terminal:not(.lm-mod-hidden) .xterm-helper-textarea`), "echo $((6*7)) $(whoami) > from-terminal.txt"))
	lab.run(chromedp.KeyEvent(kb.Enter))
	var term string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		lab.eval(`fetch("/api/contents/from-terminal.txt?content=1&type=file&format=text").then((r) => r.ok ? r.json() : {}).then((f) => (f.content || "").trim())`, &term, awaitPromise)
		if term != "" {
			break
		}
	}
	if term != "42 jupyter" {
		t.Errorf("the terminal's command wrote %q, want 42 jupyter", term)
	}
	cancel()

	auditScreens(t, []a11yPage{
		{"workbench instances with JupyterLab", listPath, rowReady(name), ""},
		{"workbench instance", "/agent-platform/workbench/instances/" + url.PathEscape(name) + q, settled, ""},
	})
}
