package main

// BigQuery Studio's scheduled notebooks, pipelines and data preparations
// (#2105), over Dataform as Google's Studio keeps them
// (https://docs.cloud.google.com/bigquery/docs/orchestrate-notebooks,
// create-pipelines, schedule-pipelines, data-prep-introduction; read
// 2026-11-22).
//
//   - A notebook schedule is, in the notebook asset's repository, a release
//     configuration (git_commitish main, pipeline_config NOTEBOOK on
//     content.ipynb, the Cloud Storage bucket and runtime template as its
//     default_notebook_runtime_options) and a workflow configuration (its
//     cron schedule, time zone and service account). Dataform runs the
//     schedule; Run compiles the release and invokes the workflow
//     configuration once.
//   - A pipeline is an asset repository whose tasks are Dataform actions:
//     definitions/actions.yaml declares its notebook tasks, a .sqlx file each
//     query and data preparation task, with workflow_settings.yaml. Studio's
//     editor keeps its model in content.pipeline.json beside them.
//   - A data preparation is an asset repository with one SQLX data
//     preparation (config type dataPreparation, a pipe-syntax query), its
//     editor's model in content.dataprep.json, as Google keeps one
//     ("saved in the SQLX file format", manage-data-preparations). A YAML
//     data preparation, Google's earlier form, opens and saves as its
//     .dp.yaml file (studiodataprepyaml.go).
//
// Run, for a pipeline or a data preparation, compiles main and invokes
// the compilation result; each action's state is read back as an
// execution's tasks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// dataformCoreVersion is the Dataform core the instance's Dataform CLI
// compiles with.
const dataformCoreVersion = "3.1.6"

// studioGeneratedKinds are the assets whose save also writes the Dataform
// project their model describes.
var studioGeneratedKinds = map[string]bool{"pipeline": true, "data_preparation": true}

// studioTaskName is what Google allows in a pipeline task's name:
// "letters, numbers, underscores, and hyphens" (create-pipelines).
var studioTaskName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// studioLoadModes are a data preparation's write modes (data-prep-introduction,
// read 2026-11-22): Full refresh (REPLACE), Append, Incremental (MAXIMUM
// for numeric and datetime columns, UNIQUE for categorical ones) and
// Upsert (MERGE, "Merges rows using the specified merge keys"), as
// Dataform core's SQLX loadMode names them (REPLACE_TABLE, APPEND,
// MAXIMUM, UNIQUE, MERGE; @dataform/core 3.1.6 configs.proto).
var studioLoadModes = map[string]string{"REPLACE": "REPLACE_TABLE", "REPLACE_TABLE": "REPLACE_TABLE", "APPEND": "APPEND", "MAXIMUM": "MAXIMUM", "UNIQUE": "UNIQUE", "MERGE": "MERGE"}

type studioPrepStep struct {
	Type       string `json:"type"`
	Expression string `json:"expression,omitempty"`
	Column     string `json:"column,omitempty"`
	Replace    bool   `json:"replace,omitempty"`
	Table      string `json:"table,omitempty"`
	JoinType   string `json:"joinType,omitempty"`
	Keys       string `json:"keys,omitempty"`
	Columns    string `json:"columns,omitempty"`
	// Validation makes a filter a validation: "Failed validation rows go
	// to error table" (data-prep-get-suggestions).
	Validation bool `json:"validation,omitempty"`
	// Keep keeps a flattened column (Google drops a flattened RECORD's
	// column unless its Drop column step is deleted).
	Keep bool `json:"keep,omitempty"`
	// JSON is an unnest of a JSON array, whose elements are JSON.
	JSON bool `json:"json,omitempty"`
}

type studioDataPrep struct {
	Source      string           `json:"source"`
	Steps       []studioPrepStep `json:"steps"`
	Destination string           `json:"destination"`
	LoadMode    string           `json:"loadMode"`
	LoadColumn  string           `json:"loadColumn,omitempty"`
	MergeKeys   string           `json:"mergeKeys,omitempty"`
	ErrorTable  string           `json:"errorTable,omitempty"`
	Retention   int              `json:"errorRetentionDays,omitempty"`
	// YAML is a YAML data preparation's file (studiodataprepyaml.go): the
	// editor shows its source, destination and error table, and saves it
	// back as YAML.
	YAML string `json:"yaml,omitempty"`
}

type studioPipelineTask struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	RunAfter        []string        `json:"runAfter"`
	SQL             string          `json:"sql,omitempty"`
	Notebook        string          `json:"notebook,omitempty"`
	DataPreparation *studioDataPrep `json:"dataPreparation,omitempty"`
	// Table, View, Declare source, Data quality test and Unit test tasks
	// (consolebigqueryscheduling.go): the target's project, dataset and
	// name, Table's Table or Incremental table and View's View or
	// Materialized view, and a unit test's action and inputs.
	Project     string            `json:"project,omitempty"`
	Dataset     string            `json:"dataset,omitempty"`
	Table       string            `json:"table,omitempty"`
	TableType   string            `json:"tableType,omitempty"`
	UniqueKey   string            `json:"uniqueKey,omitempty"`
	Description string            `json:"description,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	TestedTask  string            `json:"testedTask,omitempty"`
	Inputs      []studioUnitInput `json:"inputs,omitempty"`
}

type studioPipelineSettings struct {
	ServiceAccount  string `json:"serviceAccount,omitempty"`
	Location        string `json:"location,omitempty"`
	DefaultProject  string `json:"defaultProject,omitempty"`
	DefaultDataset  string `json:"defaultDataset,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	RuntimeTemplate string `json:"runtimeTemplate,omitempty"`
}

type studioPipeline struct {
	Settings studioPipelineSettings `json:"settings"`
	Tasks    []studioPipelineTask   `json:"tasks"`
}

// studioTableRef splits project.dataset.table (or dataset.table, in the
// project) into its parts.
func studioTableRef(project, ref string) (p, d, t string, err error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(ref), "`"), ".")
	switch len(parts) {
	case 2:
		p, d, t = project, parts[0], parts[1]
	case 3:
		p, d, t = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("%q is not a table: write dataset.table or project.dataset.table", ref)
	}
	for _, s := range []string{p, d, t} {
		if s == "" || strings.ContainsAny(s, "`\n") {
			return "", "", "", fmt.Errorf("%q is not a table: write dataset.table or project.dataset.table", ref)
		}
	}
	return p, d, t, nil
}

func studioQuoteTable(project, ref string) (string, error) {
	p, d, t, err := studioTableRef(project, ref)
	if err != nil {
		return "", err
	}
	return "`" + p + "." + d + "." + t + "`", nil
}

// studioIdent quotes a column name.
func studioIdent(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "`\n") {
		return "", fmt.Errorf("%q is not a column name", s)
	}
	return "`" + s + "`", nil
}

func studioIdents(list string) (string, error) {
	var out []string
	for _, c := range strings.Split(list, ",") {
		if strings.TrimSpace(c) == "" {
			continue
		}
		q, err := studioIdent(c)
		if err != nil {
			return "", err
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		return "", errors.New("name at least one column")
	}
	return strings.Join(out, ", "), nil
}

// dataPrepQuery is a data preparation's steps as one pipe-syntax query:
// a validation is a WHERE (the rows that pass it), as a sample shows it.
func dataPrepQuery(project string, m studioDataPrep) (string, error) {
	return dataPrepPipe(project, m, false)
}

// dataPrepPipe is the steps' query; in SQLX a validation is Dataform
// core's ${EXPECT}, whose failing rows go to the error table.
func dataPrepPipe(project string, m studioDataPrep, sqlx bool) (string, error) {
	if m.YAML != "" {
		return yamlDataPrepQuery(project, m)
	}
	src, err := studioQuoteTable(project, m.Source)
	if err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	lines := []string{"FROM " + src}
	for i, st := range m.Steps {
		bad := func(what string) error { return fmt.Errorf("step %d (%s): %s", i+1, st.Type, what) }
		expr := strings.TrimSpace(st.Expression)
		switch st.Type {
		case "filter":
			if expr == "" {
				return "", bad("a filter needs a condition")
			}
			if st.Validation && sqlx {
				lines = append(lines, "|> ${EXPECT} "+expr)
			} else {
				lines = append(lines, "|> WHERE "+expr)
			}
		case "flatten":
			// Flatten a RECORD (its nested columns as PARENT_FIELD, the
			// column dropped) or a JSON column (its top-level keys): the
			// editor generates the expressions from the sample, and they
			// stay editable, as Google's suggestions do.
			col, err := studioIdent(st.Column)
			if err != nil || expr == "" {
				return "", bad("a flatten needs a column and its expressions")
			}
			if st.Keep {
				lines = append(lines, "|> SELECT *, "+expr)
			} else {
				lines = append(lines, "|> SELECT * EXCEPT ("+col+"), "+expr)
			}
		case "unnest":
			// "expands each element in an array into its own row,
			// duplicating the other original column values into each new
			// row. By default, the original array column is dropped."
			col, err := studioIdent(st.Column)
			if err != nil {
				return "", bad("an unnest needs an array column")
			}
			arr := "`dp_unnest_input`." + col
			if st.JSON {
				arr = "JSON_QUERY_ARRAY(" + arr + ")"
			}
			lines = append(lines, "|> AS `dp_unnest_input`", "|> CROSS JOIN UNNEST("+arr+") AS `dp_unnest_element`",
				"|> SELECT * EXCEPT ("+col+", `dp_unnest_element`), `dp_unnest_element` AS "+col)
		case "transform":
			col, err := studioIdent(st.Column)
			if err != nil || expr == "" {
				return "", bad("a transformation needs a column and an expression")
			}
			if st.Replace {
				lines = append(lines, "|> SET "+col+" = "+expr)
			} else {
				lines = append(lines, "|> EXTEND "+expr+" AS "+col)
			}
		case "join":
			t, err := studioQuoteTable(project, st.Table)
			if err != nil {
				return "", bad(err.Error())
			}
			jt := strings.ToUpper(strings.TrimSpace(st.JoinType))
			if jt == "" {
				jt = "INNER"
			}
			if jt != "INNER" && jt != "LEFT" && jt != "RIGHT" && jt != "FULL" && jt != "CROSS" {
				return "", bad(jt + " is not a join type: INNER, LEFT, RIGHT, FULL or CROSS")
			}
			if jt == "CROSS" {
				lines = append(lines, "|> CROSS JOIN "+t)
				continue
			}
			if expr == "" {
				return "", bad("a join needs a join condition")
			}
			lines = append(lines, "|> "+jt+" JOIN "+t+" ON "+expr)
		case "aggregate":
			if expr == "" {
				return "", bad("an aggregation needs its aggregation expressions")
			}
			line := "|> AGGREGATE " + expr
			if strings.TrimSpace(st.Keys) != "" {
				keys, err := studioIdents(st.Keys)
				if err != nil {
					return "", bad(err.Error())
				}
				line += " GROUP BY " + keys
			}
			lines = append(lines, line)
		case "deduplicate":
			lines = append(lines, "|> SELECT DISTINCT *")
		case "delete":
			cols, err := studioIdents(st.Columns)
			if err != nil {
				return "", bad(err.Error())
			}
			lines = append(lines, "|> DROP "+cols)
		default:
			return "", bad("not a step type: filter, transform, join, aggregate, deduplicate, delete, flatten or unnest")
		}
	}
	return strings.Join(lines, "\n"), nil
}

// dataPrepSQLX is a data preparation as Dataform core's SQLX action: its
// destination, error table and load mode in its config, its query after.
func dataPrepSQLX(project string, m studioDataPrep, deps []string) (string, error) {
	if m.YAML != "" {
		return "", errors.New("a YAML data preparation is not SQLX: migrate it to SQLX first")
	}
	q, err := dataPrepPipe(project, m, true)
	if err != nil {
		return "", err
	}
	p, d, t, err := studioTableRef(project, m.Destination)
	if err != nil {
		return "", fmt.Errorf("destination: %w", err)
	}
	mode := strings.ToUpper(strings.TrimSpace(m.LoadMode))
	if mode == "" {
		mode = "REPLACE"
	}
	mode, ok := studioLoadModes[mode]
	if !ok {
		return "", fmt.Errorf("%q is not a load mode: REPLACE, APPEND, MAXIMUM, UNIQUE or MERGE", m.LoadMode)
	}
	js := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	cfg := []string{`  type: "dataPreparation"`, "  project: " + js(p), "  dataset: " + js(d), "  name: " + js(t)}
	if et := strings.TrimSpace(m.ErrorTable); et != "" {
		ep, ed, en, err := studioTableRef(project, et)
		if err != nil {
			return "", fmt.Errorf("error table: %w", err)
		}
		days := m.Retention
		if days <= 0 {
			days = 30
		}
		cfg = append(cfg, fmt.Sprintf("  errorTable: { project: %s, dataset: %s, name: %s, retentionDays: %d }", js(ep), js(ed), js(en), days))
	}
	lm := "  loadMode: { mode: " + js(mode)
	switch mode {
	case "MAXIMUM", "UNIQUE":
		if strings.TrimSpace(m.LoadColumn) == "" {
			return "", fmt.Errorf("the %s load mode needs a column", mode)
		}
		lm += ", incrementalColumn: " + js(strings.TrimSpace(m.LoadColumn))
	case "MERGE":
		var keys []string
		for _, k := range strings.Split(m.MergeKeys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				if strings.ContainsAny(k, "`\n\"") {
					return "", fmt.Errorf("%q is not a column name", k)
				}
				keys = append(keys, js(k))
			}
		}
		if len(keys) == 0 {
			return "", errors.New("Upsert needs its merge keys")
		}
		lm += ", uniqueKey: [" + strings.Join(keys, ", ") + "]"
	}
	cfg = append(cfg, lm+" }")
	if len(deps) > 0 {
		quoted := make([]string, len(deps))
		for i, x := range deps {
			quoted[i] = js(x)
		}
		cfg = append(cfg, "  dependencies: ["+strings.Join(quoted, ", ")+"]")
	}
	return "config {\n" + strings.Join(cfg, ",\n") + "\n}\n\n" + q + "\n", nil
}

// workflowSettingsYAML is a generated project's workflow_settings.yaml.
func workflowSettingsYAML(project string, s studioPipelineSettings) string {
	js := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	if s.DefaultProject == "" {
		s.DefaultProject = project
	}
	if s.DefaultDataset == "" {
		s.DefaultDataset = "dataform"
	}
	if s.Location == "" {
		s.Location = "US"
	}
	out := "defaultProject: " + js(s.DefaultProject) + "\n" +
		"defaultLocation: " + js(s.Location) + "\n" +
		"defaultDataset: " + js(s.DefaultDataset) + "\n" +
		"defaultAssertionDataset: " + js("dataform_assertions") + "\n" +
		"dataformCoreVersion: " + js(dataformCoreVersion) + "\n"
	if s.Bucket != "" {
		out += "defaultNotebookRuntimeOptions:\n  outputBucket: " + js(s.Bucket) + "\n"
		if s.RuntimeTemplate != "" {
			out += "  runtimeTemplateName: " + js(s.RuntimeTemplate) + "\n"
		}
	}
	return out
}

// studioGeneratedFiles is the Dataform project a pipeline's or data
// preparation's model describes: path to contents.
func studioGeneratedFiles(project, kind, content string) (map[string]string, error) {
	switch kind {
	case "data_preparation":
		var m studioDataPrep
		if err := json.Unmarshal([]byte(content), &m); err != nil {
			return nil, fmt.Errorf("a data preparation: %w", err)
		}
		if m.YAML != "" {
			return yamlDataPrepFiles(project, m)
		}
		sqlx, err := dataPrepSQLX(project, m, nil)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"workflow_settings.yaml":            workflowSettingsYAML(project, studioPipelineSettings{}),
			"definitions/data_preparation.sqlx": sqlx,
		}, nil
	case "pipeline":
		var m studioPipeline
		if err := json.Unmarshal([]byte(content), &m); err != nil {
			return nil, fmt.Errorf("a pipeline: %w", err)
		}
		return pipelineFiles(project, m)
	}
	return nil, nil
}

func pipelineFiles(project string, m studioPipeline) (map[string]string, error) {
	files := map[string]string{"workflow_settings.yaml": workflowSettingsYAML(project, m.Settings)}
	names := map[string]bool{}
	for _, t := range m.Tasks {
		if !studioTaskName.MatchString(t.Name) {
			return nil, fmt.Errorf("task name %q: letters, numbers, underscores and hyphens only", t.Name)
		}
		if names[t.Name] {
			return nil, fmt.Errorf("two tasks are named %q", t.Name)
		}
		names[t.Name] = true
	}
	js := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	// A task's Run after names other tasks; Dataform's dependencies name
	// their targets, a table's own name where it has one.
	targetOf := map[string]string{}
	for _, t := range m.Tasks {
		targetOf[t.Name] = studioTaskTarget(t)
		if t.Type == "data_preparation" && t.DataPreparation != nil {
			if _, _, tbl, err := studioTableRef(project, t.DataPreparation.Destination); err == nil {
				targetOf[t.Name] = tbl
			}
		}
	}
	var notebooks []string
	for _, t := range m.Tasks {
		for _, d := range t.RunAfter {
			if !names[d] {
				return nil, fmt.Errorf("task %s runs after %q, which is not a task of this pipeline", t.Name, d)
			}
			if d == t.Name {
				return nil, fmt.Errorf("task %s cannot run after itself", t.Name)
			}
		}
		depTargets := make([]string, len(t.RunAfter))
		for i, d := range t.RunAfter {
			depTargets[i] = targetOf[d]
		}
		switch t.Type {
		case "sql", "table", "view", "declaration", "assertion", "unit_test":
			f, err := pipelineTaskFile(t, depTargets)
			if err != nil {
				return nil, err
			}
			files["definitions/"+t.Name+".sqlx"] = f
		case "notebook", "spark_notebook":
			nb := t.Notebook
			if strings.TrimSpace(nb) == "" {
				nb = `{"nbformat":4,"nbformat_minor":5,"metadata":{},"cells":[]}`
			}
			if !json.Valid([]byte(nb)) {
				return nil, fmt.Errorf("task %s: its notebook is not JSON", t.Name)
			}
			files["definitions/"+t.Name+".ipynb"] = nb
			entry := "  - notebook:\n      name: " + js(t.Name) + "\n      filename: " + js(t.Name+".ipynb") + "\n"
			if len(t.RunAfter) > 0 {
				entry += "      dependencyTargets:\n"
				for _, d := range depTargets {
					entry += "        - name: " + js(d) + "\n"
				}
			}
			if tags := studioTags(t.Tags); len(tags) > 0 {
				entry += "      tags:\n"
				for _, x := range tags {
					entry += "        - " + js(x) + "\n"
				}
			}
			notebooks = append(notebooks, entry)
		case "data_preparation":
			if t.DataPreparation == nil {
				return nil, fmt.Errorf("task %s has no data preparation", t.Name)
			}
			sqlx, err := dataPrepSQLX(project, *t.DataPreparation, depTargets)
			if err != nil {
				return nil, fmt.Errorf("task %s: %w", t.Name, err)
			}
			files["definitions/"+t.Name+".sqlx"] = sqlx
		default:
			return nil, fmt.Errorf("task %s: %q is not a task type: sql, notebook, spark_notebook, data_preparation, table, view, declaration, assertion or unit_test", t.Name, t.Type)
		}
	}
	if len(notebooks) > 0 {
		files["definitions/actions.yaml"] = "actions:\n" + strings.Join(notebooks, "")
	}
	return files, nil
}

// studioGeneratedOps is the commit's file operations for a generated
// project: every file written, and every older file under definitions/
// that the model no longer has deleted.
func studioGeneratedOps(ctx context.Context, s *dataform.Server, repo string, files map[string]string) map[string]*dfpb.CommitRepositoryChangesRequest_FileOperation {
	ops := map[string]*dfpb.CommitRepositoryChangesRequest_FileOperation{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 8 {
			return
		}
		resp, err := s.QueryRepositoryDirectoryContents(ctx, &dfpb.QueryRepositoryDirectoryContentsRequest{Name: repo, Path: dir, PageSize: 1000})
		if err != nil {
			return
		}
		for _, e := range resp.GetDirectoryEntries() {
			if f := e.GetFile(); f != "" {
				if _, keep := files[f]; !keep {
					ops[f] = &dfpb.CommitRepositoryChangesRequest_FileOperation{Operation: &dfpb.CommitRepositoryChangesRequest_FileOperation_DeleteFile_{DeleteFile: &dfpb.CommitRepositoryChangesRequest_FileOperation_DeleteFile{}}}
				}
			} else if d := e.GetDirectory(); d != "" {
				walk(d, depth+1)
			}
		}
	}
	walk("definitions", 0)
	for p, c := range files {
		ops[p] = &dfpb.CommitRepositoryChangesRequest_FileOperation{Operation: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile_{
			WriteFile: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile{Contents: []byte(c)}}}
	}
	return ops
}

// studioOrchReq is a schedule, run or execution call's body.
type studioOrchReq struct {
	Name            string `json:"name"`
	Schedule        string `json:"schedule"`
	Invocation      string `json:"invocation"`
	DisplayName     string `json:"displayName"`
	Cron            string `json:"cron"`
	TimeZone        string `json:"timeZone"`
	ServiceAccount  string `json:"serviceAccount"`
	Bucket          string `json:"bucket"`
	RuntimeTemplate string `json:"runtimeTemplate"`
	Disabled        bool   `json:"disabled"`
	Content         string `json:"content"`
	// Batch is Execute as interactive job with high priority cleared;
	// MaxConcurrent, Limit concurrent actions' Maximum concurrent actions.
	Batch         bool  `json:"batch"`
	MaxConcurrent int32 `json:"maxConcurrent"`
}

type studioSchedule struct {
	Name            string   `json:"name"`
	ID              string   `json:"id"`
	Cron            string   `json:"cron"`
	TimeZone        string   `json:"timeZone"`
	ServiceAccount  string   `json:"serviceAccount"`
	Bucket          string   `json:"bucket"`
	RuntimeTemplate string   `json:"runtimeTemplate"`
	Disabled        bool     `json:"disabled"`
	Created         string   `json:"created"`
	Updated         string   `json:"updated"`
	Recent          []string `json:"recent"`
}

type studioExecution struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	State    string `json:"state"`
	Schedule string `json:"schedule"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

type studioTaskRun struct {
	Target       string `json:"target"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Failure      string `json:"failure,omitempty"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	SQL          string `json:"sql,omitempty"`
	JobID        string `json:"jobId,omitempty"`
	Notebook     string `json:"notebook,omitempty"`
	FilePath     string `json:"filePath,omitempty"`
	OutputFolder string `json:"outputFolder,omitempty"`
	OutputObject string `json:"outputObject,omitempty"`
}

// studioOrchestration answers the schedule, run and execution calls; ok
// is false for a call it does not know.
func studioOrchestration(ctx context.Context, s *dataform.Server, project, call string, body json.RawMessage) (any, bool, error) {
	var in studioOrchReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, true, fmt.Errorf("a schedule request: %w", err)
		}
	}
	for _, n := range []string{in.Name, in.Schedule, in.Invocation} {
		if n != "" {
			if err := kmsOwned(project, n); err != nil {
				return nil, true, err
			}
		}
	}
	var out any
	var err error
	switch call {
	case "schedules":
		out, err = studioSchedules(ctx, s, in.Name)
	case "createschedule":
		out, err = studioCreateSchedule(ctx, s, in)
	case "deleteschedule":
		out, err = studioDeleteSchedule(ctx, s, in.Schedule)
	case "toggleschedule":
		_, err = s.UpdateWorkflowConfig(ctx, &dfpb.UpdateWorkflowConfigRequest{WorkflowConfig: &dfpb.WorkflowConfig{Name: in.Schedule, Disabled: in.Disabled},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
		out = map[string]any{"schedule": in.Schedule, "disabled": in.Disabled}
	case "runschedule":
		out, err = studioRunSchedule(ctx, s, in.Schedule)
	case "run":
		out, err = studioRun(ctx, s, in)
	case "executions":
		out, err = studioExecutions(ctx, s, in.Name, in.Schedule)
	case "execution":
		out, err = studioExecutionDetail(ctx, s, in.Invocation)
	case "cancelexecution":
		_, err = s.CancelWorkflowInvocation(ctx, &dfpb.CancelWorkflowInvocationRequest{Name: in.Invocation})
		out = map[string]string{"cancelled": in.Invocation}
	case "dataprepsql":
		out, err = studioDataPrepSQL(project, in.Content)
	case "dataprepyaml":
		// More > Open YAML: a YAML data preparation as the editor's model.
		out, err = studioDataPrepFromYAML(in.Content)
	default:
		// The Scheduling page, triggers and Run's selection
		// (consolebigqueryscheduling.go).
		return studioScheduling(ctx, s, project, call, body)
	}
	return out, true, err
}

func studioDataPrepSQL(project, content string) (any, error) {
	var m studioDataPrep
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return nil, fmt.Errorf("a data preparation: %w", err)
	}
	q, err := dataPrepQuery(project, m)
	if err != nil {
		return nil, err
	}
	out := map[string]string{"sql": q}
	if m.YAML != "" {
		y, err := dataPrepYAML(project, m)
		if err != nil {
			out["yamlError"] = err.Error()
		} else {
			out["yaml"] = y
		}
		return out, nil
	}
	if strings.TrimSpace(m.Destination) != "" {
		if sqlx, err := dataPrepSQLX(project, m, nil); err == nil {
			out["sqlx"] = sqlx
		} else {
			out["sqlxError"] = err.Error()
		}
	}
	return out, nil
}

// studioScheduleID is a schedule's configuration ID from its name.
func studioScheduleID(name string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	id := b.String()
	if id == "" {
		return "", errors.New("a schedule needs a name: letters, numbers, underscores and hyphens")
	}
	if len(id) > 63 {
		id = id[:63]
	}
	return id, nil
}

func studioSchedules(ctx context.Context, s *dataform.Server, repo string) (any, error) {
	resp, err := s.ListWorkflowConfigs(ctx, &dfpb.ListWorkflowConfigsRequest{Parent: repo, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	out := []studioSchedule{}
	for _, wc := range resp.GetWorkflowConfigs() {
		sc := studioSchedule{Name: wc.GetName(), ID: lastSegment(wc.GetName()), Cron: wc.GetCronSchedule(), TimeZone: wc.GetTimeZone(),
			ServiceAccount: wc.GetInvocationConfig().GetServiceAccount(), Disabled: wc.GetDisabled(),
			Created: kmsTime(wc.GetCreateTime()), Updated: kmsTime(wc.GetUpdateTime()), Recent: []string{}}
		if rc, err := s.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: wc.GetReleaseConfig()}); err == nil {
			o := rc.GetCodeCompilationConfig().GetDefaultNotebookRuntimeOptions()
			sc.Bucket = o.GetGcsOutputBucket()
			sc.RuntimeTemplate = o.GetAiPlatformNotebookRuntimeTemplate()
		}
		for _, r := range wc.GetRecentScheduledExecutionRecords() {
			if inv := r.GetWorkflowInvocation(); inv != "" {
				sc.Recent = append(sc.Recent, inv)
			}
		}
		out = append(out, sc)
	}
	return map[string]any{"schedules": out}, nil
}

// studioCreateSchedule is Create schedule: the release configuration and
// the workflow configuration, named after the schedule, then a first
// compilation, so Run works at once.
func studioCreateSchedule(ctx context.Context, s *dataform.Server, in studioOrchReq) (any, error) {
	r, err := s.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: in.Name})
	if err != nil {
		return nil, err
	}
	kind := r.GetLabels()[dataformAssetLabel]
	if kind != "notebook" && kind != "pipeline" && kind != "data_preparation" {
		return nil, fmt.Errorf("%s is not a notebook, pipeline or data preparation", studioDisplay(r))
	}
	id, err := studioScheduleID(in.DisplayName)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Cron) == "" {
		return nil, errors.New("a schedule needs a frequency")
	}
	bucket := strings.TrimSpace(in.Bucket)
	if bucket != "" && !strings.HasPrefix(bucket, "gs://") {
		bucket = "gs://" + bucket
	}
	if kind == "notebook" && bucket == "" {
		return nil, errors.New("a notebook schedule needs a Cloud Storage bucket for its output")
	}
	cc := &dfpb.CodeCompilationConfig{}
	if kind == "notebook" {
		cc.PipelineConfig = &dfpb.PipelineConfig{PipelineType: dfpb.PipelineConfig_NOTEBOOK, Path: dataformAssetFiles["notebook"]}
	}
	if bucket != "" {
		cc.DefaultNotebookRuntimeOptions = &dfpb.NotebookRuntimeOptions{ExecutionSink: &dfpb.NotebookRuntimeOptions_GcsOutputBucket{GcsOutputBucket: bucket},
			AiPlatformNotebookRuntimeTemplate: strings.TrimSpace(in.RuntimeTemplate)}
	}
	ic, err := studioInvocationConfig(nil, nil, studioRunOptions{Batch: in.Batch, MaxConcurrent: in.MaxConcurrent}, strings.TrimSpace(in.ServiceAccount))
	if err != nil {
		return nil, err
	}
	rc, err := s.CreateReleaseConfig(ctx, &dfpb.CreateReleaseConfigRequest{Parent: in.Name, ReleaseConfigId: id,
		ReleaseConfig: &dfpb.ReleaseConfig{GitCommitish: "main", CodeCompilationConfig: cc}})
	if err != nil {
		return nil, err
	}
	wc, err := s.CreateWorkflowConfig(ctx, &dfpb.CreateWorkflowConfigRequest{Parent: in.Name, WorkflowConfigId: id,
		WorkflowConfig: &dfpb.WorkflowConfig{ReleaseConfig: rc.GetName(), CronSchedule: strings.TrimSpace(in.Cron), TimeZone: strings.TrimSpace(in.TimeZone),
			InvocationConfig: ic}})
	if err != nil {
		_, _ = s.DeleteReleaseConfig(ctx, &dfpb.DeleteReleaseConfigRequest{Name: rc.GetName()})
		return nil, err
	}
	if _, err := studioCompileRelease(ctx, s, rc.GetName()); err != nil {
		return map[string]any{"name": wc.GetName(), "id": id, "warning": "the schedule is created, but its first compilation failed: " + err.Error()}, nil
	}
	return map[string]any{"name": wc.GetName(), "id": id}, nil
}

// studioCompileRelease compiles a release configuration and makes the
// result its release compilation result, which a workflow configuration's
// invocation runs.
func studioCompileRelease(ctx context.Context, s *dataform.Server, release string) (*dfpb.CompilationResult, error) {
	repo := release[:strings.Index(release, "/releaseConfigs/")]
	cr, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo,
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_ReleaseConfig{ReleaseConfig: release}}})
	if err != nil {
		return nil, err
	}
	if errs := cr.GetCompilationErrors(); len(errs) > 0 {
		return nil, fmt.Errorf("compilation failed: %s", studioCompileErrors(errs))
	}
	if _, err := s.UpdateReleaseConfig(ctx, &dfpb.UpdateReleaseConfigRequest{ReleaseConfig: &dfpb.ReleaseConfig{Name: release, ReleaseCompilationResult: cr.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"release_compilation_result"}}}); err != nil {
		return nil, err
	}
	return cr, nil
}

func studioCompileErrors(errs []*dfpb.CompilationResult_CompilationError) string {
	var msgs []string
	for _, e := range errs {
		m := e.GetMessage()
		if p := e.GetPath(); p != "" {
			m = p + ": " + m
		}
		msgs = append(msgs, m)
	}
	return strings.Join(msgs, "; ")
}

func studioDeleteSchedule(ctx context.Context, s *dataform.Server, schedule string) (any, error) {
	wc, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: schedule})
	if err != nil {
		return nil, err
	}
	if _, err := s.DeleteWorkflowConfig(ctx, &dfpb.DeleteWorkflowConfigRequest{Name: schedule}); err != nil {
		return nil, err
	}
	// The release configuration was made for this schedule alone.
	if rc := wc.GetReleaseConfig(); rc != "" && lastSegment(rc) == lastSegment(schedule) {
		_, _ = s.DeleteReleaseConfig(ctx, &dfpb.DeleteReleaseConfigRequest{Name: rc})
	}
	return map[string]string{"deleted": schedule}, nil
}

// studioRunSchedule is a schedule's Run: the latest saved version
// compiled, then one invocation of the workflow configuration.
func studioRunSchedule(ctx context.Context, s *dataform.Server, schedule string) (any, error) {
	wc, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: schedule})
	if err != nil {
		return nil, err
	}
	if _, err := studioCompileRelease(ctx, s, wc.GetReleaseConfig()); err != nil {
		return nil, err
	}
	repo := schedule[:strings.Index(schedule, "/workflowConfigs/")]
	inv, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo,
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_WorkflowConfig{WorkflowConfig: schedule}}})
	if err != nil {
		return nil, err
	}
	return map[string]string{"invocation": inv.GetName(), "state": inv.GetState().String()}, nil
}

// studioRun is a pipeline's or data preparation's Run (Run all tasks):
// main compiled, then the compilation result invoked.
func studioRun(ctx context.Context, s *dataform.Server, in studioOrchReq) (any, error) {
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
	inv, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: in.Name,
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: cr.GetName()},
			InvocationConfig: &dfpb.InvocationConfig{ServiceAccount: strings.TrimSpace(in.ServiceAccount)}}})
	if err != nil {
		return nil, err
	}
	return map[string]string{"invocation": inv.GetName(), "state": inv.GetState().String(), "compilationResult": cr.GetName()}, nil
}

// studioExecutions is an asset's executions (every one, or one schedule's),
// newest first.
func studioExecutions(ctx context.Context, s *dataform.Server, repo, schedule string) (any, error) {
	resp, err := s.ListWorkflowInvocations(ctx, &dfpb.ListWorkflowInvocationsRequest{Parent: repo, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	type row struct {
		studioExecution
		at string
	}
	var rows []row
	for _, inv := range resp.GetWorkflowInvocations() {
		if schedule != "" && inv.GetWorkflowConfig() != schedule {
			continue
		}
		t := inv.GetInvocationTiming()
		e := studioExecution{Name: inv.GetName(), ID: lastSegment(inv.GetName()), State: inv.GetState().String(),
			Schedule: lastSegment(inv.GetWorkflowConfig()), Start: kmsTime(t.GetStartTime()), End: kmsTime(t.GetEndTime())}
		at := ""
		if st := t.GetStartTime(); st != nil {
			at = st.AsTime().UTC().Format("20060102150405.000000000")
		}
		rows = append(rows, row{e, at})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at > rows[j].at })
	out := []studioExecution{}
	for _, r := range rows {
		out = append(out, r.studioExecution)
	}
	return map[string]any{"executions": out}, nil
}

// studioExecutionDetail is an execution's tasks: each action's state,
// failure reason and SQL, and a notebook's input contents with the
// Cloud Storage object its executed notebook was written to
// (gs://BUCKET/JOB_ID/FILE), which the screen reads from Cloud Storage.
func studioExecutionDetail(ctx context.Context, s *dataform.Server, name string) (any, error) {
	inv, err := s.GetWorkflowInvocation(ctx, &dfpb.GetWorkflowInvocationRequest{Name: name})
	if err != nil {
		return nil, err
	}
	bucket := ""
	cr := inv.GetResolvedCompilationResult()
	if cr == "" {
		cr = inv.GetCompilationResult()
	}
	if cr != "" {
		if c, err := s.GetCompilationResult(ctx, &dfpb.GetCompilationResultRequest{Name: cr}); err == nil {
			bucket = c.GetCodeCompilationConfig().GetDefaultNotebookRuntimeOptions().GetGcsOutputBucket()
		}
	}
	tasks := []studioTaskRun{}
	resp, err := s.QueryWorkflowInvocationActions(ctx, &dfpb.QueryWorkflowInvocationActionsRequest{Name: name, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	for _, a := range resp.GetWorkflowInvocationActions() {
		t := studioTaskRun{Target: a.GetTarget().GetName(), State: a.GetState().String(), Failure: a.GetFailureReason(),
			Start: kmsTime(a.GetInvocationTiming().GetStartTime()), End: kmsTime(a.GetInvocationTiming().GetEndTime())}
		if d := a.GetTarget().GetSchema(); d != "" {
			t.Target = d + "." + t.Target
		}
		switch {
		case a.GetNotebookAction() != nil:
			n := a.GetNotebookAction()
			t.Kind, t.Notebook, t.JobID, t.FilePath = "notebook", n.GetContents(), n.GetJobId(), n.GetFilePath()
			// notebook_action.contents is the notebook as it was run; the
			// executed one, with its outputs, is the object
			// gs://BUCKET/JOB_ID/<file name>, written even when a cell fails.
			if bucket != "" && t.JobID != "" {
				t.OutputFolder = strings.TrimSuffix(bucket, "/") + "/" + t.JobID + "/"
				base := path.Base(t.FilePath)
				if t.FilePath == "" || base == "." || base == "/" {
					base = dataformAssetFiles["notebook"]
				}
				t.OutputObject = t.OutputFolder + base
			}
		case a.GetDataPreparationAction() != nil:
			d := a.GetDataPreparationAction()
			t.Kind, t.SQL, t.JobID = "data_preparation", d.GetGeneratedSql(), d.GetJobId()
			if t.SQL == "" {
				t.SQL = d.GetContentsSql().GetQuery()
			}
		default:
			b := a.GetBigqueryAction()
			t.Kind, t.SQL, t.JobID = "sql", b.GetSqlScript(), b.GetJobId()
		}
		tasks = append(tasks, t)
	}
	t := inv.GetInvocationTiming()
	return map[string]any{"name": inv.GetName(), "id": lastSegment(inv.GetName()), "state": inv.GetState().String(),
		"schedule": lastSegment(inv.GetWorkflowConfig()), "start": kmsTime(t.GetStartTime()), "end": kmsTime(t.GetEndTime()),
		"outputBucket": bucket, "tasks": tasks, "done": studioInvocationDone(inv.GetState())}, nil
}

func studioInvocationDone(st dfpb.WorkflowInvocation_State) bool {
	switch st {
	case dfpb.WorkflowInvocation_SUCCEEDED, dfpb.WorkflowInvocation_FAILED, dfpb.WorkflowInvocation_CANCELLED:
		return true
	}
	return false
}
