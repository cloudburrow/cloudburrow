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
)

// TestLogRouterFolderSinksThroughTheBrowser (#1572), in the storage shard,
// whose instance serves Cloud Logging and Resource Manager. A folder's page
// has a Log router tab and a Log router link on its Details, which opens the
// Log router with the folder chosen in its Parent picker. Create sink there
// (a page of its own) with Intercept children and not Include children is
// refused on the open form with CreateSink's own message; with both, and the
// Other project service, the sink is created and listed with both columns
// Yes, and the folder's Log router tab lists it, linking to its page, which
// shows Intercept children.
func TestLogRouterFolderSinksThroughTheBrowser(t *testing.T) {
	needService(t, "logging-sinks")
	needService(t, "hierarchy")
	project := uniqueProject(t)
	code, body := consoleDo(t, http.MethodPost, "/api/resources/hierarchy",
		fmt.Sprintf(`{"displayName":"Sinks %d","parent":"organizations/100000000001"}`, time.Now().UnixNano()%1e8))
	if code != http.StatusOK {
		t.Fatalf("create folder = %d: %s", code, body)
	}
	var created struct{ Name string }
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Name == "" {
		t.Fatalf("create folder answered %s: %v", body, err)
	}
	folder := "folders/" + strings.TrimPrefix(created.Name, "folders/")
	sink := folder + "/sinks/browser-central"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&parent="+url.QueryEscape(folder)+"&name="+url.QueryEscape(sink), "")
		consoleDo(t, http.MethodDelete, "/api/resources/hierarchy?name="+url.QueryEscape(strings.TrimPrefix(folder, "folders/")), "")
	})
	p := open(t)
	p.navigate("/resource-hierarchy/" + strings.TrimPrefix(folder, "folders/") + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Log router")`)
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.startsWith("Sinks of folders/"))`)
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent.startsWith("Sinks of folders/")).click(), true`, nil)
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/sinks" && new URLSearchParams(location.search).get("parent") === %q`, folder))
	p.waitFor(fmt.Sprintf(`document.querySelector("#scope-parent") !== null && document.querySelector("#scope-parent").value === %q`, folder))

	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create sink")`)
	p.clickText(`#view .state button.primary`, "Create sink")
	p.run(chromedp.WaitVisible(`form.create-page #f-destination`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("parent") === %q`, folder))
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector("form.create-page " + s);
		const set = (id, v) => { const f = q("#f-" + id); f.value = v; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); };
		set("name", "browser-central"); set("service", "Other project"); set("destination", %q);
		q("#f-intercept_children").checked = true; q("#f-intercept_children").dispatchEvent(new Event("change", { bubbles: true }));
		return true; })()`, project), nil)
	p.run(chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector("form.create-page .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("form.create-page .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "intercept_children requires include_children") {
		t.Errorf("intercepting without include children was refused with %q, want CreateSink's INVALID_ARGUMENT", refusal)
	}
	p.forgive()

	p.eval(`(() => { const f = document.querySelector("form.create-page #f-include_children"); f.checked = true;
		f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+sink)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	var cells string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, row), &cells)
	if !strings.Contains(cells, "logging.googleapis.com/projects/"+project) || strings.Count(cells, "Yes") != 2 {
		t.Errorf("the sink's row reads %q; want the project destination and Include and Intercept children Yes", cells)
	}

	p.navigate("/resource-hierarchy/" + strings.TrimPrefix(folder, "folders/") + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Log router")`)
	p.clickText("#view .tab-strip .tab", "Log router")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, sink))
	p.clickText("#view tbody a", sink)
	p.waitFor(fmt.Sprintf(`location.pathname.startsWith("/logs/sinks/") && new URLSearchParams(location.search).get("parent") === %q`, folder))
	p.waitFor(`document.querySelector("#view").textContent.includes("Intercept children")`)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit sink")`)
}
