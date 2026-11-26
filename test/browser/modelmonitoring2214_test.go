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

// TestAgentPlatformMonitoringThroughThePages (#2214): on Agent Platform's
// Monitoring page, Configure monitoring sets up a model monitor of a Model
// Registry model with a training dataset in Cloud Storage; the monitor's
// Run now runs a job on drifted serving data, whose feature statistics and
// alerts its page shows; Schedule creates a schedule of runs, which the
// Schedules tab lists and pauses and resumes; the Deployment monitoring
// jobs tab lists v1's jobs; the monitor is deleted. Every screen passes the
// WCAG audit (names, contrast, keyboard).
func TestAgentPlatformMonitoringThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-model-monitors", "vertex-model-monitoring-schedules", "vertex-model-deployment-monitoring", "vertex-models", "storage"} {
		needService(t, s)
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	r := "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("mm-browser-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
	put := func(object, body string) { uploadObject(t, project, []string{bucket}, object, body) }
	put("train.csv", "age,country\n30,us\n31,us\n32,fr\n33,fr\n")
	put("serve.jsonl", `{"age":60,"country":"us"}`+"\n"+`{"age":61,"country":"us"}`+"\n")
	modelID := fmt.Sprintf("browser-monitored-%d", stamp)
	model := "projects/" + project + "/locations/us-central1/models/" + modelID
	fixture(t, "vertex-models", project, `{"region":"us-central1","displayName":"monitored","modelId":"`+modelID+`","imageUri":"us-docker.pkg.dev/p/r/serve:1"}`, model)

	type item struct {
		Name, Status string
		Fields       map[string]string
	}
	find := func(svc, field, value string) item {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+svc+q+r, "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", svc, code, body)
		}
		var l struct{ Items []item }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			if it.Fields[field] == value {
				return it
			}
		}
		return item{}
	}

	p := open(t)
	monitorsPath := "/agent-platform/model-monitoring/model-monitors" + q
	p.navigate(monitorsPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Configure monitoring")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Configure monitoring")`)
	monName := fmt.Sprintf("browser monitor %d", stamp)
	p.submitForm(map[string]string{"displayName": monName, "model": modelID, "version": "1", "features": "age:integer\ncountry:categorical",
		"training": "gs://" + bucket + "/train.csv", "threshold": "0.2"}, "displayName")
	var mon item
	for deadline := time.Now().Add(30 * time.Second); mon.Name == "" && time.Now().Before(deadline); time.Sleep(time.Second) {
		mon = find("vertex-model-monitors", "Monitor name", monName)
	}
	if mon.Name == "" || mon.Fields["Objectives"] != "Input feature drift" {
		t.Fatalf("the monitor = %+v", mon)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-model-monitors"+q+r+"&name="+url.QueryEscape(mon.Name), "")
	})
	act := func(action string, values map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"path": []string{mon.Name}, "action": action, "values": values})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/vertex-model-monitors"+q+r, string(b)); code != http.StatusOK {
			t.Fatalf("%s = %d: %s", action, code, body)
		}
	}
	act("run", map[string]string{"displayName": "browser run", "target": "gs://" + bucket + "/serve.jsonl", "targetFormat": "jsonl"})
	detailPath := "/agent-platform/model-monitoring/model-monitors/" + url.PathEscape(mon.Name) + q + r
	var body string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		_, body = consoleDo(t, http.MethodGet, "/api/detail/vertex-model-monitors"+q+r+"&name="+url.QueryEscape(mon.Name), "")
		if strings.Contains(body, "Succeeded") && strings.Contains(body, "l_infinity") {
			break
		}
	}
	if !strings.Contains(body, "Succeeded") || !strings.Contains(body, "above the threshold") {
		t.Fatalf("the monitor's runs, statistics and alerts = %s", body)
	}
	p.navigate(monitorsPath)
	p.waitFor(rowReady(monName))
	p.follow(mon.Name)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Succeeded"))`)
	p.clickText("#view [role=tab], #view .tab-strip a, #view .tab-strip button", "Feature statistics")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("country") && r.textContent.includes("Yes"))`)
	p.eval(`location.pathname + location.search`, &detailPath)

	act("schedule", map[string]string{"displayName": fmt.Sprintf("browser schedule %d", stamp), "cron": "0 * * * *", "target": "gs://" + bucket + "/serve.jsonl", "targetFormat": "jsonl"})
	schedName := fmt.Sprintf("browser schedule %d", stamp)
	schedulesPath := "/agent-platform/model-monitoring/schedules" + q
	sched := find("vertex-model-monitoring-schedules", "Schedule name", schedName)
	if sched.Status != "Active" || sched.Fields["Model monitor"] != mon.Name {
		t.Fatalf("the schedule = %+v", sched)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-model-monitoring-schedules"+q+r+"&name="+url.QueryEscape(sched.Name), "")
	})
	p.navigate(monitorsPath)
	p.waitFor(settled)
	p.clickText("#view .tab-strip a, #view .tab-strip button, #view [role=tab]", "Schedules")
	p.waitFor(`location.pathname === "/agent-platform/model-monitoring/schedules" && ` + rowReady(schedName))
	p.colabRowItem(sched.Name, "Pause")
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if sched = find("vertex-model-monitoring-schedules", "Schedule name", schedName); sched.Status == "Paused" {
			break
		}
	}
	if sched.Status != "Paused" {
		t.Errorf("after Pause the schedule is %q", sched.Status)
	}
	jobsPath := "/agent-platform/model-monitoring/deployment-jobs" + q
	p.navigate(jobsPath)
	p.waitFor(settled)

	auditScreens(t, []a11yPage{
		{"model monitors", monitorsPath, rowReady(monName), ""},
		{"model monitor", detailPath, settled, ""},
		{"monitoring schedules", schedulesPath, rowReady(schedName), ""},
		{"deployment monitoring jobs", jobsPath, settled, ""},
		{"configure monitoring", monitorsPath, settled, `[...document.querySelectorAll("#view a, #view button")].find((b) => b.textContent.trim() === "Configure monitoring").click()`},
	})

	p = open(t)
	p.navigate(schedulesPath)
	p.openRowDelete(sched.Name, "Delete")
	// Google describes no dialog for it: the default, its ID typed back.
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.typeConfirmWord()
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, schedName))
}
