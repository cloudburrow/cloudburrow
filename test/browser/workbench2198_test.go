//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestWorkbenchJupyterLabFromTheConsole (#2198): an instance's row's Open
// JupyterLab opens, in a new window, the instance's own JupyterLab on its
// proxy_uri host, through the console. In the real JupyterLab UI: Google's
// JupyterLab plugins (the Dataproc plugin's Clusters page, from the
// instance's Dataproc API; the BigQuery Dataset explorer, showing the
// project's dataset; the Cloud Storage browser); the Launcher's Python 3
// (ipykernel) notebook, a cell that creates a table in
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

	// The instance runs as the project's Compute Engine default service
	// account, which, as on Google where automatic grants are off, needs a
	// BigQuery role: granted on the project's IAM page's API.
	sa := fmt.Sprintf("serviceAccount:%d-compute@developer.gserviceaccount.com", resource.ProjectNumber(project))
	code, body := consoleDo(t, http.MethodPost, "/api/permissions/iam"+q, `{"Path":["`+project+`"],"Read":{}}`)
	var pol struct {
		Permissions struct{ Etag string }
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &pol) != nil || pol.Permissions.Etag == "" {
		t.Fatalf("read the project's policy = %d %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/permissions/iam"+q, `{"Path":["`+project+`"],"Etag":"`+pol.Permissions.Etag+
		`","Grant":{"Members":["`+sa+`"],"Role":"roles/bigquery.admin"}}`); code != http.StatusOK {
		t.Fatalf("grant roles/bigquery.admin to %s = %d %s", sa, code, body)
	}

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
	t.Cleanup(func() {
		if t.Failed() && ctx.Err() == nil {
			var text string
			if _, err := chromedp.Run(ctx, evaluateInto(`document.body.innerText.slice(0, 3000)`, &text)); err == nil {
				t.Logf("JupyterLab showed: %s", text)
			}
			if png, err := chromedp.Run(ctx, chromedp.FullScreenshot(90)); err == nil {
				t.Logf("JupyterLab's screenshot: %s", writeShot(t, "jupyterlab", png))
			}
		}
	})
	lab.waitForWithin(`location.pathname.startsWith("/lab") && document.querySelector(".jp-Launcher") !== null`, 3*time.Minute)
	var host string
	lab.eval(`location.host`, &host)
	if !strings.Contains(host, "-dot-us-central1.notebooks.localhost:") {
		t.Errorf("JupyterLab opened at %s, want the instance's proxy_uri host", host)
	}

	// Google's JupyterLab plugins, as on a Workbench VM: the Launcher's
	// Google Cloud Resources (the Dataproc plugin's Clusters and
	// Serverless), whose Clusters page lists the project's clusters from
	// the instance's Dataproc API; the BigQuery plugin's Dataset explorer,
	// whose project opens on the dataset made above; the Cloud Storage
	// plugin's bucket browser.
	lab.waitFor(`[...document.querySelectorAll(".jp-Launcher-section")].some((s) => /Google Cloud Resources/.test(s.textContent) && /Clusters/.test(s.textContent) && /Serverless/.test(s.textContent))`)
	lab.eval(`[...document.querySelectorAll(".jp-LauncherCard")].find((c) => c.textContent.trim() === "Clusters").click(), true`, nil)
	lab.waitForWithin(`[...document.querySelectorAll(".jp-MainAreaWidget:not(.lm-mod-hidden)")].some((w) => /CREATE CLUSTER/i.test(w.innerText) && /No rows to display|RUNNING|Running/.test(w.innerText))`, 2*time.Minute)
	lab.run(chromedp.Click(chromedp.CSS(`.jp-SideBar [data-id="bigquery-jupyter-plugin-explorer"]`)))
	lab.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll("#jp-left-stack > .lm-Widget:not(.lm-mod-hidden) .bq-row.bq-project .bq-label")].some((l) => l.textContent.trim() === %q)`, project), time.Minute)
	lab.eval(fmt.Sprintf(`[...document.querySelectorAll(".bq-row.bq-project")].find((r) => r.querySelector(".bq-label").textContent.trim() === %q).click(), true`, project), nil)
	lab.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll(".bq-label")].some((l) => l.textContent.trim() === %q)`, dataset), time.Minute)
	lab.run(chromedp.Click(chromedp.CSS(`.jp-SideBar [data-id="GCS-bucket-tab"]`)))
	lab.waitForWithin(`(() => { const p = document.querySelector("#jp-left-stack > .lm-Widget:not(.lm-mod-hidden)"); return !!p && /Google Cloud Storage/.test(p.innerText) && !/error/i.test(p.innerText); })()`, time.Minute)
	lab.run(chromedp.Click(chromedp.CSS(`.jp-SideBar [data-id="filebrowser"]`)))
	lab.eval(`(() => { const b = document.querySelector('[data-command="launcher:create"], .jp-ToolbarButtonComponent[title="New Launcher"]'); if (b) b.click(); return true; })()`, nil)
	// The new Launcher's notebook card is the one shown (offsetParent: the
	// hidden tabs' cards have none).
	const shownCard = `[...document.querySelectorAll('.jp-LauncherCard[data-category="Notebook"]')].filter((c) => c.offsetParent !== null).pop()`
	lab.waitFor(shownCard + ` !== undefined`)

	// The Launcher's Python 3 notebook; a cell querying the instance's
	// BigQuery with Google's client, run with Shift+Enter.
	lab.eval(shownCard+`.click(), true`, nil)
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
	// Typed as text input, as a keyboard's characters reach xterm.js.
	lab.run(cmd(input.InsertText, input.InsertTextParams{Text: "echo $((6*7)) $(whoami) > from-terminal.txt"}))
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

	auditScreens(t, []a11yPage{
		{"workbench instances with JupyterLab", listPath, rowReady(name), ""},
		{"workbench instance", "/agent-platform/workbench/instances/" + url.PathEscape(name) + q, settled, ""},
	})
}

// writeShot keeps a failing JupyterLab window's screenshot beside the
// harness's own.
func writeShot(t *testing.T, name string, png []byte) string {
	dir := os.Getenv(envScreenshots)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "cloudburrow-browser")
	}
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+"-"+name+".png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return err.Error()
	}
	return path
}
