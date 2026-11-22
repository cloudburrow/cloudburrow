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

// TestAppHubRegistersADataformRepository (#2110, round 4), in the run
// shard, whose instance serves App Hub and Dataform: a Dataform repository
// made through the console is a discovered service on App Hub's Services
// and workloads page, in its region, under the URI supported-resources
// writes (//dataform.googleapis.com/projects/PROJECT_ID/locations/LOCATION/repositories/REPOSITORY,
// re-read 2026-11-22), and is registered on a regional application's page
// with Register service/workload. Then the pages through the WCAG 2.1 AA
// audit, its keyboard walk among them.
func TestAppHubRegistersADataformRepository(t *testing.T) {
	needService(t, "apphub")
	needService(t, "dataform")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	const region = "us-central1"
	stamp := time.Now().UnixNano() % 1e6
	repoID := fmt.Sprintf("ah-repo-%d", stamp)
	repo := "projects/" + project + "/locations/" + region + "/repositories/" + repoID
	appID := fmt.Sprintf("df-app-%d", stamp)
	app := "projects/" + project + "/locations/" + region + "/applications/" + appID
	act := func(provider, action string, path []string, values map[string]string) (int, string) {
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		return consoleDo(t, http.MethodPost, "/api/actions/"+provider+q, string(b))
	}
	t.Cleanup(func() {
		act("apphub", "unregister", []string{app}, map[string]string{"name": app + "/services/repo"})
		consoleDo(t, http.MethodDelete, "/api/resources/apphub"+q+"&name="+url.QueryEscape(app), "")
		act("dataform", "delete", []string{repo}, map[string]string{})
		consoleDo(t, http.MethodDelete, "/api/resources/apphub-settings"+q+"&name="+
			url.QueryEscape("projects/"+project+"/locations/global/serviceProjectAttachments/"+project), "")
	})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/dataform"+q, `{"repositoryId":"`+repoID+`","region":"`+region+`"}`); code != http.StatusOK {
		t.Fatalf("create repository = %d: %s", code, body)
	}
	uri := "//dataform.googleapis.com/" + repo

	// The project attached to itself; the repository then listed on
	// Services and workloads, in its region.
	p := open(t)
	p.navigate("/apphub/settings" + q)
	p.clickText("#view button", "Attach projects")
	p.submitForm(map[string]string{"project": project}, "project")
	p.waitFor(`location.pathname === "/apphub/settings" && document.querySelectorAll("#view tbody tr").length > 0`)
	p.navigate("/apphub/services-workloads" + q)
	p.waitFor(listedRow("/apphub/services-workloads", uri))
	var row string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q)).textContent`, uri), &row)
	for _, want := range []string{"Service", region, project} {
		if !strings.Contains(row, want) {
			t.Errorf("the repository's row %q has no %q", row, want)
		}
	}

	// A regional application in the repository's region registers it.
	p.navigate("/apphub/applications" + q)
	p.clickText("#view button", "Create application")
	// Regional, as a user picks it: the scope's radio, then its region.
	p.waitFor(`document.querySelector('#view input[name="f-scope-choice"][value="REGIONAL"]') !== null`)
	p.eval(`(() => { document.querySelector('#view input[name="f-scope-choice"][value="REGIONAL"]').click(); return true; })()`, nil)
	p.waitFor(`(() => { const r = document.querySelector("#view #f-region"); return !!r && r.getClientRects().length > 0; })()`)
	p.submitForm(map[string]string{"region": region, "applicationId": appID}, "applicationId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/apphub/applications" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, app))
	p.follow(app)
	var appPath string
	p.eval(`location.pathname + location.search`, &appPath)
	p.clickText("#view .page-actions button", "Register service/workload")
	option := fmt.Sprintf(`[...document.querySelectorAll(".modal.is-open #f-resource option")].find((o) => o.textContent.includes(%q))`, uri)
	p.waitFor(option + ` !== undefined`)
	var resource string
	p.eval(option+`.value`, &resource)
	p.submitForm(map[string]string{"resource": resource, "id": "repo"}, "id")
	p.waitFor(`document.querySelector("#view").textContent.includes("repo")`)
	q2 := url.Values{"project": {project}, "name": {app}}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/apphub?"+q2.Encode(), "")
	if code != http.StatusOK || !strings.Contains(body, uri) {
		t.Errorf("the application reads back = %d: %s; want %s registered", code, body, uri)
	}

	auditScreens(t, []a11yPage{
		screen("App Hub services and workloads", "/apphub/services-workloads", project),
		{"regional application", appPath, settled, ""},
	})
}
