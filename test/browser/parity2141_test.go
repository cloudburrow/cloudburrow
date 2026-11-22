//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLConsoleParity2141 (#2141), in the sql shard, on the `up`
// instance postgres:
//   - Query insights is Google's Top queries table (Database, User, Load
//     by total time, Avg execution time (ms), Times called, Avg rows
//     returned), read from the server's pg_stat_statements, where the
//     console's own reads of the server are listed;
//   - System insights' summary has Google's cards: CPU utilization (not
//     available locally, said so), Peak connections of max_connections,
//     Transaction ID utilization, Disk utilization and Log errors;
//   - the create page's root password has Show password and Generate;
//   - Delete instance offers Google's final backup (Take a final backup,
//     Retain for (days)), then asks for the name;
//   - Clone opens Google's "Create a clone" with Clone current state
//     of instance and Clone from an earlier point in time;
//   - the Instances row menu has Create read replica and Create clone, and
//     Restart asks "Restart database instance?";
//   - each screen passes the console's WCAG 2.1 AA audit (names,
//     contrast in both themes, keyboard, motion, reflow).
func TestCloudSQLConsoleParity2141(t *testing.T) {
	needService(t, "cloudsql-instances")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	page := "/cloudsql-instances/postgres" + q

	p := open(t)
	p.navigate(page + "&tab=query-insights")
	p.waitFor(`document.querySelector("#view").textContent.includes("Top queries") && document.querySelector("#view thead") !== null`)
	var cols []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean)`, &cols)
	for _, w := range []string{"Query", "Database", "User", "Load by total time", "Avg execution time (ms)", "Times called", "Avg rows returned"} {
		if !contains(cols, w) {
			t.Errorf("the Top queries columns are %q; want %s", cols, w)
		}
	}
	var rows int
	p.eval(`document.querySelectorAll("#view tbody tr").length`, &rows)
	if rows == 0 {
		t.Error("Query insights lists no query; the server has run some")
	}

	p.navigate(page + "&tab=system-insights")
	p.waitFor(`document.querySelector("#view").textContent.includes("Peak connections")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, w := range []string{"CPU utilization - P99 | P50", "Not available locally", "Peak connections", " of ", "Transaction ID utilization", "Disk utilization", "Log errors"} {
		if !strings.Contains(text, w) {
			t.Errorf("System insights lacks %q", w)
		}
	}

	// The create page's root password: Show password, then Generate.
	p.navigate("/cloudsql-instances/create" + q)
	p.waitFor(`document.querySelector("#generate-rootPassword") !== null && document.querySelector("#show-rootPassword") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#show-rootPassword`)))
	var shown struct {
		Type, Pressed string
	}
	p.eval(`({ Type: document.querySelector("#f-rootPassword").type, Pressed: document.querySelector("#show-rootPassword").getAttribute("aria-pressed") })`, &shown)
	if shown.Type != "text" || shown.Pressed != "true" {
		t.Errorf("after Show password the field is %+v; want text, pressed", shown)
	}
	p.run(chromedp.Click(chromedp.CSS(`#generate-rootPassword`)))
	var generated string
	p.eval(`document.querySelector("#f-rootPassword").value`, &generated)
	if len(generated) != 20 {
		t.Errorf("Generate filled %d characters; want 20", len(generated))
	}

	// An instance to open Delete, Create clone and the row menu on: the
	// instances `up` runs are not deleted.
	svc := sqlAdminSDK(t)
	name := fmt.Sprintf("browser-2141-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
	page = "/cloudsql-instances/" + name + q

	// Delete: Google's final backup options, the name typed; cancelled.
	p.navigate(page)
	p.clickText(p.findAction("Delete"), "Delete")
	p.waitFor(`document.querySelector(".modal.is-open #f-finalBackup") !== null`)
	var del struct {
		Title   string
		Checked bool
		TTL     string
	}
	p.eval(`(() => { const m = document.querySelector(".modal.is-open");
		return { Title: m.querySelector("h2").textContent, Checked: m.querySelector("#f-finalBackup").checked,
			TTL: m.querySelector("#f-ttlDays").value }; })()`, &del)
	if del.Title != "Delete instance" || !del.Checked || del.TTL != "30" {
		t.Errorf("Delete instance's dialog is %+v; want Delete instance, final backup checked, 30 days", del)
	}
	// Its Delete asks for the instance's name; cancelled, nothing is deleted.
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.navigate(page)
	p.waitFor(`document.querySelector(".modal") === null`)
	if in, err := svc.Instances.Get(project, name).Do(); err != nil || in.State != "RUNNABLE" {
		t.Fatalf("after a cancelled Delete the instance reads %v, %v", in, err)
	}

	// Create clone: Google's dialog.
	p.navigate(page)
	p.clickText(p.findAction("Clone"), "Clone")
	p.waitFor(`document.querySelector(".modal.is-open #f-destination") !== null`)
	var clone struct{ Title, Labels, Submit string }
	p.eval(`(() => { const m = document.querySelector(".modal.is-open");
		return { Title: m.querySelector("h2").textContent, Submit: m.querySelector("button[type=submit]").textContent,
			Labels: [...m.querySelectorAll("label")].map((l) => l.textContent.trim()).join("|") }; })()`, &clone)
	if clone.Title != "Create a clone" || clone.Submit != "Create clone" || !strings.Contains(clone.Labels, "Clone current state of instance") ||
		!strings.Contains(clone.Labels, "Clone from an earlier point in time") {
		t.Errorf("Create clone's dialog is %+v", clone)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// The Instances row menu.
	p.navigate("/cloudsql-instances" + q)
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim().startsWith(%q)))`, name)
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.waitFor(`document.querySelector(".overflow-item") !== null`)
	var items []string
	p.eval(`[...document.querySelectorAll(".overflow-item")].map((i) => i.textContent.trim())`, &items)
	for _, w := range []string{"Create read replica", "Create clone", "Restart"} {
		if !contains(items, w) {
			t.Errorf("the row menu is %q; want %s", items, w)
		}
	}
	p.clickText(".overflow-item", "Restart")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &text)
	if text != "Restart database instance?" {
		t.Errorf("Restart's confirmation is %q", text)
	}
	p.clickText(".modal.is-open button", "Cancel")

	pageAct := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent === %q).click()`, label)
	}
	hasAct := func(label string) string {
		return settled + fmt.Sprintf(` && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === %q)`, label)
	}
	auditScreens(t, []a11yPage{
		screen("Query insights", "/cloudsql-instances/postgres?tab=query-insights", project),
		screen("System insights", "/cloudsql-instances/postgres?tab=system-insights", project),
		{"delete instance dialog", page, hasAct("Delete"), pageAct("Delete")},
		{"create clone dialog", page, hasAct("Clone"), pageAct("Clone")},
	})
}

// TestSecretManagerConsoleParity2142 (#2142), in the run shard: checking
// a secret opens Google's info panel with Permissions and Labels, whose
// Save sets the secret's labels; Delete asks for the name and ends in
// "Delete secret"; Create secret has Google's "Delay secret version
// destroy" (Destruction delay duration in days), Set expiration date with
// no TTL, and Add label and Add annotation rows. Each screen passes the
// WCAG 2.1 AA audit.
func TestSecretManagerConsoleParity2142(t *testing.T) {
	needService(t, "secrets")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	secret := "parity-2142"
	fixture(t, "secrets", project, `{"secretId":"`+secret+`","payload":"v"}`, secret)
	full := "projects/" + project + "/secrets/" + secret

	p := open(t)
	p.navigate("/secrets" + q)
	p.waitFor(rowReady(secret))
	var boxes []string
	p.eval(`[...document.querySelectorAll("#view tbody input[type=checkbox]")].map((b) => b.getAttribute("aria-label"))`, &boxes)
	if !contains(boxes, "Select "+full) {
		t.Fatalf("the secrets list's checkboxes are %q; want Select %s", boxes, secret)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view input[aria-label="Select ` + full + `"]`)))
	p.waitFor(`document.querySelector("#info-tab-permissions") !== null && document.querySelector("#info-tab-labels") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#info-tab-labels`)))
	p.waitFor(`document.querySelector("#info-tab-panel #f-labels") !== null`)
	setField(p, "#info-tab-panel", "f-labels", `{"env":"dev"}`)
	p.run(chromedp.Click(chromedp.CSS(`#info-tab-panel button[type="submit"]`)))
	eventually(t, "the Labels tab's labels to be saved", func() bool {
		_, body := consoleDo(t, http.MethodGet, "/api/resources/secrets"+q, "")
		return strings.Contains(body, "env=dev") || strings.Contains(body, `env: dev`) || strings.Contains(body, `"env":"dev"`)
	})

	// Create secret's sections.
	p.navigate("/secrets/create" + q)
	p.waitFor(`document.querySelector("#f-destroyDelayDays") !== null`)
	var create struct {
		Headings string
		TTL      bool
		Expire   bool
	}
	p.eval(`({ Headings: [...document.querySelectorAll(".form-section-title, .create-step-title")].map((h) => h.textContent.trim()).join("|"),
		TTL: document.querySelector("#f-ttl") !== null, Expire: document.querySelector("#f-expireTime") !== null })`, &create)
	if !strings.Contains(create.Headings, "Delay secret version destroy") || create.TTL || !create.Expire {
		t.Errorf("Create secret is %+v; want Delay secret version destroy, an expiration date and no TTL", create)
	}
	var adds []string
	p.eval(`[...document.querySelectorAll("#view button")].map((b) => b.textContent.trim()).filter((s) => s.startsWith("Add "))`, &adds)
	for _, w := range []string{"Add label", "Add annotation"} {
		if !contains(adds, w) {
			t.Errorf("Create secret's buttons are %q; want %s", adds, w)
		}
	}

	// Delete: the name typed, then Delete secret.
	p.navigate("/secrets" + q)
	p.waitFor(rowReady(secret))
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, secret)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText(".overflow-item", "Delete")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var submit string
	p.eval(`document.querySelector(".modal.is-open button[type=submit]").textContent`, &submit)
	if submit != "Delete secret" {
		t.Errorf("Delete's button is %q; want Delete secret", submit)
	}
	var asked string
	p.eval(`document.querySelector(".modal.is-open").textContent`, &asked)
	if !strings.Contains(asked, secret) {
		t.Errorf("Delete's dialog %q does not name the secret", asked)
	}
	p.confirmByName(full)
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	fixture(t, "secrets", project, `{"secretId":"`+secret+`-b","payload":"v"}`, secret+"-b")
	panelOn := `document.querySelector('#view input[aria-label="Select ` + full + `-b"]').click()`
	auditScreens(t, []a11yPage{
		screen("create secret", "/secrets/create", project),
		{"secrets info panel Labels", "/secrets" + q, rowReady(secret + "-b"),
			panelOn + `; setTimeout(() => document.querySelector("#info-tab-labels").click(), 50)`},
	})
}

// TestParameterManagerConsoleParity2143 (#2143), in a shard serving
// Parameter Manager: the Parameters list's Actions menu (Add new version,
// Edit, Delete), Add new version opening Google's Create parameter
// version page, Edit parameter a page ending in Save, a version row's
// Actions (View payload, Render, Disable, Delete), Delete version asking
// for the parameter's ID, and a template deleted with its versions, its
// Delete asking for its name. Each screen passes the WCAG 2.1 AA audit.
func TestParameterManagerConsoleParity2143(t *testing.T) {
	needService(t, "parametermanager")
	project := uniqueProject(t)
	pq := "?project=" + url.QueryEscape(project)
	param := "projects/" + project + "/locations/global/parameters/parity"
	tpl := "projects/" + project + "/locations/global/templates/parity-tpl"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/parametermanager"+pq,
		`{"parameterId":"parity","locationType":"multi","format":"YAML","encryption":"google"}`); code != http.StatusOK {
		t.Fatalf("create the parameter = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/parametermanager-templates"+pq,
		`{"templateId":"parity-tpl","locationType":"multi","format":"TEMPLATE_FORMAT_YAML"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}

	p := open(t)
	p.navigate("/secrets-parameters" + pq)
	p.waitFor(rowReady(param))
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, param)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.waitFor(`document.querySelector(".overflow-item") !== null`)
	var items []string
	p.eval(`[...document.querySelectorAll(".overflow-item")].map((i) => i.textContent.trim())`, &items)
	if strings.Join(items, ",") != "Add new version,Edit,Delete" {
		t.Errorf("the parameter's row menu is %q; want Add new version, Edit, Delete", items)
	}
	p.clickText(".overflow-item", "Add new version")
	p.waitFor(`new URLSearchParams(location.search).get("action") === "newversion" &&
		[...document.querySelectorAll("#view h1, #view h2")].some((h) => h.textContent.includes("Create parameter version"))`)
	var upload bool
	p.eval(`document.querySelector("#up-payload") !== null`, &upload)
	if !upload {
		t.Error("Create parameter version has no Browse for the payload")
	}
	p.submitForm(map[string]string{"versionId": "v1", "payload": "level: debug\n"}, "versionId")
	p.waitFor(`new URLSearchParams(location.search).get("action") === null && document.querySelector("#view tbody tr") !== null`)

	// The version row's menu, and Delete asking for the parameter's ID.
	vfind := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "v1"))`
	p.waitFor(vfind + ` !== undefined`)
	p.eval(vfind+`.querySelector(".overflow-trigger").click()`, nil)
	p.waitFor(`document.querySelector(".overflow-item") !== null`)
	p.eval(`[...document.querySelectorAll(".overflow-item")].map((i) => i.textContent.trim())`, &items)
	if strings.Join(items, ",") != "View payload,Render,Disable,Delete" {
		t.Errorf("the version's row menu is %q; want View payload, Render, Disable, Delete", items)
	}
	p.clickText(".overflow-item", "Delete")
	p.confirmByName("parity")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	eventually(t, "the version to be deleted", func() bool {
		_, body := consoleDo(t, http.MethodGet, "/api/detail/parametermanager?"+url.Values{"project": {project}, "name": {param, "v1"}}.Encode(), "")
		return strings.Contains(body, "unavailable")
	})

	// Edit parameter: a page ending in Save.
	paramPage := "/secrets-parameters/" + url.PathEscape(param) + pq
	p.navigate(paramPage)
	p.clickText(p.findAction("Edit"), "Edit")
	p.waitFor(`new URLSearchParams(location.search).get("action") === "edit" && document.querySelector("#view form") !== null`)
	var save string
	p.eval(`document.querySelector("#view form button[type=submit]").textContent`, &save)
	if save != "Save" {
		t.Errorf("Edit parameter ends in %q; want Save", save)
	}

	// A template with a version is deleted with it, its name typed.
	b := fmt.Sprintf(`{"Path":[%q],"Action":"newversion","Values":{"versionId":"tv1","payload":"log: {{.level}}\n"}}`, tpl)
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/parametermanager-templates"+pq, b); code != http.StatusOK {
		t.Fatalf("add the template version = %d: %s", code, body)
	}
	p.navigate("/secrets-parameters/templates/" + url.PathEscape(tpl) + pq)
	p.clickText(p.findAction("Delete"), "Delete")
	p.confirmByName("parity-tpl")
	p.waitFor(`location.pathname === "/secrets-parameters/templates"`)
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/parametermanager-templates"+pq, ""); code != http.StatusOK || strings.Contains(body, "parity-tpl") {
		t.Errorf("after Delete the templates read %d: %s", code, body)
	}

	auditScreens(t, []a11yPage{
		{"create parameter version page", paramPage + "&action=newversion", settled + ` && document.querySelector("#f-versionId") !== null`, ""},
		{"edit parameter page", paramPage + "&action=edit", settled + ` && document.querySelector("#view form") !== null`, ""},
		screen("create parameter", "/secrets-parameters/create", project),
	})
}
