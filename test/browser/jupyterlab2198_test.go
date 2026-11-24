//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// labWindow is the JupyterLab window a click opened: a new target whose
// URL matches want.
func labWindow(t *testing.T, p *tab, want func(string) bool, click func()) *tab {
	t.Helper()
	opened := chromedp.WaitNewTarget(p.ctx, func(i *target.Info) bool { return want(i.URL) })
	click()
	var id target.ID
	select {
	case id = <-opened:
	case <-time.After(time.Minute):
		t.Fatal("no JupyterLab window opened")
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
	lab.waitForWithin(`/\/lab/.test(location.pathname) && document.querySelector(".jp-Launcher") !== null`, 3*time.Minute)
	return lab
}

// labRunCell opens the Launcher's notebook card whose text matches kernel
// (a kernel's display name), types code into its first cell as a keyboard
// does, runs it with Shift+Enter, and returns the cell's output once it
// matches want or shows an error. It works with JupyterLab 3 (CodeMirror
// 5) and 4 (CodeMirror 6).
func labRunCell(t *testing.T, lab *tab, kernel, code string, want *regexp.Regexp) string {
	t.Helper()
	lab.waitFor(fmt.Sprintf(`[...document.querySelectorAll('.jp-LauncherCard[data-category="Notebook"]')].some((c) => new RegExp(%q).test(c.textContent + " " + (c.title || "")))`, kernel))
	lab.eval(fmt.Sprintf(`[...document.querySelectorAll('.jp-LauncherCard[data-category="Notebook"]')].find((c) => new RegExp(%q).test(c.textContent + " " + (c.title || ""))).click(), true`, kernel), nil)
	const panel = `.jp-NotebookPanel:not(.lm-mod-hidden):not(.p-mod-hidden)`
	lab.waitForWithin(`document.querySelector("`+panel+` .jp-Cell .jp-InputArea-editor") !== null`, 2*time.Minute)
	// The kernel is idle before the cell is run.
	lab.waitForWithin(`(() => { const s = document.querySelector("`+panel+` .jp-Notebook-ExecutionIndicator, `+panel+` .jp-KernelStatus, .jp-KernelStatus");
		const st = s && (s.getAttribute("data-status") || s.textContent || "");
		return /idle/i.test(st) || (!!document.querySelector("`+panel+` .jp-Toolbar-kernelStatus.jp-CircleIcon, `+panel+` [data-status=idle]")); })()`, 3*time.Minute)
	lab.run(chromedp.Click(chromedp.CSS(panel + ` .jp-Cell .jp-InputArea-editor`)))
	lab.run(cmd(input.InsertText, input.InsertTextParams{Text: code}))
	// CodeMirror 5 reads its input on its own poll: the cell holds the code
	// once its editor shows it.
	lab.waitFor(fmt.Sprintf(`(document.querySelector("`+panel+` .jp-Cell .jp-InputArea-editor").textContent || "").includes(%q)`, code[:min(len(code), 20)]))
	time.Sleep(500 * time.Millisecond)
	lab.run(chromedp.KeyEvent(kb.Enter, chromedp.KeyModifiers(chromedp.ModifierShift)))
	var output string
	for deadline := time.Now().Add(8 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		lab.eval(`[...document.querySelectorAll("`+panel+` .jp-OutputArea-output")].map((o) => o.textContent).join("\n")`, &output)
		if want.MatchString(output) || strings.Contains(output, "Error") {
			break
		}
	}
	if !want.MatchString(output) {
		t.Fatalf("the cell's output = %q, want %s", output, want)
	}
	return output
}

// TestColabRuntimeJupyterLabFromTheConsole (#2198): a Colab Enterprise
// runtime runs the real Jupyter Server, as Google's runtimes do. A
// runtime made on the Runtimes page (Create, from a runtime template)
// lists Running with Open JupyterLab, which opens, in a new window, the
// runtime's own JupyterLab on its proxy_uri host. In the real JupyterLab
// UI, the Launcher's Python 3 notebook runs a cell that queries the
// instance's BigQuery with Google's client (pointed at it by the runtime's
// kernel setup) and prints its result, and the screens meet WCAG 2.1 AA
// and work by keyboard.
func TestColabRuntimeJupyterLabFromTheConsole(t *testing.T) {
	needService(t, "colab-templates")
	needService(t, "colab-runtimes")
	needService(t, "bigquery")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	display := "Lab template " + suffix
	runtimeName := "lab-runtime-" + suffix
	type listing struct {
		Items []struct {
			Name, Status string
			Fields       map[string]string
		}
	}
	list := func(svc string) listing {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+svc+q+"&region=us-central1", "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", svc, code, body)
		}
		var l listing
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}

	p := openWithin(t, 40*time.Minute)
	templatesPath := "/agent-platform/colab/runtime-templates" + q
	runtimesPath := "/agent-platform/colab/runtimes" + q
	p.navigate(templatesPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "New template")
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	p.submitForm(map[string]string{"displayName": display, "region": "us-central1"}, "displayName")
	p.waitFor(`location.pathname === "/agent-platform/colab/runtime-templates" && ` + rowReady(display))
	templateID := ""
	for _, it := range list("colab-templates").Items {
		if it.Fields["Display name"] == display {
			templateID = it.Name
		}
	}
	if templateID == "" {
		t.Fatalf("the Runtime templates list has no %q", display)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-templates"+q+"&region=us-central1&name="+url.QueryEscape(templateID), "")
	})

	p.navigate(runtimesPath)
	p.waitFor(settled)
	p.clickText("#view button", "Create")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-notebookRuntimeTemplate option")].some((o) => o.value === %q)`, templateID))
	p.submitForm(map[string]string{"notebookRuntimeTemplate": templateID, "displayName": runtimeName}, "displayName")
	p.waitForWithin(`location.pathname === "/agent-platform/colab/runtimes" && `+rowReady(runtimeName), 30*time.Minute)
	runtimeID := ""
	for _, it := range list("colab-runtimes").Items {
		if it.Fields["Runtime name"] == runtimeName {
			runtimeID = it.Name
		}
	}
	if runtimeID == "" {
		t.Fatalf("the Runtimes list has no %q", runtimeName)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-runtimes"+q+"&region=us-central1&name="+url.QueryEscape(runtimeID), "")
	})
	p.navigate(runtimesPath)
	p.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Running"))`, runtimeName), 10*time.Minute)

	lab := labWindow(t, p, func(u string) bool { return strings.Contains(u, ".notebooks.localhost") }, func() { p.colabRowItem(runtimeID, "Open JupyterLab") })
	var host string
	lab.eval(`location.host`, &host)
	if !strings.Contains(host, "-dot-us-central1.notebooks.localhost:") {
		t.Errorf("JupyterLab opened at %s, want the runtime's proxy_uri host", host)
	}
	labRunCell(t, lab, `^Python 3|Python 3 \(ipykernel\)`,
		`from google.cloud import bigquery; import os; print("answer", list(bigquery.Client().query("SELECT 6 * 7 AS x").result())[0].x, os.environ.get("HOME"))`,
		regexp.MustCompile(`answer 42 /home/colab`))

	auditScreens(t, []a11yPage{
		{"runtimes with JupyterLab", runtimesPath, rowReady(runtimeName), ""},
		{"runtime row actions", runtimesPath, rowReady(runtimeName),
			fmt.Sprintf(`document.querySelector('button[aria-label=%q]').click()`, "Actions for "+runtimeID)},
	})
}

// TestDataprocJupyterLabThroughComponentGateway (#2198): a Dataproc
// cluster with the Jupyter component and Enable component gateway, made
// on the Create cluster page, lists JupyterLab on its Web interfaces tab,
// as Google's Component Gateway page documents; the link opens the
// cluster's real JupyterLab, whose Launcher's PySpark notebook runs a cell
// on the cluster's Spark and shows its result. The cluster's screens meet
// WCAG 2.1 AA and work by keyboard.
func TestDataprocJupyterLabThroughComponentGateway(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	name := fmt.Sprintf("lab-%d", time.Now().UnixNano()%1e6)
	p := openWithin(t, 40*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-clusterName", "f-comp_JUPYTER", "f-componentGateway"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"clusterName": name, "imageVersion": "2.2-debian12", "workers": "2", "comp_JUPYTER": "true", "componentGateway": "true"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/" + name
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 20*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", name))
	p.follow(name)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.clickText(`#view [role="tab"]`, "Web interfaces")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "JupyterLab")`)
	var href string
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent.trim() === "JupyterLab").href`, &href)
	if !strings.HasSuffix(href, "/jupyter/lab") {
		t.Errorf("the JupyterLab link is %s, want the cluster's Component Gateway /jupyter/lab", href)
	}
	lab := labWindow(t, p, func(u string) bool { return strings.Contains(u, "/jupyter/lab") }, func() {
		// The link opens a new window, as the console's web interfaces do;
		// one in the same window is followed into a new one.
		p.eval(`(() => { const a = [...document.querySelectorAll("#view a")].find((a) => a.textContent.trim() === "JupyterLab");
			if (a.target !== "_blank") a.target = "_blank"; a.click(); return true; })()`, nil)
	})
	labRunCell(t, lab, `PySpark`, `print("sum", spark.range(100).selectExpr("sum(id) AS s").first()["s"], sc.master)`,
		regexp.MustCompile(`sum 4950 spark://`+name+`-m:7077`))

	auditScreens(t, []a11yPage{
		{"cluster web interfaces", clusterPath, settled, `[...document.querySelectorAll('#view [role="tab"]')].find((b) => b.textContent.trim() === "Web interfaces").click()`},
	})
}
