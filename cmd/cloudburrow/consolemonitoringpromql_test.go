package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// Metrics explorer's PromQL tab (#1991): Run query charts a range query
// over the local series, one row per result series with its points.
func TestConsoleMonitoringExplorerPromQL(t *testing.T) {
	ctx := context.Background()
	_, screen, srv := monitoringAdminFixture(t)
	explorer := monitoringExplorerProvider{screen}
	const project = "mon-promql"
	now := time.Now()
	for i, at := range []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute)} {
		var series []*monitoringpb.TimeSeries
		for _, env := range []string{"prod", "dev"} {
			series = append(series, &monitoringpb.TimeSeries{
				Metric:   &metricpb.Metric{Type: "custom.googleapis.com/shop/queue", Labels: map[string]string{"env": env}},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: float64(10*i + len(env))}}}},
			})
		}
		if _, err := srv.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: series}); err != nil {
			t.Fatal(err)
		}
	}
	if label, fields := explorer.QueryForm([]string{"promql"}); label != "Run query" || len(fields) == 0 || fields[0].Name != "query" {
		t.Errorf("the PromQL tab's form = %q %v", label, fields)
	}
	l, err := explorer.Build(ctx, project, []string{"promql"}, map[string]string{"query": `{"custom.googleapis.com/shop/queue"}`, "range": "900"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 2 {
		t.Fatalf("the query's series = %v", l.Items)
	}
	prod, ok := monRow(l, "")
	for _, it := range l.Items {
		if it.Fields["env"] == "prod" {
			prod, ok = it, true
		}
	}
	if !ok || prod.Fields["Value"] != "14" {
		t.Errorf("env=prod's row = %v", prod.Fields)
	}
	var pts []explorerPoint
	if err := json.Unmarshal([]byte(prod.Fields["points"]), &pts); err != nil || len(pts) < 2 {
		t.Errorf("env=prod's points = %s (%v)", prod.Fields["points"], err)
	}
	// An aggregation: one series.
	l, err = explorer.Build(ctx, project, []string{"promql"}, map[string]string{"query": `sum(custom_googleapis_com:shop_queue)`, "range": "900", "step": "60"})
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Value"] != "27" {
		t.Errorf("sum() = %v, %v", l.Items, err)
	}
	if _, err := explorer.Build(ctx, project, []string{"promql"}, map[string]string{"query": "sum("}); err == nil {
		t.Error("a query that does not parse ran")
	}
	if _, err := explorer.Build(ctx, project, []string{"promql"}, map[string]string{"query": " "}); err == nil {
		t.Error("an empty query ran")
	}
}

// Create policy's PromQL condition and Repeat notifications (#1991):
// saved as conditionPrometheusQueryLanguage and a notificationChannelStrategy,
// shown on the policy's page, and changed with its Repeat notifications
// action.
func TestConsoleMonitoringPromQLPolicyAndRenotify(t *testing.T) {
	ctx := context.Background()
	admin, _, srv := monitoringAdminFixture(t)
	const project = "mon-promql-alerts"
	policies, channels := monitoringAlertingProvider{admin}, monitoringChannelsProvider{admin}
	writeGauge(t, srv, project, "custom.googleapis.com/shop/backlog", 1, time.Now())
	ch, err := channels.Create(ctx, project, map[string]string{"type": "email", "displayName": "Ops", "email_address": "ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := policies.Create(ctx, project, map[string]string{"displayName": "Backlog (PromQL)", "conditionType": "promql",
		"conditionName": "Backlog above 5", "promqlQuery": `{"custom.googleapis.com/shop/backlog"} > 5`, "evaluationInterval": "60",
		"duration": "120", "channels": ch, "renotify": "45"})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := srv.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + id})
	if err != nil {
		t.Fatal(err)
	}
	pq := pol.GetConditions()[0].GetConditionPrometheusQueryLanguage()
	if pq.GetQuery() != `{"custom.googleapis.com/shop/backlog"} > 5` || pq.GetEvaluationInterval().AsDuration() != time.Minute || pq.GetDuration().AsDuration() != 2*time.Minute {
		t.Errorf("the PromQL condition = %v", pq)
	}
	st := pol.GetAlertStrategy().GetNotificationChannelStrategy()
	if len(st) != 1 || st[0].GetRenotifyInterval().AsDuration() != 45*time.Minute || len(st[0].GetNotificationChannelNames()) != 1 {
		t.Errorf("the notification channel strategy = %v", st)
	}
	d, err := policies.Detail(ctx, project, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if v := detailValue(d, "Repeat notifications"); v != "Every 45 minutes to "+ch {
		t.Errorf("Repeat notifications = %q", v)
	}
	if c := d.Sections[0].Listing.Items[0].Fields["Condition"]; !strings.Contains(c, "PromQL") || !strings.Contains(c, "> 5") {
		t.Errorf("the condition's summary = %q", c)
	}
	var act *console.Action
	for _, a := range policies.DetailActions(ctx, project, []string{id}) {
		if a.ID == "renotify" {
			act = &a
		}
	}
	if act == nil || act.Fields[0].Default != "45" {
		t.Fatalf("the Repeat notifications action = %v", act)
	}
	if err := policies.ActAt(ctx, project, []string{id}, "renotify", map[string]string{"renotify": "10", "etag": act.Fields[1].Default}); err == nil {
		t.Error("Repeat notifications took 10 minutes, under Google's 30")
	}
	if err := policies.ActAt(ctx, project, []string{id}, "renotify", map[string]string{"renotify": "120", "etag": act.Fields[1].Default}); err != nil {
		t.Fatal(err)
	}
	d, _ = policies.Detail(ctx, project, []string{id})
	if v := detailValue(d, "Repeat notifications"); v != "Every 120 minutes to "+ch {
		t.Errorf("Repeat notifications after the change = %q", v)
	}
	if err := policies.ActAt(ctx, project, []string{id}, "renotify", map[string]string{"renotify": ""}); err != nil {
		t.Fatal(err)
	}
	d, _ = policies.Detail(ctx, project, []string{id})
	if v := detailValue(d, "Repeat notifications"); v != "Not repeated" {
		t.Errorf("Repeat notifications once removed = %q", v)
	}
	if _, err := policies.Create(ctx, project, map[string]string{"displayName": "No query", "conditionType": "promql"}); err == nil {
		t.Error("Create policy took a PromQL condition without a query")
	}
	if _, err := policies.Create(ctx, project, map[string]string{"displayName": "Renotify alone", "conditionType": "threshold",
		"metric": "custom.googleapis.com/shop/backlog", "comparison": "COMPARISON_GT", "renotify": "30"}); err == nil {
		t.Error("Create policy took repeat notifications without a channel")
	}
}

func detailValue(d console.Detail, label string) string {
	for _, p := range d.Summary {
		if p.Label == label {
			return p.Value
		}
	}
	return ""
}

// A service's page (#1991): each SLO's status, compliance, error budget
// and burn rate, computed from its indicator, and the charts of both.
func TestConsoleMonitoringSLOCompliance(t *testing.T) {
	ctx := context.Background()
	admin, _, srv := monitoringAdminFixture(t)
	const project = "mon-slo"
	seedRequests(t, srv, project, time.Now())
	services := monitoringServicesProvider{admin}
	sid, err := services.Create(ctx, project, map[string]string{"id": "checkout", "displayName": "Checkout"})
	if err != nil {
		t.Fatal(err)
	}
	if err := services.ActAt(ctx, project, []string{sid}, "create-slo", map[string]string{"id": "avail", "goal": "80", "days": "1",
		"good": `metric.type="custom.googleapis.com/req/good"`, "total": `metric.type="custom.googleapis.com/req/total"`}); err != nil {
		t.Fatal(err)
	}
	d, err := services.Detail(ctx, project, []string{sid})
	if err != nil {
		t.Fatal(err)
	}
	row := d.Sections[0].Listing.Items[0]
	// 9 good of every 10: 90% compliance against 80%; 10% bad of a 20%
	// budget leaves half, burning at 0.5.
	if row.Fields["Compliance"] != "90%" || row.Fields["Error budget"] != "50%" || row.Fields["Burn rate (1 h)"] != "0.5" || row.Fields["Status"] != "Healthy" {
		t.Errorf("the SLO's row = %v", row.Fields)
	}
	if len(d.Sections) != 3 || d.Sections[1].Kind != console.KindChart || d.Sections[2].ID != "error-budget" || len(d.Sections[2].Series) != 1 {
		t.Fatalf("the service's sections = %+v", d.Sections)
	}
	var latest *float64
	for _, p := range d.Sections[1].Series[0].Points {
		if p.Value != nil {
			latest = p.Value
		}
	}
	if latest == nil || *latest < 89.99 || *latest > 90.01 {
		t.Errorf("the compliance chart's latest point = %v", latest)
	}

	// Raise the goal past the compliance: out of SLO, the budget spent.
	if err := services.ActAt(ctx, project, []string{sid}, "create-slo", map[string]string{"id": "strict", "goal": "95", "days": "1",
		"good": `metric.type="custom.googleapis.com/req/good"`, "total": `metric.type="custom.googleapis.com/req/total"`}); err != nil {
		t.Fatal(err)
	}
	d, _ = services.Detail(ctx, project, []string{sid})
	strict, _ := monRow(d.Sections[0].Listing, "strict")
	if strict.Fields["Status"] != "Out of SLO" || strict.Fields["Error budget"] != "-100%" || strict.Status != "unhealthy" {
		t.Errorf("the strict SLO's row = %v (%s)", strict.Fields, strict.Status)
	}
}

// seedRequests writes cumulative request counters over the ten minutes
// before now: good 9 and total 10 a minute.
func seedRequests(t *testing.T, s *monitoring.Server, project string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, m := range []string{"good", "total"} {
		if _, err := s.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/" + project, MetricDescriptor: &metricpb.MetricDescriptor{
			Type: "custom.googleapis.com/req/" + m, MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64}}); err != nil {
			t.Fatal(err)
		}
	}
	start := now.Add(-11 * time.Minute)
	for i := 0; i <= 10; i++ {
		at := now.Add(time.Duration(i-10) * time.Minute)
		var ts []*monitoringpb.TimeSeries
		for m, per := range map[string]int64{"good": 9, "total": 10} {
			ts = append(ts, &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: "custom.googleapis.com/req/" + m},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: per * int64(i+1)}}}}})
		}
		if _, err := s.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: ts}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}
