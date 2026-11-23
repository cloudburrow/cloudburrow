package main

// Colab Enterprise's My notebooks, Executions and Schedules pages, and a
// runtime template's Permissions (#2105), in Google's layout
// (https://docs.cloud.google.com/colab/docs/schedule-notebook-run,
// manage-access-runtime-template; read 2026-11-22):
//
//   - My notebooks: a Region menu; each notebook's Notebook actions menu
//     has Schedule, the dialog of Schedule name, Runtime template, Run
//     schedule (One-off, which "start[s] immediately", or Recurring),
//     Cloud Storage output location and, for a template without end-user
//     credentials, Service account, ending in Submit.
//   - Executions: each run's View result and Delete (with Confirm);
//     "Deleting a schedule doesn't delete the runs it generated."
//   - Schedules: a schedule's name opens Schedule details (the output
//     bucket, start and end times and frequency); Pause, Resume and
//     Delete.
//   - Runtime templates: a template's Permissions, Add principal opening
//     Grant access (New principals, Select a role, Add another role,
//     Save).
//
// UNVERIFIED (re-read 2026-11-22: https://docs.cloud.google.com/colab/docs/schedule-notebook-run):
// the lists' columns, which Google does not list; the Recurring
// frequency is written as a cron schedule (gcloud's --cron-schedule), the
// console dialog's own frequency fields not being documented; My notebooks'
// Create notebook names the notebook and opens it in BigQuery Studio, the
// editor this instance has for a Colab Enterprise notebook.
//
// A notebook is a Dataform single-file repository (single-file-asset-type
// notebook, content.ipynb), as BigQuery Studio keeps one; executions and
// schedules go through the official aiplatform clients to the local
// Vertex AI endpoint.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

func (v vertexAPI) schedules(ctx context.Context) (*aiplatform.ScheduleClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewScheduleClient(ctx, v.opts()...)
}

// colabJobState is a job's state as the Executions list words it.
func colabJobState(s aiplatformpb.JobState) string {
	switch s {
	case aiplatformpb.JobState_JOB_STATE_QUEUED, aiplatformpb.JobState_JOB_STATE_PENDING:
		return "Pending"
	case aiplatformpb.JobState_JOB_STATE_RUNNING:
		return "Running"
	case aiplatformpb.JobState_JOB_STATE_SUCCEEDED:
		return "Succeeded"
	case aiplatformpb.JobState_JOB_STATE_FAILED:
		return "Failed"
	case aiplatformpb.JobState_JOB_STATE_CANCELLING:
		return "Cancelling"
	case aiplatformpb.JobState_JOB_STATE_CANCELLED:
		return "Cancelled"
	}
	return "Unknown"
}

// colabResultRoute is the Cloud Storage page of a job's executed notebook.
func colabResultRoute(j *aiplatformpb.NotebookExecutionJob) string {
	rest := strings.TrimPrefix(j.GetGcsOutputUri(), "gs://")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return ""
	}
	parts := []string{url.PathEscape(bucket)}
	for _, p := range strings.Split(strings.Trim(prefix, "/"), "/") {
		if p != "" {
			parts = append(parts, url.PathEscape(p))
		}
	}
	parts = append(parts, url.PathEscape(vertexLast(j.GetName())))
	return "/storage/browser/" + strings.Join(parts, "/")
}

// colabJobSource is what a job runs, as its row names it.
func colabJobSource(j *aiplatformpb.NotebookExecutionJob) string {
	switch {
	case j.GetDataformRepositorySource() != nil:
		return vertexLast(j.GetDataformRepositorySource().GetDataformRepositoryResourceName())
	case j.GetGcsNotebookSource() != nil:
		return j.GetGcsNotebookSource().GetUri()
	case j.GetDirectNotebookSource() != nil:
		return "Notebook contents"
	}
	return "-"
}

// ---- Executions

type colabExecutionsProvider struct{ api vertexAPI }

func (colabExecutionsProvider) ID() string       { return "colab-executions" }
func (colabExecutionsProvider) Title() string    { return "Executions" }
func (colabExecutionsProvider) ScopeKey() string { return colabRegionKey }

func (p colabExecutionsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Execution name", "Notebook", "Runtime template", "Schedule", "Created", "Updated"},
		NameColumn: "ID", Noun: "executions", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each execution runs its notebook on a fresh runtime of this instance's notebook runtime and writes the executed notebook to Cloud Storage. " +
			"Schedule a notebook from My notebooks."}
	if project == "" {
		l.Prompt = "Executions are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: colabParent(ctx, project), OrderBy: "create_time desc"}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		if r := colabResultRoute(j); r != "" && (j.GetJobState() == aiplatformpb.JobState_JOB_STATE_SUCCEEDED || j.GetJobState() == aiplatformpb.JobState_JOB_STATE_FAILED) {
			actions = append(actions, console.Action{ID: "view-result", Label: "View result", Href: r})
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: colabJobState(j.GetJobState()), Actions: actions, Fields: map[string]string{
			"Execution name": dashIfEmpty(j.GetDisplayName()), "Notebook": colabJobSource(j),
			"Runtime template": dashIfEmpty(vertexLast(j.GetNotebookRuntimeTemplateResourceName())),
			"Schedule":         dashIfEmpty(vertexLast(j.GetScheduleResourceName())),
			"Created":          vertexTime(j.GetCreateTime()), "Updated": vertexTime(j.GetUpdateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p colabExecutionsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	j, err := c.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: colabParent(ctx, project) + "/notebookExecutionJobs/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	identity := j.GetExecutionUser()
	if identity == "" {
		identity = j.GetServiceAccount()
	}
	result := console.Property{Label: "Result", Value: "-"}
	if r := colabResultRoute(j); r != "" {
		result = console.Property{Label: "Result", Value: strings.TrimSuffix(j.GetGcsOutputUri(), "/") + "/" + vertexLast(j.GetName()) + "/", Link: r}
	}
	d := console.Detail{Title: dashIfEmpty(j.GetDisplayName()), Summary: []console.Property{
		{Label: "ID", Value: vertexLast(j.GetName())},
		{Label: "Name", Value: j.GetName(), Copy: j.GetName()},
		{Label: "Status", Value: colabJobState(j.GetJobState())},
		{Label: "Notebook", Value: colabJobSource(j)},
		{Label: "Runtime template", Value: dashIfEmpty(j.GetNotebookRuntimeTemplateResourceName())},
		{Label: "Schedule", Value: dashIfEmpty(j.GetScheduleResourceName())},
		{Label: "Executed as", Value: dashIfEmpty(identity)},
		{Label: "Execution timeout", Value: j.GetExecutionTimeout().AsDuration().String()},
		result,
		{Label: "Created", Value: vertexTime(j.GetCreateTime())},
		{Label: "Updated", Value: vertexTime(j.GetUpdateTime())},
	}}
	if s := j.GetStatus(); s != nil && s.GetCode() != 0 {
		d.Summary = append(d.Summary, console.Property{Label: "Error", Value: s.GetMessage()})
	}
	return d, nil
}

func (p colabExecutionsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteNotebookExecutionJob(ctx, &aiplatformpb.DeleteNotebookExecutionJobRequest{Name: colabParent(ctx, project) + "/notebookExecutionJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (colabExecutionsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (colabExecutionsProvider) Act(context.Context, string, string, string) error {
	return errors.New("View result opens the executed notebook's folder in Cloud Storage")
}

// ---- Schedules

type colabSchedulesProvider struct{ api vertexAPI }

func (colabSchedulesProvider) ID() string       { return "colab-schedules" }
func (colabSchedulesProvider) Title() string    { return "Schedules" }
func (colabSchedulesProvider) ScopeKey() string { return colabRegionKey }

func colabScheduleState(s aiplatformpb.Schedule_State) string {
	switch s {
	case aiplatformpb.Schedule_ACTIVE:
		return "Active"
	case aiplatformpb.Schedule_PAUSED:
		return "Paused"
	case aiplatformpb.Schedule_COMPLETED:
		return "Completed"
	}
	return "Unknown"
}

func colabScheduleActions(s aiplatformpb.Schedule_State) []console.Action {
	switch s {
	case aiplatformpb.Schedule_ACTIVE:
		return []console.Action{{ID: "pause", Label: "Pause"}}
	case aiplatformpb.Schedule_PAUSED:
		return []console.Action{{ID: "resume", Label: "Resume"}}
	}
	return nil
}

func colabOptTime(t interface{ AsTime() time.Time }, set bool) string {
	if !set {
		return "-"
	}
	return vertexTime(t)
}

func (p colabSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Schedule name", "Frequency", "Next run", "Runs started", "Created"},
		NameColumn: "ID", Noun: "schedules", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each run of a schedule is an execution, listed on the Executions page. Schedule a notebook from My notebooks."}
	if project == "" {
		l.Prompt = "Schedules are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.schedules(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListSchedules(ctx, &aiplatformpb.ListSchedulesRequest{Parent: colabParent(ctx, project), Filter: "create_notebook_execution_job_request:*"}); ; {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(s.GetName()), Status: colabScheduleState(s.GetState()), Actions: colabScheduleActions(s.GetState()),
			Fields: map[string]string{"Schedule name": s.GetDisplayName(), "Frequency": s.GetCron(),
				"Next run": colabOptTime(s.GetNextRunTime(), s.GetNextRunTime() != nil), "Runs started": fmt.Sprint(s.GetStartedRunCount()),
				"Created": vertexTime(s.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p colabSchedulesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.schedules(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	s, err := c.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: colabParent(ctx, project) + "/schedules/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	j := s.GetCreateNotebookExecutionJobRequest().GetNotebookExecutionJob()
	last := "-"
	if r := s.GetLastScheduledRunResponse(); r != nil {
		last = vertexTime(r.GetScheduledRunTime()) + ": " + r.GetRunResponse()
	}
	maxRuns := "Unlimited"
	if s.GetMaxRunCount() > 0 {
		maxRuns = fmt.Sprint(s.GetMaxRunCount())
	}
	d := console.Detail{Title: "Schedule details", Summary: []console.Property{
		{Label: "Schedule name", Value: s.GetDisplayName()},
		{Label: "ID", Value: vertexLast(s.GetName())},
		{Label: "Name", Value: s.GetName(), Copy: s.GetName()},
		{Label: "Status", Value: colabScheduleState(s.GetState())},
		{Label: "Frequency", Value: s.GetCron()},
		{Label: "Start time", Value: colabOptTime(s.GetStartTime(), s.GetStartTime() != nil)},
		{Label: "End time", Value: colabOptTime(s.GetEndTime(), s.GetEndTime() != nil)},
		{Label: "Next run", Value: colabOptTime(s.GetNextRunTime(), s.GetNextRunTime() != nil)},
		{Label: "Notebook", Value: colabJobSource(j)},
		{Label: "Runtime template", Value: dashIfEmpty(j.GetNotebookRuntimeTemplateResourceName())},
		{Label: "Cloud Storage output location", Value: dashIfEmpty(j.GetGcsOutputUri())},
		{Label: "Runs started", Value: fmt.Sprint(s.GetStartedRunCount())},
		{Label: "Maximum runs", Value: maxRuns},
		{Label: "Last scheduled run", Value: last},
		{Label: "Created", Value: vertexTime(s.GetCreateTime())},
	}}
	return d, nil
}

func (p colabSchedulesProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.schedules(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteSchedule(ctx, &aiplatformpb.DeleteScheduleRequest{Name: colabParent(ctx, project) + "/schedules/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (colabSchedulesProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p colabSchedulesProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActAt(ctx, project, []string{name}, action, nil)
}

func (p colabSchedulesProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{{ID: "pause", Label: "Pause"}, {ID: "resume", Label: "Resume"}}
}

func (p colabSchedulesProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return errors.New("choose a schedule")
	}
	c, err := p.api.schedules(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	name := colabParent(ctx, project) + "/schedules/" + path[0]
	switch action {
	case "pause":
		return c.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: name})
	case "resume":
		return c.ResumeSchedule(ctx, &aiplatformpb.ResumeScheduleRequest{Name: name})
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- My notebooks

type colabNotebooksProvider struct {
	api vertexAPI
	df  func() *dataform.Server
}

func (colabNotebooksProvider) ID() string           { return "colab-notebooks" }
func (colabNotebooksProvider) Title() string        { return "My notebooks" }
func (colabNotebooksProvider) ScopeKey() string     { return colabRegionKey }
func (colabNotebooksProvider) CreateTitle() string  { return "Create notebook" }
func (colabNotebooksProvider) CreateSubmit() string { return "Create" }

func (p colabNotebooksProvider) server() (*dataform.Server, error) {
	if p.df == nil || p.df() == nil {
		return nil, errors.New("Dataform is not served on this instance: a Colab Enterprise notebook is kept in a Dataform repository (start with --services including dataform)")
	}
	return p.df(), nil
}

// scheduleAction is Notebook actions > Schedule, offering the region's
// runtime templates.
func (p colabNotebooksProvider) scheduleAction(ctx context.Context, project string) console.Action {
	tpl := console.Field{Name: "runtimeTemplate", Label: "Runtime template", Type: "select", Required: true,
		Help: "The runtime template determines the specifications of the runtime that runs your notebook."}
	labels := map[string]string{}
	if c, err := p.api.notebooks(ctx); err == nil {
		for it := c.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: colabParent(ctx, project)}); ; {
			t, err := it.Next()
			if err != nil {
				break
			}
			id := vertexLast(t.GetName())
			tpl.Options = append(tpl.Options, id)
			labels[id] = t.GetDisplayName() + " (" + id + ")"
		}
		_ = c.Close()
	}
	tpl.OptionLabels = labels
	if len(tpl.Options) > 0 {
		tpl.Default = tpl.Options[0]
	} else {
		tpl.Help = "No runtime templates in " + colabRegion(ctx) + ": create one on the Runtime templates page."
	}
	return console.Action{ID: "schedule", Label: "Schedule", Title: "Schedule notebook", Submit: "Submit", Fields: []console.Field{
		{Name: "displayName", Label: "Schedule name", Type: "text", Required: true, Pattern: `^.{1,128}$`, Help: "Up to 128 characters."},
		tpl,
		{Name: "runSchedule", Label: "Run schedule", Type: "select", Options: []string{"one-off", "recurring"}, Default: "one-off",
			OptionLabels: map[string]string{"one-off": "One-off", "recurring": "Recurring"},
			Help:         "One-off runs start immediately; recurring runs start automatically on the schedule that you set."},
		{Name: "cron", Label: "Frequency (cron)", Type: "text", Default: "0 9 * * *", Pattern: `^((CRON_)?TZ=\S+ )?\S+ \S+ \S+ \S+ \S+$`, ShowWhen: "runSchedule=recurring",
			Help: "Unix cron: minute, hour, day of month, month, day of week, optionally after TZ=TIME_ZONE (for example TZ=America/New_York 0 9 * * MON)."},
		{Name: "output", Label: "Cloud Storage output location", Type: "text", Required: true, Pattern: `^(gs://)?[a-z0-9][\-a-z0-9._]{1,221}(/.*)?$`,
			Help: "gs://bucket or gs://bucket/folder: each run writes its executed notebook in a folder named after the execution."},
		{Name: "serviceAccount", Label: "Service account", Type: "text", Pattern: `^([^@\s]+@[^@\s]+)?$`,
			Help: "Needed when the runtime template does not enable end-user credentials; otherwise the run executes as you."},
	}}
}

func (p colabNotebooksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Notebook name", "Created"}, NameColumn: "ID", Noun: "notebooks", Scope: colabScope(ctx),
		Note: "A notebook is kept as a Dataform repository, as BigQuery Studio keeps its notebooks; Open edits it in BigQuery Studio, whose runtimes are these templates' runtimes."}
	if project == "" {
		l.Prompt = "Notebooks are listed per project. Choose one in the toolbar."
		return l, nil
	}
	s, err := p.server()
	if err != nil {
		return l, err
	}
	resp, err := s.ListRepositories(ctx, &dfpb.ListRepositoriesRequest{Parent: colabParent(ctx, project), PageSize: 1000})
	if err != nil {
		return l, err
	}
	schedule := p.scheduleAction(ctx, project)
	for _, r := range resp.GetRepositories() {
		if dataformAssetKind(r) != "notebook" {
			continue
		}
		open := "/bigquery-studio?" + url.Values{"project": {project}, "savedQuery": {r.GetName()}}.Encode()
		l.Items = append(l.Items, console.Resource{Name: vertexLast(r.GetName()), Fields: map[string]string{
			"Notebook name": studioDisplay(r), "Created": kmsTime(r.GetCreateTime())},
			Actions: []console.Action{{ID: "open", Label: "Open", Href: open}, schedule}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (colabNotebooksProvider) CreateForm() (string, []console.Field) {
	return "Create notebook", []console.Field{
		{Name: "displayName", Label: "Notebook name", Type: "text", Required: true, Pattern: `^.{1,256}$`, Help: "Up to 256 characters."},
	}
}

func (p colabNotebooksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a notebook")
	}
	s, err := p.server()
	if err != nil {
		return "", err
	}
	out, err := studioSaveAsset(ctx, s, project, studioAssetReq{Kind: "notebook", DisplayName: values["displayName"], Location: colabRegion(ctx),
		Content: `{"cells":[],"metadata":{"kernelspec":{"name":"python3","display_name":"Python 3"}},"nbformat":4,"nbformat_minor":5}`})
	if err != nil {
		return "", err
	}
	if m, ok := out.(map[string]any); ok {
		if n, ok := m["name"].(string); ok {
			return vertexLast(n), nil
		}
	}
	return "", nil
}

func (p colabNotebooksProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteRepository(ctx, &dfpb.DeleteRepositoryRequest{Name: colabParent(ctx, project) + "/repositories/" + name, Force: true})
	return err
}

func (colabNotebooksProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p colabNotebooksProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActAt(ctx, project, []string{name}, action, nil)
}

func (p colabNotebooksProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{p.scheduleAction(ctx, project)}
}

// ActAt is Schedule: a One-off run is a notebook execution, a Recurring
// one a schedule of them.
func (p colabNotebooksProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 || action != "schedule" {
		return fmt.Errorf("unknown action %q", action)
	}
	parent := colabParent(ctx, project)
	tpl := strings.TrimSpace(values["runtimeTemplate"])
	if tpl == "" {
		return errors.New("choose a runtime template")
	}
	out := strings.TrimSpace(values["output"])
	if out == "" {
		return errors.New("a Cloud Storage output location is required")
	}
	if !strings.HasPrefix(out, "gs://") {
		out = "gs://" + out
	}
	j := &aiplatformpb.NotebookExecutionJob{DisplayName: strings.TrimSpace(values["displayName"]),
		NotebookSource: &aiplatformpb.NotebookExecutionJob_DataformRepositorySource_{DataformRepositorySource: &aiplatformpb.NotebookExecutionJob_DataformRepositorySource{
			DataformRepositoryResourceName: parent + "/repositories/" + path[0]}},
		EnvironmentSpec: &aiplatformpb.NotebookExecutionJob_NotebookRuntimeTemplateResourceName{NotebookRuntimeTemplateResourceName: parent + "/notebookRuntimeTemplates/" + tpl},
		ExecutionSink:   &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: out}}
	if sa := strings.TrimSpace(values["serviceAccount"]); sa != "" {
		j.ExecutionIdentity = &aiplatformpb.NotebookExecutionJob_ServiceAccount{ServiceAccount: sa}
	} else {
		j.ExecutionIdentity = &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: colabUser}
	}
	if values["runSchedule"] == "recurring" {
		c, err := p.api.schedules(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: j.GetDisplayName(),
			TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: strings.TrimSpace(values["cron"])}, MaxConcurrentRunCount: 1,
			Request: &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: &aiplatformpb.CreateNotebookExecutionJobRequest{
				Parent: parent, NotebookExecutionJob: j}}}})
		return err
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: j})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// ---- a runtime template's Permissions

// notebookIAM is the NotebookClient's IAM methods as an iamServer.
type notebookIAM struct{ c *aiplatform.NotebookClient }

func (n notebookIAM) GetIamPolicy(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return n.c.GetIamPolicy(ctx, r)
}

func (n notebookIAM) SetIamPolicy(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return n.c.SetIamPolicy(ctx, r)
}

func (n notebookIAM) TestIamPermissions(ctx context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return n.c.TestIamPermissions(ctx, r)
}

func (p colabTemplatesProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	t := &console.PolicyTarget{Link: compatibilityDoc + "colab-enterprise-runtime-templates-and-runtimes-2105",
		RoleHelp: roleHelp("Colab Enterprise", "roles/aiplatform.notebookRuntimeUser")}
	if p.enforced {
		t.Enforced = "This instance runs with --iam-enforcement=on: making a runtime or an execution from this template needs " +
			"aiplatform.notebookRuntimeTemplates.apply in these bindings or the project's (the Notebook Runtime User role holds it), " +
			"and reading or changing them aiplatform.notebookRuntimeTemplates.getIamPolicy and .setIamPolicy. " +
			"A request with no access token, and this console, act as the instance's own account, which holds every permission."
	}
	return t
}

func (p colabTemplatesProvider) withIAM(ctx context.Context, project string, path []string, f func(iamServer, string) error) error {
	if len(path) != 1 {
		return errors.New("only a runtime template has a Permissions tab")
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return f(notebookIAM{c: c}, colabParent(ctx, project)+"/notebookRuntimeTemplates/"+path[0])
}

func (p colabTemplatesProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	var out console.Policy
	err := p.withIAM(ctx, project, path, func(api iamServer, name string) error {
		var err error
		out, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return out, err
}

func (p colabTemplatesProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withIAM(ctx, project, path, func(api iamServer, name string) error { return setProtoPolicy(ctx, api, name, pol) })
}

func (p colabTemplatesProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	var out []string
	err := p.withIAM(ctx, project, path, func(api iamServer, name string) error {
		var err error
		out, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return out, err
}
