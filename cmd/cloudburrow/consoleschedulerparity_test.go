package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// Create job as Google's three steps (#1849): each Target type's own fields,
// the retries of Configure optional settings, and the list's columns, tabs
// and the checked jobs' Pause and Resume.
func TestSchedulerCreateJobSteps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	p := schedulerProvider{svc: svc}
	base := func(name, target string) map[string]string {
		v := map[string]string{}
		for _, f := range schedulerCreateFields() {
			v[f.Name] = f.Default
		}
		v["name"], v["targetType"] = name, target
		return v
	}
	steps := map[string]bool{}
	for _, f := range schedulerCreateFields() {
		steps[f.Section] = true
	}
	for _, s := range []string{schedulerStepSchedule, schedulerStepExecution, schedulerStepOptional} {
		if !steps[s] {
			t.Errorf("no step %q", s)
		}
	}

	h := base("web", schedulerTargetHTTP)
	h["uri"], h["httpMethod"], h["headers"], h["retryCount"] = "http://127.0.0.1:1/x", "PUT", `{"X-A":"1"}`, "3"
	h["timeZone"] = "America/New_York"
	name, err := p.Create(ctx, "sched-proj", h)
	if err != nil {
		t.Fatal(err)
	}
	j, err := svc.Store().Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if j.HTTP == nil || j.HTTP.Method != "PUT" || j.HTTP.Headers["X-A"] != "1" || j.Retry.RetryCount != 3 || j.TimeZone != "America/New_York" {
		t.Fatalf("HTTP job = %+v", j)
	}

	ps := base("topic", schedulerTargetPubSub)
	ps["topic"], ps["message"], ps["attributes"] = "projects/sched-proj/topics/t", "hi", `{"k":"v"}`
	if name, err = p.Create(ctx, "sched-proj", ps); err != nil {
		t.Fatal(err)
	}
	if j, _ = svc.Store().Get(name); j.PubSub == nil || string(j.PubSub.Data) != "hi" || j.PubSub.Attributes["k"] != "v" || !j.RetryUnset {
		t.Fatalf("Pub/Sub job = %+v", j)
	}

	ae := base("app", schedulerTargetAppEngine)
	ae["appEngineInstance"], ae["aeMethod"] = "i1", "GET"
	if name, err = p.Create(ctx, "sched-proj", ae); err != nil {
		t.Fatal(err)
	}
	if j, _ = svc.Store().Get(name); j.AppEngine == nil || j.AppEngine.Routing.Service != "default" || j.AppEngine.Routing.Instance != "i1" || j.AppEngine.RelativeURI != "/" {
		t.Fatalf("App Engine job = %+v", j)
	}

	l, err := p.List(ctx, "sched-proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tabs) != 2 || l.ListTab != "jobs" || len(l.SelectActions) != 2 || len(l.Items) != 3 {
		t.Fatalf("listing = %+v", l)
	}
	if f := l.Items[0].Fields; f["Region"] != "us-central1" || f["State"] != "Enabled" || f["Status of last execution"] != "—" {
		t.Fatalf("row = %v", f)
	}
	if err := p.ActAt(ctx, "sched-proj", nil, schedulerPauseSelected, map[string]string{"jobs": l.Items[0].Name + "\n" + l.Items[1].Name}); err != nil {
		t.Fatal(err)
	}
	if j, _ = svc.Store().Get(l.Items[1].Name); j.State != "PAUSED" {
		t.Fatalf("state after Pause = %s", j.State)
	}
	if len(schedulerTimeZones()) < 2 {
		t.Fatal("no time zones")
	}
}
