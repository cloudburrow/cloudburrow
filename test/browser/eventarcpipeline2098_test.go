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

// TestEventarcCreatePipelineWithEnrollmentsAndCloudRun (#2098), in the run
// shard: Google's Create pipeline is one page, its sections Pipeline
// details, Enrollments, Event mediation and Destination, in that order.
// Through it, two pipelines are made with their enrollments (Add an
// enrollment: the name, the bus, the CEL expression): one to a Cloud Run
// service picked by name, one to a Cloud Run job picked by name with a
// message binding and an OAuth token. Each enrollment is then listed with
// its bus and pipeline, and each pipeline's page names its destination
// type. A message published to the bus that the first enrollment matches
// reaches the service (the pipeline's platform log says it was delivered,
// written only on a 2xx); one the second matches runs the job. The page
// meets WCAG with an enrollment row and the job's fields shown.
func TestEventarcCreatePipelineWithEnrollmentsAndCloudRun(t *testing.T) {
	needService(t, "run")
	needService(t, "run-jobs")
	needService(t, "eventarc-buses")
	needService(t, "logging-buckets")
	project := uniqueProject(t)
	n := fmt.Sprintf("%06d", time.Now().UnixNano()%1e6)
	svc, job, bus := "ea-svc-"+n, "ea-job-"+n, "cbus-"+n
	plSvc, plJob, enSvc, enJob := "to-svc-"+n, "to-job-"+n, "en-svc-"+n, "en-job-"+n
	q := "?project=" + project
	const image = "ghcr.io/knative/helloworld-go:latest"
	t.Cleanup(func() {
		for _, d := range [][2]string{{"eventarc-enrollments", enSvc}, {"eventarc-enrollments", enJob}, {"eventarc-pipelines", plSvc},
			{"eventarc-pipelines", plJob}, {"eventarc-buses", bus}, {"run-jobs", job}, {"run", svc}} {
			consoleDo(t, http.MethodDelete, "/api/resources/"+d[0]+q+"&name="+d[1], "")
		}
	})

	body, _ := json.Marshal(map[string]string{"name": svc, "image": image})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run"+q, string(body))
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time") && strings.Contains(resp, `"operation"`):
	default:
		t.Fatalf("deploy a service through the console API = %d: %s", code, resp)
	}
	waitForRunService(t, project, svc, image, 5*time.Minute)
	body, _ = json.Marshal(map[string]string{"name": job, "image": "docker.io/library/busybox:1.36", "command": "sh -c",
		"args": `'echo hi'`, "taskCount": "1", "maxRetries": "0"})
	if code, resp := consoleDo(t, http.MethodPost, "/api/resources/run-jobs"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a job through the console API = %d: %s", code, resp)
	}
	if code, resp := consoleDo(t, http.MethodPost, "/api/resources/eventarc-buses"+q, `{"name":"`+bus+`","logSeverity":"DEBUG"}`); code != http.StatusOK {
		t.Fatalf("create a bus through the console API = %d: %s", code, resp)
	}

	p := open(t)
	// createPipeline fills Google's page: Pipeline details, one enrollment
	// through Add an enrollment, then the destination's fields.
	createPipeline := func(name, enrollment, match string, dest map[string]string, waitOption string, audit bool) {
		t.Helper()
		p.navigate("/eventarc/pipelines/create" + q)
		p.waitFor(`document.querySelector("#f-name") !== null && [...document.querySelectorAll("#f-runService option")].some((o) => o.value === "` + svc + `")`)
		var order []int
		p.eval(`(() => { const t = document.querySelector("#view").textContent;
			return ["Pipeline details", "Enrollments", "Event mediation", "Destination type"].map((s) => t.indexOf(s)); })()`, &order)
		for i := 1; i < len(order); i++ {
			if order[i-1] < 0 || order[i] <= order[i-1] {
				t.Errorf("Create pipeline's sections are not Google's, in order: %v", order)
				break
			}
		}
		var types []string
		p.eval(`[...document.querySelectorAll("#f-destination option")].map((o) => o.value)`, &types)
		if !strings.Contains(strings.Join(types, "|"), "Cloud Run service (via HTTP)|Cloud Run job (via HTTP)") {
			t.Errorf("Destination type options = %v", types)
		}
		p.clickText("#view button", "Add an enrollment")
		p.waitFor(`document.querySelector('[aria-label="Enrollment name 1"]') !== null`)
		p.eval(fmt.Sprintf(`(() => { const set = (l, v) => { const c = document.querySelector('[aria-label="' + l + '"]');
			c.value = v; c.dispatchEvent(new Event("input", { bubbles: true })); c.dispatchEvent(new Event("change", { bubbles: true })); };
			set("Enrollment name 1", %q); set("Message bus 1", %q); set("CEL expression 1", %q); return true; })()`, enrollment, bus, match), nil)
		values := map[string]string{"name": name, "logSeverity": "INFO"}
		for k, v := range dest {
			values[k] = v
		}
		if waitOption != "" {
			p.waitFor(`[...document.querySelectorAll("#f-` + waitOption + ` option")].some((o) => o.value === "` + values[waitOption] + `")`)
		}
		if audit {
			for k, v := range values {
				p.setField("#f-"+k, v)
			}
			for _, u := range p.unnamedControls() {
				t.Errorf("Create pipeline: an unnamed %s", u)
			}
			for _, f := range p.contrastFailures() {
				t.Errorf("Create pipeline: %s", f)
			}
		}
		p.submitForm(values, "name")
		p.waitFor(`!location.pathname.endsWith("/create")`)
	}
	createPipeline(plSvc, enSvc, `message.type == "order.created"`,
		map[string]string{"destination": "Cloud Run service (via HTTP)", "runService": svc}, "runService", false)
	createPipeline(plJob, enJob, `message.type == "order.shipped"`, map[string]string{"destination": "Cloud Run job (via HTTP)", "runJob": job,
		"messageBinding": `{"body": {"overrides": {"containerOverrides": [{"args": ["echo order " + message.id]}]}}}`,
		"authEnabled":    "true", "authType": "OAuth token", "serviceAccount": "runner@" + project + ".iam.gserviceaccount.com"}, "runJob", true)

	for _, c := range [][3]string{{enSvc, plSvc, "Cloud Run service (via HTTP)"}, {enJob, plJob, "Cloud Run job (via HTTP)"}} {
		if got := eaDetailValue(t, "eventarc-enrollments", project, c[0], "Destination pipeline"); !strings.HasSuffix(got, c[1]) {
			t.Errorf("enrollment %s's pipeline reads %q", c[0], got)
		}
		if got := eaDetailValue(t, "eventarc-pipelines", project, c[1], "Destination type"); got != c[2] {
			t.Errorf("pipeline %s's destination type reads %q, want %q", c[1], got, c[2])
		}
	}
	if got := eaDetailValue(t, "eventarc-pipelines", project, plSvc, "Cloud Run service"); got != svc {
		t.Errorf("the service pipeline's Cloud Run service reads %q", got)
	}
	p.navigate("/eventarc/enrollments" + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("` + enJob + `") && document.querySelector("#view").textContent.includes("` + enSvc + `")`)

	// Publish on the bus's page: one event for each enrollment.
	evSvc, evJob := "created-"+n, "shipped-"+n
	for _, ev := range [][2]string{{evSvc, "order.created"}, {evJob, "order.shipped"}} {
		p.navigate("/eventarc/message-buses/" + bus + q)
		p.waitFor(`document.querySelector("#view").textContent.includes("Publish message (CloudBurrow extension)")`)
		p.pageAction("Publish message (CloudBurrow extension)", map[string]string{"eventId": ev[0], "eventSource": "//shop/orders",
			"eventType": ev[1], "eventData": `{"total":42}`}, "")
	}

	// The service's delivery, from the pipeline's platform log.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		code, logs := consoleDo(t, http.MethodGet, "/api/logs?"+url.Values{"project": {project}, "contains": {evSvc}, "limit": {"50"}}.Encode(), "")
		if code == http.StatusOK && strings.Contains(logs, "message delivered") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery of %s to the service logged within 2 minutes: %s", evSvc, lastChars(logs, 800))
		}
		time.Sleep(3 * time.Second)
	}
	// The job's execution, on its page.
	deadline = time.Now().Add(2 * time.Minute)
	for {
		code, page := consoleDo(t, http.MethodGet, "/api/detail/run-jobs?"+url.Values{"project": {project}, "name": {job}}.Encode(), "")
		if code == http.StatusOK && strings.Contains(page, job+"-") {
			break
		}
		if time.Now().After(deadline) {
			_, logs := consoleDo(t, http.MethodGet, "/api/logs?"+url.Values{"project": {project}, "contains": {evJob}, "limit": {"50"}}.Encode(), "")
			t.Fatalf("the shipped event never ran the job: %s | logs: %s", lastChars(page, 600), lastChars(logs, 800))
		}
		time.Sleep(3 * time.Second)
	}
	p.navigate("/run/jobs/" + job + q)
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
}
