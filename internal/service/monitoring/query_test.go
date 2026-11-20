package monitoring

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedQueries writes, over the ten minutes before *now: a gauge
// custom.googleapis.com/test/lat for svc=a (value i) and svc=b (10*i), and
// cumulative request counters good (9 per minute) and total (10 per minute).
func seedQueries(t *testing.T, s *Server, now *time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, m := range []string{"custom.googleapis.com/test/good", "custom.googleapis.com/test/total"} {
		if _, err := s.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/p1", MetricDescriptor: &metricpb.MetricDescriptor{
			Type: m, MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64,
			Labels: []*label.LabelDescriptor{{Key: "svc"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	end := *now
	start := end.Add(-11 * time.Minute)
	for i := 0; i <= 10; i++ {
		at := end.Add(time.Duration(i-10) * time.Minute)
		var ts []*monitoringpb.TimeSeries
		for svc, k := range map[string]float64{"a": 1, "b": 10} {
			ts = append(ts, &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: "custom.googleapis.com/test/lat", Labels: map[string]string{"svc": svc}},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "p1"}}, Points: []*monitoringpb.Point{gaugePoint(at, k*float64(i))}})
		}
		for m, per := range map[string]int64{"good": 9, "total": 10} {
			ts = append(ts, &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: "custom.googleapis.com/test/" + m, Labels: map[string]string{"svc": "a"}},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "p1"}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: per * int64(i+1)}}}}})
		}
		if _, err := s.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/p1", TimeSeries: ts}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}

func TestPromQLOverLocalSeries(t *testing.T) {
	s, now := newTestServer(t, "")
	seedQueries(t, s, now)
	ctx := context.Background()
	for q, want := range map[string]float64{
		`custom_googleapis_com:test_lat{svc="b"}`:                          100,
		`{"custom.googleapis.com/test/lat", svc="a"}`:                      10,
		`sum(custom_googleapis_com:test_lat{monitored_resource="global"})`: 110,
		`sum(rate(custom_googleapis_com:test_total[5m]))`:                  10.0 / 60,
	} {
		res, err := s.PromInstant(ctx, "p1", q, *now)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		v := promResultJSON(res.Value)["result"].([]map[string]any)
		if len(v) != 1 {
			t.Fatalf("%s: %v", q, v)
		}
		got := v[0]["value"].([]any)[1].(string)
		if f, _ := strconv.ParseFloat(got, 64); math.Abs(f-want) > 1e-6 {
			t.Errorf("%s = %s, want %g", q, got, want)
		}
	}
	if legacyPromName("kubernetes.io/container/cpu/limit_cores") != "kubernetes_io:container_cpu_limit_cores" ||
		legacyPromName("prometheus.googleapis.com/up/gauge") != "up" {
		t.Error("legacy names")
	}

	// The HTTP API.
	h := NewRESTHandler(s)
	get := func(path string, v url.Values) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodGet, "/v1/projects/p1/location/global/prometheus/api/v1/"+path+"?"+v.Encode(), nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}
	code, m := get("query_range", url.Values{"query": {`custom_googleapis_com:test_lat{svc="a"}`}, "start": {formatF(float64(now.Add(-5 * time.Minute).Unix()))},
		"end": {formatF(float64(now.Unix()))}, "step": {"60"}})
	if code != 200 || len(m["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["values"].([]any)) != 6 {
		t.Errorf("query_range %d %v", code, m)
	}
	code, m = get("labels", url.Values{"match[]": {`custom_googleapis_com:test_lat`}, "end": {formatF(float64(now.Unix()))}})
	if code != 200 || !strings.Contains(toJSON(m), `"svc"`) || !strings.Contains(toJSON(m), `"monitored_resource"`) {
		t.Errorf("labels %d %v", code, m)
	}
	code, m = get("label/svc/values", url.Values{"end": {formatF(float64(now.Unix()))}})
	if code != 200 || toJSON(m["data"]) != `["a","b"]` {
		t.Errorf("label values %d %v", code, m)
	}
	code, m = get("series", url.Values{"match[]": {`custom_googleapis_com:test_lat{svc="a"}`}, "end": {formatF(float64(now.Unix()))}})
	if code != 200 || len(m["data"].([]any)) != 1 {
		t.Errorf("series %d %v", code, m)
	}
	code, m = get("query", url.Values{"query": {"sum("}})
	if code != 400 || m["errorType"] != "bad_data" {
		t.Errorf("a bad query %d %v", code, m)
	}
	code, _ = get("series", nil)
	if code != 400 {
		t.Errorf("series without match[] %d", code)
	}
}

func formatF(f float64) string { b, _ := json.Marshal(f); return string(b) }
func toJSON(v any) string      { b, _ := json.Marshal(v); return string(b) }

func TestPromQLAndMQLConditions(t *testing.T) {
	s, now := newTestServer(t, "")
	seedQueries(t, s, now)
	ctx := context.Background()
	h := &hook{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ch, err := s.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/p1",
		NotificationChannel: &monitoringpb.NotificationChannel{Type: "webhook_tokenauth", Labels: map[string]string{"url": srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(c *monitoringpb.AlertPolicy_Condition) (*monitoringpb.AlertPolicy, error) {
		return s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/p1", AlertPolicy: &monitoringpb.AlertPolicy{
			DisplayName: "x", Conditions: []*monitoringpb.AlertPolicy_Condition{c}, NotificationChannels: []string{ch.GetName()},
			AlertStrategy: &monitoringpb.AlertPolicy_AlertStrategy{NotificationChannelStrategy: []*monitoringpb.AlertPolicy_AlertStrategy_NotificationChannelStrategy{{
				NotificationChannelNames: []string{ch.GetName()}, RenotifyInterval: durationpb.New(30 * time.Minute)}}}}})
	}
	prom := func(q string, disable bool) *monitoringpb.AlertPolicy_Condition {
		return &monitoringpb.AlertPolicy_Condition{DisplayName: "p", Condition: &monitoringpb.AlertPolicy_Condition_ConditionPrometheusQueryLanguage{
			ConditionPrometheusQueryLanguage: &monitoringpb.AlertPolicy_Condition_PrometheusQueryLanguageCondition{Query: q, DisableMetricValidation: disable,
				Labels: map[string]string{"severity": "page"}}}}
	}
	if _, err := mk(prom("sum(", false)); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a bad PromQL query: %v", err)
	}
	if _, err := mk(prom("nope_metric > 1", false)); codeOf(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "disableMetricValidation") {
		t.Errorf("an unknown metric: %v", err)
	}
	if _, err := mk(prom("nope_metric > 1", true)); err != nil {
		t.Errorf("disableMetricValidation: %v", err)
	}
	if _, err := mk(prom(`custom_googleapis_com:test_lat > 50`, false)); err != nil {
		t.Fatal(err)
	}
	mql := &monitoringpb.AlertPolicy_Condition{DisplayName: "m", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMonitoringQueryLanguage{
		ConditionMonitoringQueryLanguage: &monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition{
			Query: "fetch global::custom.googleapis.com/test/lat | filter metric.svc == 'a' | every 1m | condition val() > 5", Duration: durationpb.New(0)}}}
	if _, err := mk(mql); err != nil {
		t.Fatal(err)
	}
	bad := &monitoringpb.AlertPolicy_Condition{DisplayName: "m", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMonitoringQueryLanguage{
		ConditionMonitoringQueryLanguage: &monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition{Query: "fetch global::custom.googleapis.com/test/lat"}}}
	if _, err := mk(bad); codeOf(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "boolean") {
		t.Errorf("an MQL query without a condition: %v", err)
	}
	s.Evaluate(ctx)
	ins := s.Incidents("p1")
	var promOpen, mqlOpen bool
	for _, in := range ins {
		promOpen = promOpen || strings.Contains(in.Summary, "PromQL") && in.MetricLabels["svc"] == "b" && in.MetricLabels["severity"] == "page"
		mqlOpen = mqlOpen || strings.Contains(in.Summary, "MQL")
	}
	if !promOpen || !mqlOpen || len(ins) != 2 {
		t.Fatalf("incidents %+v", ins)
	}
	sent := len(h.states())
	// renotifyInterval: 30 minutes on, each open incident is notified again.
	*now = now.Add(31 * time.Minute)
	s.renotify(ctx, *now)
	if got := len(h.states()); got != sent+2 {
		t.Errorf("after the renotify interval %d notifications, want %d", got, sent+2)
	}
	s.renotify(ctx, now.Add(time.Minute))
	if got := len(h.states()); got != sent+2 {
		t.Errorf("renotified again within the interval: %d", got)
	}
	if _, err := s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/p1", AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "x", Conditions: []*monitoringpb.AlertPolicy_Condition{mql}, NotificationChannels: []string{ch.GetName()},
		AlertStrategy: &monitoringpb.AlertPolicy_AlertStrategy{NotificationChannelStrategy: []*monitoringpb.AlertPolicy_AlertStrategy_NotificationChannelStrategy{{
			NotificationChannelNames: []string{ch.GetName()}, RenotifyInterval: durationpb.New(time.Minute)}}}}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a one-minute renotify interval: %v", err)
	}
}

func TestQueryTimeSeriesMQL(t *testing.T) {
	s, now := newTestServer(t, "")
	seedQueries(t, s, now)
	ctx := context.Background()
	resp, err := s.QueryTimeSeries(ctx, &monitoringpb.QueryTimeSeriesRequest{Name: "projects/p1",
		Query: "fetch global\n| metric 'custom.googleapis.com/test/lat'\n| filter metric.svc =~ 'a|b'\n| group_by [], sum(val())\n| every 1m\n| within 5m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTimeSeriesData()) != 1 || resp.GetTimeSeriesData()[0].GetPointData()[0].GetValues()[0].GetDoubleValue() != 110 {
		t.Errorf("sum: %v", resp)
	}
	resp, err = s.QueryTimeSeries(ctx, &monitoringpb.QueryTimeSeriesRequest{Name: "projects/p1",
		Query: "fetch global::custom.googleapis.com/test/lat | group_by [metric.svc], max(val()) | every 1m | condition val() > 50"})
	if err != nil || len(resp.GetTimeSeriesData()) != 2 || resp.GetTimeSeriesDescriptor().GetLabelDescriptors()[0].GetKey() != "metric.svc" {
		t.Fatalf("grouped: %v %v", resp, err)
	}
	for _, d := range resp.GetTimeSeriesData() {
		want := d.GetLabelValues()[0].GetStringValue() == "b"
		if d.GetPointData()[0].GetValues()[0].GetBoolValue() != want {
			t.Errorf("condition for %v", d)
		}
	}
	if _, err := s.QueryTimeSeries(ctx, &monitoringpb.QueryTimeSeriesRequest{Name: "projects/p1", Query: "fetch global::custom.googleapis.com/test/lat | join"}); codeOf(err) != codes.Unimplemented {
		t.Errorf("join: %v", err)
	}
	if _, err := s.QueryTimeSeries(ctx, &monitoringpb.QueryTimeSeriesRequest{Name: "projects/p1", Query: "metric x"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("no fetch: %v", err)
	}
}

func TestSLOSelectors(t *testing.T) {
	s, now := newTestServer(t, "")
	seedQueries(t, s, now)
	ctx := context.Background()
	if _, err := s.CreateService(ctx, &monitoringpb.CreateServiceRequest{Parent: "projects/p1", ServiceId: "web", Service: &monitoringpb.Service{DisplayName: "web"}}); err != nil {
		t.Fatal(err)
	}
	slo, err := s.CreateServiceLevelObjective(ctx, &monitoringpb.CreateServiceLevelObjectiveRequest{Parent: "projects/p1/services/web", ServiceLevelObjectiveId: "avail",
		ServiceLevelObjective: &monitoringpb.ServiceLevelObjective{Goal: 0.8, Period: &monitoringpb.ServiceLevelObjective_RollingPeriod{RollingPeriod: durationpb.New(24 * time.Hour)},
			ServiceLevelIndicator: &monitoringpb.ServiceLevelIndicator{Type: &monitoringpb.ServiceLevelIndicator_RequestBased{RequestBased: &monitoringpb.RequestBasedSli{
				Method: &monitoringpb.RequestBasedSli_GoodTotalRatio{GoodTotalRatio: &monitoringpb.TimeSeriesRatio{
					GoodServiceFilter:  `metric.type = "custom.googleapis.com/test/good"`,
					TotalServiceFilter: `metric.type = "custom.googleapis.com/test/total"`}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(sel string) []*monitoringpb.TimeSeries {
		t.Helper()
		resp, err := s.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: sel,
			Interval:    &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-3 * time.Minute)), EndTime: timestamppb.New(*now)},
			Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MEAN}})
		if err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return resp.GetTimeSeries()
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	n := `"` + slo.GetName() + `"`
	if ts := read("select_slo_health(" + n + ")"); len(ts) != 1 || !near(ts[0].GetPoints()[0].GetValue().GetDoubleValue(), 0.9) {
		t.Errorf("health %v", ts)
	}
	if ts := read("select_slo_compliance(" + n + ")"); len(ts) != 1 || !near(ts[0].GetPoints()[0].GetValue().GetDoubleValue(), 0.9) {
		t.Errorf("compliance %v", ts)
	}
	// 110 requests in the rolling day (the first point counts from its start
	// time), 11 bad: the budget is 0.2*110 = 22, 11 left, half of it.
	if ts := read("select_slo_budget(" + n + ")"); len(ts) != 1 || !near(ts[0].GetPoints()[0].GetValue().GetDoubleValue(), 11) {
		t.Errorf("budget %v", ts)
	}
	if ts := read("select_slo_budget_fraction(" + n + ")"); len(ts) != 1 || !near(ts[0].GetPoints()[0].GetValue().GetDoubleValue(), 0.5) {
		t.Errorf("budget fraction %v", ts)
	}
	if ts := read("select_slo_burn_rate(" + n + `, "300s")`); len(ts) != 1 || !near(ts[0].GetPoints()[0].GetValue().GetDoubleValue(), 0.5) {
		t.Errorf("burn rate %v", ts)
	}
	if ts := read("select_slo_counts(" + n + ")"); len(ts) != 2 || ts[0].GetMetric().GetLabels()["event_type"] != "good" || ts[0].GetPoints()[0].GetValue().GetDoubleValue() != 9 {
		t.Errorf("counts %v", ts)
	}
	_, err = s.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `select_slo_burn_rate(` + n + `)`,
		Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(*now)}})
	if codeOf(err) != codes.InvalidArgument {
		t.Errorf("burn rate without a lookback: %v", err)
	}
}

func codeOf(err error) codes.Code { return status.Code(err) }
