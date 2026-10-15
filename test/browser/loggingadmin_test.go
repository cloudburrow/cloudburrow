//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestLoggingAdminMetricsThroughTheBrowser (#1226), in the run shard, whose
// instance serves Cloud Logging. On Log-based metrics, Create metric (a page of its own,
// the form being long) with a filter outside the supported grammar is sent
// and refused on the open form
// with CreateLogMetric's own message; corrected, the metric is created and
// listed. Its page counts the entries the filter matches, and Edit metric, with
// the name shown disabled, saves a new filter that the console API reads
// back.
func TestLoggingAdminMetricsThroughTheBrowser(t *testing.T) {
	needService(t, "logging-metrics")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/metrics/browser-errors"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-metrics?project="+project+"&name="+url.QueryEscape(name), "")
	})
	const created = "/api/resources/logging-metrics"
	setField := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s") || document.querySelector("form.create-page #f-%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, id, id, v), nil)
	}

	p.navigate("/logs/metrics?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create metric")`)
	p.clickText(`#view .state button.primary`, "Create metric")
	// A form this long is a page of its own, /logs/metrics/create.
	p.run(chromedp.WaitVisible(`form.create-page #f-filter`, chromedp.ByQuery))
	p.run(chromedp.SendKeys(`form.create-page #f-name`, "browser-errors", chromedp.ByQuery))
	setField("filter", "jsonPayload.x:y")
	p.run(chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector("form.create-page .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("form.create-page .form-error").textContent`, &refusal)
	if !strings.HasPrefix(refusal, "InvalidArgument: filter term") {
		t.Errorf("a filter outside the grammar was refused with %q, want CreateLogMetric's INVALID_ARGUMENT", refusal)
	}
	p.forgive()

	setField("filter", "severity>=ERROR")
	p.run(chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	if posts := p.posts(created); len(posts) != 2 {
		t.Errorf("the create was posted %d times, want twice: %v", len(posts), posts)
	}

	p.clickText("#view tbody a", name)
	// The summary carries the count; the Matching entries tab says why it
	// is a count of entries and not a time series.
	p.waitFor(`document.querySelector("#view").textContent.includes("Matching entries, last hour")`)
	p.clickText("#view .page-actions button", "Edit metric")
	p.waitFor(`document.querySelector(".modal.is-open #f-filter") !== null`)
	var nameDisabled bool
	p.eval(`document.querySelector(".modal.is-open #f-name").disabled`, &nameDisabled)
	if !nameDisabled {
		t.Error("Edit metric offers the name as editable")
	}
	setField("filter", "severity>=WARNING")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	code, body := consoleDo(t, http.MethodGet, "/api/detail/logging-metrics?project="+project+"&name="+url.QueryEscape(name), "")
	if code != http.StatusOK || !strings.Contains(body, "severity\\u003e=WARNING") && !strings.Contains(body, "severity>=WARNING") {
		t.Errorf("the console API does not read the filter the browser saved: %d %s", code, body)
	}
}
