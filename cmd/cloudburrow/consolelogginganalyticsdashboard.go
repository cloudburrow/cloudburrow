package main

// Save a Log Analytics chart to a dashboard (#2031): Google's Log
// Analytics saves the results' chart or table to a custom dashboard, new or
// existing (docs.cloud.google.com/logging/docs/analyze/query-and-view, "Save
// to dashboard", read 2026-11-20). The widget is the Monitoring API's: an
// xyChart (or a timeSeriesTable) whose data set's timeSeriesQuery is an
// opsAnalyticsQuery holding the query's SQL, written through the dashboards
// JSON API in process, as a client would.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	analyticsSaveChart = "save-chart-to-dashboard"
	// newDashboardChoice is the Dashboard menu's first option.
	newDashboardChoice = "New dashboard"
)

// DetailActions are none: a log view has no page of its own.
func (loggingAnalyticsProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

// ListActions offers Save to dashboard on the page itself, where the
// instance serves Cloud Monitoring; the page draws it on its chart.
func (p loggingAnalyticsProvider) ListActions(context.Context, string) []console.Action {
	a := console.Action{ID: analyticsSaveChart, Label: "Save to dashboard"}
	if p.monitoring == nil || p.monitoring() == nil {
		a.Unavailable = "Not available locally: a dashboard is Cloud Monitoring's, and this instance does not serve Cloud Monitoring."
	}
	return []console.Action{a}
}

// analyticsWidget is the dashboard widget for a query's chart: kind is
// Bar, Line or Table.
func analyticsWidget(title, kind, sql string) (map[string]any, error) {
	query := map[string]any{"opsAnalyticsQuery": map[string]any{"sql": sql}}
	w := map[string]any{"title": title}
	switch kind {
	case "Table":
		w["timeSeriesTable"] = map[string]any{"dataSets": []any{map[string]any{"timeSeriesQuery": query}}}
	case "Bar", "Line":
		plot := "LINE"
		if kind == "Bar" {
			plot = "STACKED_BAR"
		}
		w["xyChart"] = map[string]any{"dataSets": []any{map[string]any{"plotType": plot, "timeSeriesQuery": query}}}
	default:
		return nil, fmt.Errorf("a chart is saved as a Bar or Line chart, or as a Table, not %q", kind)
	}
	return w, nil
}

// addWidget puts w on dashboard d (the API's JSON) after what it holds.
func addWidget(d map[string]any, w map[string]any) {
	switch {
	case d["mosaicLayout"] != nil:
		m, _ := d["mosaicLayout"].(map[string]any)
		tiles, _ := m["tiles"].([]any)
		y := 0.0
		for _, t := range tiles {
			tm, _ := t.(map[string]any)
			yp, _ := tm["yPos"].(float64)
			h, _ := tm["height"].(float64)
			y = max(y, yp+h)
		}
		cols, _ := m["columns"].(float64)
		width := 24.0
		if cols > 0 && cols < width {
			width = cols
		}
		m["tiles"] = append(tiles, map[string]any{"xPos": 0, "yPos": y, "width": width, "height": 16, "widget": w})
	case d["gridLayout"] != nil:
		g, _ := d["gridLayout"].(map[string]any)
		ws, _ := g["widgets"].([]any)
		g["widgets"] = append(ws, w)
	case d["rowLayout"] != nil:
		rl, _ := d["rowLayout"].(map[string]any)
		rows, _ := rl["rows"].([]any)
		rl["rows"] = append(rows, map[string]any{"weight": "1", "widgets": []any{w}})
	case d["columnLayout"] != nil:
		cl, _ := d["columnLayout"].(map[string]any)
		cols, _ := cl["columns"].([]any)
		cl["columns"] = append(cols, map[string]any{"weight": "1", "widgets": []any{w}})
	default:
		d["mosaicLayout"] = map[string]any{"columns": 48, "tiles": []any{
			map[string]any{"xPos": 0, "yPos": 0, "width": 24, "height": 16, "widget": w}}}
	}
}

// ActAt saves the query's chart: values are dashboard (a dashboard's ID,
// or New dashboard), dashboardName (a new one's), title, kind and sql.
func (p loggingAnalyticsProvider) ActAt(_ context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 0 || action != analyticsSaveChart {
		return fmt.Errorf("Log Analytics has no action %q", action)
	}
	if p.monitoring == nil || p.monitoring() == nil {
		return errors.New("Not available locally: a dashboard is Cloud Monitoring's, and this instance does not serve Cloud Monitoring.")
	}
	m := p.monitoring()
	if project == "" {
		return errors.New("choose a project: a dashboard is one project's")
	}
	sql := strings.TrimSpace(values["sql"])
	if sql == "" {
		return errors.New("run a query first: the chart saved is its results'")
	}
	title := strings.TrimSpace(values["title"])
	if title == "" {
		return errors.New("Chart title is required")
	}
	w, err := analyticsWidget(title, values["kind"], sql)
	if err != nil {
		return err
	}
	call := func(method, path string, body any) (map[string]any, error) {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		code, resp := m.CallDashboardsJSON(method, path, raw)
		var out map[string]any
		_ = json.Unmarshal(resp, &out)
		if code != http.StatusOK {
			if e, ok := out["error"].(map[string]any); ok {
				if msg, _ := e["message"].(string); msg != "" {
					return nil, errors.New(msg)
				}
			}
			return nil, fmt.Errorf("HTTP %d", code)
		}
		return out, nil
	}
	parent := "/v1/projects/" + url.PathEscape(project) + "/dashboards"
	choice := strings.TrimSpace(values["dashboard"])
	if choice == "" || choice == newDashboardChoice {
		name := strings.TrimSpace(values["dashboardName"])
		if name == "" {
			return errors.New("Dashboard name is required for a new dashboard")
		}
		d := map[string]any{"displayName": name}
		addWidget(d, w)
		_, err := call(http.MethodPost, parent, d)
		return err
	}
	d, err := call(http.MethodGet, parent+"/"+url.PathEscape(choice), nil)
	if err != nil {
		return err
	}
	addWidget(d, w)
	_, err = call(http.MethodPatch, parent+"/"+url.PathEscape(choice), d)
	return err
}
