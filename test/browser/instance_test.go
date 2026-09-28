//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestInstanceResetConfirmedByTypingTheScope (#801), in the storage shard:
// the Instance page asks for the admin token, since the console adds none of
// its own (#553); with it, the Reset card lists tasks, and naming a project
// leaves only the components that can be scoped to one. Reset opens a
// confirmation whose button says Reset; Cancel closes it at once and sends
// nothing (#783); a word other than the project is refused on the form and
// sends nothing; the project typed back sends one reset, for tasks in that
// project, which removes that project's queue and leaves another project's.
// Both projects are the test's own, so nothing another test holds is reset.
func TestInstanceResetConfirmedByTypingTheScope(t *testing.T) {
	needService(t, "tasks")
	token := strings.TrimSpace(os.Getenv(envAdminToken))
	if token == "" {
		t.Fatalf("%s is not set; the Instance page needs the instance's admin token", envAdminToken)
	}
	p := open(t)
	project, other := uniqueProject(t), uniqueProject(t)
	queueIn := func(project string) string {
		t.Helper()
		name := "projects/" + project + "/locations/us-central1/queues/browser-reset"
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project, `{"name":"browser-reset","location":"us-central1"}`); code != http.StatusOK {
			t.Fatalf("create a queue in %s through the console API = %d: %s", project, code, body)
		}
		t.Cleanup(func() {
			consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(name), "")
		})
		return name
	}
	gone, kept := queueIn(project), queueIn(other)
	listed := func(project, name string) bool {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/tasks?project="+project, "")
		if code != http.StatusOK {
			t.Fatalf("list %s's queues = %d: %s", project, code, body)
		}
		return strings.Contains(body, name)
	}

	p.navigate("/instance")
	p.waitFor(`document.querySelector("#instance-token-form #instance-token") !== null`)
	// The page's first read was refused with 401, on purpose; Chrome logs
	// the refused load as an error.
	p.forgive()
	var file string
	p.eval(`(document.querySelector("#instance-token-form code") || {}).textContent || ""`, &file)
	if !strings.HasSuffix(file, "admin-token") {
		t.Errorf("the token form names the token file %q, want the instance's admin-token path", file)
	}
	p.run(chromedp.SendKeys(`#instance-token`, token, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#instance-reset-tasks") !== null`)

	// Naming a project leaves only what can be reset by project; of those,
	// only tasks is kept.
	p.run(chromedp.SendKeys(`#instance-reset-project`, project, chromedp.ByQuery))
	var boxes []struct {
		Name              string
		ByProject, Enable bool
	}
	p.eval(`[...document.querySelectorAll("#instance-reset-services input[type=checkbox]")].map((b) =>
		({ Name: b.value, ByProject: b.dataset.byProject === "true", Enable: !b.disabled }))`, &boxes)
	if len(boxes) == 0 {
		t.Fatal("the Reset card lists no services")
	}
	for _, b := range boxes {
		if b.Enable != b.ByProject {
			t.Errorf("with a project named, %s is enabled %v; want only the services that can be reset by project", b.Name, b.Enable)
		}
	}
	p.eval(`(() => { for (const b of document.querySelectorAll("#instance-reset-services input[type=checkbox]")) {
		if (b.value !== "tasks" && b.checked) { b.checked = false; b.dispatchEvent(new Event("change", { bubbles: true })); } }
		return true; })()`, nil)
	var reseedDisabled bool
	p.eval(`document.querySelector("#instance-reseed").disabled`, &reseedDisabled)
	if !reseedDisabled {
		t.Error("Reseed is offered with a project named, which the admin API refuses")
	}

	openConfirm := func() {
		p.run(chromedp.Click(`#instance-reset`, chromedp.ByQuery))
		p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	}
	openConfirm()
	var label, title string
	p.eval(`document.querySelector(".modal button[type=submit]").textContent.trim()`, &label)
	p.eval(`document.querySelector(".modal h2").textContent`, &title)
	if label != "Reset" || title != "Reset project "+project {
		t.Errorf("the confirmation is headed %q with the button %q; want Reset project %s and Reset", title, label, project)
	}
	// Cancel discards without asking, whatever was typed.
	p.run(chromedp.SendKeys(`.modal #confirm-input`, project, chromedp.ByQuery))
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.posts("/api/instance/reset"); len(sent) != 0 {
		t.Fatalf("Cancel sent a reset: %v", sent)
	}

	// Another word is refused on the form.
	openConfirm()
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "all", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if want := fmt.Sprintf("Type %s exactly to confirm.", project); refusal != want {
		t.Errorf("the word all was refused with %q, want %q", refusal, want)
	}
	if sent := p.posts("/api/instance/reset"); len(sent) != 0 {
		t.Fatalf("a wrong confirmation sent a reset: %v", sent)
	}

	p.eval(`document.querySelector(".modal #confirm-input").value = ""`, nil)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, project, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#instance-reset-result").textContent !== ""`)
	sent := p.posts("/api/instance/reset")
	if len(sent) != 1 {
		t.Fatalf("the reset was sent %d times, want once: %v", len(sent), sent)
	}
	u, err := url.Parse(sent[0])
	if err != nil {
		t.Fatal(err)
	}
	if q := u.Query(); strings.Join(q["service"], ",") != "tasks" || q.Get("project") != project || q.Has("reseed") {
		t.Errorf("the reset sent %s; want service=tasks and project=%s", u.RawQuery, project)
	}
	var result string
	p.eval(`document.querySelector("#instance-reset-result").textContent`, &result)
	if result != "Reset tasks in project "+project+"." {
		t.Errorf("the Reset card says %q", result)
	}
	if listed(project, gone) {
		t.Errorf("%s is still listed after its project's reset", gone)
	}
	if !listed(other, kept) {
		t.Errorf("%s, in another project, went with the reset of %s", kept, project)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/operations?project="+project, ""); code != http.StatusOK ||
		!strings.Contains(body, `"kind":"Reset"`) || !strings.Contains(body, `"state":"SUCCEEDED"`) {
		t.Errorf("the operations ledger for %s = %d %s; want the reset, succeeded", project, code, body)
	}
}
