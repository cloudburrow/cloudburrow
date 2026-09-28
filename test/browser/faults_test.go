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

// TestFaultRuleAddedAndDeletedThroughTheForm (#800): the Fault injection
// screen asks for the admin token, since the console adds none of its own
// (#553); with it, Cancel on the Add rule dialog discards typed input without
// asking and sends nothing (#783); a rule added through the form is held by
// /admin/faults and fails the Cloud Tasks list it matches with its code; the
// fault it injects appears under Recent faults; and Delete, confirmed by the
// rule's id, removes it from /admin/faults. The rule is scoped to the test's
// own project, so it cannot fault another test's call.
func TestFaultRuleAddedAndDeletedThroughTheForm(t *testing.T) {
	needService(t, "tasks")
	token := strings.TrimSpace(os.Getenv(envAdminToken))
	if token == "" {
		t.Fatalf("%s is not set; the fault screen needs the instance's admin token", envAdminToken)
	}
	p := open(t)
	project := uniqueProject(t)

	p.navigate("/faults")
	p.waitFor(`document.querySelector("#fault-token-form #fault-token") !== null`)
	// The page's first read was refused with 401, on purpose; Chrome logs
	// the refused load as an error.
	p.forgive()
	var file string
	p.eval(`(document.querySelector("#fault-token-form code") || {}).textContent || ""`, &file)
	if !strings.HasSuffix(file, "admin-token") {
		t.Errorf("the token form names the token file %q, want the instance's admin-token path", file)
	}
	p.run(chromedp.SendKeys(`#fault-token`, token, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`(() => { const b = document.querySelector("#fault-add"); return !!b && !b.disabled; })()`)
	var interposed string
	p.eval(`document.querySelector("#fault-interposed").textContent`, &interposed)
	if !strings.Contains(interposed, "tasks") {
		t.Errorf("the Services card says %q; want tasks among the services rules apply to", interposed)
	}

	// Cancel discards what was typed, without asking again.
	p.run(chromedp.Click(`#fault-add`, chromedp.ByQuery))
	p.waitFor(`document.activeElement === document.querySelector(".modal #fault-service")`)
	p.run(chromedp.SendKeys(`.modal #fault-method`, "Get*", chromedp.ByQuery))
	p.clickText(`.modal button`, "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.posts("/api/faults"); len(sent) != 0 {
		t.Fatalf("Cancel sent a rule: %v", sent)
	}

	p.run(chromedp.Click(`#fault-add`, chromedp.ByQuery))
	p.waitFor(`document.activeElement === document.querySelector(".modal #fault-service")`)
	var method string
	p.eval(`document.querySelector(".modal #fault-method").value`, &method)
	if method != "" {
		t.Errorf("the reopened form kept %q from the cancelled one", method)
	}
	p.run(
		chromedp.SetValue(`.modal #fault-service`, "tasks", chromedp.ByQuery),
		chromedp.SendKeys(`.modal #fault-method`, "ListQueues", chromedp.ByQuery),
		chromedp.SendKeys(`.modal #fault-project`, project, chromedp.ByQuery),
		chromedp.SetValue(`.modal #fault-code`, "PERMISSION_DENIED", chromedp.ByQuery),
		chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	row := fmt.Sprintf(`[...document.querySelectorAll("#fault-rule-rows tr")].find((r) => r.textContent.includes(%q))`, project)
	p.waitFor(`document.querySelector(".modal") === null && ` + row + ` !== undefined`)
	var id string
	p.eval(row+`.dataset.rule`, &id)
	if id == "" {
		t.Fatal("the new rule's row carries no id")
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			adminDo(t, http.MethodDelete, "/admin/faults?id="+url.QueryEscape(id), "")
		}
	})
	if sent := p.posts("/api/faults"); len(sent) != 1 {
		t.Errorf("the rule was posted %d times, want once: %v", len(sent), sent)
	}
	code, body := adminDo(t, http.MethodGet, "/admin/faults", "")
	if code != http.StatusOK || !strings.Contains(body, `"id":"`+id+`"`) || !strings.Contains(body, `"project":"`+project+`"`) ||
		!strings.Contains(body, `"code":"PERMISSION_DENIED"`) {
		t.Fatalf("GET /admin/faults = %d %s; want the rule %s added in the browser", code, body, id)
	}

	// The list reads the store as ListQueues (#594), so the rule fails it
	// with the message an SDK receives, which the screen shows.
	if _, body := consoleDo(t, http.MethodGet, "/api/resources/tasks?project="+project, ""); !strings.Contains(body, "injected fault (rule "+id+"): PERMISSION_DENIED") {
		t.Errorf("the Cloud Tasks list under the rule = %s; want the injected PERMISSION_DENIED", body)
	}
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#fault-recent-rows tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("PERMISSION_DENIED"))`, id))

	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Delete rule "+id), chromedp.ByQuery))
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, id, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && ` + row + ` === undefined`)
	deleted = true
	if sent := p.sent(http.MethodDelete, "/api/faults"); len(sent) != 1 {
		t.Errorf("the delete was sent %d times, want once: %v", len(sent), sent)
	}
	if code, body := adminDo(t, http.MethodGet, "/admin/faults", ""); code != http.StatusOK || strings.Contains(body, `"id":"`+id+`"`) {
		t.Errorf("GET /admin/faults after the browser's delete = %d %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/tasks?project="+project, ""); code != http.StatusOK || strings.Contains(body, "injected fault") {
		t.Errorf("the Cloud Tasks list after the delete = %d %s; want it served", code, body)
	}
}
