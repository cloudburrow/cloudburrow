package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

func monitoringAdminFixture(t *testing.T) (monitoringAdmin, monitoringScreen, *monitoring.Server) {
	t.Helper()
	s, err := monitoring.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	return monitoringAdmin{api: func() *monitoring.Server { return s }},
		monitoringScreen{api: func() monitoringpb.MetricServiceServer { return s }}, s
}

func writeGauge(t *testing.T, s *monitoring.Server, project, metric string, v float64, at time.Time) {
	t.Helper()
	_, err := s.CreateTimeSeries(context.Background(), &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: []*monitoringpb.TimeSeries{{
		Metric:   &metricpb.Metric{Type: metric},
		Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
		Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(at)},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
}

func monRow(l console.Listing, name string) (console.Resource, bool) {
	for _, it := range l.Items {
		if it.Name == name {
			return it, true
		}
	}
	return console.Resource{}, false
}

// Alerting's pages (#1964): Create channel (a local webhook), Create policy
// with a threshold condition naming it, the policy opening an incident when
// the series crosses the threshold, the webhook receiving Google's payload,
// Acknowledge and Close on the incident, Disable, Snooze and Delete.
func TestConsoleMonitoringAlertingChannelsAndIncidents(t *testing.T) {
	ctx := context.Background()
	admin, _, srv := monitoringAdminFixture(t)
	const project = "mon-alerts"
	policies, incidents, channels := monitoringAlertingProvider{admin}, monitoringIncidentsProvider{admin}, monitoringChannelsProvider{admin}

	var mu sync.Mutex
	var bodies []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
	}))
	defer hook.Close()

	ch, err := channels.Create(ctx, project, map[string]string{"type": "webhook_tokenauth", "displayName": "Local hook", "url": hook.URL + "/alerts"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channels.Create(ctx, project, map[string]string{"type": "webhook_tokenauth", "displayName": "No URL"}); err == nil {
		t.Error("Create channel took a webhook without a URL")
	}
	email, err := channels.Create(ctx, project, map[string]string{"type": "email", "displayName": "Ops", "email_address": "ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := channels.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := monRow(cl, ch); !ok || row.Fields["Type"] != "webhook_tokenauth" || !strings.HasSuffix(row.Fields["Target"], "/alerts") {
		t.Fatalf("Notification channels' row for %s = %v", ch, row)
	}
	if row, _ := monRow(cl, email); row.Fields["Verification"] != "Unverified" {
		t.Errorf("the email channel's verification = %q", row.Fields["Verification"])
	}
	// Send verification code records the code in the channel's deliveries;
	// Verify takes it back.
	if err := channels.ActAt(ctx, project, []string{email}, "send-code", nil); err != nil {
		t.Fatal(err)
	}
	ed, err := channels.Detail(ctx, project, []string{email})
	if err != nil {
		t.Fatal(err)
	}
	code := ""
	for _, it := range ed.Sections[0].Listing.Items {
		if i := strings.Index(it.Fields["Detail"], "G-"); i >= 0 {
			code = it.Fields["Detail"][i : i+8]
		}
	}
	if code == "" {
		t.Fatalf("the email channel's deliveries name no code: %v", ed.Sections[0].Listing.Items)
	}
	if err := channels.ActAt(ctx, project, []string{email}, "verify", map[string]string{"code": code}); err != nil {
		t.Fatal(err)
	}

	const metric = "custom.googleapis.com/shop/queue"
	writeGauge(t, srv, project, metric, 1, time.Now().Add(-10*time.Second))
	id, err := policies.Create(ctx, project, map[string]string{"displayName": "Queue too long", "conditionType": "threshold",
		"conditionName": "Queue above 5", "metric": metric, "resource": "global", "comparison": "COMPARISON_GT", "threshold": "5",
		"duration": "0", "channels": ch + ", " + email, "severity": "WARNING"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policies.Create(ctx, project, map[string]string{"displayName": "Bad", "conditionType": "absence", "metric": metric, "duration": "30"}); err == nil {
		t.Error("Create policy took an absence of 30 seconds")
	}
	pl, err := policies.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := monRow(pl, id); !ok || row.Fields["Enabled"] != "Enabled" || row.Fields["Conditions"] != "Queue above 5" {
		t.Fatalf("Alerting's row = %v", row)
	}

	writeGauge(t, srv, project, metric, 9, time.Now())
	srv.Evaluate(ctx)
	il, err := incidents.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(il.Items) != 1 || il.Items[0].Status != "open" || il.Items[0].Fields["Policy"] != "Queue too long" {
		t.Fatalf("Incidents = %v", il.Items)
	}
	inc := il.Items[0]
	if acts := incidents.Actions(inc); len(acts) != 2 {
		t.Errorf("an open incident's actions = %v", acts)
	}
	mu.Lock()
	got := len(bodies)
	var state any
	if got > 0 {
		state = bodies[0]["incident"].(map[string]any)["state"]
	}
	mu.Unlock()
	if got != 1 || state != "open" {
		t.Fatalf("the webhook received %d notifications (state %v)", got, state)
	}
	idet, err := incidents.Detail(ctx, project, []string{inc.Name})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(idet.Sections[0].Listing.Items); n != 2 {
		t.Errorf("the incident's notifications = %v; want the webhook's and the email's", idet.Sections[0].Listing.Items)
	}
	if err := incidents.Act(ctx, project, inc.Name, "acknowledge"); err != nil {
		t.Fatal(err)
	}
	if err := incidents.Act(ctx, project, inc.Name, "close"); err != nil {
		t.Fatal(err)
	}
	il, _ = incidents.List(ctx, project)
	if il.Items[0].Status != "closed" {
		t.Errorf("after Close the incident is %q", il.Items[0].Status)
	}

	pd, err := policies.Detail(ctx, project, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if pd.Title != "Queue too long" || len(pd.Sections) != 4 || len(pd.Sections[1].Listing.Items) != 1 {
		t.Errorf("the policy's page = %+v", pd)
	}
	if err := policies.Act(ctx, project, id, "disable"); err != nil {
		t.Fatal(err)
	}
	pl, _ = policies.List(ctx, project)
	if row, _ := monRow(pl, id); row.Fields["Enabled"] != "Disabled" {
		t.Errorf("after Disable the policy is %q", row.Fields["Enabled"])
	}
	if err := policies.ActAt(ctx, project, []string{id}, "snooze", map[string]string{"displayName": "Maintenance", "minutes": "30"}); err != nil {
		t.Fatal(err)
	}
	snoozes := monitoringSnoozesProvider{admin}
	sl, err := snoozes.List(ctx, project)
	if err != nil || len(sl.Items) != 1 || sl.Items[0].Status != "active" {
		t.Fatalf("Snoozes = %v, %v", sl.Items, err)
	}
	if err := snoozes.Act(ctx, project, sl.Items[0].Name, "end"); err != nil {
		t.Fatal(err)
	}
	if err := policies.Delete(ctx, project, id); err != nil {
		t.Fatal(err)
	}
	if err := channels.Delete(ctx, project, ch); err != nil {
		t.Fatal(err)
	}
	if pl, _ = policies.List(ctx, project); len(pl.Items) != 0 {
		t.Errorf("after Delete policy Alerting lists %v", pl.Items)
	}
}

// Uptime checks, Dashboards, Groups and Services (#1964): an uptime check
// of a local server passes and is charted, a dashboard created from JSON
// draws its XY chart's series, a group lists its members, and a service
// takes an SLO.
func TestConsoleMonitoringUptimeDashboardsGroupsServices(t *testing.T) {
	ctx := context.Background()
	admin, screen, srv := monitoringAdminFixture(t)
	const project = "mon-uptime"

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "all good") }))
	defer site.Close()
	port := site.URL[strings.LastIndex(site.URL, ":")+1:]
	uptime := monitoringUptimeProvider{admin}
	id, err := uptime.Create(ctx, project, map[string]string{"displayName": "Home page", "protocol": "HTTP", "host": "127.0.0.1",
		"port": port, "path": "/", "period": "60", "timeout": "5", "content": "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uptime.Create(ctx, project, map[string]string{"displayName": "Bad", "host": "127.0.0.1", "period": "30", "timeout": "5"}); err == nil {
		t.Error("Create uptime check took a 30 second period")
	}
	srv.RunUptimeChecks(ctx)
	ul, err := uptime.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := monRow(ul, id); !ok || row.Status != "passed" || row.Fields["Target"] != "127.0.0.1:"+port+"/" {
		t.Fatalf("Uptime checks' row = %v", row)
	}
	ud, err := uptime.Detail(ctx, project, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(ud.Sections[0].Listing.Items) != 1 || len(ud.Sections[1].Series[0].Points) != 1 || ud.Edit == nil {
		t.Errorf("the check's page = %+v", ud)
	}
	if err := uptime.Edit(ctx, project, []string{id}, map[string]string{"displayName": "Home", "port": port, "path": "/health", "period": "300", "timeout": "5"}); err != nil {
		t.Fatal(err)
	}
	ul, _ = uptime.List(ctx, project)
	if row, _ := monRow(ul, id); row.Fields["Display name"] != "Home" || row.Fields["Period"] != "5m0s" {
		t.Errorf("after Edit the row = %v", row.Fields)
	}

	const metric = "custom.googleapis.com/shop/sales"
	writeGauge(t, srv, project, metric, 7, time.Now())
	dash := monitoringDashboardsProvider{monitoringAdmin: admin, metrics: screen}
	did, err := dash.Create(ctx, project, map[string]string{"displayName": "Shop",
		"json": `{"gridLayout":{"widgets":[{"title":"Sales","xyChart":{"dataSets":[{"timeSeriesQuery":{"timeSeriesFilter":{"filter":"metric.type=\"` +
			strings.ReplaceAll(metric, "/", "/") + `\""}}}]}},{"title":"Note","text":{"content":"hello"}}]}}`})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := dash.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := monRow(dl, did); !ok || row.Fields["Widgets"] != "2" || row.Fields["Layout"] != "Grid" {
		t.Fatalf("Dashboards' row = %v", row)
	}
	dd, err := dash.Detail(ctx, project, []string{did})
	if err != nil {
		t.Fatal(err)
	}
	var chart *console.Section
	for i := range dd.Sections {
		if dd.Sections[i].Kind == console.KindChart {
			chart = &dd.Sections[i]
		}
	}
	if chart == nil || chart.Label != "Sales" || len(chart.Series) != 1 || *chart.Series[0].Points[0].Value != 7 {
		t.Fatalf("the dashboard's chart = %+v", chart)
	}
	if err := dash.Edit(ctx, project, []string{did}, map[string]string{"json": `{"displayName":"Shop 2","mosaicLayout":{"columns":12}}`}); err != nil {
		t.Fatal(err)
	}
	if dl, _ = dash.List(ctx, project); dl.Items[0].Fields["Display name"] != "Shop 2" {
		t.Errorf("after Edit JSON the dashboard = %v", dl.Items[0].Fields)
	}
	if err := dash.Delete(ctx, project, did); err != nil {
		t.Fatal(err)
	}

	groups := monitoringGroupsProvider{admin}
	gid, err := groups.Create(ctx, project, map[string]string{"displayName": "Global", "filter": `resource.type = "global"`})
	if err != nil {
		t.Fatal(err)
	}
	gd, err := groups.Detail(ctx, project, []string{gid})
	if err != nil {
		t.Fatal(err)
	}
	if len(gd.Sections[0].Listing.Items) == 0 || gd.Sections[0].Listing.Items[0].Fields["Resource type"] != "global" {
		t.Errorf("the group's members = %v", gd.Sections[0].Listing.Items)
	}
	if err := groups.Delete(ctx, project, gid); err != nil {
		t.Fatal(err)
	}

	services := monitoringServicesProvider{admin}
	sid, err := services.Create(ctx, project, map[string]string{"id": "checkout", "displayName": "Checkout"})
	if err != nil {
		t.Fatal(err)
	}
	if err := services.ActAt(ctx, project, []string{sid}, "create-slo", map[string]string{"id": "avail", "goal": "99.5", "days": "28",
		"good": `metric.type="custom.googleapis.com/ok"`, "total": `metric.type="custom.googleapis.com/all"`}); err != nil {
		t.Fatal(err)
	}
	sd, err := services.Detail(ctx, project, []string{sid})
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.Sections[0].Listing.Items) != 1 || sd.Sections[0].Listing.Items[0].Fields["Goal"] != "99.5%" {
		t.Errorf("the service's SLOs = %v", sd.Sections[0].Listing.Items)
	}
	if err := services.ActAt(ctx, project, []string{sid}, "delete-slo:avail", nil); err != nil {
		t.Fatal(err)
	}
	if err := services.Delete(ctx, project, sid); err != nil {
		t.Fatal(err)
	}
	if err := uptime.Delete(ctx, project, id); err != nil {
		t.Fatal(err)
	}
}
