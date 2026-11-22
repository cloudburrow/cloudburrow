package main

import (
	"strings"
	"testing"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
)

// TestPipelineTaskTypes: Add task's Table, View, Declare source, Data
// quality test, Unit test and Apache Spark notebook become Dataform core
// SQLX (or actions.yaml) with their targets, tags and dependencies on the
// targets of their Run after tasks.
func TestPipelineTaskTypes(t *testing.T) {
	m := studioPipeline{Tasks: []studioPipelineTask{
		{Name: "src", Type: "declaration", Dataset: "raw", Table: "events"},
		{Name: "daily", Type: "table", TableType: "incremental", Dataset: "mart", Table: "daily_events", UniqueKey: "day, id", SQL: "select * from ${ref(\"events\")}", Tags: []string{"nightly", "nightly", " "}},
		{Name: "v", Type: "view", TableType: "materialized", Dataset: "mart", SQL: "select 1 as x", RunAfter: []string{"daily"}},
		{Name: "dq", Type: "assertion", SQL: "select * from ${ref(\"daily_events\")} where id is null", RunAfter: []string{"daily"}, Tags: []string{"dq"}},
		{Name: "ut", Type: "unit_test", TestedTask: "daily_events", Inputs: []studioUnitInput{{Name: "events", SQL: "select 1 as id"}}, SQL: "select 1 as id"},
		{Name: "spark", Type: "spark_notebook", Notebook: `{"cells":[]}`, RunAfter: []string{"v"}, Tags: []string{"spark"}},
	}}
	files, err := pipelineFiles("p", m)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]string{
		"definitions/src.sqlx":     {`type: "declaration"`, `schema: "raw"`, `name: "events"`},
		"definitions/daily.sqlx":   {`type: "incremental"`, `uniqueKey: ["day", "id"]`, `tags: ["nightly"]`, `name: "daily_events"`},
		"definitions/v.sqlx":       {`type: "view"`, `materialized: true`, `dependencies: ["daily_events"]`},
		"definitions/dq.sqlx":      {`type: "assertion"`, `tags: ["dq"]`, `dependencies: ["daily_events"]`},
		"definitions/ut.sqlx":      {`type: "test"`, `dataset: "daily_events"`, `input "events" {`},
		"definitions/actions.yaml": {`name: "spark"`, `- name: "v"`, `tags:`, `- "spark"`},
	} {
		got, ok := files[path]
		if !ok {
			t.Errorf("%s not written; files %v", path, files)
			continue
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s = %q, want it to hold %q", path, got, w)
			}
		}
	}
	if _, err := pipelineFiles("p", studioPipeline{Tasks: []studioPipelineTask{{Name: "d", Type: "declaration", Table: "x"}}}); err == nil {
		t.Error("a Declare source without its dataset was accepted")
	}
	if _, err := pipelineFiles("p", studioPipeline{Tasks: []studioPipelineTask{{Name: "u", Type: "unit_test", SQL: "select 1"}}}); err == nil {
		t.Error("a unit test without its action was accepted")
	}
	if _, err := pipelineFiles("p", studioPipeline{Tasks: []studioPipelineTask{{Name: "t", Type: "table", Dataset: "a`b", SQL: "select 1"}}}); err == nil {
		t.Error("a dataset with a backquote was accepted")
	}
}

// TestStudioTriggerLimits: the trigger form's Max Wait Duration (1 second
// to 7 days) and Min Execution Duration (3 minutes to 24 hours, 3 minutes
// by default).
func TestStudioTriggerLimits(t *testing.T) {
	base := studioSchedulingReq{Tables: []string{"ds.t", "q.ds2.u"}}
	tc, err := studioTriggerConfig("p", base)
	if err != nil {
		t.Fatal(err)
	}
	if tc.GetCondition() != b1pb.WorkflowTriggerConfig_ANY || tc.GetMinExecutionDuration().AsDuration().Minutes() != 3 || tc.GetMaxWaitDuration() != nil {
		t.Errorf("defaults = %v", tc)
	}
	if tb := tc.GetWorkflowTriggers()[1].GetTableUpdateTrigger().GetTable(); tb.GetDatabase() != "q" || tb.GetSchema() != "ds2" || tb.GetName() != "u" {
		t.Errorf("table = %v", tb)
	}
	for _, bad := range []studioSchedulingReq{
		{},
		{Tables: []string{"ds.t"}, MaxWait: 7*24*3600 + 1},
		{Tables: []string{"ds.t"}, MinExec: 179},
		{Tables: []string{"ds.t"}, MinExec: 24*3600 + 1},
		{Tables: []string{"ds.t"}, Condition: "SOME"},
	} {
		if _, err := studioTriggerConfig("p", bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	all := base
	all.Condition, all.MaxWait, all.MinExec = "ALL", 3600, 600
	if tc, err := studioTriggerConfig("p", all); err != nil || tc.GetCondition() != b1pb.WorkflowTriggerConfig_ALL || tc.GetMaxWaitDuration().AsDuration().Hours() != 1 {
		t.Errorf("ALL = %v, %v", tc, err)
	}
}

// TestStudioInvocationConfig: Select tasks to run's options.
func TestStudioInvocationConfig(t *testing.T) {
	acts := []*dfpb.CompilationResultAction{
		{FilePath: "definitions/a.sqlx", Target: &dfpb.Target{Database: "p", Schema: "d", Name: "a_table"}},
		{FilePath: "definitions/nb.ipynb", Target: &dfpb.Target{Database: "p", Schema: "d", Name: "nb"}},
	}
	ic, err := studioInvocationConfig(&dfpb.CompilationResult{Name: "x/compilationResults/1"}, acts,
		studioRunOptions{Tasks: []string{"a", "nb"}, IncludeDependencies: true, FullRefresh: true, Batch: true, MaxConcurrent: 5, Tags: []string{"t"}}, "sa@p.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(ic.GetIncludedTargets()) != 2 || ic.GetIncludedTargets()[0].GetName() != "a_table" || !ic.GetTransitiveDependenciesIncluded() ||
		!ic.GetFullyRefreshIncrementalTablesEnabled() || ic.GetQueryPriority() != dfpb.InvocationConfig_BATCH || ic.GetConcurrentActionsLimit() != 5 ||
		ic.GetIncludedTags()[0] != "t" {
		t.Errorf("invocation config = %v", ic)
	}
	if _, err := studioInvocationConfig(&dfpb.CompilationResult{}, acts, studioRunOptions{Tasks: []string{"gone"}}, ""); err == nil {
		t.Error("a task not in the compilation was accepted")
	}
	if _, err := studioInvocationConfig(&dfpb.CompilationResult{}, acts, studioRunOptions{MaxConcurrent: 5001}, ""); err == nil {
		t.Error("5001 concurrent actions were accepted")
	}
	if ic, _ := studioInvocationConfig(&dfpb.CompilationResult{}, acts, studioRunOptions{UnitTests: true}, ""); ic.GetExecutionMode() != dfpb.InvocationConfig_UNIT_TESTS_ONLY {
		t.Errorf("unit tests mode = %v", ic.GetExecutionMode())
	}
}
