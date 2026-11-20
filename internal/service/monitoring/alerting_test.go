package monitoring

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type hook struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	h.mu.Lock()
	h.bodies = append(h.bodies, m)
	h.mu.Unlock()
}

func (h *hook) states() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, b := range h.bodies {
		out = append(out, b["incident"].(map[string]any)["state"].(string))
	}
	return out
}

// TestThresholdPolicyOpensAndClosesIncidents: a policy over a gauge opens
// an incident when a point is above the threshold for the duration, posts
// it to a local webhook, closes it when the value drops, and records an
// email channel's notification without sending it.
func TestThresholdPolicyOpensAndClosesIncidents(t *testing.T) {
	ctx := context.Background()
	s, now := newTestServer(t, filepath.Join(t.TempDir(), "m.json"))
	h := &hook{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hookCh, err := s.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/p1",
		NotificationChannel: &monitoringpb.NotificationChannel{Type: "webhook_tokenauth", DisplayName: "hook", Labels: map[string]string{"url": srv.URL + "/hook?token=x"}}})
	if err != nil {
		t.Fatal(err)
	}
	mail, err := s.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/p1",
		NotificationChannel: &monitoringpb.NotificationChannel{Type: "email", Labels: map[string]string{"email_address": "a@example.com"}}})
	if err != nil || mail.GetVerificationStatus() != monitoringpb.NotificationChannel_UNVERIFIED {
		t.Fatalf("email channel %v, %v", mail, err)
	}
	const m = "custom.googleapis.com/test/queue"
	p, err := s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/p1", AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "Queue too deep", Combiner: monitoringpb.AlertPolicy_OR, NotificationChannels: []string{hookCh.GetName(), mail.GetName()},
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "depth > 10", Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{
			ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{Filter: `metric.type = "` + m + `" AND resource.type = "global"`,
				Comparison: monitoringpb.ComparisonType_COMPARISON_GT, ThresholdValue: 10, Duration: durationpb.New(time.Minute),
				Aggregations: []*monitoringpb.Aggregation{{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MAX}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.GetEnabled().GetValue() || p.GetConditions()[0].GetName() == "" || p.GetCreationRecord() == nil {
		t.Fatalf("created policy %v", p)
	}
	for i := 0; i < 3; i++ {
		if err := write(s, tseries(m, nil, gaugePoint(*now, 20))); err != nil {
			t.Fatal(err)
		}
		s.Evaluate(ctx)
		*now = now.Add(time.Minute)
	}
	in := s.Incidents("p1")
	if len(in) != 1 || in[0].State != IncidentOpen || in[0].Observed != "20" {
		t.Fatalf("incidents %+v", in)
	}
	if got := h.states(); len(got) != 1 || got[0] != "open" {
		t.Fatalf("webhook got %v", got)
	}
	if err := write(s, tseries(m, nil, gaugePoint(*now, 1))); err != nil {
		t.Fatal(err)
	}
	s.Evaluate(ctx)
	if in := s.Incidents("p1"); in[0].State != IncidentClosed {
		t.Fatalf("after the drop %+v", in)
	}
	if got := h.states(); len(got) != 2 || got[1] != "closed" {
		t.Fatalf("webhook got %v", got)
	}
	var recorded int
	for _, d := range s.Deliveries("p1") {
		if d.Type == "email" && d.State == DeliveryRecorded {
			recorded++
		}
	}
	if recorded != 2 {
		t.Errorf("email deliveries recorded = %d; want 2 (never sent)", recorded)
	}
	// State survives a restart.
	s2, err := NewServer(s.state)
	if err != nil || len(s2.Incidents("p1")) != 1 {
		t.Fatalf("reloaded: %v", err)
	}
	// A channel in use is refused without force.
	_, err = s.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: hookCh.GetName()})
	wantCode(t, err, codes.FailedPrecondition, "referenced")
}

func TestAbsenceAndSnooze(t *testing.T) {
	ctx := context.Background()
	s, now := newTestServer(t, "")
	const m = "custom.googleapis.com/test/heartbeat"
	if err := write(s, tseries(m, nil, gaugePoint(*now, 1))); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/p1", AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "absent", Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "short", Condition: &monitoringpb.AlertPolicy_Condition_ConditionAbsent{
			ConditionAbsent: &monitoringpb.AlertPolicy_Condition_MetricAbsence{Filter: `metric.type = "` + m + `"`, Duration: durationpb.New(time.Minute)}}}}}})
	wantCode(t, err, codes.InvalidArgument, "120 seconds")
	p, err := s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/p1", AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "absent", Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "no heartbeat", Condition: &monitoringpb.AlertPolicy_Condition_ConditionAbsent{
			ConditionAbsent: &monitoringpb.AlertPolicy_Condition_MetricAbsence{Filter: `metric.type = "` + m + `"`, Duration: durationpb.New(3 * time.Minute)}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := s.CreateSnooze(ctx, &monitoringpb.CreateSnoozeRequest{Parent: "projects/p1", Snooze: &monitoringpb.Snooze{DisplayName: "quiet",
		Criteria: &monitoringpb.Snooze_Criteria{Policies: []string{p.GetName()}},
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(*now), EndTime: timestamppb.New(now.Add(5 * time.Minute))}}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(4 * time.Minute)
	s.Evaluate(ctx)
	if len(s.Incidents("p1")) != 0 {
		t.Fatalf("a snoozed policy opened %+v", s.Incidents("p1"))
	}
	sn.Interval.EndTime = timestamppb.New(now.Add(time.Second))
	if _, err := s.UpdateSnooze(ctx, &monitoringpb.UpdateSnoozeRequest{Snooze: sn, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"interval.end_time"}}}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	s.Evaluate(ctx)
	if in := s.Incidents("p1"); len(in) != 1 || in[0].State != IncidentOpen {
		t.Fatalf("absence incidents %+v", in)
	}
	if err := write(s, tseries(m, nil, gaugePoint(*now, 1))); err != nil {
		t.Fatal(err)
	}
	s.Evaluate(ctx)
	if in := s.Incidents("p1"); in[0].State != IncidentClosed {
		t.Fatalf("data returned, incidents %+v", in)
	}
}

func TestRefusedConditions(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t, "")
	mk := func(c *monitoringpb.AlertPolicy_Condition, n int) error {
		var cs []*monitoringpb.AlertPolicy_Condition
		for i := 0; i < n; i++ {
			cs = append(cs, c)
		}
		_, err := s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/p1", AlertPolicy: &monitoringpb.AlertPolicy{DisplayName: "x", Conditions: cs}})
		return err
	}
	mql := &monitoringpb.AlertPolicy_Condition{DisplayName: "m", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMonitoringQueryLanguage{
		ConditionMonitoringQueryLanguage: &monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition{Query: "fetch x"}}}
	wantCode(t, mk(mql, 1), codes.InvalidArgument, "not valid MQL")
	th := &monitoringpb.AlertPolicy_Condition{DisplayName: "t", Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{
		ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{Filter: `metric.type = "custom.googleapis.com/x"`, Comparison: monitoringpb.ComparisonType_COMPARISON_GT}}}
	wantCode(t, mk(th, 2), codes.InvalidArgument, "combiner")
	wantCode(t, mk(th, 7), codes.InvalidArgument, "at most 6")
	lg := &monitoringpb.AlertPolicy_Condition{DisplayName: "l", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMatchedLog{
		ConditionMatchedLog: &monitoringpb.AlertPolicy_Condition_LogMatch{Filter: "severity>=ERROR"}}}
	wantCode(t, mk(lg, 1), codes.InvalidArgument, "notificationRateLimit")
}

// TestDeltaAndRateFillEmptyPeriods: ALIGN_DELTA and ALIGN_RATE give a
// period with no point an interpolated value, and the period after the gap
// keeps only its share.
func TestDeltaAndRateFillEmptyPeriods(t *testing.T) {
	ctx := context.Background()
	s, now := newTestServer(t, "")
	const m = "custom.googleapis.com/test/requests"
	start := now.Add(-10 * time.Minute)
	cum := func(end time.Time, v int64) *monitoringpb.TimeSeries {
		return &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: m}, Resource: &monitoredres.MonitoredResource{Type: "global"},
			MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64,
			Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
				Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}}}}}
	}
	*now = start.Add(time.Minute)
	if err := write(s, cum(*now, 10)); err != nil {
		t.Fatal(err)
	}
	// Nothing for three minutes, then 40 more.
	*now = now.Add(4 * time.Minute)
	if err := write(s, cum(*now, 50)); err != nil {
		t.Fatal(err)
	}
	resp, err := s.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `metric.type = "` + m + `"`,
		Interval:    &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-4 * time.Minute)), EndTime: timestamppb.New(*now)},
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_DELTA}})
	if err != nil {
		t.Fatal(err)
	}
	pts := resp.GetTimeSeries()[0].GetPoints()
	if len(pts) != 4 {
		t.Fatalf("%d points; want the 4 one-minute periods, the 3 empty ones filled", len(pts))
	}
	var sum int64
	for _, p := range pts {
		if v := p.GetValue().GetInt64Value(); v != 10 {
			t.Errorf("point at %s = %d; want 10 (40 over 4 minutes)", p.GetInterval().GetEndTime().AsTime(), v)
		}
		sum += p.GetValue().GetInt64Value()
	}
	if sum != 40 {
		t.Errorf("the deltas sum to %d; want 40", sum)
	}
}

func TestRequestMetricsPerProject(t *testing.T) {
	src := &fakeRequests{}
	now := t0
	r := NewRequestBuiltin(src, "default-p", func() time.Time { return now })
	src.counts = []RequestCount{{Service: "kms", Method: "GetKeyRing", Code: "OK", Project: "p1", Count: 2, Buckets: make([]uint64, 1)},
		{Service: "kms", Method: "GetKeyRing", Code: "OK", Project: "p2", Count: 5, Buckets: make([]uint64, 1)},
		{Service: "kms", Method: "ListKeyRings", Code: "OK", Count: 1, Buckets: make([]uint64, 1)}}
	now = now.Add(time.Minute)
	count := func(project string) int64 {
		ts, err := r.Series(project, requestCount, t0, now)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, s := range ts {
			if s.GetResource().GetLabels()["project_id"] != project {
				t.Errorf("series of %s labelled %s", project, s.GetResource().GetLabels()["project_id"])
			}
			for _, p := range s.GetPoints() {
				n += p.GetValue().GetInt64Value()
			}
		}
		return n
	}
	if a, b, d := count("p1"), count("p2"), count("default-p"); a != 2 || b != 5 || d != 1 {
		t.Errorf("per project %d, %d, %d; want 2, 5 and the unattributed 1 in the default project", a, b, d)
	}
}

type fakeRequests struct{ counts []RequestCount }

func (f *fakeRequests) Snapshot() []RequestCount  { return f.counts }
func (f *fakeRequests) Bounds() []float64         { return []float64{1} }
func (f *fakeRequests) API(service string) string { return service + ".googleapis.com" }

func TestDashboardsEtagAndUptime(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t, "")
	d, err := s.CreateDashboard(ctx, &dashboardpb.CreateDashboardRequest{Parent: "projects/p1", Dashboard: &dashboardpb.Dashboard{DisplayName: "ops",
		Layout: &dashboardpb.Dashboard_GridLayout{GridLayout: &dashboardpb.GridLayout{Widgets: []*dashboardpb.Widget{{Title: "t",
			Content: &dashboardpb.Widget_Text{Text: &dashboardpb.Text{Content: "hi"}}}}}}}})
	if err != nil || d.GetEtag() == "" {
		t.Fatalf("%v, %v", d, err)
	}
	stale := d.GetEtag()
	d.DisplayName = "ops 2"
	if d, err = s.UpdateDashboard(ctx, &dashboardpb.UpdateDashboardRequest{Dashboard: d}); err != nil || d.GetEtag() == stale {
		t.Fatalf("update %v, %v", d, err)
	}
	d.Etag = stale
	_, err = s.UpdateDashboard(ctx, &dashboardpb.UpdateDashboardRequest{Dashboard: d})
	wantCode(t, err, codes.Aborted, "etag")

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; _, _ = io.WriteString(w, "ok healthy") }))
	defer srv.Close()
	host, port := hostPort(t, srv.URL)
	c, err := s.CreateUptimeCheckConfig(ctx, &monitoringpb.CreateUptimeCheckConfigRequest{Parent: "projects/p1", UptimeCheckConfig: &monitoringpb.UptimeCheckConfig{
		DisplayName: "Health page", Timeout: durationpb.New(5 * time.Second),
		Resource:         &monitoringpb.UptimeCheckConfig_MonitoredResource{MonitoredResource: &monitoredres.MonitoredResource{Type: "uptime_url", Labels: map[string]string{"host": host}}},
		CheckRequestType: &monitoringpb.UptimeCheckConfig_HttpCheck_{HttpCheck: &monitoringpb.UptimeCheckConfig_HttpCheck{Port: port, Path: "/healthz"}},
		ContentMatchers:  []*monitoringpb.UptimeCheckConfig_ContentMatcher{{Content: "healthy"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s.RunUptimeChecks(ctx)
	if rs := s.UptimeResults(c.GetName()); hits != 1 || len(rs) != 1 || !rs[0].Passed {
		t.Fatalf("results %+v, hits %d", rs, hits)
	}
	resp, err := s.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `metric.type = "` + uptimePassed + `"`,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(t0.Add(-time.Minute)), EndTime: timestamppb.New(t0.Add(time.Minute))}})
	if err != nil || len(resp.GetTimeSeries()) != 1 || !resp.GetTimeSeries()[0].GetPoints()[0].GetValue().GetBoolValue() {
		t.Fatalf("check_passed %v, %v", resp, err)
	}
	// Not a local host: never sent.
	r := runCheck(ctx, &monitoringpb.UptimeCheckConfig{Timeout: durationpb.New(time.Second),
		CheckRequestType: &monitoringpb.UptimeCheckConfig_HttpCheck_{HttpCheck: &monitoringpb.UptimeCheckConfig_HttpCheck{Port: 80, Path: "/"}}}, "example.com")
	if r.Passed || r.Error == "" {
		t.Errorf("a non-local check %+v", r)
	}
}

func hostPort(t *testing.T, u string) (string, int32) {
	t.Helper()
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(pu.Port())
	return pu.Hostname(), int32(p)
}
