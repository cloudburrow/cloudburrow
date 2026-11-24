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
	"github.com/chromedp/chromedp/kb"
)

// colabRowItem opens a list row's Actions menu and chooses item.
func (p *tab) colabRowItem(name, item string) {
	p.t.Helper()
	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, menu))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.run(chromedp.Click(chromedp.CSS(menu)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, item)
}

// TestColabEnterpriseRuntimesThroughThePages (#2105): Agent Platform >
// Colab Enterprise, as Google's "Create a runtime template" and "Create a
// runtime" walk it. Runtime templates' New template opens Create new
// runtime template (Runtime basics, Configure compute, Environment,
// Networking and security); the template lists by its display name, its
// ID opens its page, whose Edit (Edit runtime template, Update) renames
// it; the row's Set as Default sets it. Runtimes' Create (Create Agent
// Platform runtime: Runtime template and Runtime name) starts a runtime
// from it, listed Running; Stop and Start move it, and Delete with
// Confirm removes it; the template's Delete removes the template. The
// console API reads each result back; every screen meets WCAG 2.1 AA and
// New template is reached by keyboard.
func TestColabEnterpriseRuntimesThroughThePages(t *testing.T) {
	needService(t, "colab-templates")
	needService(t, "colab-runtimes")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	display := fmt.Sprintf("Browser template %d", time.Now().UnixNano()%1e6)
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
	templateID := func(name string) string {
		for _, it := range list("colab-templates").Items {
			if it.Fields["Display name"] == name {
				return it.Name
			}
		}
		return ""
	}

	p := open(t)
	templatesPath := "/agent-platform/colab/runtime-templates" + q
	runtimesPath := "/agent-platform/colab/runtimes" + q
	// New template, by keyboard: Tab to it, Enter opens its page.
	p.navigate(templatesPath)
	p.waitFor(settled)
	p.tabTo(`a.textContent.trim() === "New template" || (a.tagName === "BUTTON" && a.textContent.trim() === "New template")`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	var createPath, heading string
	p.eval(`location.pathname + location.search`, &createPath)
	p.eval(`document.querySelector("#view h1").textContent.trim()`, &heading)
	if heading != "Create new runtime template" {
		t.Errorf("the create page's heading = %q, want Create new runtime template", heading)
	}
	var sections []string
	p.eval(`[...document.querySelectorAll("#view form .create-step-title, #view form .form-section-title, #view form h2, #view form h3, #view form legend")].map((n) => n.textContent.trim())`, &sections)
	for _, want := range []string{"Runtime basics", "Configure compute", "Environment", "Networking and security"} {
		found := false
		for _, s := range sections {
			found = found || s == want || len(s) > len(want) && s[len(s)-len(want):] == want
		}
		if !found {
			t.Errorf("the create page has no %q section: %q", want, sections)
		}
	}
	p.submitForm(map[string]string{"displayName": display, "region": "us-central1", "idleMinutes": "30", "env": "STAGE=browser"}, "displayName")
	p.waitFor(`location.pathname === "/agent-platform/colab/runtime-templates" && ` + rowReady(display))
	id := templateID(display)
	if id == "" {
		t.Fatalf("the Runtime templates list has no %q", display)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-templates"+q+"&region=us-central1&name="+url.QueryEscape(id), "")
	})
	for _, it := range list("colab-templates").Items {
		if it.Name == id && (it.Fields["Machine type"] != "e2-standard-4" || it.Fields["Idle shutdown"] != "30 minutes" || it.Fields["Data disk"] != "100 GB pd-standard") {
			t.Errorf("the template's row = %+v", it.Fields)
		}
	}

	// Its ID opens its page; Edit renames it.
	p.follow(id)
	var templatePath string
	p.eval(`location.pathname + location.search`, &templatePath)
	p.pageAction("Edit", map[string]string{"displayName": display + " edited"}, "")
	if templateID(display+" edited") != id {
		t.Errorf("after Edit, the template is not listed as %q", display+" edited")
	}
	display += " edited"

	// Set as Default, from its row.
	p.navigate(templatesPath)
	p.waitFor(rowReady(display))
	p.colabRowItem(id, "Set as Default")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// Runtimes: Create from the template.
	p.navigate(runtimesPath)
	p.waitFor(settled)
	p.clickText("#view button", "Create")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-notebookRuntimeTemplate option")].some((o) => o.value === %q)`, id))
	var dialogTitle string
	p.eval(`(document.querySelector(".modal.is-open h2") || {}).textContent || ""`, &dialogTitle)
	if dialogTitle != "Create Agent Platform runtime" {
		t.Errorf("the create runtime title = %q", dialogTitle)
	}
	runtimeName := "browser-runtime"
	p.submitForm(map[string]string{"notebookRuntimeTemplate": id, "displayName": runtimeName}, "displayName")
	p.waitForWithin(`location.pathname === "/agent-platform/colab/runtimes" && `+rowReady(runtimeName), 30*time.Minute)
	// Create answers once the runtime is assigned, as Google's does: it is
	// Starting until its container is up (on a runner, the first one
	// builds the runtime image), then Running.
	runtimeID, created := "", ""
	for deadline := time.Now().Add(10 * time.Minute); ; time.Sleep(2 * time.Second) {
		for _, it := range list("colab-runtimes").Items {
			if it.Fields["Runtime name"] == runtimeName && it.Fields["Runtime template"] == id {
				runtimeID, created = it.Name, it.Status
			}
		}
		if created != "Starting" || time.Now().After(deadline) {
			break
		}
	}
	if runtimeID == "" {
		t.Fatalf("the Runtimes list has no %q", runtimeName)
	}
	if created != "Running" {
		t.Errorf("the new runtime is %q, want Running", created)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-runtimes"+q+"&region=us-central1&name="+url.QueryEscape(runtimeID), "")
	})
	status := func() string {
		for _, it := range list("colab-runtimes").Items {
			if it.Name == runtimeID {
				return it.Status
			}
		}
		return ""
	}
	p.navigate(runtimesPath)
	p.colabRowItem(runtimeID, "Stop")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if s := status(); s != "Stopped" {
		t.Errorf("after Stop the runtime is %q", s)
	}
	p.navigate(runtimesPath)
	p.colabRowItem(runtimeID, "Start")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, 10*time.Minute)
	// Start is an operation: the runtime is Starting until its container is up.
	started := status()
	for deadline := time.Now().Add(10 * time.Minute); started == "Starting" && time.Now().Before(deadline); started = status() {
		time.Sleep(2 * time.Second)
	}
	if started != "Running" {
		t.Errorf("after Start the runtime is %q", started)
	}

	auditScreens(t, []a11yPage{
		{"runtime templates", templatesPath, rowReady(display), ""},
		{"create runtime template", createPath, settled + ` && document.querySelector("#view form input") !== null`, ""},
		{"runtime template details", templatePath, settled, ""},
		{"runtimes", runtimesPath, rowReady(runtimeName), ""},
		{"create runtime dialog", runtimesPath, rowReady(runtimeName),
			`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Create").click()`},
	})

	// Delete, with Confirm, as Google's runtimes page asks.
	p = open(t)
	p.navigate(runtimesPath)
	p.openRowDelete(runtimeID, "Delete")
	p.confirmOnlyDialog()
	p.pressDelete()
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, runtimeName))
	if s := status(); s != "" {
		t.Errorf("a deleted runtime is still listed, %q", s)
	}
	p.navigate(templatesPath)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, display))
}

// TestColabEnterpriseExecutionsThroughThePages (#2105): Agent Platform >
// Colab Enterprise, as Google's "Schedule a notebook run" and "Manage
// access to a runtime template" walk it. My notebooks' Create notebook
// makes a notebook (a Dataform repository, as BigQuery Studio keeps one);
// its Schedule (Schedule notebook: Schedule name, Runtime template, Run
// schedule, Cloud Storage output location, Submit) with One-off runs it
// once, listed on Executions until Succeeded, whose View result opens the
// executed notebook's folder in Cloud Storage; Recurring makes a schedule,
// listed on Schedules, whose Schedule details, Pause and Resume work; Delete
// removes the execution and the schedule. The runtime template's
// Permissions tab grants the Notebook Runtime User role, read back. The
// console API reads each result back; every screen meets WCAG 2.1 AA.
func TestColabEnterpriseExecutionsThroughThePages(t *testing.T) {
	for _, s := range []string{"colab-templates", "colab-notebooks", "colab-executions", "colab-schedules", "dataform", "storage"} {
		needService(t, s)
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	r := "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("colab-exec-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
	type item struct {
		Name, Status string
		Fields       map[string]string
		Actions      []struct{ ID, Label, Href string }
	}
	list := func(svc string) []item {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+svc+q+r, "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", svc, code, body)
		}
		var l struct{ Items []item }
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		return l.Items
	}
	find := func(svc, field, value string) item {
		for _, it := range list(svc) {
			if it.Fields[field] == value {
				return it
			}
		}
		return item{}
	}
	// A runtime template, through the console API (the Runtime templates
	// pages are TestColabEnterpriseRuntimesThroughThePages').
	tplName := fmt.Sprintf("Exec template %d", stamp)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/colab-templates"+q+r, `{"displayName":"`+tplName+`","region":"us-central1","machineType":"e2-standard-4","acceleratorType":"NONE","diskType":"pd-standard","diskSize":"100","idleShutdown":"true","idleMinutes":"180","environment":"latest","postStartupBehavior":"RUN_ONCE","internet":"true","euc":"true"}`); code != http.StatusOK {
		t.Fatalf("create a runtime template = %d: %s", code, body)
	}
	tpl := find("colab-templates", "Display name", tplName).Name
	if tpl == "" {
		t.Fatal("the new runtime template is not listed")
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-templates"+q+r+"&name="+url.QueryEscape(tpl), "")
	})

	p := open(t)
	notebooksPath := "/agent-platform/colab/notebooks" + q
	executionsPath := "/agent-platform/colab/executions" + q
	schedulesPath := "/agent-platform/colab/schedules" + q
	nbName := fmt.Sprintf("Browser notebook %d", stamp)
	p.navigate(notebooksPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create notebook")
	p.submitForm(map[string]string{"displayName": nbName}, "displayName")
	p.waitFor(`location.pathname === "/agent-platform/colab/notebooks" && ` + rowReady(nbName))
	nb := find("colab-notebooks", "Notebook name", nbName)
	if nb.Name == "" {
		t.Fatalf("My notebooks does not list %q", nbName)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-notebooks"+q+r+"&name="+url.QueryEscape(nb.Name), "")
	})
	openHref := ""
	for _, a := range nb.Actions {
		if a.Label == "Open" {
			openHref = a.Href
		}
	}
	if !strings.HasPrefix(openHref, "/bigquery-studio?") {
		t.Errorf("the notebook's Open = %q, want BigQuery Studio", openHref)
	}

	schedule := func(name, run string, extra map[string]string) {
		t.Helper()
		p.navigate(notebooksPath)
		p.waitFor(rowReady(nbName))
		p.colabRowItem(nb.Name, "Schedule")
		var title string
		p.waitFor(`document.querySelector(".modal.is-open h2") !== null`)
		p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
		if title != "Schedule notebook" {
			t.Errorf("the Schedule dialog's title = %q", title)
		}
		v := map[string]string{"displayName": name, "runtimeTemplate": tpl, "runSchedule": run, "output": "gs://" + bucket}
		for k, x := range extra {
			v[k] = x
		}
		p.submitForm(v, "displayName")
		p.waitForWithin(`document.querySelector(".modal") === null`, 2*time.Minute)
	}
	// One-off: an execution.
	runName := fmt.Sprintf("once %d", stamp)
	schedule(runName, "one-off", nil)
	var exec item
	for deadline := time.Now().Add(20 * time.Minute); ; {
		exec = find("colab-executions", "Execution name", runName)
		if exec.Status == "Succeeded" || exec.Status == "Failed" || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if exec.Status != "Succeeded" {
		t.Fatalf("the one-off execution = %+v", exec)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-executions"+q+r+"&name="+url.QueryEscape(exec.Name), "")
	})
	if exec.Fields["Runtime template"] != tpl {
		t.Errorf("the execution's runtime template = %q", exec.Fields["Runtime template"])
	}
	p.navigate(executionsPath)
	p.waitFor(rowReady(runName))
	p.colabRowItem(exec.Name, "View result")
	p.waitFor(`location.pathname.startsWith("/storage/browser/` + bucket + `/` + exec.Name + `")`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("content.ipynb"))`)

	// Recurring: a schedule.
	schedName := fmt.Sprintf("weekly %d", stamp)
	schedule(schedName, "recurring", map[string]string{"cron": "0 9 * * MON"})
	sched := find("colab-schedules", "Schedule name", schedName)
	if sched.Name == "" || sched.Status != "Active" || sched.Fields["Frequency"] != "0 9 * * MON" {
		t.Fatalf("the recurring schedule = %+v", sched)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-schedules"+q+r+"&name="+url.QueryEscape(sched.Name), "")
	})
	p.navigate(schedulesPath)
	p.waitFor(rowReady(schedName))
	waitSchedule := func(what, want string) {
		t.Helper()
		got := ""
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if got = find("colab-schedules", "Schedule name", schedName).Status; got == want {
				return
			}
		}
		t.Errorf("after %s the schedule is %q, want %s", what, got, want)
	}
	p.colabRowItem(sched.Name, "Pause")
	waitSchedule("Pause", "Paused")
	p.navigate(schedulesPath)
	p.follow(sched.Name)
	var detailsPath, heading string
	p.waitFor(settled)
	p.eval(`location.pathname + location.search`, &detailsPath)
	p.eval(`document.querySelector("#view h1").textContent.trim()`, &heading)
	if heading != "Schedule details" && heading != schedName {
		t.Errorf("the schedule's page heading = %q", heading)
	}
	p.clickText(p.findAction("Resume"), "Resume")
	waitSchedule("Resume", "Active")

	// The template's Permissions.
	p.navigate("/agent-platform/colab/runtime-templates" + q)
	p.waitFor(rowReady(tplName))
	p.follow(tpl)
	var templatePath string
	p.eval(`location.pathname + location.search`, &templatePath)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#view #grant-access") !== null`)
	p.grantOnTab("user:ana@example.com", "roles/aiplatform.notebookRuntimeUser")
	b, _ := json.Marshal(map[string]any{"Path": []string{tpl}, "Read": map[string]any{}})
	if code, body := consoleDo(t, http.MethodPost, "/api/permissions/colab-templates"+q+r, string(b)); code != http.StatusOK ||
		!strings.Contains(body, "ana@example.com") || !strings.Contains(body, "roles/aiplatform.notebookRuntimeUser") {
		t.Errorf("after Grant access the template's policy reads %d %s", code, body)
	}

	auditScreens(t, []a11yPage{
		{"my notebooks", notebooksPath, rowReady(nbName), ""},
		{"schedule notebook dialog", notebooksPath, rowReady(nbName),
			fmt.Sprintf(`document.querySelector('button[aria-label=%q]').click(); setTimeout(() => [...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].find((b) => b.textContent.trim() === "Schedule").click(), 50)`, "Actions for "+nb.Name)},
		{"executions", executionsPath, rowReady(runName), ""},
		{"schedules", schedulesPath, rowReady(schedName), ""},
		{"schedule details", detailsPath, settled, ""},
		{"runtime template permissions", templatePath, settled,
			`[...document.querySelectorAll("#view .tab-strip .tab")].find((b) => b.textContent.trim() === "Permissions").click()`},
	})

	// Delete: the execution, then the schedule (whose runs stay).
	p = open(t)
	p.navigate(executionsPath)
	p.openRowDelete(exec.Name, "Delete")
	p.confirmOnlyDialog()
	p.pressDelete()
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, runName))
	p.navigate(schedulesPath)
	p.openRowDelete(sched.Name, "Delete")
	p.confirmByName(sched.Name)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, schedName))

	// My notebooks: Actions > Delete notebook, then the Delete notebook
	// dialog's Confirm (#2173), as Google's quickstart words it.
	p.navigate(notebooksPath)
	p.waitFor(rowReady(nbName))
	p.openRowDelete(nb.Name, "Delete notebook")
	var dialogTitle string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &dialogTitle)
	if dialogTitle != "Delete notebook" {
		t.Errorf("the notebook's delete dialog is titled %q, want Delete notebook", dialogTitle)
	}
	p.pressConfirm("Confirm")
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, nbName))
	if find("colab-notebooks", "Notebook name", nbName).Name != "" {
		t.Error("Delete notebook left the notebook listed")
	}
}
