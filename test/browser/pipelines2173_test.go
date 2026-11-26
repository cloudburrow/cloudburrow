//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestVertexAIPipelinesThroughThePages (#2173): on Pipelines, Create run
// opens Create pipeline run; a compiled pipeline pasted as the run source,
// a run name, a Cloud storage location and its parameters, then Submit,
// start a run whose two tasks run as containers and end Succeeded; the
// run's page draws its tasks on its runtime graph; the same form with a
// Recurring run schedule makes a schedule on the Schedules tab, paused
// and resumed from its row and deleted with the dialog's Delete. Every screen passes the WCAG audit.
func TestVertexAIPipelinesThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-pipeline-runs", "vertex-pipeline-schedules", "storage"} {
		needService(t, s)
	}
	img := os.Getenv("CLOUDBURROW_TEST_PIPELINE_IMAGE")
	if img == "" {
		img = "python:3-alpine"
	}
	if err := exec.Command("docker", "image", "inspect", img).Run(); err != nil {
		t.Skipf("the pipeline task image %s is not on this machine's Docker", img)
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	r := "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("pl-browser-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
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
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		for _, it := range l.Items {
			if it.Fields[field] == value {
				return it
			}
		}
		return item{}
	}
	spec := strings.ReplaceAll(`{"pipelineInfo": {"name": "browser-pipeline"}, "schemaVersion": "2.1.0", "sdkVersion": "kfp-2.7.0",
 "root": {"inputDefinitions": {"parameters": {"greeting": {"parameterType": "STRING"}}},
  "dag": {"tasks": {
   "produce": {"taskInfo": {"name": "produce"}, "componentRef": {"name": "comp-produce"}, "inputs": {"parameters": {"msg": {"componentInputParameter": "greeting"}}}},
   "consume": {"taskInfo": {"name": "consume"}, "componentRef": {"name": "comp-consume"}, "dependentTasks": ["produce"],
     "inputs": {"parameters": {"word": {"taskOutputParameter": {"producerTask": "produce", "outputParameterKey": "word"}}}}}}}},
 "components": {
  "comp-produce": {"executorLabel": "exec-produce", "inputDefinitions": {"parameters": {"msg": {"parameterType": "STRING"}}},
    "outputDefinitions": {"parameters": {"word": {"parameterType": "STRING"}}}},
  "comp-consume": {"executorLabel": "exec-consume", "inputDefinitions": {"parameters": {"word": {"parameterType": "STRING"}}}}},
 "deploymentSpec": {"executors": {
  "exec-produce": {"container": {"image": "IMAGE", "command": ["sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && printf '%s' \"$0\" > \"$1\""],
    "args": ["{{$.inputs.parameters['msg']}}", "{{$.outputs.parameters['word'].output_file}}"]}},
  "exec-consume": {"container": {"image": "IMAGE", "command": ["sh", "-c", "test \"$0\" = hello"], "args": ["{{$.inputs.parameters['word']}}"]}}}}}`, "IMAGE", img)

	p := open(t)
	runsPath := "/agent-platform/pipelines" + q
	schedulesPath := "/agent-platform/pipelines/schedules" + q
	p.navigate(runsPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create run")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Create pipeline run")`)
	runName := fmt.Sprintf("browser run %d", stamp)
	p.submitForm(map[string]string{"spec": spec, "displayName": runName, "output": "gs://" + bucket + "/root", "parameters": `{"greeting": "hello"}`}, "displayName")
	var run item
	for deadline := time.Now().Add(5 * time.Minute); ; {
		run = find("vertex-pipeline-runs", "Run name", runName)
		if run.Status == "Succeeded" || run.Status == "Failed" || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if run.Status != "Succeeded" || run.Fields["Pipeline"] != "browser-pipeline" {
		t.Fatalf("the run = %+v", run)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-pipeline-runs"+q+r+"&name="+url.QueryEscape(run.Name), "")
	})
	p.navigate(runsPath)
	p.waitFor(rowReady(runName))
	p.follow(run.Name)
	// The run's page is its runtime graph (#2205): consume, after
	// produce, Succeeded.
	p.waitFor(`[...document.querySelectorAll("#view button.pl-node")].some((b) => b.getAttribute("aria-label") === "consume, Step, Succeeded")`)
	var runPath string
	p.eval(`location.pathname + location.search`, &runPath)

	// Recurring: a schedule on the Schedules tab.
	p.navigate(runsPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create run")
	schedName := fmt.Sprintf("browser schedule %d", stamp)
	p.submitForm(map[string]string{"spec": spec, "displayName": schedName, "runSchedule": "recurring", "cron": "0 9 * * MON",
		"output": "gs://" + bucket + "/root", "parameters": `{"greeting": "hello"}`}, "displayName")
	var sched item
	for deadline := time.Now().Add(30 * time.Second); sched.Name == "" && time.Now().Before(deadline); time.Sleep(time.Second) {
		sched = find("vertex-pipeline-schedules", "Schedule name", schedName)
	}
	if sched.Status != "Active" || sched.Fields["Frequency"] != "0 9 * * MON" {
		t.Fatalf("the schedule = %+v", sched)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-pipeline-schedules"+q+r+"&name="+url.QueryEscape(sched.Name), "")
	})
	p.navigate(runsPath)
	p.waitFor(settled)
	p.clickText("#view .tab-strip a, #view .tab-strip button, #view [role=tab]", "Schedules")
	p.waitFor(`location.pathname === "/agent-platform/pipelines/schedules" && ` + rowReady(schedName))
	waitSchedule := func(what, want string) {
		t.Helper()
		got := ""
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if got = find("vertex-pipeline-schedules", "Schedule name", schedName).Status; got == want {
				return
			}
		}
		t.Errorf("after %s the schedule is %q, want %s", what, got, want)
	}
	p.colabRowItem(sched.Name, "Pause")
	waitSchedule("Pause", "Paused")
	p.navigate(schedulesPath)
	p.waitFor(rowReady(schedName))
	p.colabRowItem(sched.Name, "Resume")
	waitSchedule("Resume", "Active")

	auditScreens(t, []a11yPage{
		{"pipeline runs", runsPath, rowReady(runName), ""},
		{"pipeline run", runPath, settled, ""},
		{"pipeline schedules", schedulesPath, rowReady(schedName), ""},
		{"create pipeline run", runsPath, settled, `[...document.querySelectorAll("#view a, #view button")].find((b) => b.textContent.trim() === "Create run").click()`},
	})

	p = open(t)
	p.navigate(schedulesPath)
	p.openRowDelete(sched.Name, "Delete")
	p.confirmOnlyDialog()
	p.pressDelete()
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, schedName))
}
