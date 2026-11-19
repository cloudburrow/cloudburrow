//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestTasksQueuePagesMatchGoogle (#1848), in the storage shard: the Queues
// list has Google's columns and the checked queues' Pause queue, Resume
// queue and Purge queue; Create queue is a page with the Region list and the
// queue's configuration; the queue page has Pause queue, Purge queue and
// Delete queue beside its title and the tabs Tasks, Metrics, Logs,
// Configuration and Permissions, Logs holding Configure logging; Pause
// queue pauses it, as the console API then reads.
func TestTasksQueuePagesMatchGoogle(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/queues/parity-q"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project,
		`{"name":"parity-q","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(name), "")
	})

	p.navigate("/tasks/queues?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("parity-q"))`)
	var list struct {
		Header  []string
		Buttons []string
	}
	p.eval(`(() => ({ Header: [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim()),
		Buttons: [...document.querySelectorAll("#view button")].map((b) => b.textContent.trim()) }))()`, &list)
	header := strings.Join(list.Header, "|")
	for _, c := range []string{"Region", "State", "Tasks in queue", "Tasks running", "Max rate", "Max concurrent", "Max attempts", "Oldest task"} {
		if !strings.Contains(header, c) {
			t.Errorf("the Queues list has no %s column: %v", c, list.Header)
		}
	}
	for _, b := range []string{"Pause queue", "Resume queue", "Purge queue"} {
		if !contains(list.Buttons, b) {
			t.Errorf("the Queues list has no %s for the checked queues: %v", b, list.Buttons)
		}
	}

	p.navigate("/tasks/queues/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-location") !== null`)
	var form struct {
		Region string
		Fields bool
	}
	p.eval(`(() => ({ Region: document.querySelector("#view #f-location").tagName,
		Fields: ["maxDispatchesPerSecond", "maxAttempts", "minBackoff", "samplingRatio", "targetURI"]
			.every((f) => document.querySelector("#view #f-" + f) !== null) }))()`, &form)
	if form.Region != "SELECT" || !form.Fields {
		t.Errorf("Create queue = %+v; want the Region list and the rate, retry, logging and HTTP target fields", form)
	}

	p.navigate("/tasks/queues?project=" + project)
	p.clickText("#view tbody a", name)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Pause queue")`)
	var page struct {
		Actions, Tabs []string
	}
	p.eval(`(() => ({ Actions: [...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent),
		Tabs: [...document.querySelectorAll("#view [role=tab]")].map((b) => b.textContent.trim()) }))()`, &page)
	for _, a := range []string{"Create task", "Pause queue", "Purge queue", "Delete queue"} {
		if !contains(page.Actions, a) {
			t.Errorf("the queue page's title line has %v; want %s", page.Actions, a)
		}
	}
	if contains(page.Actions, "Configure logging") {
		t.Errorf("Configure logging is beside the title: %v", page.Actions)
	}
	if got := strings.Join(page.Tabs, "|"); !strings.HasPrefix(got, "Tasks|Metrics|Logs|Configuration|Permissions") {
		t.Errorf("the queue page's tabs are %v", page.Tabs)
	}
	p.clickText(`#view [role="tab"]`, "Logs")
	p.waitFor(`[...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent === "Configure logging")`)

	p.clickText("#view .page-actions button", "Pause queue")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Resume queue")`)
	code, body := consoleDo(t, http.MethodGet, "/api/resources/tasks?project="+project, "")
	var listing struct {
		Items []struct{ Name, Status string }
	}
	if err := json.Unmarshal([]byte(body), &listing); code != http.StatusOK || err != nil {
		t.Fatalf("list queues through the console API = %d (%v): %s", code, err, body)
	}
	paused := false
	for _, it := range listing.Items {
		paused = paused || (it.Name == name && it.Status == "PAUSED")
	}
	if !paused {
		t.Errorf("Pause queue did not pause the queue: %s", body)
	}
}

// TestSchedulerJobPagesMatchGoogle (#1849), in the storage shard: the jobs
// list has Google's columns and its Scheduler jobs and App Engine cron jobs
// tabs; Create job is Google's three steps, the Timezone a list, and the
// Target type shows only its own fields.
func TestSchedulerJobPagesMatchGoogle(t *testing.T) {
	needService(t, "scheduler")
	p := open(t)
	project := uniqueProject(t)
	job := createSchedulerJob(t, project, "parity-job")

	p.navigate("/scheduler/jobs?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, job))
	var list struct {
		Header, Tabs, Buttons []string
	}
	p.eval(`(() => ({ Header: [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim()),
		Tabs: [...document.querySelectorAll("#view [role=tab]")].map((b) => b.textContent.trim()),
		Buttons: [...document.querySelectorAll("#view button")].map((b) => b.textContent.trim()) }))()`, &list)
	header := strings.Join(list.Header, "|")
	for _, c := range []string{"Region", "State", "Description", "Frequency", "Target", "Last run", "Status of last execution", "Next run"} {
		if !strings.Contains(header, c) {
			t.Errorf("the jobs list has no %s column: %v", c, list.Header)
		}
	}
	if strings.Join(list.Tabs, "|") != "Scheduler jobs|App Engine cron jobs" {
		t.Errorf("the Cloud Scheduler page's tabs are %v", list.Tabs)
	}
	for _, b := range []string{"Pause", "Resume"} {
		if !contains(list.Buttons, b) {
			t.Errorf("the jobs list has no %s for the checked jobs: %v", b, list.Buttons)
		}
	}

	p.navigate("/scheduler/jobs/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-targetType") !== null`)
	var form struct {
		Steps []string
		Zone  string
	}
	p.eval(`(() => ({ Steps: [...document.querySelectorAll("#view .create-step-title")].map((n) => n.textContent),
		Zone: document.querySelector("#view #f-timeZone").tagName }))()`, &form)
	if strings.Join(form.Steps, "|") != "Define the schedule|Configure the execution|Configure optional settings" {
		t.Errorf("Create job's steps are %v", form.Steps)
	}
	if form.Zone != "SELECT" {
		t.Errorf("Timezone is a %s, want a list", form.Zone)
	}
	// Every step opened, so only Target type decides what is shown.
	p.eval(`[...document.querySelectorAll("#view .create-step-body")].forEach((b) => { b.hidden = false; })`, nil)
	p.setField("#view #f-targetType", "Pub/Sub")
	var shown struct{ URI, Topic bool }
	p.eval(`(() => { const vis = (id) => { let n = document.querySelector("#view #f-" + id);
			for (; n && n !== document.body; n = n.parentElement) if (n.hidden) return false; return true; };
		return { URI: vis("uri"), Topic: vis("topic") }; })()`, &shown)
	if shown.URI || !shown.Topic {
		t.Errorf("with Target type Pub/Sub the URL is shown %v and the topic %v; want only the topic", shown.URI, shown.Topic)
	}
}
