//go:build browser

package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	dashboard "cloud.google.com/go/monitoring/dashboard/apiv1"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// envMonitoring is the Cloud Monitoring API v3 endpoint (#1939), gRPC and
// JSON on one port, as the compat harness names it.
const envMonitoring = "CLOUDBURROW_TEST_MONITORING"

// TestMonitoringThroughThePage (#1939): a custom metric's points written
// with the official client are charted in Metrics explorer, picked from
// Select a metric, and listed in its table, one row per series; Metrics
// management creates a user-defined metric through Create metric and
// deletes it through the row's Delete metric, each read back with the
// client.
func TestMonitoringThroughThePage(t *testing.T) {
	needService(t, "monitoring-explorer")
	addr := endpoint(t, envMonitoring)
	ctx := context.Background()
	c, err := monitoring.NewMetricClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	project := uniqueProject(t)
	metricType := "custom.googleapis.com/browser/queue_depth"
	now := time.Now()
	// Two series, one point each per request, the second request newer.
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
		if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: series}); err != nil {
			t.Fatalf("write the points: %v", err)
		}
	}

	p := open(t)
	p.navigate("/monitoring/metrics-explorer?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#explorer-metric option")].some((o) => o.value === %q)`, metricType))
	p.setField("#explorer-resource", "global")
	p.setField("#explorer-metric", metricType)
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 2`)
	p.waitFor(`document.querySelectorAll("#view svg.explorer-chart polyline").length === 2`)
	var rows string
	p.eval(`[...document.querySelectorAll("#view .explorer-table tbody tr")].map((r) => r.textContent).join("|")`, &rows)
	if !strings.Contains(rows, "env=prod") || !strings.Contains(rows, "env=dev") {
		t.Errorf("the explorer's table = %q, want a row for env=prod and env=dev", rows)
	}

	// A reducer over both series: one row.
	p.setField("#explorer-aligner", "ALIGN_MEAN")
	p.setField("#explorer-reducer", "REDUCE_SUM")
	p.clickText("#view .explorer-builder button", "Apply")
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 1`)

	// Metrics management: Create metric, then Delete metric.
	p.navigate("/monitoring/metrics-management?project=" + project)
	p.clickText("#view button", "Create metric")
	p.submitForm(map[string]string{"type": "browser/created", "metricKind": "GAUGE", "valueType": "INT64", "description": "from the page"}, "type")
	created := "custom.googleapis.com/browser/created"
	p.waitFor(fmt.Sprintf(`location.pathname === "/monitoring/metrics-management" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, created))
	d, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + created})
	if err != nil {
		t.Fatalf("read the metric Create metric made: %v", err)
	}
	if d.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || d.GetValueType() != metricpb.MetricDescriptor_INT64 || d.GetDescription() != "from the page" {
		t.Errorf("the metric Create metric made = %v", d)
	}
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, created)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Delete metric")
	p.confirmByName(created)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, created))
	if _, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + created}); err == nil {
		t.Error("the metric Delete metric deleted is still there")
	}
}

// monitoringOpts are the official clients' options for the instance's
// Monitoring endpoint, never Google's.
func monitoringOpts(t *testing.T) []option.ClientOption {
	t.Helper()
	addr := endpoint(t, envMonitoring)
	if addr == "" {
		t.Fatal("no Monitoring endpoint")
	}
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func writeBrowserGauge(t *testing.T, c *monitoring.MetricClient, project, metricType string, v float64) {
	t.Helper()
	err := c.CreateTimeSeries(context.Background(), &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: []*monitoringpb.TimeSeries{{
		Metric:   &metricpb.Metric{Type: metricType},
		Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
		Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(time.Now())},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}}}}}}})
	if err != nil {
		t.Fatalf("write a point: %v", err)
	}
}

// TestMonitoringAlertingThroughThePage (#1964): Notification channels'
// Create channel makes a webhook channel to a server on this machine,
// Alerting's Create policy a threshold policy notifying it, each read back
// with the official clients; a point above the threshold opens an incident,
// which the webhook receives and the Incidents page lists, and Close
// incident closes it; Delete policy and Delete channel remove both.
func TestMonitoringAlertingThroughThePage(t *testing.T) {
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

	var mu sync.Mutex
	var received []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(b))
		mu.Unlock()
	}))
	defer hook.Close()

	p := open(t)
	p.navigate("/monitoring/alerting/channels?project=" + project)
	p.clickText("#view button", "Create channel")
	p.submitForm(map[string]string{"type": "webhook_tokenauth", "displayName": "Browser hook", "url": hook.URL + "/alerts"}, "type")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Browser hook"))`)
	var channel string
	it := nc.ListNotificationChannels(ctx, &monitoringpb.ListNotificationChannelsRequest{Name: "projects/" + project})
	for {
		c, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.GetDisplayName() == "Browser hook" && c.GetLabels()["url"] == hook.URL+"/alerts" {
			channel = c.GetName()
		}
	}
	if channel == "" {
		t.Fatal("the channel Create channel made is not listed by the official client")
	}

	metricType := "custom.googleapis.com/browser/backlog"
	writeBrowserGauge(t, mc, project, metricType, 1)
	p.navigate("/monitoring/alerting?project=" + project)
	p.clickText("#view button", "Create policy")
	p.submitForm(map[string]string{"displayName": "Backlog too long", "conditionType": "threshold", "conditionName": "Backlog above 5",
		"metric": metricType, "resource": "global", "comparison": "COMPARISON_GT", "threshold": "5", "duration": "0",
		"channels": channel[strings.LastIndex(channel, "/")+1:]}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Backlog too long"))`)
	var policy *monitoringpb.AlertPolicy
	pit := ac.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{Name: "projects/" + project})
	for {
		pol, err := pit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pol.GetDisplayName() == "Backlog too long" {
			policy = pol
		}
	}
	if policy == nil || len(policy.GetNotificationChannels()) != 1 || policy.GetNotificationChannels()[0] != channel ||
		policy.GetConditions()[0].GetConditionThreshold().GetThresholdValue() != 5 {
		t.Fatalf("the policy Create policy made = %v", policy)
	}

	// Above the threshold: the next evaluation opens an incident and posts
	// it. A series takes at most one point every 5 seconds.
	time.Sleep(6 * time.Second)
	writeBrowserGauge(t, mc, project, metricType, 9)
	deadline := time.Now().Add(90 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the webhook received no notification in 90 seconds")
		}
		time.Sleep(time.Second)
	}
	mu.Lock()
	first := received[0]
	mu.Unlock()
	if !strings.Contains(first, `"state":"open"`) || !strings.Contains(first, "Backlog too long") {
		t.Errorf("the webhook's notification = %s", first)
	}
	p.navigate("/monitoring/alerting/incidents?project=" + project)
	row := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("Backlog too long"))`
	p.waitFor(row + ` !== undefined`)
	p.eval(row+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Close alert")
	p.waitFor(row + ` !== undefined && ` + row + `.textContent.includes("closed")`)

	p.navigate("/monitoring/alerting?project=" + project)
	prow := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("Backlog too long"))`
	p.waitFor(prow + ` !== undefined`)
	p.eval(prow+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Delete policy")
	// "In the confirmation dialog, select Delete." (docs/monitoring/alerts/manage-alerts)
	p.pressConfirm("Delete")
	p.waitFor(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Backlog too long"))`)
	if _, err := ac.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: policy.GetName()}); err == nil {
		t.Error("the policy Delete policy deleted is still there")
	}
}

// TestMonitoringUptimeAndDashboardsThroughThePage (#1964): Uptime checks'
// Create uptime check makes an HTTP check of a server on this machine,
// read back with the official client, whose row shows it passed; the
// Dashboards page's Create dashboard makes one from its JSON, read back
// with the official Dashboards client, whose page draws its chart widget.
func TestMonitoringUptimeAndDashboardsThroughThePage(t *testing.T) {
	needService(t, "monitoring-uptime")
	opts := monitoringOpts(t)
	ctx := context.Background()
	uc, err := monitoring.NewUptimeCheckClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	dc, err := dashboard.NewDashboardsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	mc, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	project := uniqueProject(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "healthy") }))
	defer site.Close()
	port := site.URL[strings.LastIndex(site.URL, ":")+1:]
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/monitoring/uptime?project=" + project)
	p.clickText("#view button", "Create uptime check")
	p.submitForm(map[string]string{"displayName": "Browser site", "protocol": "HTTP", "host": "127.0.0.1", "port": port,
		"path": "/", "period": "60", "timeout": "5", "content": "healthy"}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Browser site"))`)
	var check *monitoringpb.UptimeCheckConfig
	uit := uc.ListUptimeCheckConfigs(ctx, &monitoringpb.ListUptimeCheckConfigsRequest{Parent: "projects/" + project})
	for {
		c, err := uit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.GetDisplayName() == "Browser site" {
			check = c
		}
	}
	if check == nil || check.GetHttpCheck().GetPort() != int32(portNum) || check.GetMonitoredResource().GetLabels()["host"] != "127.0.0.1" {
		t.Fatalf("the check Create uptime check made = %v", check)
	}
	// The check runs at the next tick; reload until its row says Passed.
	deadline := time.Now().Add(90 * time.Second)
	for {
		p.navigate("/monitoring/uptime?project=" + project)
		p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Browser site"))`)
		var passed bool
		p.eval(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Browser site") && r.textContent.includes("Passed"))`, &passed)
		if passed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the uptime check did not pass in 90 seconds")
		}
		time.Sleep(3 * time.Second)
	}

	metricType := "custom.googleapis.com/browser/orders"
	writeBrowserGauge(t, mc, project, metricType, 4)
	p.navigate("/monitoring/dashboards?project=" + project)
	p.clickText("#view button", "Create custom dashboard")
	p.submitForm(map[string]string{"displayName": "Browser board",
		"json": `{"gridLayout":{"widgets":[{"title":"Orders","xyChart":{"dataSets":[{"timeSeriesQuery":{"timeSeriesFilter":{"filter":"metric.type=\"` + metricType + `\""}}}]}}]}}`},
		"displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Browser board"))`)
	var board *dashboardpb.Dashboard
	dit := dc.ListDashboards(ctx, &dashboardpb.ListDashboardsRequest{Parent: "projects/" + project})
	for {
		d, err := dit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if d.GetDisplayName() == "Browser board" {
			board = d
		}
	}
	if board == nil || len(board.GetGridLayout().GetWidgets()) != 1 || board.GetGridLayout().GetWidgets()[0].GetTitle() != "Orders" {
		t.Fatalf("the dashboard Create dashboard made = %v", board)
	}
	p.navigate("/monitoring/dashboards/" + board.GetName()[strings.LastIndex(board.GetName(), "/")+1:] + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Orders")`)
}

// TestMonitoringAlertingUptimeAndDashboardsScreensMeetWCAG (#1964): the
// Alerting, Incidents, Notification channels, Uptime checks, Dashboards,
// Groups, Services and Snoozes screens and their create forms, through the
// console's WCAG 2.1 AA audit.
func TestMonitoringAlertingUptimeAndDashboardsScreensMeetWCAG(t *testing.T) {
	needService(t, "monitoring-alerting")
	project := servedProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Alerting", "/monitoring/alerting"}, {"Notification channels", "/monitoring/alerting/channels"},
		{"Uptime checks", "/monitoring/uptime"}, {"Dashboards", "/monitoring/dashboards"},
		{"Monitoring groups", "/monitoring/groups"}, {"Monitoring services", "/monitoring/services"},
		{"Snoozes", "/monitoring/snoozes"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	pages = append(pages, screen("Incidents", "/monitoring/alerting/incidents", project))
	auditScreens(t, pages)
}
