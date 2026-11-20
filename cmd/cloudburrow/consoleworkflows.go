package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
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
type workflowsProvider struct{ svc *workflowsService }

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

var workflowsCallLogLevels = []string{"CALL_LOG_LEVEL_UNSPECIFIED", "LOG_ALL_CALLS", "LOG_ERRORS_ONLY", "LOG_NONE"}

func (workflowsProvider) CreateForm() (string, []console.Field) {
	return "Create workflow", []console.Field{
		{Name: "workflowId", Label: "Workflow name", Type: "text", Required: true, Pattern: `^[a-zA-Z]([a-zA-Z0-9_\-]{0,62}[a-zA-Z0-9])?$`,
			Help: "Letters, numbers, underscores and hyphens; starts with a letter; at most 64 characters."},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: workflowsRegions()},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "serviceAccount", Label: "Service account", Type: "text",
			Help: "The account the workflow's calls are authorized as; empty uses the Compute Engine default service account."},
		{Name: "callLogLevel", Label: "Call log level", Type: "select", Default: "CALL_LOG_LEVEL_UNSPECIFIED", Options: workflowsCallLogLevels},
		{Name: "sourceContents", Label: "Source (YAML or JSON)", Type: "textarea", Required: true, Default: workflowsSample,
			Help: "Google's workflow syntax. http.* calls and connectors reach this instance's own endpoints only."},
		labelsField(nil, ""),
	}
}

func (p workflowsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s := p.svc.API()
	if s == nil {
		return "", errors.New(workflowsNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a workflow")
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	w := &workflowspb.Workflow{Description: strings.TrimSpace(values["description"]), Labels: labels,
		ServiceAccount: strings.TrimSpace(values["serviceAccount"]),
		CallLogLevel:   workflowspb.Workflow_CallLogLevel(workflowspb.Workflow_CallLogLevel_value[values["callLogLevel"]]),
		SourceCode:     &workflowspb.Workflow_SourceContents{SourceContents: values["sourceContents"]}}
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
				{Label: "Last updated", Value: kmsTime(w.GetUpdateTime())}, {Label: "Call log level", Value: w.GetCallLogLevel().String()},
				{Label: "Environment variables", Value: kmsOrDash(strings.Join(env, ", "))},
				{Label: "Labels", Value: kmsOrDash(labelsText(w.GetLabels()))}}}}},
			{ID: "source", Label: "Source", Kind: console.KindText, Text: w.GetSourceContents()},
			{ID: "executions", Label: "Executions", Listing: execs},
			{ID: "revisions", Label: "Revisions", Listing: revs},
		},
	}, nil
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
		{Label: "State", Value: e.GetState().String()}, {Label: "Revision", Value: e.GetWorkflowRevisionId()},
		{Label: "Start time", Value: kmsTime(e.GetStartTime())}, {Label: "End time", Value: kmsTime(e.GetEndTime())},
		{Label: "Duration", Value: dur}, {Label: "Call log level", Value: e.GetCallLogLevel().String()}}
	for _, cs := range e.GetStatus().GetCurrentSteps() {
		props = append(props, console.Property{Label: "Current step", Value: cs.GetRoutine() + " › " + cs.GetStep()})
	}
	steps := console.Listing{Columns: []string{"Routine", "Type", "Started", "Updated", "Exception"}, NameColumn: "Step", Noun: "steps"}
	if entries, err := s.StepEntries(name); err == nil {
		for _, st := range entries {
			state := strings.TrimPrefix(st.State, "STATE_")
			steps.Items = append(steps.Items, console.Resource{Name: st.Step, Status: state, Fields: map[string]string{
				"Routine": st.Routine, "Type": strings.TrimPrefix(st.StepType, "STEP_"), "Started": st.CreateTime, "Updated": st.UpdateTime,
				"Exception": kmsOrDash(st.Exception)}})
		}
	}
	steps.Total = len(steps.Items)
	sections := []console.Section{
		{ID: "summary", Label: "Summary", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Execution", Properties: props}}},
		{ID: "input", Label: "Input", Kind: console.KindText, Text: kmsOrDash(e.GetArgument())},
	}
	switch {
	case e.GetError() != nil:
		sections = append(sections, console.Section{ID: "output", Label: "Output", Kind: console.KindText,
			Text: e.GetError().GetContext() + "\n\n" + e.GetError().GetPayload()})
	default:
		sections = append(sections, console.Section{ID: "output", Label: "Output", Kind: console.KindText, Text: kmsOrDash(e.GetResult())})
	}
	sections = append(sections, console.Section{ID: "steps", Label: "Steps", Listing: steps})
	return console.Detail{Summary: []console.Property{{Label: "State", Value: e.GetState().String()}, {Label: "Workflow", Value: wf}}, Sections: sections}, nil
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
				{Name: "callLogLevel", Label: "Call log level", Type: "select", Default: "CALL_LOG_LEVEL_UNSPECIFIED", Options: workflowsCallLogLevels}}},
			{ID: "edit", Label: "Edit", Fields: []console.Field{
				{Name: "description", Label: "Description", Type: "text", Default: w.GetDescription()},
				{Name: "serviceAccount", Label: "Service account", Type: "text", Default: w.GetServiceAccount()},
				{Name: "sourceContents", Label: "Source (YAML or JSON)", Type: "textarea", Required: true, Default: w.GetSourceContents(),
					Help: "Deploying a changed source makes a new revision."},
				labelsField(w.GetLabels(), "")}},
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
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		op, err := s.Workflows().UpdateWorkflow(ctx, &workflowspb.UpdateWorkflowRequest{
			Workflow: &workflowspb.Workflow{Name: path[0], Description: strings.TrimSpace(values["description"]), Labels: labels,
				ServiceAccount: strings.TrimSpace(values["serviceAccount"]),
				SourceCode:     &workflowspb.Workflow_SourceContents{SourceContents: values["sourceContents"]}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels", "service_account", "source_contents"}}})
		if err != nil {
			return err
		}
		_ = op
		return nil
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
