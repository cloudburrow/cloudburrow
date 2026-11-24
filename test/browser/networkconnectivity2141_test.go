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
)

// TestConnectionPoliciesThroughTheBrowser (#2141), on an instance serving
// Cloud SQL and Compute Engine: Private Service Connect's Connection
// Policies tab, Create connection policy (Name, Source service class
// Google services, Service class, Network, Region, Custom service
// instance scope, Subnetworks, Connection limit) ending in Create policy,
// lists the policy and opens its page, whose Edit ends in Update policy;
// Cloud SQL's create page has Enable Managed Connection Pool (MySQL's
// with max_pool_size alone) and, with
// Private Service Connect chosen, Add an Endpoint; an instance's Query
// insights has the Database load chart. Each screen passes the WCAG 2.1
// AA audit, keyboard included.
func TestConnectionPoliciesThroughTheBrowser(t *testing.T) {
	needService(t, "psc-policies")
	needService(t, "cloudsql-instances")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	name := fmt.Sprintf("browser-pol-%d", time.Now().UnixNano()%1e6)

	p := open(t)
	p.navigate("/net-services/psc/list/policies" + q)
	p.waitFor(`[...document.querySelectorAll("#view [role=tab], #view a")].some((a) => a.textContent.trim() === "Connected endpoints")`)
	p.clickText("#view button, #view a", "Create connection policy")
	p.waitFor(`document.querySelector("#f-serviceClass") !== null`)
	var labels string
	p.eval(`[...document.querySelectorAll("#view label")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, w := range []string{"Name", "Google services", "Service class", "Network", "Region", "Custom service instance scope", "Subnetworks", "Connection limit"} {
		if !strings.Contains(labels, w) {
			t.Errorf("Create connection policy lacks %q", w)
		}
	}
	var submit string
	p.eval(`document.querySelector("#f-serviceClass").form.querySelector("button[type=submit]").textContent.trim()`, &submit)
	if submit != "Create policy" {
		t.Errorf("Create connection policy ends in %q; want Create policy", submit)
	}
	p.submitForm(map[string]string{"name": name, "subnetworks": "default", "limit": "5"}, "name")
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, name)
	p.waitFor(`location.pathname.startsWith("/net-services/psc/list/policies") && ` + row + ` !== undefined`)
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/psc-policies"+q+"&name="+url.QueryEscape("us-central1/"+name), "")
	})
	var text string
	p.eval(row+`.textContent`, &text)
	if !strings.Contains(text, "google-cloud-sql") || !strings.Contains(text, "default") {
		t.Errorf("the policy's row reads %q", text)
	}
	p.navigate("/net-services/psc/list/policies/" + url.PathEscape("us-central1/"+name) + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Endpoints configuration")`)

	p.navigate("/cloudsql-instances/create" + q)
	p.waitFor(`document.querySelector("#f-connectionPooling") !== null`)
	p.eval(`[...document.querySelectorAll("#view label")].map((l) => l.textContent.trim()).join("|")`, &labels)
	if !strings.Contains(labels, "Enable Managed Connection Pool") {
		t.Error("the create page lacks Enable Managed Connection Pool")
	}
	// MySQL's thread pool (#2141): with MySQL chosen, Enable Managed
	// Connection Pool, checked by keyboard, shows max_pool_size alone.
	p.eval(`(() => { document.querySelector('input[name="f-engine-choice"][value="MySQL"]').click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-mysqlConnectionPooling").getClientRects().length > 0 && document.querySelector("#f-connectionPooling").getClientRects().length === 0`)
	p.run(chromedp.Focus(`#f-mysqlConnectionPooling`), chromedp.KeyEvent(" "))
	p.waitFor(`document.querySelector("#f-mysqlConnectionPooling").checked`)
	p.eval(`(() => { const b = [...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Advanced pooling options" &&
		b.getClientRects().length > 0); b.click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-mysql_pool_max_pool_size").getClientRects().length > 0`)
	var poolMode bool
	p.eval(`!!document.querySelector("#f-poolMode") && document.querySelector("#f-poolMode").getClientRects().length > 0`, &poolMode)
	if poolMode {
		t.Error("MySQL's pooling shows PostgreSQL's Pool mode")
	}
	p.eval(`(() => { document.querySelector('input[name="f-engine-choice"][value="PostgreSQL"]').click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-connectionPooling").getClientRects().length > 0`)
	p.eval(`(() => { const c = document.querySelector("#f-privateIp"); if (!c.checked) c.click();
		return true; })()`, nil)
	p.waitFor(`document.querySelector('input[name="f-privateMethod-choice"][value="psc"]') !== null`)
	p.eval(`(() => { document.querySelector('input[name="f-privateMethod-choice"][value="psc"]').click(); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Add an Endpoint")`)

	p.navigate("/cloudsql-instances/postgres" + q + "&tab=query-insights")
	p.waitFor(`document.querySelector("#view").textContent.includes("Database load - all top queries")`)

	auditScreens(t, []a11yPage{
		screen("connection policies", "/net-services/psc/list/policies", project),
		screen("create connection policy", "/net-services/psc/list/policies/create", project),
		screen("connection policy", "/net-services/psc/list/policies/"+url.PathEscape("us-central1/"+name), project),
		screen("query insights database load", "/cloudsql-instances/postgres?tab=query-insights", project),
	})
}
