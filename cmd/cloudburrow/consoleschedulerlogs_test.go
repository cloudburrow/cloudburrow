package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// TestConsoleSchedulerLogsAndEditSteps (#1849): a forced run writes the
// attempt's AttemptStarted and AttemptFinished to the instance's Cloud
// Logging under cloud_scheduler_job; the job's page lists them on its Logs
// tab, newest first, the failure with its status and debugInfo; each row's
// menu has View logs, opening the Logs Explorer on the job's entries; and
// Edit job is Create job's three steps, each field in its step.
func TestConsoleSchedulerLogsAndEditSteps(t *testing.T) {
	ctx := context.Background()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(target.Close)

	st := logging.NewStore(logging.DefaultLimit)
	logs := &loggingService{store: st, api: logging.NewServer(st)}
	svc := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	svc.useLogging(func(ctx context.Context, e *loggingpb.LogEntry) error {
		_, err := logs.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}})
		return err
	})
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	go func() { _ = svc.Worker().Run(t.Context()) }()
	p := schedulerProvider{svc: svc, logs: logs}

	const project = "logs-proj"
	v := map[string]string{}
	for _, f := range schedulerCreateFields() {
		v[f.Name] = f.Default
	}
	v["name"], v["targetType"], v["uri"], v["retryCount"] = "nightly", schedulerTargetHTTP, target.URL+"/run", "0"
	v["schedule"] = "0 0 1 1 *"
	name, err := p.Create(ctx, project, v)
	if err != nil {
		t.Fatal(err)
	}

	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	var viewLogs console.Action
	for _, a := range p.Actions(l.Items[0]) {
		if a.Label == "View logs" {
			viewLogs = a
		}
	}
	u, err := url.Parse(viewLogs.Href)
	if err != nil || u.Path != "/logs" || u.Query().Get("project") != project ||
		!strings.Contains(u.Query().Get("query"), `resource.type="cloud_scheduler_job"`) ||
		!strings.Contains(u.Query().Get("query"), `resource.labels.job_id="nightly"`) {
		t.Fatalf("the row's View logs is %+v; want the Logs Explorer on the job's entries", viewLogs)
	}

	if _, err := svc.API().RunJob(ctx, &schedulerpb.RunJobRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	var sec console.Section
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := p.Detail(ctx, project, []string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Sections {
			if s.ID == "logs" {
				sec = s
			}
		}
		if sec.Listing.Total >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sec.Unavailable != "" || sec.Listing.Total != 2 {
		t.Fatalf("the Logs tab = %+v; want the attempt's two entries", sec)
	}
	finished, started := sec.Listing.Items[0].Fields, sec.Listing.Items[1].Fields
	if finished["Log entry"] != "AttemptFinished" || finished["Severity"] != "ERROR" ||
		!strings.Contains(finished["Summary"], "NOT_FOUND") || !strings.Contains(finished["Summary"], "Original HTTP response code number = 404") {
		t.Errorf("the newest entry = %v; want AttemptFinished, ERROR, NOT_FOUND with its debugInfo", finished)
	}
	if started["Log entry"] != "AttemptStarted" || started["Severity"] != "INFO" || !strings.Contains(started["Summary"], target.URL+"/run") {
		t.Errorf("the older entry = %v; want AttemptStarted, INFO, the URL", started)
	}

	// The entries are Cloud Logging's, read by the Explorer's own filter.
	resp, err := logs.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + project},
		Filter: `logName="projects/logs-proj/logs/cloudscheduler.googleapis.com%2Fexecutions" AND resource.labels.location="us-central1"`})
	if err != nil || len(resp.GetEntries()) != 2 {
		t.Fatalf("ListLogEntries on the executions log = %d entries, %v", len(resp.GetEntries()), err)
	}
	if got := resp.GetEntries()[0].GetJsonPayload().GetFields()["jobName"].GetStringValue(); got != name {
		t.Errorf("jobName = %q, want %q", got, name)
	}

	// Without Cloud Logging, the tab says so rather than listing nothing.
	d, err := (schedulerProvider{svc: svc}).Detail(ctx, project, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Sections {
		if s.ID == "logs" && !strings.Contains(s.Unavailable, "Cloud Logging is not served") {
			t.Errorf("without Cloud Logging the Logs tab = %+v", s)
		}
	}

	// Edit job follows Create job's steps.
	if d.Edit == nil || d.Edit.Steps != "continue" {
		t.Fatalf("Edit job = %+v; want Create job's steps", d.Edit)
	}
	// And Create job's page, not a dialog (#2062).
	if !d.Edit.Page {
		t.Error("Edit job is a dialog; want a page, as Create job is")
	}
	want := map[string]string{"name": schedulerStepSchedule, "schedule": schedulerStepSchedule, "timeZone": schedulerStepSchedule,
		"targetType": schedulerStepExecution, "uri": schedulerStepExecution, "httpMethod": schedulerStepExecution,
		"authHeader": schedulerStepExecution, "retryCount": schedulerStepOptional, "attemptDeadline": schedulerStepOptional}
	for _, f := range d.Edit.Fields {
		if s, ok := want[f.Name]; ok && f.Section != s {
			t.Errorf("Edit job's %s is in %q, want %q", f.Name, f.Section, s)
		}
		if f.Name == "httpMethod" && (f.Type != "select" || f.Default != "POST") {
			t.Errorf("Edit job's HTTP method = %+v; want Create job's list", f)
		}
		if f.Section == "" {
			t.Errorf("Edit job's %s is in no step", f.Name)
		}
	}
}
