package monitoring

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestDashboardOpsAnalyticsQuery (#2031): a dashboard whose chart's
// timeSeriesQuery is an opsAnalyticsQuery, as a Log Analytics chart saved
// to a dashboard is, keeps its SQL through create, get, list, patch and a
// restart; a query with a second source, or no sql, is INVALID_ARGUMENT;
// validateOnly keeps nothing; delete forgets it.
func TestDashboardOpsAnalyticsQuery(t *testing.T) {
	state := filepath.Join(t.TempDir(), "m.json")
	s, _ := newTestServer(t, state)
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	sqlOf := func(d map[string]any) string {
		w := d["mosaicLayout"].(map[string]any)["tiles"].([]any)[0].(map[string]any)["widget"].(map[string]any)
		ds := w["xyChart"].(map[string]any)["dataSets"].([]any)[0].(map[string]any)
		q, _ := ds["timeSeriesQuery"].(map[string]any)["opsAnalyticsQuery"].(map[string]any)
		s, _ := q["sql"].(string)
		return s
	}
	const sql = "SELECT severity, COUNT(*) AS n FROM `p1.global._Default._AllLogs` GROUP BY severity"
	dash := func(sql string) string {
		b, _ := json.Marshal(map[string]any{"displayName": "Logs", "mosaicLayout": map[string]any{"columns": 48, "tiles": []any{
			map[string]any{"width": 24, "height": 16, "widget": map[string]any{"title": "Severity", "xyChart": map[string]any{
				"dataSets": []any{map[string]any{"plotType": "STACKED_BAR", "timeSeriesQuery": map[string]any{
					"opsAnalyticsQuery": map[string]any{"sql": sql}}}}}}}}}})
		return string(b)
	}
	code, d := call(http.MethodPost, "/v1/projects/p1/dashboards", dash(sql))
	if code != http.StatusOK || sqlOf(d) != sql {
		t.Fatalf("create = %d %v", code, d)
	}
	name := d["name"].(string)
	if _, got := call(http.MethodGet, "/v1/"+name, ""); sqlOf(got) != sql {
		t.Errorf("get = %v", got)
	}
	if _, got := call(http.MethodGet, "/v1/projects/p1/dashboards", ""); sqlOf(got["dashboards"].([]any)[0].(map[string]any)) != sql {
		t.Errorf("list = %v", got)
	}
	if got := s.DashboardSQL(name); got["mosaicLayout/tiles/0/widget/xyChart/dataSets/0/timeSeriesQuery"] != sql {
		t.Errorf("DashboardSQL = %v", got)
	}
	s2, err := NewServer(state)
	if err != nil || s2.DashboardSQL(name)["mosaicLayout/tiles/0/widget/xyChart/dataSets/0/timeSeriesQuery"] != sql {
		t.Errorf("after a restart = %v, %v", s2.DashboardSQL(name), err)
	}
	patched := strings.Replace(dash("SELECT 1"), `"displayName"`, `"name":"`+name+`","displayName"`, 1)
	if code, got := call(http.MethodPatch, "/v1/"+name, patched); code != http.StatusOK || sqlOf(got) != "SELECT 1" {
		t.Errorf("patch = %d %v", code, got)
	}
	for _, bad := range []string{
		strings.Replace(dash(sql), `"opsAnalyticsQuery"`, `"prometheusQuery":"up","opsAnalyticsQuery"`, 1),
		dash(""),
	} {
		if code, _ := call(http.MethodPost, "/v1/projects/p1/dashboards", bad); code != http.StatusBadRequest {
			t.Errorf("create %s = %d, want 400", bad, code)
		}
	}
	if code, got := call(http.MethodPost, "/v1/projects/p1/dashboards?validateOnly=true", dash("SELECT 2")); code != http.StatusOK || sqlOf(got) != "SELECT 2" {
		t.Errorf("validateOnly = %d %v", code, got)
	}
	if code, _ := call(http.MethodDelete, "/v1/"+name, ""); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if got := s.DashboardSQL(name); len(got) != 0 {
		t.Errorf("after delete = %v", got)
	}
}
