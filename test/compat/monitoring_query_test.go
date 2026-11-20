//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// backfill writes points a minute apart over the last n minutes, one write
// per minute: gauge GAUGE (svc=a: i, svc=b: 10*i) and the cumulative
// counters GOOD (9 a minute) and TOTAL (10 a minute).
func backfill(t *testing.T, h *Harness, c *monitoring.MetricClient, gauge, good, total string, n int) time.Time {
	t.Helper()
	ctx := h.Context()
	for _, m := range []string{good, total} {
		if _, err := c.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/" + h.Project(), MetricDescriptor: &metricpb.MetricDescriptor{
			Type: m, MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64,
			Labels: []*label.LabelDescriptor{{Key: "svc"}}}}); err != nil {
			t.Fatalf("CreateMetricDescriptor: %v", err)
		}
		t.Cleanup(func() {
			_ = c.DeleteMetricDescriptor(context.Background(), &monitoringpb.DeleteMetricDescriptorRequest{Name: "projects/" + h.Project() + "/metricDescriptors/" + m})
		})
	}
	res := &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": h.Project()}}
	end := time.Now().Truncate(time.Second)
	start := end.Add(-time.Duration(n+1) * time.Minute)
	for i := 0; i <= n; i++ {
		at := timestamppb.New(end.Add(time.Duration(i-n) * time.Minute))
		var ts []*monitoringpb.TimeSeries
		for svc, k := range map[string]float64{"a": 1, "b": 10} {
			ts = append(ts, &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: gauge, Labels: map[string]string{"svc": svc}}, Resource: res,
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: at},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: k * float64(i)}}}}})
		}
		for m, per := range map[string]int64{good: 9, total: 10} {
			ts = append(ts, &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: m, Labels: map[string]string{"svc": "a"}}, Resource: res,
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: at},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: per * int64(i+1)}}}}})
		}
		if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + h.Project(), TimeSeries: ts}); err != nil {
			t.Fatalf("CreateTimeSeries %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		_ = c.DeleteMetricDescriptor(context.Background(), &monitoringpb.DeleteMetricDescriptorRequest{Name: "projects/" + h.Project() + "/metricDescriptors/" + gauge})
	})
	return end
}

// legacy is a metric type's legacy PromQL name.
func legacy(metricType string) string {
	s := strings.Replace(metricType, "/", ":", 1)
	return regexp.MustCompile(`[^a-zA-Z0-9_:]`).ReplaceAllString(s, "_")
}

// TestMonitoringPromQLAndMQL (#1966):
//   - ten minutes of a two-series gauge and two cumulative counters are
//     written with cloud.google.com/go/monitoring/apiv3/v2;
//   - the Prometheus HTTP API at /v1/projects/P/location/global/prometheus/api/v1/
//     is read raw (query by the legacy and the quoted UTF-8 name, query_range,
//     labels, label/NAME/values, series, a bad query) and through
//     github.com/prometheus/client_golang/api/prometheus/v1 (rate over the
//     counter);
//   - QueryService.QueryTimeSeries runs MQL: a sum, and a grouped max with a
//     condition; an unsupported table operation is UNIMPLEMENTED;
//   - a PromQL condition and an MQL condition each open an incident a local
//     webhook receives; a PromQL condition over a missing metric is refused
//     unless disableMetricValidation; renotifyInterval is kept and a
//     one-minute one refused.
//
// covers: google.monitoring.v3.QueryService/QueryTimeSeries, google.monitoring.v3.AlertPolicyService/CreateAlertPolicy, google.monitoring.v3.AlertPolicyService/GetAlertPolicy
func TestMonitoringPromQLAndMQL(t *testing.T) {
	h := New(t)
	opts := monitoringOpts(h)
	ctx := h.Context()
	mc := monitoringClient(t, h)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	gauge, good, total := "custom.googleapis.com/compat/lat_"+sfx, "custom.googleapis.com/compat/good_"+sfx, "custom.googleapis.com/compat/total_"+sfx
	end := backfill(t, h, mc, gauge, good, total, 10)

	base := "http://" + h.Endpoint(EnvMonitoring) + "/v1/projects/" + h.Project() + "/location/global/prometheus"
	get := func(path string, v url.Values) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(base + "/api/v1/" + path + "?" + v.Encode())
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %s", path, b)
		}
		return resp.StatusCode, m
	}
	at := fmt.Sprintf("%d", end.Unix())
	result := func(m map[string]any) []any { return m["data"].(map[string]any)["result"].([]any) }
	if code, m := get("query", url.Values{"query": {legacy(gauge) + `{svc="b"}`}, "time": {at}}); code != 200 ||
		len(result(m)) != 1 || result(m)[0].(map[string]any)["value"].([]any)[1] != "100" {
		t.Errorf("query by the legacy name = %d %v", code, m)
	}
	if code, m := get("query", url.Values{"query": {`sum({"` + gauge + `"})`}, "time": {at}}); code != 200 || result(m)[0].(map[string]any)["value"].([]any)[1] != "110" {
		t.Errorf("query by the UTF-8 name = %d %v", code, m)
	}
	if code, m := get("query_range", url.Values{"query": {legacy(gauge) + `{svc="a"}`}, "start": {fmt.Sprint(end.Add(-5 * time.Minute).Unix())}, "end": {at}, "step": {"60s"}}); code != 200 ||
		m["data"].(map[string]any)["resultType"] != "matrix" || len(result(m)[0].(map[string]any)["values"].([]any)) != 6 {
		t.Errorf("query_range = %d %v", code, m)
	}
	if code, m := get("labels", url.Values{"match[]": {legacy(gauge)}}); code != 200 || !strings.Contains(fmt.Sprint(m["data"]), "monitored_resource") || !strings.Contains(fmt.Sprint(m["data"]), "svc") {
		t.Errorf("labels = %d %v", code, m)
	}
	if code, m := get("label/svc/values", url.Values{"match[]": {legacy(gauge)}}); code != 200 || fmt.Sprint(m["data"]) != "[a b]" {
		t.Errorf("label values = %d %v", code, m)
	}
	if code, m := get("series", url.Values{"match[]": {legacy(gauge) + `{svc="a"}`}}); code != 200 || len(m["data"].([]any)) != 1 ||
		m["data"].([]any)[0].(map[string]any)["project_id"] != h.Project() {
		t.Errorf("series = %d %v", code, m)
	}
	if code, m := get("query", url.Values{"query": {"sum("}}); code != 400 || m["status"] != "error" || m["errorType"] != "bad_data" {
		t.Errorf("a bad query = %d %v", code, m)
	}

	pc, err := promapi.NewClient(promapi.Config{Address: base})
	if err != nil {
		t.Fatal(err)
	}
	papi := promv1.NewAPI(pc)
	v, _, _, err := papi.Query(ctx, "sum(rate("+legacy(total)+"[5m]))", end)
	if err != nil {
		t.Fatalf("client_golang Query: %v", err)
	}
	if vec, ok := v.(model.Vector); !ok || len(vec) != 1 || float64(vec[0].Value) < 0.16 || float64(vec[0].Value) > 0.17 {
		t.Errorf("rate of 10 a minute = %v", v)
	}
	if names, _, _, err := papi.LabelValues(ctx, "monitored_resource", []string{legacy(gauge)}, end.Add(-time.Hour), end); err != nil || len(names) != 1 || names[0] != "global" {
		t.Errorf("client_golang LabelValues = %v, %v", names, err)
	}

	qc, err := monitoring.NewQueryClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer qc.Close()
	rows := func(q string) ([]*monitoringpb.TimeSeriesData, error) {
		var out []*monitoringpb.TimeSeriesData
		it := qc.QueryTimeSeries(ctx, &monitoringpb.QueryTimeSeriesRequest{Name: "projects/" + h.Project(), Query: q})
		for {
			d, err := it.Next()
			if err == iterator.Done {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
	}
	if ds, err := rows("fetch global\n| metric '" + gauge + "'\n| group_by [], sum(val())\n| every 1m\n| within 5m"); err != nil || len(ds) != 1 ||
		ds[0].GetPointData()[0].GetValues()[0].GetDoubleValue() != 110 {
		t.Errorf("MQL sum = %v, %v", ds, err)
	}
	if ds, err := rows("fetch global::" + gauge + " | filter metric.svc == 'b' | group_by [metric.svc], max(val()) | every 1m | condition val() > 50"); err != nil || len(ds) != 1 ||
		!ds[0].GetPointData()[0].GetValues()[0].GetBoolValue() || ds[0].GetLabelValues()[0].GetStringValue() != "b" {
		t.Errorf("MQL condition = %v, %v", ds, err)
	}
	_, err = rows("fetch global::" + gauge + " | join")
	wantMonitoringCode(t, "an MQL join", err, codes.Unimplemented, "join")

	nc, err := monitoring.NewNotificationChannelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	parent := "projects/" + h.Project()
	hook := newWebhook(t)
	ch, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
		Type: "webhook_tokenauth", DisplayName: "query hook", Labels: map[string]string{"url": hook.srv.URL + "/q"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: ch.GetName(), Force: true})
	policy := func(name string, c *monitoringpb.AlertPolicy_Condition, renotify time.Duration) (*monitoringpb.AlertPolicy, error) {
		p := &monitoringpb.AlertPolicy{DisplayName: name, Conditions: []*monitoringpb.AlertPolicy_Condition{c}, NotificationChannels: []string{ch.GetName()}}
		if renotify > 0 {
			p.AlertStrategy = &monitoringpb.AlertPolicy_AlertStrategy{NotificationChannelStrategy: []*monitoringpb.AlertPolicy_AlertStrategy_NotificationChannelStrategy{{
				NotificationChannelNames: []string{ch.GetName()}, RenotifyInterval: durationpb.New(renotify)}}}
		}
		out, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: p})
		if err == nil {
			t.Cleanup(func() {
				_ = ac.DeleteAlertPolicy(context.Background(), &monitoringpb.DeleteAlertPolicyRequest{Name: out.GetName()})
			})
		}
		return out, err
	}
	prom := func(q string, disable bool) *monitoringpb.AlertPolicy_Condition {
		return &monitoringpb.AlertPolicy_Condition{DisplayName: "promql", Condition: &monitoringpb.AlertPolicy_Condition_ConditionPrometheusQueryLanguage{
			ConditionPrometheusQueryLanguage: &monitoringpb.AlertPolicy_Condition_PrometheusQueryLanguageCondition{Query: q, DisableMetricValidation: disable,
				EvaluationInterval: durationpb.New(30 * time.Second), Labels: map[string]string{"team": "web"}}}}
	}
	_, err = policy("missing", prom("compat_missing_metric_"+sfx+" > 1", false), 0)
	wantMonitoringCode(t, "a PromQL condition over a missing metric", err, codes.InvalidArgument, "disableMetricValidation")
	if _, err := policy("missing ok", prom("compat_missing_metric_"+sfx+" > 1", true), 0); err != nil {
		t.Errorf("disableMetricValidation: %v", err)
	}
	_, err = policy("bad", prom("sum(", true), 0)
	wantMonitoringCode(t, "a bad PromQL query", err, codes.InvalidArgument, "conditionPrometheusQueryLanguage.query")
	_, err = policy("one minute", prom(legacy(gauge)+" > 50", false), time.Minute)
	wantMonitoringCode(t, "a one-minute renotify interval", err, codes.InvalidArgument, "renotifyInterval")
	pp, err := policy("PromQL lat", prom(legacy(gauge)+` > 50`, false), 30*time.Minute)
	if err != nil {
		t.Fatalf("CreateAlertPolicy PromQL: %v", err)
	}
	if g, err := ac.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: pp.GetName()}); err != nil ||
		g.GetAlertStrategy().GetNotificationChannelStrategy()[0].GetRenotifyInterval().AsDuration() != 30*time.Minute ||
		g.GetConditions()[0].GetConditionPrometheusQueryLanguage().GetLabels()["team"] != "web" {
		t.Errorf("GetAlertPolicy PromQL = %v, %v", g, err)
	}
	if _, err := policy("MQL lat", &monitoringpb.AlertPolicy_Condition{DisplayName: "mql", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMonitoringQueryLanguage{
		ConditionMonitoringQueryLanguage: &monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition{
			Query: "fetch global::" + gauge + " | filter metric.svc == 'a' | every 1m | condition val() > 5", Duration: durationpb.New(0)}}}, 0); err != nil {
		t.Fatalf("CreateAlertPolicy MQL: %v", err)
	}
	// Keep the series current while the policies are evaluated.
	writeLatest := func() {
		for svc, v := range map[string]float64{"a": 10, "b": 100} {
			_ = mc.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: parent, TimeSeries: []*monitoringpb.TimeSeries{{
				Metric: &metricpb.Metric{Type: gauge, Labels: map[string]string{"svc": svc}}, Resource: &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": h.Project()}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.Now()}, Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}}}}}}})
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	var sawProm, sawMQL bool
	for time.Now().Before(deadline) && !(sawProm && sawMQL) {
		writeLatest()
		time.Sleep(6 * time.Second)
		hook.mu.Lock()
		for _, b := range hook.bodies {
			in, _ := b["incident"].(map[string]any)
			sawProm = sawProm || in["policy_name"] == "PromQL lat" && in["state"] == "open"
			sawMQL = sawMQL || in["policy_name"] == "MQL lat" && in["state"] == "open"
		}
		hook.mu.Unlock()
	}
	if !sawProm || !sawMQL {
		t.Errorf("open incidents reached the webhook: PromQL %v, MQL %v", sawProm, sawMQL)
	}
}

// TestMonitoringSLOCompliance (#1966): an SLO with a request-based
// goodTotalRatio SLI over two cumulative counters (9 good of 10 a minute)
// is read with ListTimeSeries' select_slo_health, select_slo_compliance,
// select_slo_budget, select_slo_budget_fraction, select_slo_burn_rate and
// select_slo_counts filters; a burn rate without a lookback is
// INVALID_ARGUMENT and an unknown SLO NOT_FOUND.
//
// covers: google.monitoring.v3.MetricService/ListTimeSeries, google.monitoring.v3.ServiceMonitoringService/CreateService, google.monitoring.v3.ServiceMonitoringService/CreateServiceLevelObjective
func TestMonitoringSLOCompliance(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	mc := monitoringClient(t, h)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	gauge, good, total := "custom.googleapis.com/compat/slat_"+sfx, "custom.googleapis.com/compat/sgood_"+sfx, "custom.googleapis.com/compat/stotal_"+sfx
	end := backfill(t, h, mc, gauge, good, total, 10)
	sc, err := monitoring.NewServiceMonitoringClient(ctx, monitoringOpts(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	svc, err := sc.CreateService(ctx, &monitoringpb.CreateServiceRequest{Parent: "projects/" + h.Project(), ServiceId: "slo-" + sfx, Service: &monitoringpb.Service{DisplayName: "checkout"}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	defer sc.DeleteService(ctx, &monitoringpb.DeleteServiceRequest{Name: svc.GetName()})
	slo, err := sc.CreateServiceLevelObjective(ctx, &monitoringpb.CreateServiceLevelObjectiveRequest{Parent: svc.GetName(), ServiceLevelObjectiveId: "avail",
		ServiceLevelObjective: &monitoringpb.ServiceLevelObjective{DisplayName: "80% good", Goal: 0.8,
			Period: &monitoringpb.ServiceLevelObjective_RollingPeriod{RollingPeriod: durationpb.New(24 * time.Hour)},
			ServiceLevelIndicator: &monitoringpb.ServiceLevelIndicator{Type: &monitoringpb.ServiceLevelIndicator_RequestBased{RequestBased: &monitoringpb.RequestBasedSli{
				Method: &monitoringpb.RequestBasedSli_GoodTotalRatio{GoodTotalRatio: &monitoringpb.TimeSeriesRatio{
					GoodServiceFilter: `metric.type = "` + good + `"`, TotalServiceFilter: `metric.type = "` + total + `"`}}}}}}})
	if err != nil {
		t.Fatalf("CreateServiceLevelObjective: %v", err)
	}
	read := func(filter string) ([]*monitoringpb.TimeSeries, error) {
		var out []*monitoringpb.TimeSeries
		it := mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + h.Project(), Filter: filter,
			Interval:    &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-3 * time.Minute)), EndTime: timestamppb.New(end)},
			Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MEAN}})
		for {
			ts, err := it.Next()
			if err == iterator.Done {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, ts)
		}
	}
	n := `"` + slo.GetName() + `"`
	near := func(a, b float64) bool { return a-b < 1e-6 && b-a < 1e-6 }
	for sel, want := range map[string]float64{
		"select_slo_health(" + n + ")":            0.9,
		"select_slo_compliance(" + n + ")":        0.9,
		"select_slo_budget(" + n + ")":            11, // 110 requests, 11 bad, a budget of 22
		"select_slo_budget_fraction(" + n + ")":   0.5,
		"select_slo_burn_rate(" + n + `, "300s")`: 0.5,
		"select_slo_counts(" + n + `)`:            9,
	} {
		ts, err := read(sel)
		if err != nil || len(ts) == 0 || len(ts[0].GetPoints()) == 0 || !near(ts[0].GetPoints()[0].GetValue().GetDoubleValue(), want) {
			t.Errorf("%s = %v, %v; want %g", sel, ts, err, want)
		}
	}
	_, err = read("select_slo_burn_rate(" + n + ")")
	wantMonitoringCode(t, "a burn rate without a lookback", err, codes.InvalidArgument, "lookback")
	_, err = read(`select_slo_health("` + svc.GetName() + `/serviceLevelObjectives/nope")`)
	wantMonitoringCode(t, "an unknown SLO", err, codes.NotFound, "not found")
}

var verificationCodeRE = regexp.MustCompile(`G-[0-9]{6}`)

// TestMonitoringPubSubChannelAndVerification (#1966):
//   - a pubsub notification channel publishes a threshold policy's open
//     incident, in Google's 1.2 schema, to a topic of the local Pub/Sub,
//     which cloud.google.com/go/pubsub/v2 pulls;
//   - an email channel is sent a verification code (recorded, read from
//     the console's Deliveries), verified with VerifyNotificationChannel;
//     GetNotificationChannelVerificationCode then gives a code that
//     verifies a second channel with the same address; a wrong code is
//     INVALID_ARGUMENT and a code for an unverified channel
//     FAILED_PRECONDITION.
//
// covers: google.monitoring.v3.NotificationChannelService/GetNotificationChannelVerificationCode, google.monitoring.v3.NotificationChannelService/VerifyNotificationChannel, google.monitoring.v3.NotificationChannelService/SendNotificationChannelVerificationCode, google.monitoring.v3.NotificationChannelService/CreateNotificationChannel
func TestMonitoringPubSubChannelAndVerification(t *testing.T) {
	h := New(t)
	pubsubAddr := h.Endpoint(EnvPubSub)
	consoleAt := consoleAddr(t, h)
	opts := monitoringOpts(h)
	ctx := h.Context()
	mc := monitoringClient(t, h)
	nc, err := monitoring.NewNotificationChannelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	parent := "projects/" + h.Project()
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)

	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	ps, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	topic, sub := parent+"/topics/alerts-"+sfx, parent+"/subscriptions/alerts-"+sfx
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	defer ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	defer ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	pch, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
		Type: "pubsub", DisplayName: "alerts topic", Labels: map[string]string{"topic": topic}}})
	if err != nil {
		t.Fatalf("CreateNotificationChannel pubsub: %v", err)
	}
	defer nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: pch.GetName(), Force: true})
	metric := "custom.googleapis.com/compat/backlog_" + sfx
	writeGauge(t, h, mc, metric, 500)
	p, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "Backlog " + sfx, Combiner: monitoringpb.AlertPolicy_OR, NotificationChannels: []string{pch.GetName()},
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "backlog high", Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{
			ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{Filter: `metric.type = "` + metric + `" AND resource.type = "global"`,
				Comparison: monitoringpb.ComparisonType_COMPARISON_GT, ThresholdValue: 100, Duration: durationpb.New(0)}}}}}})
	if err != nil {
		t.Fatalf("CreateAlertPolicy: %v", err)
	}
	defer ac.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: p.GetName()})
	var incident map[string]any
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && incident == nil; {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		cancel()
		if err != nil && pctx.Err() == nil {
			t.Fatalf("Pull: %v", err)
		}
		for _, m := range resp.GetReceivedMessages() {
			var body map[string]any
			if json.Unmarshal(m.GetMessage().GetData(), &body) == nil && body["version"] == "1.2" {
				incident, _ = body["incident"].(map[string]any)
			}
			_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m.GetAckId()}})
		}
	}
	if incident == nil || incident["state"] != "open" || incident["policy_name"] != "Backlog "+sfx || incident["observed_value"] != "500" {
		t.Errorf("the Pub/Sub notification = %v", incident)
	}

	mail := func(name string) *monitoringpb.NotificationChannel {
		c, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
			Type: "email", DisplayName: name, Labels: map[string]string{"email_address": "verify-" + sfx + "@example.com"}}})
		if err != nil {
			t.Fatalf("CreateNotificationChannel email: %v", err)
		}
		t.Cleanup(func() {
			_ = nc.DeleteNotificationChannel(context.Background(), &monitoringpb.DeleteNotificationChannelRequest{Name: c.GetName(), Force: true})
		})
		return c
	}
	first, second := mail("first"), mail("second")
	_, err = nc.GetNotificationChannelVerificationCode(ctx, &monitoringpb.GetNotificationChannelVerificationCodeRequest{Name: first.GetName()})
	wantMonitoringCode(t, "a code for an unverified channel", err, codes.FailedPrecondition, "not verified")
	if err := nc.SendNotificationChannelVerificationCode(ctx, &monitoringpb.SendNotificationChannelVerificationCodeRequest{Name: first.GetName()}); err != nil {
		t.Fatalf("SendNotificationChannelVerificationCode: %v", err)
	}
	q := url.Values{"project": {h.Project()}, "name": {first.GetName()[strings.LastIndex(first.GetName(), "/")+1:]}}
	code, body := consoleDo(t, consoleAt, http.MethodGet, "/api/detail/monitoring-channels?"+q.Encode(), "")
	sent := verificationCodeRE.FindString(body)
	if code != http.StatusOK || sent == "" {
		t.Fatalf("the console's Deliveries show no verification code: %d %s", code, body)
	}
	_, err = nc.VerifyNotificationChannel(ctx, &monitoringpb.VerifyNotificationChannelRequest{Name: first.GetName(), Code: "G-000000x"})
	wantMonitoringCode(t, "a wrong code", err, codes.InvalidArgument, "verification code")
	if v, err := nc.VerifyNotificationChannel(ctx, &monitoringpb.VerifyNotificationChannelRequest{Name: first.GetName(), Code: sent}); err != nil ||
		v.GetVerificationStatus() != monitoringpb.NotificationChannel_VERIFIED {
		t.Fatalf("VerifyNotificationChannel = %v, %v", v, err)
	}
	got, err := nc.GetNotificationChannelVerificationCode(ctx, &monitoringpb.GetNotificationChannelVerificationCodeRequest{Name: first.GetName(),
		ExpireTime: timestamppb.New(time.Now().Add(10 * time.Minute))})
	if err != nil || got.GetCode() == "" || got.GetExpireTime().AsTime().After(time.Now().Add(11*time.Minute)) {
		t.Fatalf("GetNotificationChannelVerificationCode = %v, %v", got, err)
	}
	if v, err := nc.VerifyNotificationChannel(ctx, &monitoringpb.VerifyNotificationChannelRequest{Name: second.GetName(), Code: got.GetCode()}); err != nil ||
		v.GetVerificationStatus() != monitoringpb.NotificationChannel_VERIFIED {
		t.Errorf("verifying an equivalent channel with the copied code = %v, %v", v, err)
	}
}
