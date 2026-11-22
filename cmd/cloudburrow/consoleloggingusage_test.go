package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// TestSinkVolumeAndUsageAlert (#2021): what a sink routes is counted, as
// the Log Router's Volume and Cloud Monitoring's
// logging.googleapis.com/exports/* on the logging_sink resource; what log
// buckets take is billing/bytes_ingested and the month-to-date
// billing/monthly_bytes_ingested, which the Logs Storage summary reads;
// Create usage alert makes Google's policy on the monthly metric.
func TestSinkVolumeAndUsageAlert(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	mon, err := monitoring.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	mon.AddBuiltin(monitoring.NewLogBuiltin(monitoringLogs{svc: svc}))
	screen := loggingAdminScreen{svc: svc, monitoring: func() *monitoring.Server { return mon }}
	buckets, sinks := loggingBucketsProvider{screen}, loggingSinksProvider{screen}
	if _, err := buckets.Create(ctx, project, map[string]string{"name": "errs", "location": "global", "retention": "30"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.admin.ConfigServer().CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/" + project, Sink: &loggingpb.LogSink{
		Name: "errors", Filter: "severity>=ERROR", Destination: "logging.googleapis.com/projects/proj-1/locations/global/buckets/errs"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/app",
		Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{{Severity: ltype.LogSeverity_ERROR, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "boom"}},
			{Severity: ltype.LogSeverity_ERROR, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "bang"}},
			{Severity: ltype.LogSeverity_INFO, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "fine"}}}}); err != nil {
		t.Fatal(err)
	}

	l, err := sinks.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 3 || !strings.HasSuffix(l.Items[0].Fields["Volume"], "B") || l.Items[0].Fields["Volume"] == "0 B" {
		t.Errorf("the Log Router row = %+v; want a Volume", l.Items)
	}
	link := sinks.Actions(l.Items[0])[3]
	if link.Label != "View sink log volume and error rates" || !strings.Contains(link.Href, "exports%2Fbyte_count") ||
		!strings.Contains(link.Href, "logging_sink") || link.Unavailable != "" {
		t.Errorf("the volume link = %+v", link)
	}

	now := svc.api.Now()
	series := func(metric string) []*monitoringpb.TimeSeries {
		t.Helper()
		resp, err := mon.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project,
			Filter:   `metric.type = "` + metric + `"`,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-10 * time.Minute)), EndTime: timestamppb.New(now.Add(2 * time.Minute))}})
		if err != nil {
			t.Fatalf("%s: %v", metric, err)
		}
		return resp.GetTimeSeries()
	}
	total := func(ts []*monitoringpb.TimeSeries) int64 {
		var n int64
		for _, s := range ts {
			for _, p := range s.GetPoints() {
				n += p.GetValue().GetInt64Value()
			}
		}
		return n
	}
	exported := series("logging.googleapis.com/exports/log_entry_count")
	if total(exported) != 2 || exported[0].GetResource().GetType() != "logging_sink" || exported[0].GetResource().GetLabels()["name"] != "errors" {
		t.Errorf("exports/log_entry_count = %v; want 2 on the sink errors", exported)
	}
	if total(series("logging.googleapis.com/exports/byte_count")) <= 0 {
		t.Error("exports/byte_count counted no bytes")
	}
	ingested := series("logging.googleapis.com/billing/bytes_ingested")
	if total(ingested) <= 0 || ingested[0].GetMetric().GetLabels()["resource_type"] != "global" {
		t.Errorf("billing/bytes_ingested = %v", ingested)
	}
	monthly := series("logging.googleapis.com/billing/monthly_bytes_ingested")
	if len(monthly) != 1 || len(monthly[0].GetPoints()) == 0 || monthly[0].GetMetricKind().String() != "GAUGE" {
		t.Errorf("billing/monthly_bytes_ingested = %v", monthly)
	}

	bl, err := buckets.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(bl.Summary) != 1 || bl.Summary[0].Properties[0].Label != "Current month ingestion" || bl.Summary[0].Properties[0].Value == "0 B" {
		t.Errorf("the Logs Storage summary = %+v", bl.Summary)
	}
	acts := buckets.ListActions(ctx, project)
	if len(acts) != 1 || acts[0].Label != "Create usage alert" || acts[0].Unavailable != "" {
		t.Fatalf("the Logs Storage actions = %+v", acts)
	}
	if err := buckets.ActAt(ctx, project, nil, usageAlertAction, map[string]string{"displayName": "Too many logs", "threshold": "2", "retest": "10"}); err == nil {
		t.Error("a 10-minute retest window was accepted")
	}
	if err := buckets.ActAt(ctx, project, nil, usageAlertAction, map[string]string{"displayName": "Too many logs", "threshold": "2"}); err != nil {
		t.Fatal(err)
	}
	pols, err := mon.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{Name: "projects/" + project})
	if err != nil || len(pols.GetAlertPolicies()) != 1 {
		t.Fatalf("policies = %v, %v", pols, err)
	}
	c := pols.GetAlertPolicies()[0].GetConditions()[0].GetConditionThreshold()
	agg := c.GetAggregations()[0]
	if !strings.Contains(c.GetFilter(), "billing/monthly_bytes_ingested") || c.GetThresholdValue() != 2*(1<<30) ||
		c.GetComparison() != monitoringpb.ComparisonType_COMPARISON_GT || c.GetDuration().AsDuration() != 30*time.Minute ||
		agg.GetPerSeriesAligner() != monitoringpb.Aggregation_ALIGN_MAX || agg.GetCrossSeriesReducer() != monitoringpb.Aggregation_REDUCE_SUM ||
		agg.GetAlignmentPeriod().AsDuration() != time.Hour {
		t.Errorf("the usage alert's condition = %v", c)
	}

	// Without Cloud Monitoring the action and the link say why.
	bare := loggingAdminScreen{svc: svc}
	if a := (loggingBucketsProvider{bare}).ListActions(ctx, project); len(a) != 1 || a[0].Unavailable == "" {
		t.Errorf("Create usage alert without Monitoring = %+v", a)
	}
}
