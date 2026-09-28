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

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestCloudRunJobCreatedExecutedAndDeletedInTheBrowser (#785): Cloud Run's
// product navigation has Services and Jobs; the Jobs page's Create job form
// creates a job with one POST, which the console API lists; the job's page
// offers Execute, which is one action and puts an execution on its Executions
// tab; Edit job opens prefilled, and Cancel closes it at once with nothing
// sent (#783); the row's Delete asks for the job's name back, refuses a wrong
// one without sending anything, and deletes it with one DELETE.
func TestCloudRunJobCreatedExecutedAndDeletedInTheBrowser(t *testing.T) {
	needService(t, "run-jobs")
	p := open(t)
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-job-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run-jobs?project="+project+"&name="+id, "")
	})

	p.navigate("/run/jobs?project=" + project)
	var pages []string
	p.waitFor(`document.querySelectorAll("#product-nav a").length >= 2`)
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &pages)
	if !contains(pages, "Services") || !contains(pages, "Jobs") {
		t.Errorf("Cloud Run's pages are %v; want Services and Jobs", pages)
	}

	p.navigate("/run/jobs/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-name") !== null`)
	var defaults struct{ Tasks, Retries string }
	p.eval(`({ Tasks: document.querySelector("#f-taskCount").value, Retries: document.querySelector("#f-maxRetries").value })`, &defaults)
	if defaults.Tasks != "1" || defaults.Retries != "3" {
		t.Errorf("the create form opened with %s tasks and %s retries; want Cloud Run's defaults, 1 and 3", defaults.Tasks, defaults.Retries)
	}
	p.run(chromedp.SendKeys(`#view #f-name`, id, chromedp.ByQuery),
		chromedp.SendKeys(`#view #f-image`, "docker.io/library/busybox:1.36", chromedp.ByQuery),
		chromedp.SendKeys(`#view #f-command`, "sh -c", chromedp.ByQuery),
		chromedp.SendKeys(`#view #f-args`, `'echo "from the browser"'`, chromedp.ByQuery))
	p.clickText(`#view button[type="submit"]`, "Create job")
	p.waitFor(fmt.Sprintf(`location.pathname === "/run/jobs" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	if posts := p.posts("/api/resources/run-jobs"); len(posts) != 1 {
		t.Errorf("the create was posted %d times, want once: %v", len(posts), posts)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/run-jobs?project="+project, ""); code != http.StatusOK || !strings.Contains(body, `"`+id+`"`) {
		t.Fatalf("the console API does not list the job created in the browser: %d %s", code, body)
	}

	p.clickText("#view tbody a", id)
	p.clickText("#view .page-actions button", "Execute")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("completed"))`)
	if sent := p.sent(http.MethodPost, "/api/actions/run-jobs"); len(sent) != 1 {
		t.Errorf("Execute sent %d actions, want one: %v", len(sent), sent)
	}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/run-jobs?"+url.Values{"project": {project}, "name": {id}}.Encode(), "")
	var job struct {
		Sections []struct {
			ID      string
			Listing struct{ Items []struct{ Name string } }
		}
	}
	if err := json.Unmarshal([]byte(body), &job); code != http.StatusOK || err != nil {
		t.Fatalf("the job's detail through the console API = %d (%v): %s", code, err, body)
	}
	executions := 0
	for _, s := range job.Sections {
		if s.ID == "executions" {
			executions = len(s.Listing.Items)
		}
	}
	if executions != 1 {
		t.Errorf("the job has %d executions after one Execute: %s", executions, body)
	}

	// Edit job opens prefilled; Cancel discards typed input without asking.
	p.clickText("#view .page-actions button", "Edit job")
	p.waitFor(`document.querySelector(".modal.is-open #f-image") !== null`)
	var form struct {
		Image, Args  string
		NameDisabled bool
	}
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		return { Image: q("#f-image").value, Args: q("#f-args").value, NameDisabled: q("#f-name").disabled }; })()`, &form)
	if form.Image != "docker.io/library/busybox:1.36" || form.Args != `'echo "from the browser"'` || !form.NameDisabled {
		t.Errorf("Edit job opened with %+v; want the job's image and arguments, and the name disabled", form)
	}
	p.run(chromedp.SendKeys(`.modal.is-open #f-args`, " changed", chromedp.ByQuery))
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/run-jobs"); len(sent) != 0 {
		t.Errorf("cancelling Edit job sent %v", sent)
	}

	// Delete from the row menu, confirmed by name.
	p.navigate("/run/jobs?project=" + project)
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+id)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+id), chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "not-"+id, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	if sent := p.sent(http.MethodDelete, "/api/resources/run-jobs"); len(sent) != 0 {
		t.Fatalf("a delete confirmed with the wrong name was sent: %v", sent)
	}
	p.eval(`(() => { document.querySelector(".modal #confirm-input").value = ""; return true; })()`, nil)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, id, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, row))
	if sent := p.sent(http.MethodDelete, "/api/resources/run-jobs"); len(sent) != 1 {
		t.Errorf("the delete was sent %d times, want once: %v", len(sent), sent)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/run-jobs?project="+project, ""); code != http.StatusOK || strings.Contains(body, `"`+id+`"`) {
		t.Errorf("the console API still lists the job deleted in the browser: %d %s", code, body)
	}
}
