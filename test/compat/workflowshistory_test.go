//go:build compat

package compat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/api/option"
	"google.golang.org/api/workflowexecutions/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// covers: google.cloud.workflows.executions.v1.Executions/CreateExecution, google.cloud.workflows.executions.v1.Executions/GetExecution, google.cloud.workflows.executions.v1.Executions/ListExecutions
//
// TestWorkflowsExecutionHistoryLevel (#2011), with the official REST
// client (google.golang.org/api/workflowexecutions/v1), whose Execution
// has executionHistoryLevel (the Go gRPC client's does not): an execution
// created at EXECUTION_HISTORY_DETAILED over a workflow at the default
// (basic) level reads back with its level from get and list, and its step
// entries carry variableData and a for loop's expectedIteration; one
// created without a level has neither; every entry has its
// navigationInfo; an unknown level is refused 400.
func TestWorkflowsExecutionHistoryLevel(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	wf := w.deploy(t, h, "histlevel", `main:
  steps:
    - init:
        assign:
          - total: 0
    - loop:
        for:
          value: v
          in: [1, 2, 3]
          steps:
            - add:
                assign:
                  - total: ${total + v}
    - done:
        return: ${total}
`)
	execs := w.rest.Projects.Locations.Workflows.Executions
	detailed, err := execs.Create(wf.GetName(), &workflowexecutions.Execution{ExecutionHistoryLevel: "EXECUTION_HISTORY_DETAILED"}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("create detailed: %v", err)
	}
	if detailed.ExecutionHistoryLevel != "EXECUTION_HISTORY_DETAILED" {
		t.Errorf("create answered level %q", detailed.ExecutionHistoryLevel)
	}
	basic, err := execs.Create(wf.GetName(), &workflowexecutions.Execution{}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("create basic: %v", err)
	}
	for _, name := range []string{detailed.Name, basic.Name} {
		if got := w.await(t, h, name, time.Minute); got.GetState() != executionspb.Execution_SUCCEEDED || got.GetResult() != "6" {
			t.Fatalf("execution %v", got)
		}
	}
	got, err := execs.Get(detailed.Name).Context(ctx).Do()
	if err != nil || got.ExecutionHistoryLevel != "EXECUTION_HISTORY_DETAILED" {
		t.Errorf("get: level %q, %v", got.ExecutionHistoryLevel, err)
	}
	list, err := execs.List(wf.GetName()).Context(ctx).Do()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	levels := map[string]string{}
	for _, e := range list.Executions {
		levels[e.Name] = e.ExecutionHistoryLevel
	}
	if levels[detailed.Name] != "EXECUTION_HISTORY_DETAILED" || levels[basic.Name] != "" {
		t.Errorf("list levels = %v", levels)
	}
	entries := func(name string) []*workflowexecutions.StepEntry {
		t.Helper()
		l, err := execs.StepEntries.List(name).Context(ctx).Do()
		if err != nil {
			t.Fatalf("stepEntries.list: %v", err)
		}
		return l.StepEntries
	}
	byStep := map[string]*workflowexecutions.StepEntry{}
	for _, e := range entries(detailed.Name) {
		byStep[e.Step] = e
		if e.NavigationInfo == nil {
			t.Errorf("entry %s has no navigationInfo", e.Step)
		}
	}
	loop, done := byStep["loop"], byStep["done"]
	if loop == nil || done == nil {
		t.Fatalf("entries %v", byStep)
	}
	if loop.StepEntryMetadata == nil || loop.StepEntryMetadata.ExpectedIteration != 3 || loop.StepEntryMetadata.ProgressType != "PROGRESS_TYPE_FOR" ||
		loop.StepEntryMetadata.ProgressNumber != 3 {
		t.Errorf("the loop's metadata = %+v", loop.StepEntryMetadata)
	}
	if n := loop.NavigationInfo; n == nil || len(n.Children) != 3 || n.Previous != byStep["init"].EntryId || n.Next != done.EntryId {
		t.Errorf("the loop's navigationInfo = %+v", n)
	}
	var vars map[string]any
	if done.VariableData != nil {
		_ = json.Unmarshal(done.VariableData.Variables, &vars)
	}
	if vars["total"] != float64(6) {
		t.Errorf("done's variableData = %+v", done.VariableData)
	}
	for _, e := range entries(basic.Name) {
		if e.VariableData != nil || (e.StepEntryMetadata != nil && e.StepEntryMetadata.ExpectedIteration != 0) {
			t.Errorf("a basic execution's entry %s recorded detailed history: %+v %+v", e.Step, e.VariableData, e.StepEntryMetadata)
		}
	}
	if _, err := execs.Create(wf.GetName(), &workflowexecutions.Execution{ExecutionHistoryLevel: "EVERYTHING"}).Context(ctx).Do(); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Errorf("an unknown level: %v", err)
	}
}

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/RunJob
//
// TestWorkflowsCloudSchedulerTrigger (#2011): the job Google's Workflows
// page makes for a workflow's Cloud Scheduler trigger posts the argument
// to https://workflowexecutions.googleapis.com/v1/{workflow}/executions
// with an OAuth token; this instance's scheduler sends it to its own
// Workflow Executions, never to Google, and the execution runs with the
// argument.
func TestWorkflowsCloudSchedulerTrigger(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	schedAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_SCHEDULER", EnvScheduler)
	ctx := h.Context()
	wf := w.deploy(t, h, "scheduled", "main:\n  params: [args]\n  steps:\n    - r:\n        return: ${args.n * 2}\n")
	sc, err := scheduler.NewCloudSchedulerClient(ctx, option.WithEndpoint(schedAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	body, _ := json.Marshal(map[string]string{"argument": `{"n": 21}`, "callLogLevel": "LOG_ERRORS_ONLY"})
	parent := "projects/" + h.Project() + "/locations/us-central1"
	job, err := sc.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/wf-trigger", Schedule: "0 0 1 1 *", TimeZone: "Etc/UTC",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: "https://workflowexecutions.googleapis.com/v1/" + wf.GetName() + "/executions", HttpMethod: schedulerpb.HttpMethod_POST, Body: body,
			AuthorizationHeader: &schedulerpb.HttpTarget_OauthToken{OauthToken: &schedulerpb.OAuthToken{
				ServiceAccountEmail: "wf-invoker@" + h.Project() + ".iam.gserviceaccount.com"}}}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteJob(ctx, &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })
	if _, err := sc.RunJob(ctx, &schedulerpb.RunJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(300 * time.Millisecond) {
		l, err := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: wf.GetName(), View: executionspb.ExecutionView_FULL}).Next()
		if err == nil && l.GetState() == executionspb.Execution_SUCCEEDED {
			if l.GetResult() != "42" || l.GetCallLogLevel() != executionspb.Execution_LOG_ERRORS_ONLY {
				t.Fatalf("the scheduled execution = %v", l)
			}
			return
		}
		if time.Now().After(deadline) {
			j, _ := sc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()})
			t.Fatalf("no execution succeeded: %v; job status %v", err, j.GetStatus())
		}
	}
}
