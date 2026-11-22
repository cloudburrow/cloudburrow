package main

// BigQuery Studio's Scheduling page, trigger-based schedules, Run > Select
// tasks to run / Run by tag, and the pipeline task types beyond Query,
// Notebook and Data preparation (#2105), as Google documents them
// (https://docs.cloud.google.com/bigquery/docs/create-pipelines,
// schedule-pipelines, orchestrate-workloads; read 2026-11-22).
//
//   - Add task's Table (Table or Incremental table), View (View or
//     Materialized view), Declare source, Data quality test (an assertion)
//     and Unit test are Dataform core SQLX actions, each with its project,
//     dataset and name; Apache Spark notebook is a notebook task whose
//     cells make a DataprocSparkSession (#2156). Each task may carry tags,
//     which Run by tag selects.
//   - Run > Select tasks to run is an invocation of the compiled main
//     branch with its InvocationConfig: the chosen tasks' targets (or
//     tags), Include dependencies, Include dependents, Run with full
//     refresh, Run as interactive job with high priority (BATCH when
//     cleared) and Limit concurrent actions (1 to 5000). A unit test run is
//     execution_mode UNIT_TESTS_ONLY.
//   - A trigger (Configuration Type: Trigger (event-based execution)) is a
//     workflow configuration with no cron schedule and a v1beta1
//     WorkflowTriggerConfig: the tables, Wait for ALL tables to update or
//     Trigger if ANY table updates, Max Wait Duration (1 second to 7 days)
//     and Min Execution Duration (3 minutes to 24 hours, 3 minutes by
//     default).
//   - The Scheduling page lists, in one table, every schedule of the
//     project's notebooks, pipelines and data preparations (their workflow
//     configurations) with the project's scheduled queries (the BigQuery
//     Data Transfer Service's, read by the page from their own provider).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// studioTaskTypes are a pipeline's task types, as Add task names them.
var studioTaskTypes = map[string]string{
	"sql": "Query", "notebook": "Notebook", "data_preparation": "Data preparation", "table": "Table", "view": "View",
	"declaration": "Declare source", "assertion": "Data quality test", "unit_test": "Unit test", "spark_notebook": "Apache Spark notebook",
}

// studioUnitInput is a unit test's input: the referenced action's name
// and the query standing in for it.
type studioUnitInput struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

// studioTaskTarget is the target name of a task: its table's name when
// it has one, else its own.
func studioTaskTarget(t studioPipelineTask) string {
	if n := strings.TrimSpace(t.Table); n != "" {
		return n
	}
	return t.Name
}

// studioConfigHead is the start of a SQLX config: its type, project,
// dataset, name, description and tags.
func studioConfigHead(kind string, t studioPipelineTask) []string {
	js := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	cfg := []string{"  type: " + js(kind)}
	if p := strings.TrimSpace(t.Project); p != "" {
		cfg = append(cfg, "  database: "+js(p))
	}
	if d := strings.TrimSpace(t.Dataset); d != "" {
		cfg = append(cfg, "  schema: "+js(d))
	}
	if n := strings.TrimSpace(t.Table); n != "" {
		cfg = append(cfg, "  name: "+js(n))
	}
	if d := strings.TrimSpace(t.Description); d != "" && kind != "test" && kind != "operations" {
		cfg = append(cfg, "  description: "+js(d))
	}
	if tags := studioTags(t.Tags); len(tags) > 0 && kind != "declaration" {
		q := make([]string, len(tags))
		for i, x := range tags {
			q[i] = js(x)
		}
		cfg = append(cfg, "  tags: ["+strings.Join(q, ", ")+"]")
	}
	return cfg
}

func studioTags(tags []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// studioCheckIdent is a project, dataset or table name of a task's.
func studioCheckIdent(task, field, v string) error {
	if v == "" {
		return nil
	}
	if strings.ContainsAny(v, "`\"'\n\\") {
		return fmt.Errorf("task %s: the %s %q has a character a name cannot hold", task, field, v)
	}
	return nil
}

// pipelineTaskFile is the file a Table, View, Declare source, Data
// quality test, Unit test or Query task (with tags) is kept in, as
// Dataform core's SQLX; deps are its Run after tasks' target names.
func pipelineTaskFile(t studioPipelineTask, deps []string) (string, error) {
	for _, f := range [][2]string{{"project", t.Project}, {"dataset", t.Dataset}, {"name", t.Table}} {
		if err := studioCheckIdent(t.Name, f[0], strings.TrimSpace(f[1])); err != nil {
			return "", err
		}
	}
	js := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	depLine := func() string {
		if len(deps) == 0 {
			return ""
		}
		q := make([]string, len(deps))
		for i, d := range deps {
			q[i] = js(d)
		}
		return "  dependencies: [" + strings.Join(q, ", ") + "]"
	}
	body := strings.TrimSpace(t.SQL)
	var cfg []string
	switch t.Type {
	case "sql":
		cfg = studioConfigHead("operations", studioPipelineTask{Tags: t.Tags})
		if body == "" {
			return "", fmt.Errorf("task %s has no query", t.Name)
		}
	case "table":
		kind := "table"
		if t.TableType == "incremental" {
			kind = "incremental"
		}
		cfg = studioConfigHead(kind, t)
		if kind == "incremental" {
			if keys := studioTags(strings.Split(t.UniqueKey, ",")); len(keys) > 0 {
				q := make([]string, len(keys))
				for i, k := range keys {
					q[i] = js(k)
				}
				cfg = append(cfg, "  uniqueKey: ["+strings.Join(q, ", ")+"]")
			}
		}
		if body == "" {
			return "", fmt.Errorf("task %s: a table needs its SELECT query", t.Name)
		}
	case "view":
		cfg = studioConfigHead("view", t)
		if t.TableType == "materialized" {
			cfg = append(cfg, "  materialized: true")
		}
		if body == "" {
			return "", fmt.Errorf("task %s: a view needs its SELECT query", t.Name)
		}
	case "declaration":
		if strings.TrimSpace(t.Dataset) == "" || strings.TrimSpace(t.Table) == "" {
			return "", fmt.Errorf("task %s: Declare source needs the source's dataset and table or view", t.Name)
		}
		cfg = studioConfigHead("declaration", t)
		// A declaration has no query and runs nothing; its Run after is
		// not carried (Dataform core gives a declaration no dependencies).
		return "config {\n" + strings.Join(cfg, ",\n") + "\n}\n", nil
	case "assertion":
		cfg = studioConfigHead("assertion", t)
		if body == "" {
			return "", fmt.Errorf("task %s: a data quality test needs the query that returns the rows breaking it", t.Name)
		}
	case "unit_test":
		tested := strings.TrimSpace(t.TestedTask)
		if tested == "" {
			return "", fmt.Errorf("task %s: a unit test needs its Action to test", t.Name)
		}
		if body == "" {
			return "", fmt.Errorf("task %s: a unit test needs its expected output query", t.Name)
		}
		// A test names the action it tests by its dataset field (Dataform
		// core's TestConfig), and carries no dependencies of its own.
		// The tested action's dataset and project, when the pipeline gives
		// it its own, name it in full ({schema, name, database}); a bare
		// name is looked up in the default dataset.
		ref := js(tested)
		if d := strings.TrimSpace(t.Dataset); d != "" {
			ref = "{ schema: " + js(d) + ", name: " + js(tested)
			if p := strings.TrimSpace(t.Project); p != "" {
				ref += ", database: " + js(p)
			}
			ref += " }"
		}
		cfg = []string{`  type: "test"`, "  dataset: " + ref}
		out := "config {\n" + strings.Join(cfg, ",\n") + "\n}\n\n"
		for _, in := range t.Inputs {
			n, q := strings.TrimSpace(in.Name), strings.TrimSpace(in.SQL)
			if n == "" || q == "" {
				return "", fmt.Errorf("task %s: each input needs the name it stands for and its query", t.Name)
			}
			out += "input " + js(n) + " {\n  " + strings.ReplaceAll(q, "\n", "\n  ") + "\n}\n\n"
		}
		return out + body + "\n", nil
	default:
		return "", fmt.Errorf("task %s: %q is not a task type", t.Name, t.Type)
	}
	if d := depLine(); d != "" {
		cfg = append(cfg, d)
	}
	return "config {\n" + strings.Join(cfg, ",\n") + "\n}\n\n" + body + "\n", nil
}

// studioRunOptions are Run's execution options.
type studioRunOptions struct {
	Tasks               []string `json:"tasks"`
	Tags                []string `json:"tags"`
	IncludeDependencies bool     `json:"includeDependencies"`
	IncludeDependents   bool     `json:"includeDependents"`
	FullRefresh         bool     `json:"fullRefresh"`
	// Batch is Run as interactive job with high priority cleared.
	Batch         bool  `json:"batch"`
	MaxConcurrent int32 `json:"maxConcurrent"`
	UnitTests     bool  `json:"unitTests"`
}

// studioInvocationConfig turns Run's options into an InvocationConfig:
// each chosen task is the target of the compiled action from its file.
func studioInvocationConfig(cr *dfpb.CompilationResult, actions []*dfpb.CompilationResultAction, o studioRunOptions, sa string) (*dfpb.InvocationConfig, error) {
	ic := &dfpb.InvocationConfig{ServiceAccount: sa, TransitiveDependenciesIncluded: o.IncludeDependencies,
		TransitiveDependentsIncluded: o.IncludeDependents, FullyRefreshIncrementalTablesEnabled: o.FullRefresh}
	if o.Batch {
		p := dfpb.InvocationConfig_BATCH
		ic.QueryPriority = &p
	}
	if o.MaxConcurrent != 0 {
		if o.MaxConcurrent < 1 || o.MaxConcurrent > 5000 {
			return nil, errors.New("Maximum concurrent actions is a whole number from 1 to 5000")
		}
		n := o.MaxConcurrent
		ic.ConcurrentActionsLimit = &n
	}
	if o.UnitTests {
		ic.ExecutionMode = dfpb.InvocationConfig_UNIT_TESTS_ONLY
	}
	ic.IncludedTags = studioTags(o.Tags)
	for _, task := range o.Tasks {
		found := false
		for _, a := range actions {
			fp := a.GetFilePath()
			if fp == "definitions/"+task+".sqlx" || fp == "definitions/"+task+".ipynb" || (a.GetNotebook() != nil && a.GetTarget().GetName() == task) {
				ic.IncludedTargets = append(ic.IncludedTargets, a.GetTarget())
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("task %s is not in the compiled pipeline (%s): save the pipeline first", task, lastSegment(cr.GetName()))
		}
	}
	return ic, nil
}

// studioScheduleRow is one row of the Scheduling page.
type studioScheduleRow struct {
	Name         string   `json:"name"`
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Asset        string   `json:"asset"`
	AssetName    string   `json:"assetName"`
	AssetKind    string   `json:"assetKind"`
	Region       string   `json:"region"`
	Config       string   `json:"config"`
	Frequency    string   `json:"frequency"`
	TimeZone     string   `json:"timeZone,omitempty"`
	Tables       []string `json:"tables,omitempty"`
	Condition    string   `json:"condition,omitempty"`
	MaxWait      string   `json:"maxWait,omitempty"`
	MinExec      string   `json:"minExec,omitempty"`
	Disabled     bool     `json:"disabled"`
	Author       string   `json:"serviceAccount"`
	Bucket       string   `json:"bucket,omitempty"`
	Template     string   `json:"runtimeTemplate,omitempty"`
	LastRun      string   `json:"lastRun,omitempty"`
	LastRunState string   `json:"lastRunState,omitempty"`
	Created      string   `json:"created"`
	Updated      string   `json:"updated"`
}

// studioSchedulingReq is a Scheduling page or trigger call's body.
type studioSchedulingReq struct {
	studioOrchReq
	Tables    []string         `json:"tables"`
	Condition string           `json:"condition"`
	MaxWait   int64            `json:"maxWaitSeconds"`
	MinExec   int64            `json:"minExecSeconds"`
	Run       studioRunOptions `json:"run"`
}

// studioScheduling answers the Scheduling page's, a trigger's and Run's
// selection calls; ok is false for a call it does not know.
func studioScheduling(ctx context.Context, s *dataform.Server, project, call string, body json.RawMessage) (any, bool, error) {
	var in studioSchedulingReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, true, fmt.Errorf("a schedule request: %w", err)
		}
	}
	for _, n := range []string{in.Name, in.Schedule} {
		if n != "" {
			if err := kmsOwned(project, n); err != nil {
				return nil, true, err
			}
		}
	}
	var out any
	var err error
	switch call {
	case "allschedules":
		out, err = studioAllSchedules(ctx, s, project)
	case "schedule":
		out, err = studioOneSchedule(ctx, s, in.Schedule)
	case "createtrigger":
		out, err = studioCreateTrigger(ctx, s, project, in)
	case "updateschedule":
		out, err = studioUpdateSchedule(ctx, s, project, in)
	case "runselected":
		out, err = studioRunSelected(ctx, s, in)
	default:
		return nil, false, nil
	}
	return out, true, err
}

// studioAssetRepos is every Studio notebook, pipeline and data preparation
// of the project, in every location.
func studioAssetRepos(ctx context.Context, s *dataform.Server, project string) ([]*dfpb.Repository, error) {
	var out []*dfpb.Repository
	for _, loc := range dataform.Locations() {
		resp, err := s.ListRepositories(ctx, &dfpb.ListRepositoriesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, r := range resp.GetRepositories() {
			switch r.GetLabels()[dataformAssetLabel] {
			case "notebook", "pipeline", "data_preparation":
				out = append(out, r)
			}
		}
	}
	return out, nil
}

var studioTypeOf = map[string]string{"notebook": "Notebook", "pipeline": "Pipeline", "data_preparation": "Data preparation"}

// studioScheduleRowOf is a workflow configuration as the Scheduling page's row.
func studioScheduleRowOf(ctx context.Context, s *dataform.Server, r *dfpb.Repository, wc *dfpb.WorkflowConfig) studioScheduleRow {
	kind := r.GetLabels()[dataformAssetLabel]
	parts := strings.Split(wc.GetName(), "/")
	row := studioScheduleRow{Name: wc.GetName(), ID: lastSegment(wc.GetName()), Type: studioTypeOf[kind], Asset: r.GetName(), AssetName: studioDisplay(r),
		AssetKind: kind, Region: parts[3], Config: "Schedule", Frequency: wc.GetCronSchedule(), TimeZone: wc.GetTimeZone(), Disabled: wc.GetDisabled(),
		Author: wc.GetInvocationConfig().GetServiceAccount(), Created: kmsTime(wc.GetCreateTime()), Updated: kmsTime(wc.GetUpdateTime())}
	if b, err := s.CallBeta(ctx, "GetWorkflowConfig", &b1pb.GetWorkflowConfigRequest{Name: wc.GetName()}); err == nil {
		if tc := b.(*b1pb.WorkflowConfig).GetWorkflowTriggerConfig(); tc != nil {
			row.Config = "Trigger"
			for _, t := range tc.GetWorkflowTriggers() {
				row.Tables = append(row.Tables, dataformTriggerTarget(t.GetTableUpdateTrigger().GetTable()))
			}
			row.Condition = "ANY"
			if tc.GetCondition() == b1pb.WorkflowTriggerConfig_ALL {
				row.Condition = "ALL"
			}
			if d := tc.GetMaxWaitDuration(); d != nil {
				row.MaxWait = d.AsDuration().String()
			}
			if d := tc.GetMinExecutionDuration(); d != nil {
				row.MinExec = d.AsDuration().String()
			}
		}
	}
	if rc, err := s.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: wc.GetReleaseConfig()}); err == nil {
		o := rc.GetCodeCompilationConfig().GetDefaultNotebookRuntimeOptions()
		row.Bucket, row.Template = o.GetGcsOutputBucket(), o.GetAiPlatformNotebookRuntimeTemplate()
	}
	if recs := wc.GetRecentScheduledExecutionRecords(); len(recs) > 0 {
		last := recs[0]
		row.LastRun = kmsTime(last.GetExecutionTime())
		if inv := last.GetWorkflowInvocation(); inv != "" {
			if wi, err := s.GetWorkflowInvocation(ctx, &dfpb.GetWorkflowInvocationRequest{Name: inv}); err == nil {
				row.LastRunState = wi.GetState().String()
			}
		} else if st := last.GetErrorStatus(); st != nil {
			row.LastRunState = "FAILED"
		}
	}
	return row
}

func studioAllSchedules(ctx context.Context, s *dataform.Server, project string) (any, error) {
	repos, err := studioAssetRepos(ctx, s, project)
	if err != nil {
		return nil, err
	}
	rows := []studioScheduleRow{}
	assets := []map[string]string{}
	for _, r := range repos {
		assets = append(assets, map[string]string{"name": r.GetName(), "displayName": studioDisplay(r), "kind": r.GetLabels()[dataformAssetLabel],
			"location": strings.Split(r.GetName(), "/")[3]})
		resp, err := s.ListWorkflowConfigs(ctx, &dfpb.ListWorkflowConfigsRequest{Parent: r.GetName(), PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, wc := range resp.GetWorkflowConfigs() {
			rows = append(rows, studioScheduleRowOf(ctx, s, r, wc))
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Updated > rows[j].Updated })
	sort.SliceStable(assets, func(i, j int) bool { return assets[i]["displayName"] < assets[j]["displayName"] })
	return map[string]any{"schedules": rows, "assets": assets}, nil
}

func studioOneSchedule(ctx context.Context, s *dataform.Server, schedule string) (any, error) {
	i := strings.Index(schedule, "/workflowConfigs/")
	if i < 0 {
		return nil, fmt.Errorf("%q is not a schedule", schedule)
	}
	r, err := s.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: schedule[:i]})
	if err != nil {
		return nil, err
	}
	wc, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: schedule})
	if err != nil {
		return nil, err
	}
	return studioScheduleRowOf(ctx, s, r, wc), nil
}

// studioTriggerConfig is the trigger form's tables, condition and
// durations as a v1beta1 trigger config, with the form's limits.
func studioTriggerConfig(project string, in studioSchedulingReq) (*b1pb.WorkflowTriggerConfig, error) {
	if len(in.Tables) == 0 {
		return nil, errors.New("a trigger needs at least one table to monitor: Search tables")
	}
	tc := &b1pb.WorkflowTriggerConfig{Condition: b1pb.WorkflowTriggerConfig_ANY}
	switch in.Condition {
	case "ALL":
		tc.Condition = b1pb.WorkflowTriggerConfig_ALL
	case "", "ANY":
	default:
		return nil, fmt.Errorf("Trigger Condition %q: Wait for ALL tables to update or Trigger if ANY table updates", in.Condition)
	}
	for _, ref := range in.Tables {
		p, d, t, err := studioTableRef(project, ref)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", ref, err)
		}
		tc.WorkflowTriggers = append(tc.WorkflowTriggers, &b1pb.WorkflowTrigger{Trigger: &b1pb.WorkflowTrigger_TableUpdateTrigger{
			TableUpdateTrigger: &b1pb.TableUpdateTrigger{Table: &b1pb.Target{Database: p, Schema: d, Name: t}}}})
	}
	if in.MaxWait != 0 {
		if in.MaxWait < 1 || in.MaxWait > 7*24*3600 {
			return nil, errors.New("Max Wait Duration is from 1 second to 7 days")
		}
		tc.MaxWaitDuration = durationpb.New(time.Duration(in.MaxWait) * time.Second)
	}
	minExec := in.MinExec
	if minExec == 0 {
		minExec = 180
	}
	if minExec < 180 || minExec > 24*3600 {
		return nil, errors.New("Min Execution Duration is from 3 minutes to 24 hours")
	}
	tc.MinExecutionDuration = durationpb.New(time.Duration(minExec) * time.Second)
	return tc, nil
}

// studioCreateTrigger is Trigger's Create schedule: the release
// configuration, then a workflow configuration with no cron schedule and
// the trigger config, then a first compilation.
func studioCreateTrigger(ctx context.Context, s *dataform.Server, project string, in studioSchedulingReq) (any, error) {
	r, err := s.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: in.Name})
	if err != nil {
		return nil, err
	}
	kind := r.GetLabels()[dataformAssetLabel]
	if kind != "pipeline" {
		// Google documents the trigger form for pipelines only.
		return nil, fmt.Errorf("%s is not a pipeline: notebook and data preparation schedules are time-based", studioDisplay(r))
	}
	id, err := studioScheduleID(in.DisplayName)
	if err != nil {
		return nil, err
	}
	tc, err := studioTriggerConfig(project, in)
	if err != nil {
		return nil, err
	}
	cc := &dfpb.CodeCompilationConfig{}
	if b := strings.TrimSpace(in.Bucket); b != "" {
		if !strings.HasPrefix(b, "gs://") {
			b = "gs://" + b
		}
		cc.DefaultNotebookRuntimeOptions = &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: b},
			AiPlatformNotebookRuntimeTemplate: strings.TrimSpace(in.RuntimeTemplate)}
	}
	rc, err := s.CreateReleaseConfig(ctx, &dfpb.CreateReleaseConfigRequest{Parent: in.Name, ReleaseConfigId: id,
		ReleaseConfig: &dfpb.ReleaseConfig{GitCommitish: "main", CodeCompilationConfig: cc}})
	if err != nil {
		return nil, err
	}
	ic, err := studioInvocationConfig(nil, nil, in.Run, strings.TrimSpace(in.ServiceAccount))
	if err != nil {
		return nil, err
	}
	b1ic := &b1pb.InvocationConfig{ServiceAccount: ic.GetServiceAccount(), ConcurrentActionsLimit: ic.ConcurrentActionsLimit}
	if ic.QueryPriority != nil {
		p := b1pb.InvocationConfig_QueryPriority(*ic.QueryPriority)
		b1ic.QueryPriority = &p
	}
	out, err := s.CallBeta(ctx, "CreateWorkflowConfig", &b1pb.CreateWorkflowConfigRequest{Parent: in.Name, WorkflowConfigId: id,
		WorkflowConfig: &b1pb.WorkflowConfig{ReleaseConfig: rc.GetName(), InvocationConfig: b1ic, WorkflowTriggerConfig: tc}})
	if err != nil {
		_, _ = s.DeleteReleaseConfig(ctx, &dfpb.DeleteReleaseConfigRequest{Name: rc.GetName()})
		return nil, err
	}
	name := out.(*b1pb.WorkflowConfig).GetName()
	if _, err := studioCompileRelease(ctx, s, rc.GetName()); err != nil {
		return map[string]any{"name": name, "id": id, "warning": "the trigger is created, but its first compilation failed: " + err.Error()}, nil
	}
	return map[string]any{"name": name, "id": id}, nil
}

// studioUpdateSchedule is Edit's Update schedule: the frequency, time zone
// and service account of a time-based schedule, or a trigger's tables,
// condition and durations, and the notebook options of its release.
func studioUpdateSchedule(ctx context.Context, s *dataform.Server, project string, in studioSchedulingReq) (any, error) {
	wc, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: in.Schedule})
	if err != nil {
		return nil, err
	}
	b, err := s.CallBeta(ctx, "GetWorkflowConfig", &b1pb.GetWorkflowConfigRequest{Name: in.Schedule})
	if err != nil {
		return nil, err
	}
	trigger := b.(*b1pb.WorkflowConfig).GetWorkflowTriggerConfig() != nil
	sa := strings.TrimSpace(in.ServiceAccount)
	if trigger {
		tc, err := studioTriggerConfig(project, in)
		if err != nil {
			return nil, err
		}
		if _, err := s.CallBeta(ctx, "UpdateWorkflowConfig", &b1pb.UpdateWorkflowConfigRequest{
			WorkflowConfig: &b1pb.WorkflowConfig{Name: in.Schedule, WorkflowTriggerConfig: tc, InvocationConfig: &b1pb.InvocationConfig{ServiceAccount: sa}},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"workflow_trigger_config", "invocation_config"}}}); err != nil {
			return nil, err
		}
	} else {
		if strings.TrimSpace(in.Cron) == "" {
			return nil, errors.New("a schedule needs a frequency")
		}
		if _, err := s.UpdateWorkflowConfig(ctx, &dfpb.UpdateWorkflowConfigRequest{
			WorkflowConfig: &dfpb.WorkflowConfig{Name: in.Schedule, CronSchedule: strings.TrimSpace(in.Cron), TimeZone: strings.TrimSpace(in.TimeZone),
				InvocationConfig: &dfpb.InvocationConfig{ServiceAccount: sa}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cron_schedule", "time_zone", "invocation_config"}}}); err != nil {
			return nil, err
		}
	}
	if rc := wc.GetReleaseConfig(); rc != "" {
		cur, err := s.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: rc})
		if err == nil {
			cc := cur.GetCodeCompilationConfig()
			if cc == nil {
				cc = &dfpb.CodeCompilationConfig{}
			}
			if bkt := strings.TrimSpace(in.Bucket); bkt != "" {
				if !strings.HasPrefix(bkt, "gs://") {
					bkt = "gs://" + bkt
				}
				cc.DefaultNotebookRuntimeOptions = &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: bkt},
					AiPlatformNotebookRuntimeTemplate: strings.TrimSpace(in.RuntimeTemplate)}
			} else if cc.GetPipelineConfig().GetPipelineType() == dfpb.PipelineConfig_NOTEBOOK {
				return nil, errors.New("a notebook schedule needs a Cloud Storage bucket for its output")
			} else {
				cc.DefaultNotebookRuntimeOptions = nil
			}
			if _, err := s.UpdateReleaseConfig(ctx, &dfpb.UpdateReleaseConfigRequest{ReleaseConfig: &dfpb.ReleaseConfig{Name: rc, CodeCompilationConfig: cc},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"code_compilation_config"}}}); err != nil {
				return nil, err
			}
		}
	}
	return studioOneSchedule(ctx, s, in.Schedule)
}

// studioRunSelected is Run > Select tasks to run, Run by tag and a unit
// test run: main compiled, then invoked with the selection and options.
func studioRunSelected(ctx context.Context, s *dataform.Server, in studioSchedulingReq) (any, error) {
	o := in.Run
	if len(o.Tasks) == 0 && len(studioTags(o.Tags)) == 0 && !o.UnitTests {
		return nil, errors.New("select at least one task or tag to run")
	}
	cc := &dfpb.CodeCompilationConfig{}
	if b := strings.TrimSpace(in.Bucket); b != "" {
		if !strings.HasPrefix(b, "gs://") {
			b = "gs://" + b
		}
		cc.DefaultNotebookRuntimeOptions = &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: b},
			AiPlatformNotebookRuntimeTemplate: strings.TrimSpace(in.RuntimeTemplate)}
	}
	cr, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: in.Name,
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_GitCommitish{GitCommitish: "main"}, CodeCompilationConfig: cc}})
	if err != nil {
		return nil, err
	}
	if errs := cr.GetCompilationErrors(); len(errs) > 0 {
		return nil, fmt.Errorf("compilation failed: %s", studioCompileErrors(errs))
	}
	acts, err := s.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: cr.GetName(), PageSize: 1000})
	if err != nil {
		return nil, err
	}
	ic, err := studioInvocationConfig(cr, acts.GetCompilationResultActions(), o, strings.TrimSpace(in.ServiceAccount))
	if err != nil {
		return nil, err
	}
	inv, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: in.Name,
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: cr.GetName()}, InvocationConfig: ic}})
	if err != nil {
		return nil, err
	}
	return map[string]string{"invocation": inv.GetName(), "state": inv.GetState().String(), "compilationResult": cr.GetName()}, nil
}
