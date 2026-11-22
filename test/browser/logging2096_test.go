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
)

// TestLoggingAppHubDimensions (#2096), in the run shard, whose instance
// serves Cloud Run, App Hub and Cloud Logging. A Cloud Run job made on its
// Create job form is registered, on its App Hub application's page, as a
// workload of the application; an entry for the job's monitored resource
// (cloud_run_job) then carries the output-only apphub, which entries.list
// answers and the Logging API's own filter (apphub.application.id) matches,
// and the Logs Explorer's System metadata has Google's dynamic dimensions
// in Google's order (logs-explorer-interface, re-read 2026-11-21): Workload
// / Service, made from apphub.workload.id (and from
// resource.labels.namespace_name), then Application, from
// apphub.application.id. Choosing the application's value adds
// apphub.application.id="APP" to the query and leaves the job's entry
// alone; filtering by the Audited Resource type makes Workload / Service
// from resource.labels.service. Then the pages through the WCAG 2.1 AA
// audit.
func TestLoggingAppHubDimensions(t *testing.T) {
	needService(t, "apphub")
	needService(t, "run-jobs")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	job := fmt.Sprintf("logs-job-%d", time.Now().UnixNano()%1e6)
	const appID, workloadID, region = "logs-app", "logs-workload", "us-central1"
	app := "projects/" + project + "/locations/global/applications/" + appID
	act := func(provider, action string, path []string, values map[string]string) (int, string) {
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		return consoleDo(t, http.MethodPost, "/api/actions/"+provider+"?project="+url.QueryEscape(project), string(b))
	}
	t.Cleanup(func() {
		act("apphub", "unregister", []string{app}, map[string]string{"name": app + "/workloads/" + workloadID})
		consoleDo(t, http.MethodDelete, "/api/resources/apphub?project="+project+"&name="+url.QueryEscape(app), "")
		consoleDo(t, http.MethodDelete, "/api/resources/run-jobs?project="+project+"&name="+job, "")
		consoleDo(t, http.MethodDelete, "/api/resources/apphub-settings?project="+project+"&name="+
			url.QueryEscape("projects/"+project+"/locations/global/serviceProjectAttachments/"+project), "")
	})

	// The job, made on Cloud Run's Create job form.
	p := open(t)
	p.navigate("/run/jobs/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-name") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#view #f-name`), job),
		chromedp.SendKeys(chromedp.CSS(`#view #f-image`), "docker.io/library/busybox:1.36"))
	p.clickText(`#view button[type="submit"]`, "Create")
	p.waitFor(fmt.Sprintf(`location.pathname === "/run/jobs" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, job))

	// The project attached to itself, and a global application.
	p.navigate("/apphub/settings?project=" + project)
	p.clickText("#view button", "Attach projects")
	p.submitForm(map[string]string{"project": project}, "project")
	p.waitFor(`location.pathname === "/apphub/settings" && document.querySelectorAll("#view tbody tr").length > 0`)
	p.navigate("/apphub/applications?project=" + project)
	p.clickText("#view button", "Create application")
	p.submitForm(map[string]string{"applicationId": appID, "criticality": "HIGH", "environment": "PRODUCTION"}, "applicationId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/apphub/applications" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, app))

	// The job registered as a workload, on the application's page.
	p.follow(app)
	var appPath string
	p.eval(`location.pathname + location.search`, &appPath)
	p.clickText("#view .page-actions button", "Register service/workload")
	jobURI := "//run.googleapis.com/projects/" + project + "/locations/" + region + "/jobs/" + job
	option := fmt.Sprintf(`[...document.querySelectorAll(".modal.is-open #f-resource option")].find((o) => o.textContent.includes(%q))`, jobURI)
	p.waitFor(option + ` !== undefined`)
	var resource string
	p.eval(option+`.value`, &resource)
	p.submitForm(map[string]string{"resource": resource, "id": workloadID, "criticality": "HIGH", "environment": "PRODUCTION"}, "id")
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, workloadID))

	// Entries: the job's, a Kubernetes namespace's and an audited service's.
	logName := "projects/" + project + "/logs/apphub-dimensions"
	write := func(resource map[string]any, text string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"logName": logName, "resource": resource, "entries": []map[string]any{{"textPayload": text}}})
		if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
			t.Fatalf("entries:write = %d: %s", code, out)
		}
	}
	write(map[string]any{"type": "cloud_run_job", "labels": map[string]string{"project_id": project, "location": region, "job_name": job}}, "from the job")
	write(map[string]any{"type": "k8s_container", "labels": map[string]string{"project_id": project, "location": region,
		"cluster_name": "c", "namespace_name": "shop", "pod_name": "p", "container_name": "c"}}, "from the namespace")
	write(map[string]any{"type": "audited_resource", "labels": map[string]string{"project_id": project, "service": "storage.googleapis.com", "method": "m"}}, "from the audit")

	// entries.list: the job's entry has apphub, and the API's own filter
	// on apphub.* matches it alone.
	list := func(filter string) []map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"resourceNames": []string{"projects/" + project}, "filter": filter})
		code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:list", string(b), "")
		if code != http.StatusOK {
			t.Fatalf("entries.list %s = %d: %s", filter, code, out)
		}
		var l struct{ Entries []map[string]any }
		_ = json.Unmarshal([]byte(out), &l)
		return l.Entries
	}
	got := list(fmt.Sprintf("logName=%q AND apphub.application.id=%q", logName, appID))
	if len(got) != 1 || got[0]["textPayload"] != "from the job" {
		t.Fatalf("entries.list with apphub.application.id=%q = %v; want the job's entry alone", appID, got)
	}
	a, _ := got[0]["apphub"].(map[string]any)
	w, _ := a["workload"].(map[string]any)
	if ap, _ := a["application"].(map[string]any); ap["id"] != appID || w["id"] != workloadID ||
		w["environmentType"] != "PRODUCTION" || w["criticalityType"] != "HIGH" {
		t.Errorf("the job's entry's apphub = %v", a)
	}
	if n := len(list(fmt.Sprintf("logName=%q AND apphub.workload.id=%q", logName, workloadID))); n != 1 {
		t.Errorf("entries.list with apphub.workload.id = %d entries, want 1", n)
	}

	// The Logs Explorer's System metadata, in Google's order.
	q := url.QueryEscape(fmt.Sprintf("logName=%q", logName))
	explorer := "/logs?scope=project&project=" + project + "&query=" + q
	p.navigate(explorer)
	group := func(field string) string {
		return fmt.Sprintf(`document.querySelector('#log-fields-system .log-field-group[data-field=%q]')`, field)
	}
	p.waitFor(group("apphub.application.id") + ` !== null`)
	var system string
	p.eval(`[...document.querySelectorAll("#log-fields-system .log-field-group h4")].map((h) => h.textContent).join("|")`, &system)
	if system != "Resource type|Severity|Workload / Service|Application|Log name|Project ID" {
		t.Errorf("System metadata = %q; want Workload / Service, then Application, after Resource type and Severity", system)
	}
	values := func(field string) string {
		t.Helper()
		var out string
		p.eval(`(() => { const g = `+group(field)+`; return g ? [...g.querySelectorAll(".log-field-value")]
			.map((b) => b.dataset.value + "=" + b.querySelector(".log-field-count").textContent).sort().join("|") : ""; })()`, &out)
		return out
	}
	if got := values("apphub.application.id"); got != appID+"=1" {
		t.Errorf("Application = %q; want %s (1)", got, appID)
	}
	if got := values("apphub.workload.id"); got != workloadID+"=1|shop=1" {
		t.Errorf("Workload / Service = %q; want %s (1) from apphub.workload.id and shop (1) from resource.labels.namespace_name", got, workloadID)
	}
	var title string
	p.eval(group("apphub.workload.id")+`.querySelector('.log-field-value[data-value="shop"]').title`, &title)
	if title != `Add resource.labels.namespace_name="shop" to the query` {
		t.Errorf("shop's value adds %q; want its own label's clause", title)
	}
	p.eval(group("apphub.application.id")+`.querySelector('.log-field-value[data-value=`+fmt.Sprintf("%q", appID)+`]').click(), true`, nil)
	p.waitFor(fmt.Sprintf(`document.querySelector("#log-query").value.includes(%q)`, `apphub.application.id="`+appID+`"`))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].filter((r) => r.textContent.includes("from the")).length === 1 &&
		document.querySelector("#view tbody").textContent.includes("from the job")`)

	// Filtered by the Audited Resource type, Workload / Service is made
	// from resource.labels.service too.
	p.navigate("/logs?scope=project&project=" + project + "&query=" + url.QueryEscape(fmt.Sprintf(`logName=%q AND resource.type="audited_resource"`, logName)))
	p.waitFor(group("apphub.workload.id") + ` !== null`)
	if got := values("apphub.workload.id"); got != "storage.googleapis.com=1" {
		t.Errorf("Workload / Service under the Audited Resource type = %q; want storage.googleapis.com (1)", got)
	}
	if got := values("apphub.application.id"); got != "" {
		t.Errorf("Application with no App Hub entry = %q; want no dimension", got)
	}

	auditScreens(t, []a11yPage{
		{"Logs Explorer with the Application dimension", explorer, settled + ` && ` + group("apphub.application.id") + ` !== null`, ""},
		{"App Hub application with its workload", appPath, settled, ""},
	})
}
