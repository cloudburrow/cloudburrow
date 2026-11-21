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
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 2`)
	p.waitFor(`document.querySelectorAll("#view svg.trace-chart circle").length === 2`)
	p.setField("#trace-service", "cartdb")
	p.clickText("#view .explorer-builder button", "Run query")
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 1`)
	var row string
	p.eval(`document.querySelector("#view .explorer-table tbody tr").textContent`, &row)
	if !strings.Contains(row, "db.query") || !strings.Contains(row, browserTraceID) || !strings.Contains(row, project) {
		t.Errorf("the filtered row = %q", row)
	}
	p.clickText("#view .explorer-table button", "db.query")
	p.waitFor(`!document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`)
	p.waitFor(`document.querySelectorAll("#trace-details .trace-bar-dot").length === 1`)
	p.clickText("#trace-details [role=tab]", "Logs & Events")
	p.waitFor(`document.querySelector("#trace-tab-panel").textContent.includes("cart query slow")`)
	p.clickText("#trace-details [role=tab]", "Metadata & Links")
	p.waitFor(`document.querySelector("#trace-tab-panel").textContent.includes("00f067aa0ba902b7")`)

	// The Logs Explorer's entry links to its trace.
	p.navigate("/logs?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tr.log-entry")].some((r) => r.textContent.includes("cart query slow"))`)
	p.eval(`[...document.querySelectorAll("#view tr.log-entry")].find((r) => r.textContent.includes("cart query slow")).querySelector(".log-expand").click()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view .log-detail a")].some((a) => a.textContent === "View trace details")`)
	p.eval(`[...document.querySelectorAll("#view .log-detail a")].find((a) => a.textContent === "View trace details").click()`, nil)
	p.waitFor(fmt.Sprintf(`location.pathname === "/traces/explorer" && location.search.includes(%q)`, browserTraceID))
	p.waitFor(`document.querySelector("#trace-details") !== null && !document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`)
}

// TestTraceExplorerMeetsWCAG (#2118): Trace explorer, and its trace
// details opened, through the console's WCAG 2.1 AA audit.
func TestTraceExplorerMeetsWCAG(t *testing.T) {
	needService(t, "trace-explorer")
	project := writeBrowserTrace(t)
	auditScreens(t, []a11yPage{
		screen("Trace explorer", "/traces/explorer", project),
		{"Trace details", "/traces/explorer?trace=" + browserTraceID + "&project=" + project,
			settled + ` && !document.querySelector("#trace-details").hidden && document.querySelectorAll("#trace-details .trace-row").length === 2`, ""},
	})
}
