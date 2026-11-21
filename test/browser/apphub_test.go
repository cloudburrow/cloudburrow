//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestAppHubThroughThePage (#2110), in the run shard, whose instance
// serves App Hub. Reached from App Hub's menu under Operations: Settings'
// Attach projects (the project attached to itself), Applications' Create
// application (a global one with a criticality and an environment), the
// application's page with its Details, and Services and workloads. The
// console API reads each result back. Every screen meets WCAG 2.1 AA, as
// the console's others do.
func TestAppHubThroughThePage(t *testing.T) {
	needService(t, "apphub")
	project := uniqueProject(t)
	detail := func(provider string, path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/"+provider+"?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	p := open(t)
	p.navigateByDrawer("Operations", "/apphub/applications", "App Hub")
	p.waitFor(`[...document.querySelectorAll('a[href^="/apphub/settings"]')].some((a) => a.textContent.includes("Settings"))`)

	p.navigate("/apphub/settings?project=" + project)
	p.clickText("#view button", "Attach projects")
	p.waitFor(`document.getElementById("f-project") !== null`)
	p.submitForm(map[string]string{"project": project}, "project")
	attachment := "projects/" + project + "/locations/global/serviceProjectAttachments/" + project
	p.waitFor(fmt.Sprintf(`location.pathname === "/apphub/settings" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, attachment))
	if got := detail("apphub-settings", attachment); !strings.Contains(got, "ACTIVE") {
		t.Errorf("the attachment reads back as %s", got)
	}

	p.navigate("/apphub/applications?project=" + project)
	p.clickText("#view button", "Create application")
	p.waitFor(`["f-applicationId", "f-criticality", "f-environment"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"applicationId": "browser-app", "displayName": "Browser app", "criticality": "HIGH", "environment": "STAGING"}, "applicationId")
	app := "projects/" + project + "/locations/global/applications/browser-app"
	p.waitFor(fmt.Sprintf(`location.pathname === "/apphub/applications" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, app))
	if got := detail("apphub", app); !strings.Contains(got, "High") || !strings.Contains(got, "Staging") || !strings.Contains(got, "Global") {
		t.Errorf("the application reads back as %s", got)
	}
	p.follow(app)
	var appPath string
	p.eval(`location.pathname + location.search`, &appPath)
	p.clickText(`#view [role="tab"]`, "Details")
	p.waitFor(`document.querySelector("#view").textContent.includes("Browser app")`)

	auditScreens(t, []a11yPage{
		screen("App Hub applications", "/apphub/applications", project),
		screen("create application", "/apphub/applications/create", project),
		{"application", appPath, settled, ""},
		screen("App Hub services and workloads", "/apphub/services-workloads", project),
		screen("App Hub settings", "/apphub/settings", project),
		screen("attach projects", "/apphub/settings/create", project),
	})
}
