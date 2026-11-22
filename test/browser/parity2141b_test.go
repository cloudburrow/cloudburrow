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
	"github.com/chromedp/chromedp/kb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLZonesNetworkingBackupsAndCharts (#2141), in the sql shard:
//   - the create page's Specify zones (Primary zone; Secondary zone only
//     for Multiple zones) and Connections' Private IP (Private Service
//     Access with its network, Show allocated IP range option and Enable
//     private path, or Private Service Connect);
//   - an instance's Connections > Networking Edit, ending in Save;
//   - Backups' Edit, Google's Manage automated backups page, saved;
//   - the Overview's chart, its metric chosen from a select;
//   - each screen passes the WCAG 2.1 AA audit, keyboard included.
func TestCloudSQLZonesNetworkingBackupsAndCharts(t *testing.T) {
	needService(t, "cloudsql-instances")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)

	p := open(t)
	p.navigate("/cloudsql-instances/create" + q)
	p.waitFor(`document.querySelector("#f-zone") !== null && document.querySelector("#f-privateIp") !== null`)
	visible := func(id string) bool {
		var v bool
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector("#f-%s"); return !!f && f.getClientRects().length > 0; })()`, id), &v)
		return v
	}
	var zones []string
	p.eval(`[...document.querySelector("#f-zone").options].map((o) => o.value)`, &zones)
	if len(zones) < 2 || zones[0] != "" || !strings.HasPrefix(zones[1], "us-central1-") {
		t.Errorf("Primary zone offers %q; want Any, then us-central1's zones", zones)
	}
	if visible("secondaryZone") {
		t.Error("Secondary zone is shown for Single zone")
	}
	p.eval(`(() => { const r = document.querySelector('input[name="f-availabilityType-choice"][value="REGIONAL"]'); r.click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-secondaryZone").getClientRects().length > 0`)
	if visible("privateNetwork") {
		t.Error("the private IP's network is shown with Private IP off")
	}
	// Private IP by keyboard: focus it and press Space.
	p.run(chromedp.Focus(`#f-privateIp`), chromedp.KeyEvent(" "))
	p.waitFor(`document.querySelector("#f-privateIp").checked && document.querySelector("#f-privateNetwork").getClientRects().length > 0`)
	var labels string
	p.eval(`[...document.querySelectorAll("#view label")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, w := range []string{"Private Service Access (PSA)", "Private Service Connect (PSC)", "Enable private path"} {
		if !strings.Contains(labels, w) {
			t.Errorf("Connections lacks %q", w)
		}
	}
	p.eval(`document.querySelector('input[name="f-privateMethod-choice"][value="psc"]').click()`, nil)
	p.waitFor(`document.querySelector("#f-pscAllowedProjects").getClientRects().length > 0 && document.querySelector("#f-privateNetwork").getClientRects().length === 0`)

	// An instance for Networking, Backups and the chart.
	svc := sqlAdminSDK(t)
	name := fmt.Sprintf("browser-2141b-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-east1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", LocationPreference: &sqladmin.LocationPreference{Zone: "us-east1-c"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
	if in, err := svc.Instances.Get(project, name).Do(); err != nil || in.GceZone != "us-east1-c" {
		t.Fatalf("the instance's zone reads %v, %v; want us-east1-c", in, err)
	}
	page := "/cloudsql-instances/" + name + q

	// The Overview's chart: CPU utilization first, then another metric.
	p.navigate(page)
	p.waitFor(`document.querySelector("#overview-chart-metric") !== null`)
	var metrics []string
	p.eval(`[...document.querySelector("#overview-chart-metric").options].map((o) => o.textContent.trim())`, &metrics)
	if len(metrics) < 2 || metrics[0] != "CPU utilization" {
		t.Errorf("the Overview chart offers %q; want CPU utilization first", metrics)
	}

	// Backups' Edit: Manage automated backups, saved.
	p.navigate(page + "&action=edit-backups")
	p.waitFor(`document.querySelector("#f-retainedBackups") !== null`)
	var title string
	p.eval(`[...document.querySelectorAll("#view h1, #view h2")].map((h) => h.textContent.trim()).join("|")`, &title)
	if !strings.Contains(title, "Manage automated backups") {
		t.Errorf("Backups' Edit is %q; want Manage automated backups", title)
	}
	p.submitForm(map[string]string{"retainedBackups": "12"}, "retainedBackups")
	eventually(t, "the backup settings to be saved", func() bool {
		in, err := svc.Instances.Get(project, name).Do()
		return err == nil && in.Settings.BackupConfiguration != nil && in.Settings.BackupConfiguration.BackupRetentionSettings != nil &&
			in.Settings.BackupConfiguration.BackupRetentionSettings.RetainedBackups == 12
	})

	// Connections > Networking's Edit: Public IP and Private IP, Save.
	p.navigate(page + "&tab=connections")
	p.clickText("#view [role=tab]", "Networking")
	p.clickText("#view .section-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-privateIp") !== null`)
	var net struct{ Title, Submit string }
	p.eval(`(() => { const m = document.querySelector(".modal.is-open");
		return { Title: m.querySelector("h2").textContent, Submit: m.querySelector("button[type=submit]").textContent }; })()`, &net)
	if net.Title != "Networking" || net.Submit != "Save" {
		t.Errorf("Networking's Edit is %+v; want Networking, Save", net)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	auditScreens(t, []a11yPage{
		screen("create instance (zones, private IP)", "/cloudsql-instances/create", project),
		{"manage automated backups page", page + "&action=edit-backups", settled + ` && document.querySelector("#f-retainedBackups") !== null`, ""},
		{"overview chart", page, settled + ` && document.querySelector("#overview-chart-metric") !== null`, ""},
	})
}

// TestSecretManagerTagsColumn2142 (#2142), in the run shard: the Secrets
// list has Google's columns, Tags among them, after Labels.
func TestSecretManagerTagsColumn2142(t *testing.T) {
	needService(t, "secrets")
	project := uniqueProject(t)
	secret := "tags-2142"
	fixture(t, "secrets", project, `{"secretId":"`+secret+`","payload":"v"}`, secret)
	p := open(t)
	p.navigate("/secrets?project=" + url.QueryEscape(project))
	p.waitFor(rowReady(secret))
	var cols []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean)`, &cols)
	at := map[string]int{}
	for i, c := range cols {
		at[c] = i + 1
	}
	if at["Tags"] == 0 || at["Labels"] == 0 || at["Tags"] != at["Labels"]+1 {
		t.Errorf("the Secrets columns are %q; want Tags after Labels", cols)
	}
	auditScreens(t, []a11yPage{screen("secrets list with Tags", "/secrets", project)})
}

// TestParameterManagerSecretPickerAndRender2143 (#2143): Create parameter
// version's payload opens Google's Secret picker on Ctrl+Space then
// Enter, its Insert putting the secret version's __REF__ in the payload;
// and a template version's Render is its own page, Render template
// version, which opens a page with the payload and the rendered value.
// Each screen passes the WCAG 2.1 AA audit, keyboard included.
func TestParameterManagerSecretPickerAndRender2143(t *testing.T) {
	needService(t, "parametermanager")
	needService(t, "secrets")
	project := uniqueProject(t)
	pq := "?project=" + url.QueryEscape(project)
	fixture(t, "secrets", project, `{"secretId":"picked","payload":"s3cr3t"}`, "picked")
	param := "projects/" + project + "/locations/global/parameters/picker"
	tpl := "projects/" + project + "/locations/global/templates/render-tpl"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/parametermanager"+pq,
		`{"parameterId":"picker","locationType":"multi","format":"YAML","encryption":"google"}`); code != http.StatusOK {
		t.Fatalf("create the parameter = %d: %s", code, body)
	}
	paramPage := "/secrets-parameters/" + url.PathEscape(param) + pq

	p := open(t)
	p.navigate(paramPage + "&action=newversion")
	p.waitFor(`document.querySelector("#f-payload") !== null`)
	p.run(chromedp.Focus(`#f-payload`), chromedp.KeyEvent(" ", chromedp.KeyModifiers(chromedp.ModifierCtrl)))
	p.waitFor(`document.querySelector("#f-payload").getAttribute("aria-expanded") === "true"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#secret-picker #secret-picker-secret") !== null`)
	var picker struct {
		Title    string
		Secrets  []string
		Versions []string
	}
	p.eval(`(() => { const d = document.querySelector("#secret-picker");
		return { Title: d.querySelector("h2").textContent,
			Secrets: [...d.querySelectorAll("#secret-picker-secret option")].map((o) => o.textContent.trim()),
			Versions: [...d.querySelectorAll("#secret-picker-version option")].map((o) => o.textContent.trim()) }; })()`, &picker)
	if picker.Title != "Secret picker" || !contains(picker.Secrets, "picked") || !contains(picker.Versions, "latest") {
		t.Errorf("the Secret picker is %+v; want picked, latest", picker)
	}
	p.run(chromedp.Click(chromedp.CSS(`#secret-picker-insert`)))
	p.waitFor(`document.querySelector("#secret-picker") === null || !document.querySelector("#secret-picker").closest(".modal.is-open")`)
	var payload string
	p.eval(`document.querySelector("#f-payload").value`, &payload)
	if want := `__REF__("//secretmanager.googleapis.com/projects/` + project + `/secrets/picked/versions/latest")`; !strings.Contains(payload, want) {
		t.Errorf("after Insert the payload is %q; want %s", payload, want)
	}
	p.submitForm(map[string]string{"versionId": "v1", "payload": "level: debug\n"}, "versionId")
	p.waitFor(`new URLSearchParams(location.search).get("action") === null && document.querySelector("#view tbody tr") !== null`)

	// Render template version: its own page, then the render's.
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/parametermanager-templates"+pq,
		`{"templateId":"render-tpl","locationType":"multi","format":"TEMPLATE_FORMAT_YAML"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}
	b := fmt.Sprintf(`{"Path":[%q],"Action":"newversion","Values":{"versionId":"tv1","payload":"log: {{.level}}\n"}}`, tpl)
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/parametermanager-templates"+pq, b); code != http.StatusOK {
		t.Fatalf("add the template version = %d: %s", code, body)
	}
	versionPage := "/secrets-parameters/templates/" + url.PathEscape(tpl) + "/tv1" + pq
	p.navigate(versionPage + "&action=render")
	p.waitFor(`document.querySelector("#f-parameterVersion") !== null && document.querySelector("#f-parameterVersion").value !== ""`)
	var heads string
	p.eval(`[...document.querySelectorAll("#view h1, #view h2")].map((h) => h.textContent.trim()).join("|")`, &heads)
	if !strings.Contains(heads, "Render template version") {
		t.Errorf("Render is %q; want the Render template version page", heads)
	}
	p.submitForm(map[string]string{}, "parameterVersion")
	p.waitFor(`location.pathname.includes("/render/") && document.querySelector("#view").textContent.includes("Rendered value")`)
	p.clickText("#view [role=tab]", "Rendered value")
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "log: debug") {
		t.Errorf("the render's page lacks the rendered value: %q", text)
	}
	var renderPath string
	p.eval(`location.pathname + location.search`, &renderPath)

	auditScreens(t, []a11yPage{
		{"render template version page", versionPage + "&action=render", settled + ` && document.querySelector("#f-parameterVersion") !== null`, ""},
		{"rendered page", renderPath, settled, ""},
		{"secret picker", paramPage + "&action=newversion", settled + ` && document.querySelector("#f-payload") !== null`,
			`(() => { const f = document.querySelector("#f-payload"); f.focus();
				f.dispatchEvent(new KeyboardEvent("keydown", { key: " ", code: "Space", ctrlKey: true, bubbles: true }));
				f.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true })); })()`},
	})
}
