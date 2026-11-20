package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestDashboardDrawsLogAnalyticsCharts (#2032): a dashboard's widget whose
// query is Log Analytics SQL (opsAnalyticsQuery) is drawn, its SQL run
// through Log Analytics: a Bar or Line chart, or a Table, as it was saved;
// where Log Analytics is not served, or the query fails, the chart says so.
func TestDashboardDrawsLogAnalyticsCharts(t *testing.T) {
	ctx := context.Background()
	admin, screen, srv := monitoringAdminFixture(t)
	const project = "dash-la"
	body := `{"displayName":"Errors","mosaicLayout":{"columns":48,"tiles":[` +
		`{"width":24,"height":16,"widget":{"title":"By severity","xyChart":{"dataSets":[{"plotType":"STACKED_BAR","timeSeriesQuery":{"opsAnalyticsQuery":{"sql":"SELECT severity, COUNT(*) AS n FROM t GROUP BY severity"}}}]}}},` +
		`{"yPos":16,"width":24,"height":16,"widget":{"title":"Over time","xyChart":{"dataSets":[{"plotType":"LINE","timeSeriesQuery":{"opsAnalyticsQuery":{"sql":"SELECT 2"}}}]}}},` +
		`{"yPos":32,"width":24,"height":16,"widget":{"title":"Rows","timeSeriesTable":{"dataSets":[{"timeSeriesQuery":{"opsAnalyticsQuery":{"sql":"SELECT 3"}}}]}}}]}}`
	code, resp := srv.CallDashboardsJSON(http.MethodPost, "/v1/projects/"+project+"/dashboards", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, resp)
	}
	dl, err := monitoringDashboardsProvider{monitoringAdmin: admin, metrics: screen}.List(ctx, project)
	if err != nil || len(dl.Items) != 1 {
		t.Fatalf("List = %v, %v", dl, err)
	}
	id := dl.Items[0].Name
	var ran []string
	dash := monitoringDashboardsProvider{monitoringAdmin: admin, metrics: screen,
		analytics: func(_ context.Context, p, sql string) (console.Listing, error) {
			if p != project {
				t.Errorf("the query ran in %q", p)
			}
			ran = append(ran, sql)
			if sql == "SELECT 2" {
				return console.Listing{}, errors.New("boom")
			}
			return console.Listing{NameColumn: "severity", Columns: []string{"n"},
				ColumnTypes: map[string]string{"severity": "STRING", "n": "INTEGER"},
				Items:       []console.Resource{{Name: "ERROR", Fields: map[string]string{"n": "4"}}}}, nil
		}}
	d, err := dash.Detail(ctx, project, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]console.Section{}
	for _, s := range d.Sections {
		if s.Kind == console.KindAnalytics {
			got[s.Label] = s
		}
	}
	if len(ran) != 3 {
		t.Errorf("the queries run = %q", ran)
	}
	if s := got["By severity"]; s.Visualization != "Bar" || s.Group != "Charts" || len(s.Listing.Items) != 1 ||
		s.Listing.ColumnTypes["n"] != "INTEGER" || !strings.Contains(s.Note, "GROUP BY severity") || s.Unavailable != "" {
		t.Errorf("the bar chart = %+v", s)
	}
	if s := got["Over time"]; s.Visualization != "Line" || !strings.Contains(s.Unavailable, "boom") {
		t.Errorf("the failed line chart = %+v", s)
	}
	if s := got["Rows"]; s.Visualization != "Table" || len(s.Listing.Items) != 1 {
		t.Errorf("the table = %+v", s)
	}
	if rows := d.Sections[0].Listing.Items; len(rows) != 3 || !strings.HasSuffix(rows[0].Fields["Detail"], "(1 rows)") {
		t.Errorf("the Widgets listing = %+v", rows)
	}

	// Without Log Analytics, each chart says why it is not drawn.
	for _, p := range []monitoringDashboardsProvider{
		{monitoringAdmin: admin, metrics: screen},
		{monitoringAdmin: admin, metrics: screen, analytics: func(context.Context, string, string) (console.Listing, error) {
			return console.Listing{}, errAnalyticsUnavailable
		}},
	} {
		d, err := p.Detail(ctx, project, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Sections {
			if s.Kind == console.KindAnalytics && s.Unavailable != analyticsUnavailable {
				t.Errorf("%s without Log Analytics = %q", s.Label, s.Unavailable)
			}
		}
	}
}

// TestConsoleEntryCarriesLabels (#2032): a Logging entry's labels reach the
// Explorer, redacted, for labels.KEY and the Fields pane's dimensions.
func TestConsoleEntryCarriesLabels(t *testing.T) {
	e := consoleEntry(&loggingpb.LogEntry{LogName: "projects/p/logs/app",
		Labels: map[string]string{"k8s-pod/app": "frontend"}})
	if e.Labels["k8s-pod/app"] != "frontend" {
		t.Errorf("Labels = %v", e.Labels)
	}
	if consoleEntry(&loggingpb.LogEntry{LogName: "projects/p/logs/app"}).Labels != nil {
		t.Error("an entry with no labels has a labels map")
	}
}
