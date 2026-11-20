//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	loggingjson "google.golang.org/api/logging/v2"
	monitoringjson "google.golang.org/api/monitoring/v1"
	"google.golang.org/api/option"
)

// TestLoggingRecentQueriesList (#2031): the queries the Logs Explorer and
// Log Analytics run are the user's recent queries, which the generated
// google.golang.org/api/logging/v2 client lists with
// projects.locations.recentQueries.list: most recent first, a query run
// again once, with its lastRunTime; type("Logging") and
// type("OpsAnalytics") keep one kind, any other filter is INVALID_ARGUMENT;
// pageSize and pageToken page it; "-" lists every location's. The console
// is served as the instance's own account, as a client without a token
// is, so the client lists what the console ran.
func TestLoggingRecentQueriesList(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	svc, err := loggingjson.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvLogging)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("logging.NewService: %v", err)
	}
	project := h.Project()
	n := time.Now().UnixNano()
	logging1 := fmt.Sprintf(`severity>=ERROR AND textPayload:"rq-%d"`, n)
	logging2 := fmt.Sprintf(`resource.type="global" AND textPayload:"rq-%d"`, n)
	sql := fmt.Sprintf("SELECT COUNT(*) AS n, %d AS run FROM `%s.global._Default._AllLogs`", n, project)
	run := func(q string, isSQL bool) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"query": q, "sql": isSQL})
		if code, out := consoleDo(t, addr, http.MethodPost, "/api/logs/recent-queries?project="+url.QueryEscape(project), string(b)); code != http.StatusOK {
			t.Fatalf("the console's run of %q = %d %s", q, code, out)
		}
		time.Sleep(5 * time.Millisecond)
	}
	run(logging1, false)
	run(sql, true)
	run(logging2, false)
	run(logging1, false) // again: it moves to the top

	parent := "projects/" + project + "/locations/global"
	resp, err := svc.Projects.Locations.RecentQueries.List(parent).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("recentQueries.list: %v", err)
	}
	var mine []*loggingjson.RecentQuery
	seen := map[string]int{}
	for _, q := range resp.RecentQueries {
		text := ""
		if q.LoggingQuery != nil {
			text = q.LoggingQuery.Filter
		} else if q.OpsAnalyticsQuery != nil {
			text = q.OpsAnalyticsQuery.SqlQueryText
		}
		if text == logging1 || text == logging2 || text == sql {
			mine = append(mine, q)
			seen[text]++
		}
	}
	if len(mine) != 3 || seen[logging1] != 1 {
		t.Fatalf("recentQueries.list = %d of this test's queries (%v), want 3, each once", len(mine), seen)
	}
	if mine[0].LoggingQuery == nil || mine[0].LoggingQuery.Filter != logging1 || mine[1].LoggingQuery == nil ||
		mine[1].LoggingQuery.Filter != logging2 || mine[2].OpsAnalyticsQuery == nil {
		t.Errorf("the order = %v, want %q (run last), %q, then the SQL", mine, logging1, logging2)
	}
	for _, q := range mine {
		if _, err := time.Parse(time.RFC3339Nano, q.LastRunTime); err != nil || q.Name == "" || len(q.Name) <= len(parent+"/recentQueries/") {
			t.Errorf("a recent query = %+v; want its name and lastRunTime", q)
		}
	}

	ops, err := svc.Projects.Locations.RecentQueries.List(parent).Filter(`type("OpsAnalytics")`).Do()
	if err != nil {
		t.Fatalf(`type("OpsAnalytics"): %v`, err)
	}
	for _, q := range ops.RecentQueries {
		if q.OpsAnalyticsQuery == nil || q.LoggingQuery != nil {
			t.Errorf(`type("OpsAnalytics") listed %+v`, q)
		}
	}
	lq, err := svc.Projects.Locations.RecentQueries.List("projects/" + project + "/locations/-").Filter(`type("Logging")`).Do()
	if err != nil {
		t.Fatalf(`locations/- with type("Logging"): %v`, err)
	}
	for _, q := range lq.RecentQueries {
		if q.LoggingQuery == nil {
			t.Errorf(`type("Logging") listed %+v`, q)
		}
	}

	// Paged one at a time, the same queries in the same order.
	var paged []string
	token := ""
	for i := 0; i < 1000; i++ {
		page, err := svc.Projects.Locations.RecentQueries.List(parent).PageSize(1).PageToken(token).Do()
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if len(page.RecentQueries) > 1 {
			t.Fatalf("pageSize 1 returned %d", len(page.RecentQueries))
		}
		for _, q := range page.RecentQueries {
			paged = append(paged, q.Name)
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(paged) != len(resp.RecentQueries) {
		t.Errorf("paged %d, listed %d", len(paged), len(resp.RecentQueries))
	}
	for i := range paged {
		if i < len(resp.RecentQueries) && paged[i] != resp.RecentQueries[i].Name {
			t.Errorf("page %d = %s, list[%d] = %s", i, paged[i], i, resp.RecentQueries[i].Name)
			break
		}
	}

	_, err = svc.Projects.Locations.RecentQueries.List(parent).Filter("visibility = PRIVATE").Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Errorf("a filter other than type() = %v, want INVALID_ARGUMENT", err)
	}
}

// TestMonitoringDashboardOpsAnalyticsQuery (#2031): a chart whose
// timeSeriesQuery is an opsAnalyticsQuery, as a Log Analytics chart saved
// to a dashboard is, through the generated google.golang.org/api/monitoring/v1
// client: created, read back by get and list with its SQL, patched to
// another statement, refused with a second source, and deleted.
//
// covers: google.monitoring.dashboard.v1.DashboardsService/CreateDashboard, google.monitoring.dashboard.v1.DashboardsService/GetDashboard, google.monitoring.dashboard.v1.DashboardsService/ListDashboards, google.monitoring.dashboard.v1.DashboardsService/UpdateDashboard, google.monitoring.dashboard.v1.DashboardsService/DeleteDashboard
func TestMonitoringDashboardOpsAnalyticsQuery(t *testing.T) {
	h := New(t)
	svc, err := monitoringjson.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvMonitoring)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("monitoring.NewService: %v", err)
	}
	parent := "projects/" + h.Project()
	sql := "SELECT severity, COUNT(*) AS n FROM `" + h.Project() + ".global._Default._AllLogs` GROUP BY severity"
	chart := func(sql string) *monitoringjson.Widget {
		return &monitoringjson.Widget{Title: "By severity", XyChart: &monitoringjson.XyChart{DataSets: []*monitoringjson.DataSet{{
			PlotType: "STACKED_BAR", TimeSeriesQuery: &monitoringjson.TimeSeriesQuery{OpsAnalyticsQuery: &monitoringjson.OpsAnalyticsQuery{Sql: sql}}}}}}
	}
	sqlOf := func(d *monitoringjson.Dashboard) string {
		if d == nil || d.MosaicLayout == nil || len(d.MosaicLayout.Tiles) != 1 || d.MosaicLayout.Tiles[0].Widget.XyChart == nil {
			return ""
		}
		q := d.MosaicLayout.Tiles[0].Widget.XyChart.DataSets[0].TimeSeriesQuery
		if q == nil || q.OpsAnalyticsQuery == nil {
			return ""
		}
		return q.OpsAnalyticsQuery.Sql
	}
	d, err := svc.Projects.Dashboards.Create(parent, &monitoringjson.Dashboard{DisplayName: "Log Analytics",
		MosaicLayout: &monitoringjson.MosaicLayout{Columns: 48, Tiles: []*monitoringjson.Tile{{Width: 24, Height: 16, Widget: chart(sql)}}}}).Do()
	if err != nil {
		t.Fatalf("dashboards.create: %v", err)
	}
	t.Cleanup(func() { _, _ = svc.Projects.Dashboards.Delete(d.Name).Do() })
	if sqlOf(d) != sql {
		t.Errorf("create answered %+v, want the chart's SQL", d.MosaicLayout)
	}
	got, err := svc.Projects.Dashboards.Get(d.Name).Do()
	if err != nil || sqlOf(got) != sql {
		t.Fatalf("dashboards.get = %v, %v", got, err)
	}
	list, err := svc.Projects.Dashboards.List(parent).Do()
	if err != nil {
		t.Fatalf("dashboards.list: %v", err)
	}
	found := false
	for _, x := range list.Dashboards {
		found = found || (x.Name == d.Name && sqlOf(x) == sql)
	}
	if !found {
		t.Error("dashboards.list does not hold the chart's SQL")
	}
	got.MosaicLayout.Tiles[0].Widget = chart("SELECT 1 AS n")
	patched, err := svc.Projects.Dashboards.Patch(d.Name, got).Do()
	if err != nil || sqlOf(patched) != "SELECT 1 AS n" {
		t.Fatalf("dashboards.patch = %v, %v", patched, err)
	}
	if again, err := svc.Projects.Dashboards.Get(d.Name).Do(); err != nil || sqlOf(again) != "SELECT 1 AS n" {
		t.Errorf("after patch, get = %v, %v", again, err)
	}

	bad := chart(sql)
	bad.XyChart.DataSets[0].TimeSeriesQuery.PrometheusQuery = "up"
	_, err = svc.Projects.Dashboards.Create(parent, &monitoringjson.Dashboard{DisplayName: "Two sources",
		GridLayout: &monitoringjson.GridLayout{Widgets: []*monitoringjson.Widget{bad}}}).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Errorf("a query with opsAnalyticsQuery and prometheusQuery = %v, want INVALID_ARGUMENT", err)
	}

	if _, err := svc.Projects.Dashboards.Delete(d.Name).Do(); err != nil {
		t.Fatalf("dashboards.delete: %v", err)
	}
	if _, err := svc.Projects.Dashboards.Get(d.Name).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("get after delete = %v, want NOT_FOUND", err)
	}
}
