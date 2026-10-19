//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestLoggingViewsThroughTheBrowser (#1283), in the run shard, whose
// instance serves Cloud Logging. On Logs Storage, _Default's page offers
// Create view; a view made there with a filter is listed in its Views
// section, which the console API reads back. Its row's Edit, prefilled with
// the filter, saves a new one, and its Delete, confirmed by typing the view's
// ID, removes it. _AllLogs's row offers neither, only Permissions (#1519).
func TestLoggingViewsThroughTheBrowser(t *testing.T) {
	needService(t, "logging-buckets")
	p := open(t)
	project := uniqueProject(t)
	bucket := "projects/" + project + "/locations/global/buckets/_Default"
	detail := "/api/detail/logging-buckets?project=" + project + "&name=" + url.QueryEscape(bucket)
	views := func() string {
		code, body := consoleDo(t, http.MethodGet, detail, "")
		if code != http.StatusOK {
			t.Fatalf("the bucket's page = %d: %s", code, body)
		}
		return body
	}
	setField := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, id, v), nil)
	}

	p.navigate("/logs/storage?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, bucket))
	p.clickText("#view tbody a", bucket)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Create view")`)
	p.clickText("#view .page-actions button", "Create view")
	p.waitFor(`document.querySelector(".modal.is-open #f-id") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-id`), "errors"))
	setField("filter", "severity>=ERROR")
	setField("description", "just errors")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if body := views(); !strings.Contains(body, `"errors"`) || !strings.Contains(body, "just errors") {
		t.Fatalf("the console API does not list the view the browser created: %s", body)
	}

	// _AllLogs's menu offers Permissions alone (#1519); the new view's
	// offers Edit and Delete too.
	p.run(chromedp.Click(chromedp.CSS(`#tab-views`)))
	p.waitFor(`document.querySelector('button[aria-label="Actions for errors"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for _AllLogs"]`)))
	var allLogsItems []string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((i) => i.textContent.trim())`, &allLogsItems)
	if strings.Join(allLogsItems, ",") != "Permissions" {
		t.Errorf("_AllLogs's row offers %v; want Permissions alone", allLogsItems)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for errors"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-filter") !== null`)
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-filter").value`, &prefilled)
	if prefilled != "severity>=ERROR" {
		t.Errorf("Edit's filter is prefilled with %q, want the view's", prefilled)
	}
	setField("filter", "severity>=WARNING")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if body := views(); !strings.Contains(body, "severity\\u003e=WARNING") && !strings.Contains(body, "severity>=WARNING") {
		t.Errorf("the console API does not read the filter the browser saved: %s", body)
	}

	p.waitFor(`document.querySelector("#tab-views") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-views`)))
	p.waitFor(`document.querySelector('button[aria-label="Actions for errors"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for errors"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "errors"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null`)
	if body := views(); strings.Contains(body, `"errors"`) {
		t.Errorf("the deleted view is still listed: %s", body)
	}
}
