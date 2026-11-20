package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/workflows"
)

// TestConsoleWorkflowsTriggersHistoryAndSteps (#2011): Create workflow's
// Triggers section, before the editor, makes a Cloud Scheduler job that
// posts to the workflow's executions (a bad row makes nothing); the
// workflow's Triggers tab lists it; Execute's Execution history level runs
// one execution with detailed history; and the Steps tab carries each step
// entry for the step detail panel with its navigation and variables.
func TestConsoleWorkflowsTriggersHistoryAndSteps(t *testing.T) {
	ctx := context.Background()
	sched := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })
	api, err := workflows.NewServer("", workflows.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	p := workflowsProvider{svc: &workflowsService{api: api}, scheduler: sched}

	_, fields := p.CreateForm()
	var order []string
	for _, f := range fields {
		order = append(order, f.Name)
		if f.Name == wfSchedulerField && (f.Type != console.GroupFieldType || f.Section != "Triggers") {
			t.Errorf("the Cloud Scheduler trigger field = %+v", f)
		}
	}
	got := strings.Join(order, ",")
	if !strings.Contains(got, "labels,"+wfSchedulerField+",sourceContents") || strings.Contains(got, wfEventarcField) {
		t.Fatalf("Create workflow's fields = %s; want the Cloud Scheduler trigger before the editor and no Eventarc trigger without Eventarc", got)
	}

	const project = "trig-proj"
	src := "main:\n  steps:\n    - init:\n        assign:\n          - x: 1\n    - loop:\n        for:\n          value: v\n          in: [1, 2]\n" +
		"          steps:\n            - add:\n                assign:\n                  - x: ${x + v}\n    - done:\n        return: ${x}\n"
	row := func(arg string) string {
		b, _ := json.Marshal([]map[string]string{{"name": "every-five", "region": "us-east1", "schedule": "*/5 * * * *",
			"timeZone": "Etc/UTC", "argument": arg, "callLogLevel": "LOG_ALL_CALLS", "serviceAccount": "runner@trig-proj.iam.gserviceaccount.com"}})
		return string(b)
	}
	values := map[string]string{"workflowId": "trig", "location": "us-central1", "encryption": "google", "sourceContents": src,
		"callLogLevel": "CALL_LOG_LEVEL_UNSPECIFIED", "executionHistoryLevel": "EXECUTION_HISTORY_BASIC"}
	values[wfSchedulerField] = row("{not json")
	if _, err := p.Create(ctx, project, values); err == nil {
		t.Fatal("a trigger whose argument is not JSON was accepted")
	}
	if l, _ := p.List(ctx, project); len(l.Items) != 0 {
		t.Fatalf("a refused trigger left the workflow made: %+v", l.Items)
	}
	values[wfSchedulerField] = row(`{"n": 1}`)
	name, err := p.Create(ctx, project, values)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := sched.Store().List("projects/" + project + "/")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	j := jobs[0]
	if j.Name != "projects/trig-proj/locations/us-east1/jobs/every-five" || j.HTTP == nil ||
		j.HTTP.URI != "https://workflowexecutions.googleapis.com/v1/"+name+"/executions" || j.HTTP.Method != "POST" ||
		j.HTTP.OAuthToken == nil || j.HTTP.OAuthToken.ServiceAccountEmail != "runner@trig-proj.iam.gserviceaccount.com" {
		t.Fatalf("the trigger's job = %+v %+v", j, j.HTTP)
	}
	var body map[string]string
	if err := json.Unmarshal(j.HTTP.Body, &body); err != nil || body["argument"] != `{"n": 1}` || body["callLogLevel"] != "LOG_ALL_CALLS" {
		t.Fatalf("the job's body = %s (%v)", j.HTTP.Body, err)
	}
	d, err := p.Detail(ctx, project, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	tr := workflowsSection(d, "triggers")
	if tr == nil || len(tr.Listing.Items) != 1 || tr.Listing.Items[0].Name != "every-five" || tr.Listing.Items[0].Fields["Type"] != "Cloud Scheduler" {
		t.Fatalf("the Triggers tab = %+v", tr)
	}

	// Execute at the Detailed level, over the workflow's Basic.
	var execute *console.Action
	for _, a := range p.DetailActions(ctx, project, []string{name}) {
		if a.ID == "execute" {
			execute = &a
		}
	}
	if execute == nil || execute.Fields[len(execute.Fields)-1].Name != "executionHistoryLevel" {
		t.Fatalf("Execute = %+v", execute)
	}
	if err := p.ActAt(ctx, project, []string{name}, "execute", map[string]string{"executionHistoryLevel": "EXECUTION_HISTORY_DETAILED"}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, project, []string{name})
	ex := workflowsSection(d, "executions").Listing.Items[0].Opens[1]
	api.Wait(ex)
	if l := api.HistoryLevel(ex); l != "EXECUTION_HISTORY_DETAILED" {
		t.Fatalf("the execution's level = %q", l)
	}
	ed, _ := p.Detail(ctx, project, []string{name, ex})
	steps := workflowsSection(ed, "steps")
	if steps == nil || len(steps.StepEntries) != 5 {
		t.Fatalf("the Steps tab's entries = %+v", steps)
	}
	if cols := strings.Join(steps.Listing.Columns, ","); strings.Contains(cols, "Entry") || strings.Contains(cols, "Parent") {
		t.Errorf("the Steps table still has the Entry and Parent columns: %s", cols)
	}
	var loop, done console.StepEntryView
	for _, e := range steps.StepEntries {
		switch e.Step {
		case "loop":
			loop = e
		case "done":
			done = e
		}
	}
	if len(loop.Children) != 2 || loop.Previous != "1" || loop.Next != done.ID || loop.Progress != "Iteration 2 of 2" {
		t.Errorf("the loop's entry = %+v", loop)
	}
	if done.Previous != loop.ID || done.Variables["x"] != "4" {
		t.Errorf("done's entry = %+v", done)
	}
}
