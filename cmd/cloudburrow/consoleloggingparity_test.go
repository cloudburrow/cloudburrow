package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/distribution"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestLoggingAdminFormsFollowGoogle (#1863): the Log Router, Log-based
// Metrics and Logs Storage forms and lists carry Google's labels, sections
// and order.
func TestLoggingAdminFormsFollowGoogle(t *testing.T) {
	sections := func(fields []console.Field) []string {
		var out []string
		for _, f := range fields {
			if len(out) == 0 || out[len(out)-1] != f.Section {
				out = append(out, f.Section)
			}
		}
		return out
	}
	label, fields := loggingSinksProvider{}.CreateForm()
	if got := strings.Join(sections(fields), "|"); label != "Create sink" ||
		got != "Sink details|Sink destination|Choose logs to include in sink|Choose logs to filter out of sink" {
		t.Errorf("Create sink = %q with sections %s", label, got)
	}
	if (loggingSinksProvider{}).CreateLayout() != console.CreateLayoutPage {
		t.Error("Create sink is not one page")
	}
	for _, f := range fields {
		if f.Name == "service" {
			if strings.Join(f.Options, "|") != "Cloud Logging bucket|BigQuery dataset|Cloud Storage bucket|Pub/Sub topic|Splunk|Google Cloud project|Other resource" ||
				f.UnavailableOptions[sinkSplunk] == "" || f.Label != "Select sink service" {
				t.Errorf("Select sink service = %+v", f)
			}
		}
		if f.Name == "disabled" {
			t.Error("Create sink offers Disabled, which Google's form has not")
		}
	}

	label, fields = loggingMetricsProvider{}.CreateForm()
	if got := strings.Join(sections(fields), "|"); label != "Create metric" || (loggingMetricsProvider{}).CreateTitle() != "Create logs metric" ||
		got != "Metric type|Details|Filter selection|Labels" || fields[0].Name != "type" || fields[0].Type != "radio" {
		t.Errorf("Create metric = %q with sections %s, first %+v", label, got, fields[0])
	}

	label, fields = loggingBucketsProvider{}.CreateForm()
	// The last step: Retention period, then Custom indexes (#2108).
	if n := len(fields); label != "Create log bucket" || fields[n-2].Label != "Retention period" || fields[n-2].Default != "30" ||
		fields[n-1].Label != "Custom indexes" || fields[n-1].AddLabel != "Add index" || len(fields[n-1].Group) != 2 {
		t.Errorf("Create log bucket = %q %+v", label, fields)
	}
}

// TestConsoleLoggingParityActions (#1863): Disable sink and Enable sink on a
// Log Router row, Splunk refused, a distribution made from Field name,
// Regular expression and Linear buckets, and a log bucket created upgraded
// to Observability Analytics (its linked dataset needs BigQuery served,
// which this test's instance has not).
func TestConsoleLoggingParityActions(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	sinks := srv.URL + "/api/resources/logging-sinks?project=" + project
	if code, out := consolePost(t, sinks, map[string]string{"name": "to-topic", "service": "Pub/Sub topic", "destination": "projects/proj-1/topics/t",
		"description": "errors out"}); code != http.StatusOK {
		t.Fatalf("create sink = %d %s", code, out)
	}
	if code, out := consolePost(t, sinks, map[string]string{"name": "to-splunk", "service": "Splunk", "destination": "x"}); code == http.StatusOK || !strings.Contains(out, "Dataflow") {
		t.Errorf("a Splunk sink = %d %s; want the refusal naming Dataflow", code, out)
	}
	var l console.Listing
	getJSON200(t, sinks, &l)
	// The _Required sink is listed after it, Edit, Disable and Delete
	// greyed out (#2108).
	if len(l.Items) != 2 || l.Items[1].Name != "projects/proj-1/sinks/_Required" || l.Items[1].Withheld["edit"] == "" || l.Items[1].Actions[1].Unavailable == "" {
		t.Errorf("the _Required row = %+v", l.Items)
	}
	if len(l.Items) < 1 || l.Items[0].Fields["Type"] != "Pub/Sub topic" || l.Items[0].Fields["Enabled"] != "Enabled" ||
		l.Items[0].Fields["Description"] != "errors out" || len(l.Items[0].Actions) != 4 || l.Items[0].Actions[1].Label != "Disable sink" || l.Items[0].Fields["Volume"] != "0 B" ||
		l.Items[0].Actions[2].Label != "Troubleshoot sink" {
		t.Fatalf("the Log Router row = %+v", l.Items)
	}
	name := "projects/proj-1/sinks/to-topic"
	act := srv.URL + "/api/actions/logging-sinks?project=" + project
	if code, out := consolePost(t, act, map[string]string{"Name": name, "Action": "disable"}); code != http.StatusOK {
		t.Fatalf("Disable sink = %d %s", code, out)
	}
	// Troubleshoot sink on the row answers with its findings (#1863).
	if code, out := consolePost(t, act, map[string]any{"Path": []string{name}, "Action": "troubleshoot-sink"}); code != http.StatusOK ||
		!strings.Contains(out, "the inclusion filter") && !strings.Contains(out, "no inclusion filter") {
		t.Errorf("Troubleshoot sink = %d %s", code, out)
	}
	s, _ := svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
	if !s.GetDisabled() || s.GetDestination() != "pubsub.googleapis.com/projects/proj-1/topics/t" || s.GetDescription() != "errors out" {
		t.Errorf("after Disable sink, the sink is %+v", s)
	}
	getJSON200(t, sinks, &l)
	if l.Items[0].Actions[1].ID != "enable" {
		t.Errorf("a disabled sink's row offers %+v", l.Items[0].Actions)
	}
	if code, out := consolePost(t, act, map[string]string{"Name": name, "Action": "enable"}); code != http.StatusOK {
		t.Fatalf("Enable sink = %d %s", code, out)
	}
	if s, _ := svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name}); s.GetDisabled() {
		t.Error("Enable sink left the sink disabled")
	}

	metrics := srv.URL + "/api/resources/logging-metrics?project=" + project
	if code, out := consolePost(t, metrics, map[string]string{"type": "Distribution", "name": "took", "unit": "ms", "filter": "severity>=INFO",
		"field_name": "textPayload", "regex": `took ([0-9]+)ms`, "bucket_type": "Linear", "linear_start": "0", "linear_count": "10",
		"linear_width": "5"}); code != http.StatusOK {
		t.Fatalf("create distribution = %d %s", code, out)
	}
	m, err := svc.admin.MetricsServer().GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: "projects/proj-1/metrics/took"})
	lin, _ := m.GetBucketOptions().GetOptions().(*distribution.Distribution_BucketOptions_LinearBuckets)
	if err != nil || m.GetValueExtractor() != `REGEXP_EXTRACT(textPayload, "took ([0-9]+)ms")` || lin == nil ||
		lin.LinearBuckets.GetNumFiniteBuckets() != 10 || lin.LinearBuckets.GetWidth() != 5 {
		t.Fatalf("the distribution = %+v, %v", m, err)
	}
	if f, re := extractorParts(m.GetValueExtractor()); f != "textPayload" || re != "took ([0-9]+)ms" {
		t.Errorf("the edit form reads the extractor back as %q, %q", f, re)
	}

	buckets := srv.URL + "/api/resources/logging-buckets?project=" + project
	if code, out := consolePost(t, buckets, map[string]string{"name": "analysed", "location": "us", "retention": "30",
		"analytics": "true", "link": "false", "linkId": ""}); code != http.StatusOK {
		t.Fatalf("create log bucket = %d %s", code, out)
	}
	getJSON200(t, buckets, &l)
	found := false
	for _, it := range l.Items {
		if it.Name == "projects/proj-1/locations/us/buckets/analysed" {
			found = it.Fields["Observability Analytics available"] == "Yes" && it.Fields["Linked dataset in BigQuery"] == "—" &&
				it.Fields["Status"] == "Unlocked" && it.Fields["Region"] == "us"
		}
	}
	if !found {
		t.Errorf("the upgraded bucket's row is not in %+v", l.Items)
	}
}
