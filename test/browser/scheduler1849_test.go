//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestSchedulerLogsAndEditStepsThroughThePage (#1849), in the storage
// shard, whose instance serves Cloud Scheduler and Cloud Logging. A job's
// row menu has Pause, Force run and View logs, as Google's list has
// "Actions > View logs"; Force run from it makes an attempt, whose
// AttemptStarted and AttemptFinished are on the job's Logs tab, and View
// logs opens the Logs Explorer with the job's query, listing them. Edit job
// is Create job's three steps, Define the schedule, Configure the execution
// and Configure optional settings, each with Continue, on a page (#2062); a new Frequency
// saved through it is what the console API then reads. The list, the job's
// Logs tab and the Edit job page pass the WCAG 2.1 AA audit.
func TestSchedulerLogsAndEditStepsThroughThePage(t *testing.T) {
	needService(t, "scheduler")
	p := open(t)
	project := uniqueProject(t)
	job := createSchedulerJob(t, project, "logs-job")
	list := "/scheduler/jobs?project=" + project

	p.navigate(list)
	p.waitFor(rowReady(job))
	p.run(chromedp.Click(chromedp.CSS(`#view tbody tr .overflow-trigger`)))
	p.waitFor(`document.querySelector('.overflow-menu:not([hidden]) [role="menuitem"]') !== null`)
	var items []string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((i) => i.textContent.trim())`, &items)
	for _, want := range []string{"Pause", "Force run", "View logs", "Edit"} {
		if !contains(items, want) {
			t.Errorf("the row menu has no %s: %v", want, items)
		}
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Force run")

	// The attempt's two entries on the job's Logs tab.
	p.navigate("/scheduler/jobs/" + url.PathEscape(job) + "?project=" + project)
	p.waitFor(settled)
	p.clickText("#view [role=tab]", "Logs")
	var unavailable bool
	p.eval(`document.querySelector("#view").textContent.includes("Cloud Logging is not served")`, &unavailable)
	if unavailable {
		t.Skip("this instance does not serve Cloud Logging, which keeps a job's execution logs")
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("AttemptFinished"))`)
	var rows []string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent)`, &rows)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "AttemptStarted") || !strings.Contains(joined, "http://127.0.0.1:9/cloudburrow-browser") {
		t.Errorf("the Logs tab lists %q; want the attempt's AttemptStarted with its URL beside AttemptFinished", joined)
	}

	// The row menu's View logs: the Explorer opens on the job's query,
	// listing them.
	p.navigate(list)
	p.waitFor(rowReady(job))
	p.run(chromedp.Click(chromedp.CSS(`#view tbody tr .overflow-trigger`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "View logs")
	p.waitFor(`location.pathname === "/logs" && document.querySelector("#log-query") !== null`)
	var query string
	p.eval(`document.querySelector("#log-query").value`, &query)
	if !strings.Contains(query, `resource.type="cloud_scheduler_job"`) || !strings.Contains(query, `resource.labels.job_id="logs-job"`) {
		t.Errorf("View logs opened the Logs Explorer on %q; want the job's query", query)
	}
	p.waitFor(`document.querySelector("#view").textContent.includes("AttemptFinished")`)

	// Edit job: Create job's three steps, on a page of its own (#2062), as
	// Create job is ("on the Create a job page").
	p.navigate("/scheduler/jobs/" + url.PathEscape(job) + "?project=" + project)
	p.waitFor(settled)
	p.clickText("#view .page-actions button", "Edit job")
	p.waitFor(`new URLSearchParams(location.search).get("edit") === "1" && document.querySelector("#view form.create-page #f-schedule") !== null`)
	var form struct {
		Steps, Continue    []string
		DeadlineStep, Head string
		Modal              bool
	}
	p.eval(`(() => { const m = document.querySelector("#view form.create-page");
		const dl = m.querySelector("#f-attemptDeadline");
		const step = dl && dl.closest(".create-step");
		return { Steps: [...m.querySelectorAll(".create-step-title")].map((n) => n.textContent),
			Continue: [...m.querySelectorAll(".create-step-continue button")].map((b) => b.textContent.trim()),
			DeadlineStep: step ? step.querySelector(".create-step-title").textContent : "",
			Head: document.querySelector("#view h1").textContent,
			Modal: document.querySelector(".modal") !== null }; })()`, &form)
	if form.Modal || form.Head != "Edit job" {
		t.Errorf("Edit job opened a dialog (%v) or a page headed %q; want the Edit job page", form.Modal, form.Head)
	}
	if strings.Join(form.Steps, "|") != "Define the schedule|Configure the execution|Configure optional settings" {
		t.Errorf("Edit job's steps are %v; want Create job's", form.Steps)
	}
	if len(form.Continue) != 2 || form.Continue[0] != "Continue" {
		t.Errorf("Edit job's steps end in %v; want Continue on the first two", form.Continue)
	}
	if form.DeadlineStep != "Configure optional settings" {
		t.Errorf("the Attempt deadline config is in %q; want Configure optional settings", form.DeadlineStep)
	}
	editPage := "/scheduler/jobs/" + url.PathEscape(job) + "?project=" + project + "&edit=1"
	auditScreens(t, []a11yPage{
		{"Cloud Scheduler jobs list", list, rowReady(job), ""},
		{"Cloud Scheduler job Logs tab", "/scheduler/jobs/" + url.PathEscape(job) + "?project=" + project + "&tab=logs",
			settled + ` && [...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("AttemptFinished"))`, ""},
		{"Edit job page", editPage, `document.querySelector("#view form.create-page #f-schedule") !== null`, ""},
	})

	p.navigate(editPage)
	p.waitFor(`document.querySelector("#view form.create-page #f-schedule") !== null`)
	p.clickText("#view form.create-page .create-step-continue button", "Continue")
	p.setField("#view form.create-page #f-schedule", "15 6 * * *")
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	// Saved, the page goes back to the job's.
	p.waitFor(`location.pathname.startsWith("/scheduler/jobs/") && !new URLSearchParams(location.search).has("edit") && document.querySelector("#view form.create-page") === null`)
	code, body := consoleDo(t, http.MethodGet, "/api/detail/scheduler?project="+project+"&name="+url.QueryEscape(job), "")
	var detail struct {
		Edit struct {
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
		t.Fatalf("read the job through the console API = %d (%v): %s", code, err, body)
	}
	saved := false
	for _, f := range detail.Edit.Fields {
		saved = saved || (f.Name == "schedule" && f.Default == "15 6 * * *")
	}
	if !saved {
		t.Errorf("the console API does not read the Frequency saved through the steps: %s", body)
	}
}
