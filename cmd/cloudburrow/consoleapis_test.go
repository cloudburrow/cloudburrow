package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The APIs & Services pages' Studio (#1866): apis lists every API, served
// with its state and its calls from the request counters, unserved with the
// reason; api adds the methods; disable and enable change what Service
// Usage reports.
func TestConsoleAPIsStudio(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceTasks}
	reg := metrics.New("storage", "tasks")
	reg.Measure("tasks")
	reg.Observe("tasks", "/google.cloud.tasks.v2.CloudTasks/ListQueues", "OK", 3*time.Millisecond)
	reg.Observe("tasks", "/google.cloud.tasks.v2.CloudTasks/GetQueue", "NOT_FOUND", 2*time.Millisecond)
	p := serviceUsageProvider{api: newServiceUsageServer(cfg, nil, store.NewMemory()), metrics: reg}
	ctx := context.Background()
	call := func(name string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		out, err := p.StudioCall(ctx, "su-apis", name, raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(out)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	find := func(m map[string]any, name string) map[string]any {
		for _, a := range m["apis"].([]any) {
			if e := a.(map[string]any); e["name"] == name {
				return e
			}
		}
		t.Fatalf("%s is not listed", name)
		return nil
	}
	all := call("apis", nil)
	tasks := find(all, "cloudtasks.googleapis.com")
	totals := tasks["totals"].(map[string]any)
	if tasks["state"] != "ENABLED" || totals["requests"] != 2.0 || totals["errors"] != 1.0 || tasks["category"] != "Developer tools" {
		t.Errorf("cloudtasks = %v", tasks)
	}
	if st := find(all, "storage.googleapis.com")["totals"].(map[string]any); st["measured"] != false {
		t.Errorf("storage is not measured in this registry, but reads %v", st)
	}
	if sp := find(all, "spanner.googleapis.com"); sp["unavailable"] == nil || sp["state"] != nil {
		t.Errorf("spanner is not served here, but reads %v", sp)
	}
	one := call("api", map[string]string{"service": "cloudtasks.googleapis.com"})
	if ms := one["methods"].([]any); len(ms) != 2 || ms[0].(map[string]any)["method"] != "GetQueue" {
		t.Errorf("methods = %v", one["methods"])
	}
	call("disable", map[string]string{"service": "cloudtasks.googleapis.com"})
	if got := find(call("apis", nil), "cloudtasks.googleapis.com")["state"]; got != "DISABLED" {
		t.Errorf("after disable, state = %v", got)
	}
	call("enable", map[string]string{"service": "cloudtasks.googleapis.com"})
	if got := find(call("apis", nil), "cloudtasks.googleapis.com")["state"]; got != "ENABLED" {
		t.Errorf("after enable, state = %v", got)
	}
	if _, err := p.StudioCall(ctx, "su-apis", "enable", json.RawMessage(`{"service":"spanner.googleapis.com"}`)); err == nil {
		t.Error("enabling Spanner on an instance without it succeeded")
	}
	if _, err := p.StudioCall(ctx, "", "apis", nil); err == nil {
		t.Error("apis without a project succeeded")
	}
}

// Every API in the catalogue has its Library entry.
func TestAPILibraryCoversTheCatalogue(t *testing.T) {
	for _, e := range serviceUsageAPIs {
		if apiLibrary[e.api.Name].Category == "" {
			t.Errorf("%s has no Library category", e.api.Name)
		}
	}
}
