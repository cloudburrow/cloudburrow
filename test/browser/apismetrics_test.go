//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/api/iterator"
)

// TestAPIsMetricsPerProjectOverATimeRange (#1866): calls the official
// Cloud Tasks client makes in one project are counted on that
// project's Enabled APIs & services and not on another's; the dashboard
// and an API's page have Google's time ranges (1 hour to 30 days); the
// Overview draws Traffic by response code (200 and 404 here); Metrics
// shows Google's four default graphs, and Select Graphs adds Traffic by
// API method and removes a graph. The pages, Select Graphs open among
// them, meet the WCAG checks.
func TestAPIsMetricsPerProjectOverATimeRange(t *testing.T) {
	needService(t, "serviceusage")
	needService(t, "tasks")
	ctx := context.Background()
	c, err := cloudtasks.NewClient(ctx, appEngineClientOpts(appEngineAddr(t, envTasks))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	busy, quiet := uniqueProject(t), uniqueProject(t)
	const api = "cloudtasks.googleapis.com"
	for i := 0; i < 3; i++ {
		it := c.ListQueues(ctx, &taskspb.ListQueuesRequest{Parent: "projects/" + busy + "/locations/us-central1"})
		if _, err := it.Next(); err != nil && err != iterator.Done {
			t.Fatalf("ListQueues in %s: %v", busy, err)
		}
	}
	if _, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: "projects/" + busy + "/locations/us-central1/queues/absent"}); err == nil {
		t.Fatal("GetQueue of a queue that does not exist succeeded")
	}

	p := open(t)
	requests := func(project string) string {
		t.Helper()
		p.navigate("/apis?project=" + project)
		row := fmt.Sprintf(`#apis-table tr[data-api=%q]`, api)
		p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null && document.querySelector("#apis-range") !== null`, row))
		var n string
		p.eval(fmt.Sprintf(`document.querySelector(%q).cells[1].textContent`, row), &n)
		return n
	}
	if got := requests(busy); got != "4" {
		t.Errorf("Cloud Tasks' requests in %s = %q, want its 4 calls", busy, got)
	}
	var ranges []string
	p.eval(`[...document.querySelectorAll("#apis-range button")].map((b) => b.textContent)`, &ranges)
	if strings.Join(ranges, "|") != "1 hour|6 hours|12 hours|1 day|2 days|4 days|7 days|14 days|30 days" {
		t.Errorf("time ranges = %q", ranges)
	}
	var note string
	p.eval(`document.querySelector("#apis-counters-note").textContent`, &note)
	if !strings.Contains(note, busy) {
		t.Errorf("the counters note does not name the project: %q", note)
	}
	p.clickExpr(`document.querySelector('#apis-range button[data-range="P7D"]')`)
	p.waitFor(`document.querySelector('#apis-range button[data-range="P7D"][aria-pressed="true"]') !== null`)
	if got := requests(quiet); got != "0" {
		t.Errorf("Cloud Tasks' requests in %s, which made none = %q", quiet, got)
	}

	// Overview: Traffic by response code.
	p.navigate("/apis/api/" + api + "/overview?project=" + busy)
	p.waitFor(`document.querySelector("#api-overview-chart-traffic-by-code") !== null`)
	var codes []string
	p.eval(`[...document.querySelectorAll("#api-overview-chart-traffic-by-code .bq-chart-legend li")].map((n) => n.textContent.trim())`, &codes)
	if strings.Join(codes, "|") != "200|404" {
		t.Errorf("Traffic by response code legend = %q, want 200 and 404", codes)
	}

	// Metrics: the four default graphs; Select Graphs changes them.
	p.navigate("/apis/api/" + api + "/metrics?project=" + busy)
	p.waitFor(`document.querySelector("#api-metrics-select-graphs") !== null`)
	shown := func() string {
		var titles []string
		p.eval(`[...document.querySelectorAll("#api-metrics-charts .chart-card h3")].map((n) => n.textContent)`, &titles)
		return strings.Join(titles, "|")
	}
	if got := shown(); got != "Traffic by response code|Errors by API method|Overall latency|Latency by API method (median)" {
		t.Errorf("default graphs = %q", got)
	}
	var latency []string
	p.eval(`[...document.querySelectorAll("#api-metrics-chart-latency-overall .bq-chart-legend li")].map((n) => n.textContent.trim())`, &latency)
	if strings.Join(latency, "|") != "50th percentile|95th percentile|99th percentile" {
		t.Errorf("Overall latency series = %q", latency)
	}
	p.clickExpr(`document.querySelector("#api-metrics-select-graphs")`)
	p.clickExpr(`document.querySelector("#api-metrics-graph-traffic-by-method")`)
	p.clickExpr(`document.querySelector("#api-metrics-graph-errors-by-method")`)
	p.waitFor(`document.querySelector("#api-metrics-chart-traffic-by-method") !== null && document.querySelector("#api-metrics-chart-errors-by-method") === null`)
	var methods []string
	p.eval(`[...document.querySelectorAll("#api-metrics-chart-traffic-by-method .bq-chart-legend li")].map((n) => n.textContent.trim())`, &methods)
	if strings.Join(methods, "|") != "GetQueue|ListQueues" {
		t.Errorf("Traffic by API method = %q, want GetQueue and ListQueues", methods)
	}

	auditScreens(t, []a11yPage{
		screen("APIs dashboard with traffic", "/apis", busy),
		screen("API overview with traffic", "/apis/api/"+api+"/overview", busy),
		screen("API metrics with traffic", "/apis/api/"+api+"/metrics", busy),
		{"API metrics Select Graphs", "/apis/api/" + api + "/metrics?project=" + busy,
			`document.querySelector("#api-metrics-select-graphs") !== null`,
			`document.querySelector("#api-metrics-select-graphs").click()`},
	})
}
