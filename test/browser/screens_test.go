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

// The screens docs/compatibility.md marked "Not checked in a browser" (#700),
// and the Cloud Run edit form. Cloud KMS and Cloud Run are not served by the
// instance the rest of this suite runs against, so CI runs the tests named
// TestKMS* and TestCloudRun* in the run shard, whose instance serves both.

// TestKMSCreateKeyRingThroughTheForm: from the empty Cloud KMS screen of a
// new project, Create key ring opens its dialog with focus on the name and the
// location prefilled; the ring created through it is listed, exactly one
// POST was sent, the console API lists it, and its row opens the ring's page,
// where keys are created.
func TestKMSCreateKeyRingThroughTheForm(t *testing.T) {
	needService(t, "kms")
	p := open(t)
	project := uniqueProject(t)
	ring := "projects/" + project + "/locations/global/keyRings/browser-ring"

	p.navigate("/kms/keyrings?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create key ring")`)
	p.run(chromedp.Click(`#view .state button.primary`, chromedp.ByQuery))
	p.run(chromedp.WaitVisible(`.modal #f-keyRingId`, chromedp.ByQuery))
	var form struct{ Focus, Location string }
	p.eval(`({ Focus: document.activeElement.id, Location: document.querySelector(".modal #f-location").value })`, &form)
	if form.Focus != "f-keyRingId" || form.Location != "global" {
		t.Errorf("the Create key ring dialog opened with focus on #%s and location %q; want the name focused and global", form.Focus, form.Location)
	}

	p.run(chromedp.SendKeys(`.modal #f-keyRingId`, "browser-ring", chromedp.ByQuery),
		chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, ring))
	if posts := p.posts("/api/resources/kms"); len(posts) != 1 {
		t.Errorf("the create was posted %d times, want once: %v", len(posts), posts)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/kms?project="+project, ""); code != http.StatusOK || !strings.Contains(body, `"`+ring+`"`) {
		t.Errorf("the console API does not list the key ring created in the browser: %d %s", code, body)
	}

	p.clickText("#view tbody a", ring)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Create key")`)
}

// TestSchedulerPauseFromARow: a job's row menu offers Pause; choosing it
// sends one action, the row's status becomes PAUSED, the console API reads
// the job as paused, and the menu then offers Resume in its place.
func TestSchedulerPauseFromARow(t *testing.T) {
	needService(t, "scheduler")
	p := open(t)
	project := uniqueProject(t)
	job := createSchedulerJob(t, project, "browser-job")

	p.navigate("/scheduler/jobs?project=" + project)
	status := fmt.Sprintf(`tr[aria-label=%q] .status`, "Inspect "+job)
	p.waitFor(fmt.Sprintf(`(() => { const s = document.querySelector(%q); return !!s && s.textContent.trim() === "ENABLED"; })()`, status))

	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+job)
	p.run(chromedp.Click(menu, chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Pause")
	p.waitFor(fmt.Sprintf(`(() => { const s = document.querySelector(%q); return !!s && s.textContent.trim() === "PAUSED"; })()`, status))
	if sent := p.sent(http.MethodPost, "/api/actions/scheduler"); len(sent) != 1 {
		t.Errorf("Pause sent %d actions, want one: %v", len(sent), sent)
	}

	code, body := consoleDo(t, http.MethodGet, "/api/resources/scheduler?project="+project, "")
	var listing struct {
		Items []struct{ Name, Status string }
	}
	if err := json.Unmarshal([]byte(body), &listing); code != http.StatusOK || err != nil {
		t.Fatalf("list jobs through the console API = %d (%v): %s", code, err, body)
	}
	paused := false
	for _, it := range listing.Items {
		paused = paused || (it.Name == job && it.Status == "PAUSED")
	}
	if !paused {
		t.Errorf("the console API does not read the job paused from the browser as PAUSED: %s", body)
	}

	p.run(chromedp.Click(menu, chromedp.ByQuery))
	var items []string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent)`, &items)
	if !contains(items, "Resume") || contains(items, "Pause") {
		t.Errorf("a paused job's menu offers %v; want Resume and not Pause", items)
	}
}

// TestSubscriptionsDeleteConfirmedByName: on the Subscriptions screen, a
// subscription's row menu opens the delete confirmation with focus in the
// field that takes its name. A wrong name is refused on the form and nothing
// is sent; the full name deletes it, with one DELETE: the row goes, the
// console API no longer lists it, and its topic is still there.
func TestSubscriptionsDeleteConfirmedByName(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-topic"
	sub := "projects/" + project + "/subscriptions/browser-topic-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-topic","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	p.navigate("/pubsub/subscriptions?project=" + project)
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+sub)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+sub), chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null && document.activeElement === document.querySelector(".modal #confirm-input")`)

	// The short name is not the name.
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "browser-topic-sub", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if want := "Type " + sub + " exactly to confirm."; refusal != want {
		t.Errorf("a wrong name was refused with %q, want %q", refusal, want)
	}
	if sent := p.sent(http.MethodDelete, "/api/resources/pubsub-subscriptions"); len(sent) != 0 {
		t.Fatalf("a delete confirmed with the wrong name was sent: %v", sent)
	}

	p.eval(`(() => { const f = document.querySelector(".modal #confirm-input"); f.value = ""; return true; })()`, nil)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, sub, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, row))
	if sent := p.sent(http.MethodDelete, "/api/resources/pubsub-subscriptions"); len(sent) != 1 {
		t.Errorf("the delete was sent %d times, want once: %v", len(sent), sent)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-subscriptions?project="+project, ""); code != http.StatusOK || strings.Contains(body, `"`+sub+`"`) {
		t.Errorf("the console API still lists the subscription deleted in the browser: %d %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub?project="+project, ""); code != http.StatusOK || !strings.Contains(body, `"`+topic+`"`) {
		t.Errorf("deleting the subscription took its topic with it: %d %s", code, body)
	}
}

// TestCloudRunEditFormIsPrefilledAndGivesFocusBack: a service deployed
// through the console API has "Edit and deploy new revision" on its page.
// The dialog it opens is headed with the service's name, shows the name
// disabled, and is prefilled from the serving revision: its image and its
// variable. Focus is inside it, Escape closes it without sending a change,
// and focus returns to the button.
func TestCloudRunEditFormIsPrefilledAndGivesFocusBack(t *testing.T) {
	needService(t, "run")
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-edit-%d", time.Now().UnixNano()%1e6)
	const image = "ghcr.io/knative/helloworld-go:latest"
	body, _ := json.Marshal(map[string]string{"name": id, "image": image, "env": `{"TARGET":"browser"}`})
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run?project="+project, string(body))
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time") && strings.Contains(resp, `"operation"`):
		// The console gives up waiting after its own minute and names the
		// operation, which carries on. On an instance just brought up again
		// the rollout took longer than that (#765), so wait for the service
		// to have a serving revision the edit form can be prefilled from.
		waitForRunService(t, project, id, image, 5*time.Minute)
	default:
		t.Fatalf("deploy a service through the console API = %d: %s", code, resp)
	}
	// The tab is opened after the rollout, which can take minutes, so its
	// own three-minute budget is spent on the page.
	p := open(t)

	p.navigate("/run/" + id + "?project=" + project)
	p.clickText("#view .page-actions button", "Edit and deploy new revision")
	p.waitFor(`document.querySelector(".modal.is-open #f-image") !== null`)

	var form struct {
		Title, Name, Image, Env, Focus string
		NameDisabled, FocusInside      bool
	}
	p.eval(`(() => { const d = document.querySelector(".modal"), q = (s) => d.querySelector(s);
		return { Title: q("#edit-title").textContent, Name: q("#f-name").value, NameDisabled: q("#f-name").disabled,
		         Image: q("#f-image").value, Env: q("#f-env").value,
		         Focus: document.activeElement.id, FocusInside: d.contains(document.activeElement) }; })()`, &form)
	if form.Title != "Edit and deploy new revision "+id {
		t.Errorf("the edit dialog is headed %q", form.Title)
	}
	if form.Name != id || !form.NameDisabled {
		t.Errorf("the service name field shows %q, disabled %v; want %q, disabled", form.Name, form.NameDisabled, id)
	}
	if form.Image != image || !strings.Contains(form.Env, "TARGET=browser") {
		t.Errorf("the edit dialog is prefilled with image %q and variables %q; want %q and TARGET=browser", form.Image, form.Env, image)
	}
	if !form.FocusInside || form.Focus == "f-name" {
		t.Errorf("focus is on #%s, inside the dialog %v; want a field that can be edited", form.Focus, form.FocusInside)
	}

	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector(".modal") === null`)
	var back bool
	p.eval(`document.activeElement.hasAttribute("data-cb-click") && document.activeElement.textContent === "Edit and deploy new revision"`, &back)
	if !back {
		t.Error("closing the edit dialog did not return focus to its button")
	}
	if sent := p.sent(http.MethodPatch, "/api/resources/run"); len(sent) != 0 {
		t.Errorf("closing the edit dialog sent a change: %v", sent)
	}
}

// clickText clicks, with the mouse, the element sel matches whose text is
// text, once it is on screen.
func (p *tab) clickText(sel, text string) {
	p.t.Helper()
	find := fmt.Sprintf(`[...document.querySelectorAll(%q)].find((n) => n.textContent.trim() === %q && n.getClientRects().length)`, sel, text)
	p.waitFor(find + ` !== undefined`)
	p.eval(fmt.Sprintf(`(() => { document.querySelectorAll("[data-cb-click]").forEach((n) => n.removeAttribute("data-cb-click"));
		%s.setAttribute("data-cb-click", ""); return true; })()`, find), nil)
	p.run(chromedp.Click(`[data-cb-click]`, chromedp.ByQuery))
}

// sent are the requests the page made with a method to a path on the console.
func (p *tab) sent(method, path string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.requests {
		m, u, _ := strings.Cut(r, " ")
		if m == method && strings.HasPrefix(u, p.origin+path) {
			out = append(out, u)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// waitForRunService polls the console's detail for a Cloud Run service
// until its edit form is prefilled from a serving revision running image,
// or fails the test after within.
func waitForRunService(t *testing.T, project, id, image string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/run?project="+project+"&name="+id, "")
		if code == http.StatusOK && strings.Contains(body, `"edit":{`) && strings.Contains(body, image) {
			return
		}
		last = fmt.Sprintf("%d %s", code, body)
		time.Sleep(3 * time.Second)
	}
	if len(last) > 400 {
		last = last[:400]
	}
	t.Fatalf("service %s had no serving revision of %s within %s; last detail: %s", id, image, within, last)
}
