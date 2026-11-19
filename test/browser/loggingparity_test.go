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

// sectionTitles reads a create page's section headings, in order.
const sectionTitles = `[...document.querySelectorAll("#view form.create-page .form-section-title")].map((h) => h.textContent).join("|")`

// TestLoggingRouterPagesFollowGoogle (#1863), in the storage shard, whose
// instance serves Cloud Logging. Log Router's Create sink is Google's one
// page of four panels (Sink details, Sink destination, Choose logs to
// include in sink, Choose logs to filter out of sink) with Select sink
// service's seven choices, Splunk disabled; choosing Pub/Sub topic and
// Create sink lists the sink under Google's columns, and the row's More
// actions Disable sink disables it, which the console API reads back.
func TestLoggingRouterPagesFollowGoogle(t *testing.T) {
	needService(t, "logging-sinks")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/sinks/browser-router"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&name="+url.QueryEscape(name), "")
	})

	p.navigate("/logs/sinks?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent === "Create sink")`)
	p.eval(`[...document.querySelectorAll("#view button.primary")].find((b) => b.textContent === "Create sink").click(), true`, nil)
	p.run(chromedp.WaitVisible(chromedp.CSS(`form.create-page #f-destination`)))
	var titles string
	p.eval(sectionTitles, &titles)
	if titles != "Sink details|Sink destination|Choose logs to include in sink|Choose logs to filter out of sink" {
		t.Errorf("Create sink's panels = %q", titles)
	}
	var choices string
	p.eval(`[...document.querySelectorAll("form.create-page fieldset.is-radio input[type=radio]")].map((r) => r.value + (r.disabled ? "(off)" : "")).join("|")`, &choices)
	if choices != "Cloud Logging bucket|BigQuery dataset|Cloud Storage bucket|Pub/Sub topic|Splunk(off)|Google Cloud project|Other resource" {
		t.Errorf("Select sink service offers %q", choices)
	}
	var disabledOffered bool
	p.eval(`document.querySelector("form.create-page #f-disabled") !== null`, &disabledOffered)
	if disabledOffered {
		t.Error("Create sink offers Disabled, which Google's form has not")
	}
	p.eval(`[...document.querySelectorAll("form.create-page fieldset.is-radio input[type=radio]")].find((r) => r.value === "Pub/Sub topic").click(), true`, nil)
	setField(p, "form.create-page", "f-name", "browser-router")
	setField(p, "form.create-page", "f-description", "browser sink")
	setField(p, "form.create-page", "f-destination", "projects/"+project+"/topics/t")
	setField(p, "form.create-page", "f-filter", "severity>=ERROR")
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"]`)))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+name)
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/sinks" && document.querySelector(%q) !== null`, row))

	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join("|")`, &heads)
	for _, want := range []string{"Enabled", "Type", "Name", "Description", "Destination", "Created", "Last updated"} {
		if !strings.Contains("|"+heads+"|", "|"+want+"|") {
			t.Errorf("the Log Router's columns %q lack %s", heads, want)
		}
	}
	var cells string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, row), &cells)
	if !strings.Contains(cells, "Pub/Sub topic") || !strings.Contains(cells, "browser sink") ||
		!strings.Contains(cells, "pubsub.googleapis.com/projects/"+project+"/topics/t") {
		t.Errorf("the sink's row reads %q", cells)
	}

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name))))
	var items string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent.trim()).join("|")`, &items)
	if !strings.HasPrefix(items, "View sink details|Disable sink") {
		t.Errorf("the row's menu = %q, want View sink details then Disable sink", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Disable sink")
	p.waitFor(fmt.Sprintf(`(document.querySelector(%q) || { textContent: "" }).textContent.includes("Disabled")`, row))
	if code, body := consoleDo(t, http.MethodGet, "/api/detail/logging-sinks?project="+project+"&name="+url.QueryEscape(name), ""); code != http.StatusOK ||
		!strings.Contains(body, `"Disabled"`) {
		t.Errorf("the console API does not read the sink disabled: %d %s", code, body)
	}
}

// TestLoggingMetricAndBucketFormsFollowGoogle (#1863), in the storage
// shard: Create metric opens Google's Create logs metric page, Metric type
// first, then Details, Filter selection and Labels; choosing Distribution
// shows Field name and the Advanced (Histogram buckets) choice, Counter hides
// them. Create log bucket has Google's Name, Select log bucket region and a
// Retention period of 30, and Logs Storage lists Google's columns.
func TestLoggingMetricAndBucketFormsFollowGoogle(t *testing.T) {
	needService(t, "logging-metrics")
	needService(t, "logging-buckets")
	p := open(t)
	project := uniqueProject(t)

	p.navigate("/logs/metrics/create?project=" + project)
	p.run(chromedp.WaitVisible(chromedp.CSS(`form.create-page #f-filter`)))
	p.waitFor(`[...document.querySelectorAll("#view h1")].some((h) => h.textContent === "Create logs metric")`)
	var titles string
	p.eval(sectionTitles, &titles)
	if titles != "Metric type|Details|Filter selection|Labels" {
		t.Errorf("Create logs metric's sections = %q", titles)
	}
	visible := func(id string) bool {
		var on bool
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector("form.create-page #%s"); return !!f && !f.closest(".form-row, fieldset").hidden; })()`, id), &on)
		return on
	}
	pick := func(v string) {
		p.eval(fmt.Sprintf(`[...document.querySelectorAll("form.create-page fieldset.is-radio input[type=radio]")].find((r) => r.value === %q).click(), true`, v), nil)
	}
	if visible("f-field_name") {
		t.Error("a counter's form shows Field name")
	}
	pick("Distribution")
	p.waitFor(`!document.querySelector("form.create-page #f-field_name").closest(".form-row, fieldset").hidden`)
	if !visible("f-bucket_type") || !visible("f-linear_width") {
		t.Error("a distribution's form does not show Advanced (Histogram buckets) with Linear's Bucket width (b)")
	}
	pick("Counter")
	p.waitFor(`document.querySelector("form.create-page #f-field_name").closest(".form-row, fieldset").hidden`)

	p.navigate("/logs/storage?project=" + project)
	p.waitFor(`document.querySelector("#view thead") !== null`)
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join("|")`, &heads)
	for _, want := range []string{"Name", "Description", "Retention period", "Region", "Status", "Observability Analytics available", "Linked dataset in BigQuery"} {
		if !strings.Contains("|"+heads+"|", "|"+want+"|") {
			t.Errorf("Logs Storage's columns %q lack %s", heads, want)
		}
	}
	p.navigate("/logs/storage/create?project=" + project)
	p.waitFor(`document.querySelector("form.create-page #f-location") !== null`)
	var form string
	p.eval(`[...document.querySelectorAll("form.create-page label, form.create-page legend")].map((l) => l.textContent).join("|")`, &form)
	for _, want := range []string{"Name", "Description", "Upgrade to use Observability Analytics", "Select log bucket region", "Retention period"} {
		if !strings.Contains(form, want) {
			t.Errorf("Create log bucket's labels %q lack %s", form, want)
		}
	}
	var retention string
	p.eval(`document.querySelector("form.create-page #f-retention").value`, &retention)
	if retention != "30" {
		t.Errorf("Retention period starts at %q, want Google's 30", retention)
	}
}
