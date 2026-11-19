package main

import (
	"context"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Create queue takes the queue's configuration, as Google's page does
// (#1848): the rates, retries and sampling ratio given are the queue's.
func TestTasksCreateQueueTakesItsConfiguration(t *testing.T) {
	t.Parallel()
	p := tasksProvider{svc: &tasksService{store: tasks.NewStore(store.NewMemory())}}
	values := map[string]string{}
	for _, f := range tasksCreateQueueFields() {
		values[f.Name] = f.Default
	}
	values["name"], values["location"] = "configured", "europe-west1"
	values["maxDispatchesPerSecond"], values["maxAttempts"], values["samplingRatio"] = "7", "4", "0.5"
	name, err := p.Create(context.Background(), "parity-proj", values)
	if err != nil {
		t.Fatal(err)
	}
	if name != "projects/parity-proj/locations/europe-west1/queues/configured" {
		t.Fatalf("created %s", name)
	}
	q, err := p.svc.Store().GetQueue(name)
	if err != nil {
		t.Fatal(err)
	}
	if q.RateLimits.MaxDispatchesPerSecond != 7 || q.RetryConfig.MaxAttempts != 4 || q.Logging == nil || q.Logging.SamplingRatio != 0.5 {
		t.Fatalf("queue = %+v", q)
	}

	// A form with only the name and region gets the service's defaults.
	plain, err := p.Create(context.Background(), "parity-proj", map[string]string{"name": "plain", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	pq, _ := p.svc.Store().GetQueue(plain)
	if pq.RateLimits != tasks.DefaultRateLimits() || pq.RetryConfig.MaxAttempts != tasks.DefaultRetryConfig().MaxAttempts {
		t.Fatalf("plain queue = %+v", pq)
	}

	// The list shows Google's columns for it.
	l, err := p.List(context.Background(), "parity-proj")
	if err != nil {
		t.Fatal(err)
	}
	var row console.Resource
	for _, it := range l.Items {
		if it.Name == name {
			row = it
		}
	}
	want := map[string]string{"Region": "europe-west1", "State": "Running", "Tasks in queue": "0", "Tasks running": "0",
		"Max rate": "7/s", "Max attempts": "4", "Oldest task": "—"}
	for k, v := range want {
		if row.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, row.Fields[k], v)
		}
	}
	if len(l.SelectActions) != 3 {
		t.Errorf("select actions = %+v", l.SelectActions)
	}
}

// The checked queues' Pause queue, Resume queue and Purge queue, and the
// queue page's Pause queue, Configure logging and Delete queue (#1848).
func TestTasksQueueHeaderActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)
	if err := p.ActAt(ctx, "edit-proj", nil, tasksPauseSelected, map[string]string{"queues": name}); err != nil {
		t.Fatal(err)
	}
	if q, _ := p.svc.Store().GetQueue(name); q.State != tasks.StatePaused {
		t.Fatalf("state after Pause queue = %s", q.State)
	}
	if err := p.ActAt(ctx, "other-proj", nil, tasksResumeSelected, map[string]string{"queues": name}); err == nil {
		t.Fatal("a queue of another project was resumed")
	}
	acts := p.DetailActions(ctx, "edit-proj", []string{name})
	if acts[1].ID != "resumequeue" {
		t.Fatalf("paused queue page offers %s", acts[1].ID)
	}
	if err := p.ActAt(ctx, "edit-proj", []string{name}, "resumequeue", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "edit-proj", []string{name}, "logging", map[string]string{"logging": "On", "samplingRatio": "0.25"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := p.svc.Store().GetQueue(name); q.State != tasks.StateRunning || q.Logging == nil || q.Logging.SamplingRatio != 0.25 {
		t.Fatalf("queue = %+v", q)
	}
	if err := p.ActAt(ctx, "edit-proj", []string{name}, "logging", map[string]string{"logging": "Off"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := p.svc.Store().GetQueue(name); q.Logging != nil && q.Logging.SamplingRatio != 0 {
		t.Fatalf("logging still on: %+v", q.Logging)
	}

	d, err := p.Detail(ctx, "edit-proj", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range d.Sections {
		ids = append(ids, s.ID)
	}
	if got := len(ids); got != 5 || ids[0] != "tasks" || ids[1] != "metrics" || ids[2] != "logs" || ids[3] != "configuration" || ids[4] != "permissions" {
		t.Fatalf("tabs = %v", ids)
	}
	for _, a := range d.Actions {
		if a.ID == "logging" {
			t.Fatal("Configure logging is beside the title; it belongs on the Logs tab")
		}
	}

	if err := p.ActAt(ctx, "edit-proj", []string{name}, "deletequeue", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.svc.Store().GetQueue(name); err == nil {
		t.Fatal("Delete queue left the queue")
	}
}

// The list's Oldest task is the earliest task's creation.
func TestTasksDepthFields(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f := map[string]string{}
	tasksDepthFields(f, []tasks.Task{
		{Created: now.Add(-time.Hour), ScheduleTime: now.Add(time.Hour)},
		{Created: now.Add(-2 * time.Hour), ScheduleTime: now.Add(-time.Minute)},
	}, now)
	if f["Tasks in queue"] != "2" || f["Due now"] != "1" || f["Oldest task"] != now.Add(-2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("fields = %v", f)
	}
}
