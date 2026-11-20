package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// workflowsProvider is the console's Workflows page (#1986): a project's
// workflows in every region, Create (the source editor, with Google's
// sample), a workflow's page with its Source, Executions and Revisions
// tabs, Edit (a new revision), Execute and Delete; an execution's page with
// its argument, result or error and its Steps (the step entries), and
// Cancel. Paths are [workflow] and [workflow, execution].
//
// #2004: the source is edited in Google's editor, its syntax highlighted
// beside the graph of its steps (the "workflowsource" field, the Source
// tab's "workflows" language); the form has Google's Encryption, Call log
// level, Execution history and Environment variables; an execution's page
// has Google's Summary, Steps (with the visualization) and Logs tabs, and
// a workflow's page its Logs, read from the instance's Cloud Logging.
type workflowsProvider struct {
	svc *workflowsService
	// logs is the instance's Cloud Logging, which executions write their
	// start, end, sys.log and call logs to; nil without it.
	logs *loggingService
}

const workflowsNotStarted = "Workflows has not started"

func (workflowsProvider) ID() string    { return "workflows" }
func (workflowsProvider) Title() string { return "Workflows" }

// workflowsSample is the sample source Google's Create workflow offers.
const workflowsSample = `# This is a sample workflow. It calls a Cloud Run service of this instance
# and returns the response's body.
main:
  params: [input]
  steps:
    - init:
        assign:
          - name: ${default(map.get(input, "name"), "world")}
    - greet:
        return: ${"Hello, " + name}
`

func (p workflowsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s := p.svc.API()
	if s == nil {
		return console.Listing{}, errors.New(workflowsNotStarted)
	}
	listing := console.Listing{Columns: []string{"Location", "Revision", "Last updated", "Description", "Labels"}, NameColumn: "Name", Noun: "workflows"}
	if project == "" {
		listing.Prompt = "Workflows lists workflows per project. Choose one in the toolbar."
		return listing, nil
	}
	resp, err := s.Workflows().ListWorkflows(ctx, &workflowspb.ListWorkflowsRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, w := range resp.GetWorkflows() {
		listing.Items = append(listing.Items, console.Resource{Name: w.GetName(), Status: w.GetState().String(), Fields: map[string]string{
			"Location": kmsLocation(w.GetName()), "Revision": w.GetRevisionId(), "Last updated": kmsTime(w.GetUpdateTime()),
			"Description": kmsOrDash(w.GetDescription()), "Labels": kmsOrDash(labelsText(w.GetLabels()))}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func workflowsRegions() []string {
	out := append([]string(nil), locations.Regions...)
	sort.Strings(out)
	return out
}

// workflowsCallLogLevels are Google's Call log level choices, in its
// order (https://docs.cloud.google.com/workflows/docs/creating-updating-workflow).
var workflowsCallLogLevels = []string{"CALL_LOG_LEVEL_UNSPECIFIED", "LOG_ERRORS_ONLY", "LOG_ALL_CALLS", "LOG_NONE"}

var workflowsCallLogLabels = map[string]string{"CALL_LOG_LEVEL_UNSPECIFIED": "Not specified", "LOG_ERRORS_ONLY": "Errors only",
	"LOG_ALL_CALLS": "All calls", "LOG_NONE": "No logs"}

// workflowsHistoryLevels are Google's Execution history choices: Basic,
// the default, and Detailed.
var workflowsHistoryLevels = []string{"EXECUTION_HISTORY_BASIC", "EXECUTION_HISTORY_DETAILED"}

var workflowsHistoryLabels = map[string]string{"EXECUTION_HISTORY_LEVEL_UNSPECIFIED": "Basic", "EXECUTION_HISTORY_BASIC": "Basic",
	"EXECUTION_HISTORY_DETAILED": "Detailed"}

// workflowsEnvKey is what Workflows accepts as an environment variable's
// name: letters, digits and underscores, not starting with a digit
// (https://docs.cloud.google.com/workflows/docs/use-environment-variables).
// The GOOGLE and WORKFLOWS prefixes it reserves are the API's to refuse,
// a lookahead being beyond Go's regexp.
const workflowsEnvKey = `^[A-Za-z_][A-Za-z0-9_]*$`

// workflowsFields are the fields Google's Create workflow and Edit share,
// in its order, holding w's values (nil for a new workflow).
func workflowsFields(w *workflowspb.Workflow) []console.Field {
	enc, key := "google", ""
	desc, sa, lvl, hist, src := "", "", "CALL_LOG_LEVEL_UNSPECIFIED", "EXECUTION_HISTORY_BASIC", workflowsSample
	var labels, env map[string]string
	if w != nil {
		if key = w.GetCryptoKeyName(); key != "" {
			enc = "cmek"
		}
		desc, sa, lvl, src = w.GetDescription(), w.GetServiceAccount(), w.GetCallLogLevel().String(), w.GetSourceContents()
		if w.GetExecutionHistoryLevel() == workflowspb.ExecutionHistoryLevel_EXECUTION_HISTORY_DETAILED {
			hist = "EXECUTION_HISTORY_DETAILED"
		}
		labels, env = w.GetLabels(), w.GetUserEnvVars()
	}
	const details = "Workflow details"
	return []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: desc, Section: details},
		{Name: "serviceAccount", Label: "Service account", Type: "text", Default: sa, Section: details,
			Help: "The account the workflow's calls are authorized as; empty uses the Compute Engine default service account."},
		{Name: "encryption", Label: "Encryption", Type: "radio", Default: enc, Section: details, Options: []string{"google", "cmek"},
			OptionLabels: map[string]string{"google": "Google-managed encryption key", "cmek": "Cloud KMS key"}},
		{Name: "cryptoKeyName", Label: "Cloud KMS key", Type: "text", Default: key, Section: details, ShowWhen: "encryption=cmek", Browse: "kms",
			Pattern: `^projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+$`,
			Help:    "A symmetric key of this instance's Cloud KMS in the workflow's region: projects/PROJECT/locations/REGION/keyRings/RING/cryptoKeys/KEY."},
		{Name: "callLogLevel", Label: "Call log level", Type: "select", Default: lvl, Section: details, Options: workflowsCallLogLevels,
			OptionLabels: workflowsCallLogLabels, Help: "Which calls of each execution are logged to Cloud Logging; an execution can set its own."},
		{Name: "executionHistoryLevel", Label: "Execution history", Type: "select", Default: hist, Section: details,
			Options: workflowsHistoryLevels, OptionLabels: workflowsHistoryLabels,
			Help: "Detailed also records the in-scope variables and the expected iterations of each step entry."},
		{Name: "userEnvVars", Label: "Environment variables", Type: "map", Default: console.FormatMap(env), Section: details,
			KeyPattern: workflowsEnvKey, KeyHelp: "A name is letters, digits and underscores, not starting with a digit, GOOGLE or WORKFLOWS.",
			MaxEntries: 20, Help: "NAME=value, one per line; read with sys.get_env."},
		labelsField(labels, details),
		{Name: "sourceContents", Label: "Workflow editor", Type: "workflowsource", Required: true, Default: src, Section: "Workflow editor",
			Help: "Google's workflow syntax, in YAML or JSON; the graph beside it follows as you type. Tab indents while typing; Escape, then Tab, leaves the editor. http.* calls and connectors reach this instance's own endpoints only."},
	}
}

// workflowsFromForm is the workflow a Create or Edit form describes.
func workflowsFromForm(values map[string]string) (*workflowspb.Workflow, error) {
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, err
	}
	env, err := console.ParseMap(values["userEnvVars"])
	if err != nil {
		return nil, fmt.Errorf("environment variables: %w", err)
	}
	key := ""
	if values["encryption"] == "cmek" {
		if key = strings.TrimSpace(values["cryptoKeyName"]); key == "" {
			return nil, fmt.Errorf("choose a Cloud KMS key, or Google-managed encryption")
		}
	}
	return &workflowspb.Workflow{Description: strings.TrimSpace(values["description"]), Labels: labels, UserEnvVars: env,
		ServiceAccount: strings.TrimSpace(values["serviceAccount"]), CryptoKeyName: key,
		CallLogLevel:          workflowspb.Workflow_CallLogLevel(workflowspb.Workflow_CallLogLevel_value[values["callLogLevel"]]),
		ExecutionHistoryLevel: workflowspb.ExecutionHistoryLevel(workflowspb.ExecutionHistoryLevel_value[values["executionHistoryLevel"]]),
		SourceCode:            &workflowspb.Workflow_SourceContents{SourceContents: values["sourceContents"]}}, nil
}

func (workflowsProvider) CreateForm() (string, []console.Field) {
	const details = "Workflow details"
	return "Create workflow", append([]console.Field{
		{Name: "workflowId", Label: "Workflow name", Type: "text", Required: true, Section: details,
			Pattern: `^[a-zA-Z]([a-zA-Z0-9_\-]{0,62}[a-zA-Z0-9])?$`,
			Help:    "Letters, numbers, underscores and hyphens; starts with a letter and ends with a letter or number; at most 64 characters."},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1", Section: details, Options: workflowsRegions()},
	}, workflowsFields(nil)...)
}

func (p workflowsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s := p.svc.API()
	if s == nil {
		return "", errors.New(workflowsNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a workflow")
	}
	w, err := workflowsFromForm(values)
	if err != nil {
		return "", err
	}
	op, err := s.Workflows().CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{
		Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["location"]), WorkflowId: strings.TrimSpace(values["workflowId"]), Workflow: w})
	if err != nil {
		return "", err
	}
	out := &workflowspb.Workflow{}
	if err := op.GetResponse().UnmarshalTo(out); err != nil {
		return "", err
	}
	return out.GetName(), nil
}

func (p workflowsProvider) workflow(ctx context.Context, project string, name string) (*workflowspb.Workflow, error) {
	s := p.svc.API()
	if s == nil {
		return nil, errors.New(workflowsNotStarted)
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, err
	}
	return s.Workflows().GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
}

func (p workflowsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	switch len(path) {
	case 1:
		return p.workflowDetail(ctx, project, path[0])
	case 2:
		return p.executionDetail(ctx, project, path[0], path[1])
	}
	return console.DeeperThan(2, path), nil
}

func (p workflowsProvider) workflowDetail(ctx context.Context, project, name string) (console.Detail, error) {
	w, err := p.workflow(ctx, project, name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the workflow: " + consoleErr(err)}, nil
	}
	s := p.svc.API()
	execs := console.Listing{Columns: []string{"Start time", "End time", "Duration", "Revision"}, NameColumn: "Execution ID", Noun: "executions"}
	resp, err := s.Executions().ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: name, PageSize: 1000})
	if err == nil {
		for _, e := range resp.GetExecutions() {
			id := e.GetName()[strings.LastIndex(e.GetName(), "/")+1:]
			dur := "—"
			if d := e.GetDuration(); d != nil {
				dur = d.AsDuration().String()
			}
			execs.Items = append(execs.Items, console.Resource{Name: id, Status: e.GetState().String(), Opens: []string{name, e.GetName()},
				Fields: map[string]string{"Start time": kmsTime(e.GetStartTime()), "End time": kmsTime(e.GetEndTime()), "Duration": dur,
					"Revision": e.GetWorkflowRevisionId()}})
		}
	}
	execs.Total = len(execs.Items)
	revs := console.Listing{Columns: []string{"Created"}, NameColumn: "Revision ID", Noun: "revisions"}
	if rr, err := s.Workflows().ListWorkflowRevisions(ctx, &workflowspb.ListWorkflowRevisionsRequest{Name: name, PageSize: 100}); err == nil {
		for _, r := range rr.GetWorkflows() {
			revs.Items = append(revs.Items, console.Resource{Name: r.GetRevisionId(), Fields: map[string]string{"Created": kmsTime(r.GetRevisionCreateTime())}})
		}
	}
	revs.Total = len(revs.Items)
	env := []string{}
	for k, v := range w.GetUserEnvVars() {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: kmsLocation(w.GetName())}, {Label: "State", Value: w.GetState().String()},
			{Label: "Revision", Value: w.GetRevisionId()}},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Workflow", Properties: []console.Property{
				{Label: "Resource name", Value: w.GetName()}, {Label: "Description", Value: kmsOrDash(w.GetDescription())},
				{Label: "Service account", Value: kmsOrDash(w.GetServiceAccount())}, {Label: "Created", Value: kmsTime(w.GetCreateTime())},
				{Label: "Last updated", Value: kmsTime(w.GetUpdateTime())},
				{Label: "Call log level", Value: workflowsCallLogLabels[w.GetCallLogLevel().String()]},
				{Label: "Execution history", Value: workflowsHistoryLabels[w.GetExecutionHistoryLevel().String()]},
				{Label: "Encryption", Value: workflowsEncryption(w.GetCryptoKeyName())},
				{Label: "Environment variables", Value: kmsOrDash(strings.Join(env, ", "))},
				{Label: "Labels", Value: kmsOrDash(labelsText(w.GetLabels()))}}}}},
			{ID: "executions", Label: "Executions", Listing: execs},
			{ID: "source", Label: "Source", Kind: console.KindText, Text: w.GetSourceContents(), Language: "workflows"},
			{ID: "revisions", Label: "Revisions", Listing: revs},
			p.logsSection(ctx, project, `resource.labels.workflow_id="`+workflowID(name)+`" AND resource.labels.location="`+kmsLocation(name)+`"`),
		},
	}, nil
}

// workflowsEncryption is the Encryption a workflow's page shows.
func workflowsEncryption(key string) string {
	if key == "" {
		return "Google-managed encryption key"
	}
	return "Cloud KMS key: " + key
}

func workflowID(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// logsSection is the Logs tab of a workflow's or an execution's page:
// what its executions wrote to the instance's Cloud Logging (their start
// and end, sys.log and call logs), newest first, as Google's Logs tab
// lists them. filter is ANDed terms of a Cloud Logging filter over the
// project's logs.
func (p workflowsProvider) logsSection(ctx context.Context, project, filter string) console.Section {
	sec := console.Section{ID: "logs", Label: "Logs"}
	if p.logs == nil || p.logs.api == nil {
		sec.Unavailable = "Cloud Logging is not served by this instance, so executions' logs are not kept."
		return sec
	}
	l := console.Listing{Columns: []string{"Severity", "Log", "Summary"}, NameColumn: "Time", Noun: "log entries"}
	resp, err := p.logs.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + project},
		Filter: `resource.type="workflows.googleapis.com/Workflow" AND ` + filter, OrderBy: "timestamp desc", PageSize: 500})
	if err != nil {
		sec.Unavailable = "cannot read the logs: " + consoleErr(err)
		return sec
	}
	for _, e := range resp.GetEntries() {
		logID := e.GetLogName()[strings.LastIndex(e.GetLogName(), "/logs/")+len("/logs/"):]
		if u, err := url.PathUnescape(logID); err == nil {
			logID = u
		}
		summary := e.GetTextPayload()
		if j := e.GetJsonPayload(); j != nil {
			b, _ := protojson.Marshal(j)
			summary = string(b)
		}
		l.Items = append(l.Items, console.Resource{Name: e.GetTimestamp().AsTime().UTC().Format(time.RFC3339Nano), Status: e.GetSeverity().String(),
			Fields: map[string]string{"Severity": e.GetSeverity().String(), "Log": logID, "Summary": kmsOrDash(summary)}})
	}
	l.Total = len(l.Items)
	sec.Listing = l
	return sec
}

func (p workflowsProvider) executionDetail(ctx context.Context, project, wf, name string) (console.Detail, error) {
	if _, err := p.workflow(ctx, project, wf); err != nil {
		return console.Detail{Unavailable: "cannot read the workflow: " + consoleErr(err)}, nil
	}
	if !strings.HasPrefix(name, wf+"/executions/") {
		return console.Detail{Unavailable: "no execution " + name + " of " + wf}, nil
	}
	s := p.svc.API()
	e, err := s.Executions().GetExecution(ctx, &executionspb.GetExecutionRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the execution: " + consoleErr(err)}, nil
	}
	dur := "—"
	if d := e.GetDuration(); d != nil {
		dur = d.AsDuration().String()
	}
	props := []console.Property{{Label: "Execution ID", Value: name[strings.LastIndex(name, "/")+1:]},
		{Label: "Execution state", Value: e.GetState().String()}, {Label: "Revision", Value: e.GetWorkflowRevisionId()},
		{Label: "Start time", Value: kmsTime(e.GetStartTime())}, {Label: "End time", Value: kmsTime(e.GetEndTime())},
		{Label: "Execution duration", Value: dur}, {Label: "Call log level", Value: workflowsCallLogLabels[e.GetCallLogLevel().String()]}}
	for _, cs := range e.GetStatus().GetCurrentSteps() {
		props = append(props, console.Property{Label: "Current step", Value: cs.GetRoutine() + " › " + cs.GetStep()})
	}
	props = append(props, console.Property{Label: "Input", Value: kmsOrDash(e.GetArgument())})
	switch {
	case e.GetError() != nil:
		props = append(props, console.Property{Label: "Output", Value: strings.TrimSpace(e.GetError().GetContext() + "\n\n" + e.GetError().GetPayload())})
	default:
		props = append(props, console.Property{Label: "Output", Value: kmsOrDash(e.GetResult())})
	}
	steps := console.Listing{Columns: []string{"Entry", "Routine", "Type", "Started", "Updated", "Parent", "Exception"}, NameColumn: "Step", Noun: "step entries"}
	if entries, err := s.StepEntries(name); err == nil {
		for _, st := range entries {
			state := strings.TrimPrefix(st.State, "STATE_")
			parent := "—"
			if st.Parent != 0 {
				parent = strconv.FormatInt(st.Parent, 10)
			}
			steps.Items = append(steps.Items, console.Resource{Name: st.Step, Status: state, Fields: map[string]string{
				"Entry": strconv.FormatInt(st.EntryID, 10), "Routine": st.Routine, "Type": strings.TrimPrefix(st.StepType, "STEP_"),
				"Started": st.CreateTime, "Updated": st.UpdateTime, "Parent": parent, "Exception": kmsOrDash(st.Exception)}})
		}
	}
	steps.Total = len(steps.Items)
	// The graph is of the revision that ran, which a later deploy does not
	// change.
	graph := ""
	if rev, err := s.Workflows().GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: wf, RevisionId: e.GetWorkflowRevisionId()}); err == nil {
		graph = rev.GetSourceContents()
	}
	id := name[strings.LastIndex(name, "/")+1:]
	sections := []console.Section{
		{ID: "summary", Label: "Summary", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Execution", Properties: props}}},
		{ID: "steps", Label: "Steps", Listing: steps, Graph: graph},
		p.logsSection(ctx, project, `labels."workflows.googleapis.com/execution_id"="`+id+`"`),
	}
	// Headed by its execution ID, as Google's Execution details page is.
	return console.Detail{Title: id, Summary: []console.Property{{Label: "State", Value: e.GetState().String()}, {Label: "Workflow", Value: wf}},
		Sections: sections}, nil
}

func (p workflowsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		w, err := p.workflow(ctx, project, path[0])
		if err != nil {
			return nil
		}
		return []console.Action{
			{ID: "execute", Label: "Execute", Fields: []console.Field{
				{Name: "argument", Label: "Input (JSON)", Type: "textarea", Help: "The argument main receives, as JSON; empty for none."},
				{Name: "callLogLevel", Label: "Call log level", Type: "select", Default: "CALL_LOG_LEVEL_UNSPECIFIED", Options: workflowsCallLogLevels,
					OptionLabels: map[string]string{"CALL_LOG_LEVEL_UNSPECIFIED": "Not specified (the workflow's level)", "LOG_ERRORS_ONLY": "Errors only",
						"LOG_ALL_CALLS": "All calls", "LOG_NONE": "No logs"}}}},
			{ID: "edit", Label: "Edit", Fields: append([]console.Field{
				{Name: "workflowId", Label: "Workflow name", Type: "text", Default: workflowID(w.GetName()), Immutable: true, Section: "Workflow details"},
				{Name: "location", Label: "Region", Type: "text", Default: kmsLocation(w.GetName()), Immutable: true, Section: "Workflow details"},
			}, workflowsFields(w)...)},
			{ID: "delete", Label: "Delete", Destructive: true, Leaves: true,
				Confirm: "Deleting the workflow deletes its executions too."},
		}
	case 2:
		s := p.svc.API()
		if s == nil {
			return nil
		}
		e, err := s.Executions().GetExecution(ctx, &executionspb.GetExecutionRequest{Name: path[1]})
		if err != nil || e.GetState() != executionspb.Execution_ACTIVE {
			return nil
		}
		return []console.Action{{ID: "cancel", Label: "Cancel", Destructive: true}}
	}
	return nil
}

func (p workflowsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return fmt.Errorf("no workflow")
	}
	if _, err := p.workflow(ctx, project, path[0]); err != nil {
		return err
	}
	s := p.svc.API()
	switch action {
	case "execute":
		lvl := executionspb.Execution_CallLogLevel(executionspb.Execution_CallLogLevel_value[values["callLogLevel"]])
		_, err := s.Executions().CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: path[0],
			Execution: &executionspb.Execution{Argument: strings.TrimSpace(values["argument"]), CallLogLevel: lvl}})
		return err
	case "edit":
		w, err := workflowsFromForm(values)
		if err != nil {
			return err
		}
		w.Name = path[0]
		_, err = s.Workflows().UpdateWorkflow(ctx, &workflowspb.UpdateWorkflowRequest{Workflow: w,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels", "service_account", "source_contents",
				"crypto_key_name", "call_log_level", "execution_history_level", "user_env_vars"}}})
		return err
	case "delete":
		_, err := s.Workflows().DeleteWorkflow(ctx, &workflowspb.DeleteWorkflowRequest{Name: path[0]})
		return err
	case "cancel":
		if len(path) != 2 {
			return fmt.Errorf("cancel needs an execution")
		}
		_, err := s.Executions().CancelExecution(ctx, &executionspb.CancelExecutionRequest{Name: path[1]})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator   = workflowsProvider{}
	_ console.Driller   = workflowsProvider{}
	_ console.PathActor = workflowsProvider{}
)
