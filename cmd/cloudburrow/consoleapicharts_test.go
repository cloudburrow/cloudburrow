package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// With the request history (#1866), the APIs & Services pages count the
// project's own calls over the time range, a call that named no project in
// the default project, and draw Google's charts: the dashboard's Traffic,
// Errors and Median latency and an API's Traffic by response code, Errors
// by API method, Overall latency, Latency by API method and Traffic by API
// method.
func TestConsoleAPIsPerProjectHistory(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceTasks}
	reg := metrics.New("tasks")
	reg.Measure("tasks")
	at := time.Now().Add(-10 * time.Minute)
	hist := monitoring.NewRequestBuiltin(monitoringCounters{reg: reg}, "p-default", func() time.Time { return at })
	const list = "/google.cloud.tasks.v2.CloudTasks/ListQueues"
	reg.ObserveIn("p-one", "tasks", list, "OK", 3*time.Millisecond)
	reg.ObserveIn("p-one", "tasks", list, "OK", 4*time.Millisecond)
	reg.ObserveIn("p-one", "tasks", "/google.cloud.tasks.v2.CloudTasks/GetQueue", "NOT_FOUND", 2*time.Millisecond)
	reg.ObserveIn("p-two", "tasks", list, "OK", time.Millisecond)
	reg.ObserveIn("", "tasks", list, "OK", time.Millisecond)
	at = at.Add(time.Minute)
	hist.Sample()

	p := serviceUsageProvider{api: newServiceUsageServer(cfg, nil, store.NewMemory()), metrics: reg, history: hist}
	call := func(project, name string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		out, err := p.StudioCall(context.Background(), project, name, raw)
		if err != nil {
			t.Fatalf("%s %s: %v", project, name, err)
		}
		b, _ := json.Marshal(out)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	tasksTotals := func(m map[string]any) map[string]any {
		for _, a := range m["apis"].([]any) {
			if e := a.(map[string]any); e["name"] == "cloudtasks.googleapis.com" {
				return e["totals"].(map[string]any)
			}
		}
		t.Fatal("cloudtasks is not listed")
		return nil
	}
	if got := tasksTotals(call("p-one", "apis", nil)); got["requests"] != 3.0 || got["errors"] != 1.0 {
		t.Errorf("p-one totals = %v, want its 3 calls, 1 failed", got)
	}
	if got := tasksTotals(call("p-two", "apis", nil)); got["requests"] != 1.0 {
		t.Errorf("p-two totals = %v, want its 1 call", got)
	}
	dash := call("p-default", "apis", map[string]string{"range": "P1D"})
	if got := tasksTotals(dash); got["requests"] != 1.0 {
		t.Errorf("p-default totals = %v, want the call that named no project", got)
	}
	if dash["range"] != "P1D" || len(dash["ranges"].([]any)) != 9 {
		t.Errorf("range = %v, ranges = %v", dash["range"], dash["ranges"])
	}
	var titles []string
	for _, c := range dash["charts"].([]any) {
		titles = append(titles, c.(map[string]any)["title"].(string))
	}
	if len(titles) != 3 || titles[0] != "Traffic" || titles[1] != "Errors" || titles[2] != "Median latency" {
		t.Errorf("dashboard charts = %v", titles)
	}

	one := call("p-one", "api", map[string]string{"service": "cloudtasks.googleapis.com"})
	charts := map[string]map[string]any{}
	for _, c := range one["charts"].([]any) {
		m := c.(map[string]any)
		charts[m["title"].(string)] = m
	}
	for _, want := range []string{"Traffic by response code", "Errors by API method", "Overall latency", "Latency by API method (median)", "Traffic by API method"} {
		if charts[want] == nil {
			t.Errorf("the API page has no %q chart: %v", want, charts)
		}
	}
	var codes []string
	for _, s := range charts["Traffic by response code"]["series"].([]any) {
		codes = append(codes, s.(map[string]any)["name"].(string))
	}
	if len(codes) != 2 || codes[0] != "200" || codes[1] != "404" {
		t.Errorf("response codes = %v, want 200 and 404", codes)
	}
	if charts["Traffic by API method"]["default"] != false || charts["Overall latency"]["default"] != true {
		t.Error("Traffic by API method is Select Graphs' addition; the other four are shown by default")
	}
	if _, err := p.StudioCall(context.Background(), "p-one", "apis", json.RawMessage(`{"range":"P3D"}`)); err == nil {
		t.Error("an unknown range was accepted")
	}
}

// A window's points are a minute apart, or a sixtieth of the range; none
// is drawn before the history began; a step with no calls has a rate of 0
// and no error share or latency.
func TestAPIWindowSteps(t *testing.T) {
	end := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	w := apiWindow{Range: apiRanges[0], Start: end.Add(-time.Hour), End: end, Began: end.Add(-10 * time.Minute),
		Deltas: []monitoring.RequestDelta{{Service: "tasks", Method: "ListQueues", Code: "OK", ResponseCode: "200",
			End: end.Add(-5*time.Minute - 30*time.Second), Count: 6, Buckets: []uint64{6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6}}}}
	if w.step() != time.Minute {
		t.Fatalf("step = %v", w.step())
	}
	if w30 := (apiWindow{Range: apiRanges[len(apiRanges)-1]}); w30.step() != 12*time.Hour {
		t.Errorf("30 days step = %v, want 12h", w30.step())
	}
	s := w.overall(map[string]bool{"tasks": true}, []string{"r", "e"}, rateValue, errorPctValue)
	pts := s[0].Points
	if len(pts) != 10 || !pts[0].At.After(w.Began) {
		t.Fatalf("points = %d from %v, want 10 from %v", len(pts), pts[0].At, w.Began)
	}
	var calls, nilErr int
	for i, p := range pts {
		if *p.Value > 0 {
			calls++
			if *p.Value != 0.1 {
				t.Errorf("rate = %v, want 6 calls a minute", *p.Value)
			}
		}
		if s[1].Points[i].Value == nil {
			nilErr++
		}
	}
	if calls != 1 || nilErr != 9 {
		t.Errorf("steps with calls = %d, error shares undefined = %d", calls, nilErr)
	}
}
