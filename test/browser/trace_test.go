//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cloudtrace "cloud.google.com/go/trace/apiv2"
	"cloud.google.com/go/trace/apiv2/tracepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// envTrace is Cloud Trace's endpoint (#2118), gRPC and JSON on one port,
// as the compat harness names it.
const envTrace = "CLOUDBURROW_TEST_TRACE"

const browserTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// writeBrowserTrace writes a two-span trace through the official Cloud
// Trace v2 client, and a log entry of its child span through the Logging
// API, and returns the project.
func writeBrowserTrace(t *testing.T) string {
	t.Helper()
	addr := endpoint(t, envTrace)
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the span's log entry is written through the Logging API", envLogging)
	}
	ctx := context.Background()
	c, err := cloudtrace.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	project := uniqueProject(t)
	now := time.Now().Add(-time.Minute)
	span := func(id, parent, name, service string, off, d time.Duration) *tracepb.Span {
		return &tracepb.Span{Name: "projects/" + project + "/traces/" + browserTraceID + "/spans/" + id, SpanId: id, ParentSpanId: parent,
			DisplayName: &tracepb.TruncatableString{Value: name}, StartTime: timestamppb.New(now.Add(off)), EndTime: timestamppb.New(now.Add(off + d)),
			SpanKind: tracepb.Span_SERVER, Attributes: &tracepb.Span_Attributes{AttributeMap: map[string]*tracepb.AttributeValue{
				"service.name": {Value: &tracepb.AttributeValue_StringValue{StringValue: &tracepb.TruncatableString{Value: service}}}}}}
	}
	if err := c.BatchWriteSpans(ctx, &tracepb.BatchWriteSpansRequest{Name: "projects/" + project, Spans: []*tracepb.Span{
		span("00f067aa0ba902b7", "", "GET /cart", "frontend", 0, 250*time.Millisecond),
		span("00f067aa0ba902b8", "00f067aa0ba902b7", "db.query", "cartdb", 30*time.Millisecond, 80*time.Millisecond),
	}}); err != nil {
		t.Fatalf("BatchWriteSpans: %v", err)
	}
	b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
		"entries": []map[string]any{{"severity": "WARNING", "textPayload": "cart query slow",
			"trace": "projects/" + project + "/traces/" + browserTraceID, "spanId": "00f067aa0ba902b8",
			"timestamp": now.Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano)}}})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
		t.Fatalf("entries:write = %d: %s", code, out)
	}
	return project
}

// TestTraceExplorerThroughThePage (#2118): spans written with the official
// Cloud Trace v2 client are charted and listed in Trace explorer, the Span
// filters narrow them, a row opens the trace details with its timeline,
// and the child span's Logs & Events tab shows the log entry written with
// its trace and spanId; the Logs Explorer's View trace details opens the
// same trace.
func TestTraceExplorerThroughThePage(t *testing.T) {
	needService(t, "trace-explorer")
	project := writeBrowserTrace(t)

	p := open(t)
	p.navigate("/traces/explorer?project=" + project)
	p.waitFor(`document.querySelectorAll("#view .trace-spans tbody tr").length === 2`)
	// The heatmap: two spans, 250 ms and 80 ms, in two latency rows.
	p.waitFor(`document.querySelectorAll("#view svg.trace-heatmap rect.trace-cell").length === 2`)
	var cells []string
	p.eval(`[...document.querySelectorAll("#view svg.trace-heatmap rect.trace-cell")].map((r) => r.getAttribute("aria-label"))`, &cells)
	if len(cells) != 2 || !strings.Contains(strings.Join(cells, "|"), "200 ms to 500 ms") || !strings.Contains(strings.Join(cells, "|"), "50 ms to 100 ms") {
		t.Errorf("the heatmap's cells = %q", cells)
	}
	// A cell lists its spans, and clearing it lists them all again.
	p.eval(`[...document.querySelectorAll("#view svg.trace-heatmap rect.trace-cell")].find((r) => r.getAttribute("aria-label").includes("50 ms to 100 ms")).dispatchEvent(new MouseEvent("click", { bubbles: true }))`, nil)
	p.waitFor(`document.querySelector(".trace-selection-chip") !== null && document.querySelectorAll("#view .trace-spans tbody tr").length === 1 && document.querySelector("#view .trace-spans tbody tr").textContent.includes("db.query")`)
	p.clickText("#view .trace-selection-chip button", "Clear selection")
	p.waitFor(`document.querySelector(".trace-selection-chip") === null && document.querySelectorAll("#view .trace-spans tbody tr").length === 2`)
	// The Chart view menu.
	p.setField("#trace-chart-view", "percentile")
	p.waitFor(`document.querySelectorAll("#view svg.trace-series polyline").length === 4 && document.querySelector(".trace-series-legend").textContent.includes("99th percentile")`)
	p.setField("#trace-chart-view", "rate")
	p.waitFor(`document.querySelectorAll("#view svg.trace-series polyline").length === 2 && document.querySelector(".trace-series-legend").textContent.includes("Error")`)
	p.setField("#trace-chart-view", "heatmap")
	p.waitFor(`document.querySelector("#view svg.trace-heatmap") !== null`)
	// The Grouped table: one row per service and span name.
	p.clickText("#view [role=tab]", "Grouped")
	p.waitFor(`document.querySelectorAll("#view .trace-groups tbody tr").length === 2`)
	var group string
	p.eval(`[...document.querySelectorAll("#view .trace-groups tbody tr")].find((r) => r.textContent.includes("db.query")).textContent`, &group)
	if !strings.Contains(group, "cartdb") || !strings.Contains(group, "80.000") {
		t.Errorf("the db.query group = %q", group)
	}
	p.clickText("#view [role=tab]", "Spans")
	p.setField("#trace-service", "cartdb")
	p.clickText("#view .explorer-builder button", "Run query")
	p.waitFor(`document.querySelectorAll("#view .trace-spans tbody tr").length === 1`)
	var row string
	p.eval(`document.querySelector("#view .trace-spans tbody tr").textContent`, &row)
	if !strings.Contains(row, "db.query") || !strings.Contains(row, browserTraceID) || !strings.Contains(row, project) {
		t.Errorf("the filtered row = %q", row)
	}
	p.clickText("#view .trace-spans button", "db.query")
	p.waitFor(`!document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`)
	p.waitFor(`document.querySelectorAll("#trace-details .trace-bar-dot").length === 1`)
	p.clickText("#trace-details [role=tab]", "Logs & Events")
	p.waitFor(`document.querySelector("#trace-tab-panel").textContent.includes("cart query slow")`)
	p.clickText("#trace-details [role=tab]", "Metadata & Links")
	p.waitFor(`document.querySelector("#trace-tab-panel").textContent.includes("00f067aa0ba902b7")`)

	// Find in Trace: a match in the name, the service or an attribute, case
	// blind, is highlighted and selected; Next moves to the next.
	p.setField("#trace-find", "CART")
	p.waitFor(`document.querySelectorAll("#trace-details .trace-row.is-match").length === 2 && document.querySelector("#trace-find-count").textContent === "1 of 2 matches"`)
	p.waitFor(`document.querySelectorAll("#trace-details mark.trace-find-mark").length >= 2`)
	p.clickText("#trace-details .trace-find button", "Next")
	p.waitFor(`document.querySelector("#trace-find-count").textContent === "2 of 2 matches" && document.querySelector("#trace-details .trace-row.is-current-match").textContent.includes("db.query")`)
	p.setField("#trace-find", "frontend")
	p.waitFor(`document.querySelectorAll("#trace-details .trace-row.is-match").length === 1 && document.querySelector("#trace-details .trace-row.is-match").textContent.includes("GET /cart")`)
	p.setField("#trace-find", "nothing-like-it")
	p.waitFor(`document.querySelector("#trace-find-count").textContent === "No matches" && document.querySelectorAll("#trace-details .trace-row.is-match").length === 0`)
	p.setField("#trace-find", "")

	// The Graph: one node per span, an edge from the root to its child; a
	// node selects its span.
	p.clickText("#trace-details .trace-details-toolbar button", "Graph")
	p.waitFor(`document.querySelectorAll("#trace-details svg.trace-graph g.trace-node").length === 2 && document.querySelectorAll("#trace-details svg.trace-graph path.trace-edge").length === 1`)
	var node string
	p.eval(`[...document.querySelectorAll("#trace-details g.trace-node")].map((g) => g.textContent).join("|")`, &node)
	if !strings.Contains(node, "GET /cart") || !strings.Contains(node, "250.000 ms") || !strings.Contains(node, "80.000 ms") {
		t.Errorf("the graph's nodes = %q", node)
	}
	p.eval(`[...document.querySelectorAll("#trace-details g.trace-node")].find((g) => g.textContent.includes("GET /cart")).dispatchEvent(new MouseEvent("click", { bubbles: true }))`, nil)
	p.waitFor(`document.querySelector("#trace-details g.trace-node.is-selected").textContent.includes("GET /cart")`)
	p.clickText("#trace-details .trace-details-toolbar button", "Timeline")
	p.waitFor(`document.querySelectorAll("#trace-details .trace-row").length === 2`)

	// The Logs Explorer's entry links to its trace.
	p.navigate("/logs?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tr.log-entry")].some((r) => r.textContent.includes("cart query slow"))`)
	p.eval(`[...document.querySelectorAll("#view tr.log-entry")].find((r) => r.textContent.includes("cart query slow")).querySelector(".log-expand").click()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view .log-detail a")].some((a) => a.textContent === "View trace details")`)
	p.eval(`[...document.querySelectorAll("#view .log-detail a")].find((a) => a.textContent === "View trace details").click()`, nil)
	p.waitFor(fmt.Sprintf(`location.pathname === "/traces/explorer" && location.search.includes(%q)`, browserTraceID))
	p.waitFor(`document.querySelector("#trace-details") !== null && !document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`)
}

// TestTraceExplorerMeetsWCAG (#2118): Trace explorer with its heatmap, its
// Chart view's percentile and rate charts, and its Grouped table, and the
// trace details opened on the timeline with Find in Trace's matches and on
// the Graph, through the console's WCAG 2.1 AA audit.
func TestTraceExplorerMeetsWCAG(t *testing.T) {
	needService(t, "trace-explorer")
	project := writeBrowserTrace(t)
	auditScreens(t, []a11yPage{
		screen("Trace explorer", "/traces/explorer", project),
		{"Trace explorer percentiles", "/traces/explorer?chart=percentile&project=" + project,
			settled + ` && document.querySelectorAll("#view svg.trace-series polyline").length === 4`, ""},
		{"Trace explorer span rate", "/traces/explorer?chart=rate&project=" + project,
			settled + ` && document.querySelectorAll("#view svg.trace-series polyline").length === 2`, ""},
		{"Trace explorer grouped", "/traces/explorer?table=grouped&project=" + project,
			settled + ` && document.querySelectorAll("#view .trace-groups tbody tr").length === 2`, ""},
		{"Trace details", "/traces/explorer?trace=" + browserTraceID + "&project=" + project,
			settled + ` && !document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`, ""},
		{"Trace details find", "/traces/explorer?trace=" + browserTraceID + "&project=" + project,
			settled + ` && !document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`,
			`const f = document.querySelector("#trace-find"); f.value = "cart"; f.dispatchEvent(new Event("input", { bubbles: true }))`},
		{"Trace details graph", "/traces/explorer?view=graph&trace=" + browserTraceID + "&project=" + project,
			settled + ` && !document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details g.trace-node").length === 2`, ""},
	})
}
