//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestMonitoringPromQLThroughThePage (#1991): Metrics explorer's PromQL
// tab charts a query over points written with the official client, one
// line and one table row per series; an aggregation charts one.
func TestMonitoringPromQLThroughThePage(t *testing.T) {
	needService(t, "monitoring-explorer")
	opts := monitoringOpts(t)
	ctx := context.Background()
	c, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	project := uniqueProject(t)
	metricType := "custom.googleapis.com/browser/promql_depth"
	writeTwoSeries(t, c, project, metricType)

	p := open(t)
	p.navigate("/monitoring/metrics-explorer?project=" + project)
	p.waitFor(`document.querySelector("#explorer-tab-promql") !== null`)
	p.clickText("#view [role=tab]", "PromQL")
	p.waitFor(`document.querySelector("#explorer-tab-promql").getAttribute("aria-selected") === "true" && !document.querySelector("#explorer-panel-promql").hidden`)
	p.setField("#explorer-promql", `{"`+metricType+`"}`)
	p.clickText("#view .explorer-builder button", "Run query")
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 2`)
	p.waitFor(`document.querySelectorAll("#view svg.explorer-chart polyline").length === 2`)
	var rows string
	p.eval(`[...document.querySelectorAll("#view .explorer-table tbody tr")].map((r) => r.textContent).join("|")`, &rows)
	if !strings.Contains(rows, "env=prod") || !strings.Contains(rows, "env=dev") {
		t.Errorf("the PromQL table = %q, want a row for env=prod and env=dev", rows)
	}
	// The legacy name, summed: one series of 4 + 3 at the newest point.
	p.setField("#explorer-promql", "sum(custom_googleapis_com:browser_promql_depth)")
	p.clickText("#view .explorer-builder button", "Run query")
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 1`)
	var last string
	p.eval(`document.querySelector("#view .explorer-table tbody tr td:last-child").textContent`, &last)
	if last != "27" {
		t.Errorf("sum()'s latest value = %q, want 27", last)
	}
	// The query is in the address, so a reload keeps the tab and the chart.
	var href string
	p.eval(`location.search`, &href)
	if !strings.Contains(href, "language=promql") {
		t.Errorf("the address after Run query = %q", href)
	}
	p.navigate("/monitoring/metrics-explorer" + href)
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 1`)
}

// writeTwoSeries writes env=prod and env=dev of a gauge, a minute apart:
// prod 4 then 14, dev 3 then 13.
func writeTwoSeries(t *testing.T, c *monitoring.MetricClient, project, metricType string) {
	t.Helper()
	now := time.Now()
	for i, at := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		var series []*monitoringpb.TimeSeries
		for _, env := range []string{"prod", "dev"} {
			series = append(series, &monitoringpb.TimeSeries{
				Metric:   &metricpb.Metric{Type: metricType, Labels: map[string]string{"env": env}},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: float64(10*i + len(env))}}}},
			})
		}
		if err := c.CreateTimeSeries(context.Background(), &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: series}); err != nil {
			t.Fatalf("write the points: %v", err)
		}
	}
}

// TestMonitoringPromQLAlertAndRenotifyThroughThePage (#1991): Create
// policy with a PromQL condition and Repeat notifications every 30
// minutes, read back with the official client; the policy's page shows
// both, and its Repeat notifications action changes the interval, read
// back again.
func TestMonitoringPromQLAlertAndRenotifyThroughThePage(t *testing.T) {
	needService(t, "monitoring-alerting")
	opts := monitoringOpts(t)
	ctx := context.Background()
	mc, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	nc, err := monitoring.NewNotificationChannelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	project := uniqueProject(t)
	ch, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/" + project,
		NotificationChannel: &monitoringpb.NotificationChannel{Type: "email", DisplayName: "Ops", Labels: map[string]string{"email_address": "ops@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	chID := ch.GetName()[strings.LastIndex(ch.GetName(), "/")+1:]
	metricType := "custom.googleapis.com/browser/promql_backlog"
	writeBrowserGauge(t, mc, project, metricType, 1)

	p := open(t)
	p.navigate("/monitoring/alerting?project=" + project)
	p.clickText("#view button", "Create policy")
	query := `{"` + metricType + `"} > 5`
	p.submitForm(map[string]string{"displayName": "Backlog by PromQL", "conditionType": "promql", "conditionName": "PromQL backlog",
		"promqlQuery": query, "evaluationInterval": "60", "duration": "120", "channels": chID, "renotify": "30"}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Backlog by PromQL"))`)
	var policy *monitoringpb.AlertPolicy
	it := ac.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{Name: "projects/" + project})
	for {
		pol, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pol.GetDisplayName() == "Backlog by PromQL" {
			policy = pol
		}
	}
	if policy == nil {
		t.Fatal("the policy Create policy made is not listed by the official client")
	}
	pq := policy.GetConditions()[0].GetConditionPrometheusQueryLanguage()
	if pq.GetQuery() != query || pq.GetEvaluationInterval().AsDuration() != time.Minute || pq.GetDuration().AsDuration() != 2*time.Minute {
		t.Errorf("the PromQL condition Create policy made = %v", pq)
	}
	renotify := func(pol *monitoringpb.AlertPolicy) time.Duration {
		st := pol.GetAlertStrategy().GetNotificationChannelStrategy()
		if len(st) != 1 || len(st[0].GetNotificationChannelNames()) != 1 || st[0].GetNotificationChannelNames()[0] != ch.GetName() {
			t.Errorf("the notification channel strategy = %v", st)
			return 0
		}
		return st[0].GetRenotifyInterval().AsDuration()
	}
	if d := renotify(policy); d != 30*time.Minute {
		t.Errorf("renotifyInterval = %s, want 30m", d)
	}

	id := policy.GetName()[strings.LastIndex(policy.GetName(), "/")+1:]
	p.navigate("/monitoring/alerting/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Every 30 minutes to ` + chID + `")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("PromQL ` + strings.ReplaceAll(query, `"`, `\"`) + `")`)
	p.clickText("#view button", "Repeat notifications")
	p.submitForm(map[string]string{"renotify": "90"}, "renotify")
	p.waitFor(`document.querySelector("#view").textContent.includes("Every 90 minutes to ` + chID + `")`)
	got, err := ac.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: policy.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if d := renotify(got); d != 90*time.Minute {
		t.Errorf("renotifyInterval after Repeat notifications = %s, want 1h30m", d)
	}
	if err := ac.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: policy.GetName()}); err != nil {
		t.Error(err)
	}
}

// TestMonitoringSLOComplianceThroughThePage (#1991): request counters
// written with the official client, 9 good of every 10, and an SLO of 80%
// made with the official client; the service's page shows the SLO
// Healthy at 90% compliance, half its error budget left, burning at 0.5,
// and its Error budget chart.
func TestMonitoringSLOComplianceThroughThePage(t *testing.T) {
	needService(t, "monitoring-services")
	project := uniqueProject(t)
	svc := seedBrowserSLO(t, project)

	p := open(t)
	p.navigate("/monitoring/services/" + svc + "?project=" + project)
	row := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("Availability"))`
	p.waitFor(row + ` !== undefined`)
	var text string
	p.eval(row+`.textContent`, &text)
	for _, want := range []string{"Healthy", "80%", "90%", "50%", "0.5"} {
		if !strings.Contains(text, want) {
			t.Errorf("the SLO's row = %q, want %q in it", text, want)
		}
	}
	p.clickText("#view [role=tab]", "Error budget")
	p.waitFor(`document.querySelector("#view [role=tabpanel]:not([hidden]) svg") !== null`)
}

// seedBrowserSLO writes request counters (good 9 and total 10 a minute,
// over ten minutes) and a service "checkout" with an 80% availability SLO
// over a rolling day, all with the official clients.
func seedBrowserSLO(t *testing.T, project string) string {
	t.Helper()
	opts := monitoringOpts(t)
	ctx := context.Background()
	mc, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	sc, err := monitoring.NewServiceMonitoringClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	for _, m := range []string{"good", "total"} {
		if _, err := mc.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/" + project, MetricDescriptor: &metricpb.MetricDescriptor{
			Type: "custom.googleapis.com/browser/req_" + m, MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	start := now.Add(-11 * time.Minute)
	for i := 0; i <= 10; i++ {
		at := now.Add(time.Duration(i-10) * time.Minute)
		var ts []*monitoringpb.TimeSeries
		for m, per := range map[string]int64{"good": 9, "total": 10} {
			ts = append(ts, &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: "custom.googleapis.com/browser/req_" + m},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: per * int64(i+1)}}}}})
		}
		if err := mc.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: ts}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	sv, err := sc.CreateService(ctx, &monitoringpb.CreateServiceRequest{Parent: "projects/" + project, ServiceId: "checkout",
		Service: &monitoringpb.Service{DisplayName: "Checkout", Identifier: &monitoringpb.Service_Custom_{Custom: &monitoringpb.Service_Custom{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.CreateServiceLevelObjective(ctx, &monitoringpb.CreateServiceLevelObjectiveRequest{Parent: sv.GetName(), ServiceLevelObjectiveId: "avail",
		ServiceLevelObjective: &monitoringpb.ServiceLevelObjective{DisplayName: "Availability", Goal: 0.8,
			Period: &monitoringpb.ServiceLevelObjective_RollingPeriod{RollingPeriod: durationpb.New(24 * time.Hour)},
			ServiceLevelIndicator: &monitoringpb.ServiceLevelIndicator{Type: &monitoringpb.ServiceLevelIndicator_RequestBased{RequestBased: &monitoringpb.RequestBasedSli{
				Method: &monitoringpb.RequestBasedSli_GoodTotalRatio{GoodTotalRatio: &monitoringpb.TimeSeriesRatio{
					GoodServiceFilter:  `metric.type = "custom.googleapis.com/browser/req_good"`,
					TotalServiceFilter: `metric.type = "custom.googleapis.com/browser/req_total"`}}}}}}}); err != nil {
		t.Fatal(err)
	}
	return sv.GetName()[strings.LastIndex(sv.GetName(), "/")+1:]
}

// TestMonitoringPromQLSLOAndRenotifyScreensMeetWCAG (#1991): Metrics
// explorer's PromQL tab with a chart, Create policy with the PromQL
// condition shown, a policy's page with repeat notifications and its
// Repeat notifications dialog, and a service's page with its SLO
// compliance and Error budget chart, through the console's WCAG 2.1 AA
// audit.
func TestMonitoringPromQLSLOAndRenotifyScreensMeetWCAG(t *testing.T) {
	needService(t, "monitoring-alerting")
	opts := monitoringOpts(t)
	ctx := context.Background()
	mc, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	nc, err := monitoring.NewNotificationChannelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	project := uniqueProject(t)
	metricType := "custom.googleapis.com/browser/a11y_depth"
	writeTwoSeries(t, mc, project, metricType)
	svc := seedBrowserSLO(t, project)
	ch, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/" + project,
		NotificationChannel: &monitoringpb.NotificationChannel{Type: "email", DisplayName: "Ops", Labels: map[string]string{"email_address": "ops@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/" + project, AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "A11y PromQL", Combiner: monitoringpb.AlertPolicy_OR, NotificationChannels: []string{ch.GetName()},
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "Depth", Condition: &monitoringpb.AlertPolicy_Condition_ConditionPrometheusQueryLanguage{
			ConditionPrometheusQueryLanguage: &monitoringpb.AlertPolicy_Condition_PrometheusQueryLanguageCondition{Query: `{"` + metricType + `"} > 100`}}}},
		AlertStrategy: &monitoringpb.AlertPolicy_AlertStrategy{NotificationChannelStrategy: []*monitoringpb.AlertPolicy_AlertStrategy_NotificationChannelStrategy{{
			NotificationChannelNames: []string{ch.GetName()}, RenotifyInterval: durationpb.New(time.Hour)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	pid := pol.GetName()[strings.LastIndex(pol.GetName(), "/")+1:]
	q := "?project=" + project
	explorer := fmt.Sprintf("/monitoring/metrics-explorer%s&language=promql&promql=%s", q, `sum(custom_googleapis_com:browser_a11y_depth)`)
	auditScreens(t, []a11yPage{
		{"Metrics explorer PromQL", explorer, settled + ` && document.querySelector("#view svg.explorer-chart") !== null`, ""},
		{"Create policy, PromQL condition", "/monitoring/alerting/create" + q, settled + ` && document.querySelector("#f-conditionType") !== null`,
			`(() => { const f = document.querySelector("#f-conditionType"); f.value = "promql"; f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`},
		{"alert policy page", "/monitoring/alerting/" + pid + q, settled + ` && document.querySelector("#view").textContent.includes("Every 60 minutes")`, ""},
		{"Repeat notifications dialog", "/monitoring/alerting/" + pid + q, settled + ` && document.querySelector("#view").textContent.includes("Every 60 minutes")`,
			`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Repeat notifications").click()`},
		{"service page with SLO compliance", "/monitoring/services/" + svc + q, settled + ` && document.querySelector("#view").textContent.includes("Healthy")`, ""},
		{"service Error budget tab", "/monitoring/services/" + svc + q + "&tab=error-budget", settled + ` && document.querySelector("#view svg") !== null`, ""},
	})
}
