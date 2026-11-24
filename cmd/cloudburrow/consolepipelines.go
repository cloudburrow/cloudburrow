package main

// Vertex AI Pipelines' console page (#2173), its Runs and Schedules tabs,
// in Google's layout (https://docs.cloud.google.com/vertex-ai/docs/pipelines/run-pipeline,
// .../schedule-pipeline-run; read 2026-11-22):
//
//   - Pipelines has a Region menu; Create run opens Create pipeline run:
//     Run details (the run source: Upload file or Import from Cloud
//     Storage; Run name; Run schedule, One-off or Recurring, with Start
//     time Immediately or On, a cron Frequency and Ends Never or On;
//     Advanced options' service account), Continue, then Runtime
//     configuration (Cloud storage location, Advanced options' Failure
//     policy, Pipeline parameters), Submit;
//   - the Schedules tab lists the schedules with their Status; a row's
//     options menu has Pause (Active), Resume (Paused) and Delete, whose
//     dialog's Delete confirms it.
//
//   - a run's page is its runtime graph with the Pipeline run analysis
//     pane (consolepipelinegraph.go, #2205).
//
// UNVERIFIED (re-read 2026-11-22): the Runs list's columns (Google's docs
// do not show them); "Select from existing pipelines" and "Select a
// Template Gallery pipeline" are not offered (templates are not served);
// the pipeline parameters are entered as one JSON object.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (v vertexAPI) pipelines(ctx context.Context) (*aiplatform.PipelineClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewPipelineClient(ctx, v.opts()...)
}

// pipelineStateText is a run's state as the Runs list words it.
func pipelineStateText(s aiplatformpb.PipelineState) string {
	switch s {
	case aiplatformpb.PipelineState_PIPELINE_STATE_QUEUED, aiplatformpb.PipelineState_PIPELINE_STATE_PENDING:
		return "Pending"
	case aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING:
		return "Running"
	case aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED:
		return "Succeeded"
	case aiplatformpb.PipelineState_PIPELINE_STATE_FAILED:
		return "Failed"
	case aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLING:
		return "Cancelling"
	case aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED:
		return "Cancelled"
	case aiplatformpb.PipelineState_PIPELINE_STATE_PAUSED:
		return "Paused"
	}
	return "Unknown"
}

func pipelineTaskStateText(s aiplatformpb.PipelineTaskDetail_State) string {
	t := strings.ToLower(s.String())
	if t == "" || t == "state_unspecified" {
		return "Unknown"
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

func pipelineDuration(j *aiplatformpb.PipelineJob) string {
	if j.GetStartTime() == nil {
		return "-"
	}
	end := time.Now()
	if j.GetEndTime() != nil {
		end = j.GetEndTime().AsTime()
	}
	return end.Sub(j.GetStartTime().AsTime()).Round(time.Second).String()
}

func pipelineName(j *aiplatformpb.PipelineJob) string {
	return j.GetPipelineSpec().GetFields()["pipelineInfo"].GetStructValue().GetFields()["name"].GetStringValue()
}

func pipelineLabels(m map[string]string) string {
	var out []string
	for _, k := range sortedKeys(m) {
		out = append(out, k+": "+m[k])
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// ---- Runs

// pipelineRunsProvider's storage is the Cloud Storage server's address
// ("" when storage is off), for Import from Cloud Storage.
type pipelineRunsProvider struct {
	api     vertexAPI
	storage string
}

func (pipelineRunsProvider) ID() string           { return "vertex-pipeline-runs" }
func (pipelineRunsProvider) Title() string        { return "Pipelines" }
func (pipelineRunsProvider) ScopeKey() string     { return colabRegionKey }
func (pipelineRunsProvider) CreateTitle() string  { return "Create pipeline run" }
func (pipelineRunsProvider) CreateSubmit() string { return "Submit" }

func (p pipelineRunsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Run name", "Pipeline", "Created", "Duration", "Schedule", "Labels"},
		NameColumn: "ID", Noun: "runs", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each task of a run is a container of its image on this machine's Docker, with no network; its artifacts are written to the run's Cloud Storage location."}
	if project == "" {
		l.Prompt = "Pipeline runs are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListPipelineJobs(ctx, &aiplatformpb.ListPipelineJobsRequest{Parent: colabParent(ctx, project)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		switch j.GetState() {
		case aiplatformpb.PipelineState_PIPELINE_STATE_PENDING, aiplatformpb.PipelineState_PIPELINE_STATE_QUEUED, aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING:
			actions = append(actions, console.Action{ID: "cancel", Label: "Cancel", Confirm: "The run's running tasks are stopped; it is kept, Cancelled."})
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: pipelineStateText(j.GetState()), Actions: actions, Fields: map[string]string{
			"Run name": dashIfEmpty(j.GetDisplayName()), "Pipeline": dashIfEmpty(pipelineName(j)), "Created": vertexTime(j.GetCreateTime()),
			"Duration": pipelineDuration(j), "Schedule": dashIfEmpty(vertexLast(j.GetScheduleName())), "Labels": pipelineLabels(j.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p pipelineRunsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	j, err := c.GetPipelineJob(ctx, &aiplatformpb.GetPipelineJobRequest{Name: colabParent(ctx, project) + "/pipelineJobs/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	sa := j.GetServiceAccount()
	if sa == "" {
		sa = "Compute Engine default service account"
	}
	d := console.Detail{Title: dashIfEmpty(j.GetDisplayName()), Summary: []console.Property{
		{Label: "Run name", Value: dashIfEmpty(j.GetDisplayName())},
		{Label: "Name", Value: j.GetName(), Copy: j.GetName()},
		{Label: "Status", Value: pipelineStateText(j.GetState())},
		{Label: "Pipeline", Value: dashIfEmpty(pipelineName(j))},
		{Label: "Cloud storage location", Value: dashIfEmpty(j.GetRuntimeConfig().GetGcsOutputDirectory())},
		{Label: "Service account", Value: sa},
		{Label: "Schedule", Value: dashIfEmpty(j.GetScheduleName())},
		{Label: "Created", Value: vertexTime(j.GetCreateTime())},
		{Label: "Duration", Value: pipelineDuration(j)},
	}}
	if e := j.GetError(); e != nil && e.GetCode() != 0 {
		d.Summary = append(d.Summary, console.Property{Label: "Error", Value: e.GetMessage()})
	}
	// The Pipeline run analysis pane: the run's details and parameters.
	// The parameters the run used: runtime_config's, else the pipeline's
	// defaults.
	used := map[string]*structpb.Value{}
	defs := j.GetPipelineSpec().GetFields()["root"].GetStructValue().GetFields()["inputDefinitions"].GetStructValue().GetFields()["parameters"].GetStructValue()
	for k, def := range defs.GetFields() {
		if v, ok := def.GetStructValue().GetFields()["defaultValue"]; ok {
			used[k] = v
		}
	}
	for k, v := range j.GetRuntimeConfig().GetParameterValues() {
		used[k] = v
	}
	var params []console.Property
	for _, k := range sortedKeysOf(used) {
		params = append(params, console.Property{Label: k, Value: valueText(used[k])})
	}
	summary := []console.PropertyGroup{{Heading: "Run details", Properties: d.Summary}}
	if len(params) > 0 {
		summary = append(summary, console.PropertyGroup{Heading: "Run parameters", Properties: params})
	}
	d.Sections = []console.Section{
		{ID: "graph", Label: "Runtime graph", Kind: console.KindPipelineGraph, Pipeline: pipelineGraph(j, summary, project)},
	}
	return d, nil
}

func (pipelineRunsProvider) CreateForm() (string, []console.Field) {
	return "Create run", []console.Field{
		{Name: "source", Label: "Run source", Type: "select", Section: "Run details", Options: []string{"upload", "gcs"}, Default: "upload",
			OptionLabels: map[string]string{"upload": "Upload file", "gcs": "Import from Cloud Storage"},
			Help:         "The compiled pipeline definition (the JSON or YAML file the Kubeflow Pipelines compiler writes)."},
		{Name: "spec", Label: "Pipeline file contents", Type: "textarea", Section: "Run details", ShowWhen: "source=upload",
			Help: "Paste the compiled pipeline (pipeline.json or pipeline.yaml)."},
		{Name: "specUri", Label: "Cloud Storage URL", Type: "text", Section: "Run details", ShowWhen: "source=gcs", Pattern: `^(gs://[a-z0-9][\-a-z0-9._]+/.+)?$`,
			Help: "gs://BUCKET/path/pipeline.json"},
		{Name: "displayName", Label: "Run name", Type: "text", Required: true, Section: "Run details", Pattern: `^.{1,128}$`, Help: "Uniquely identifies the pipeline run."},
		{Name: "runSchedule", Label: "Run schedule", Type: "select", Section: "Run details", Options: []string{"one-off", "recurring"}, Default: "one-off",
			OptionLabels: map[string]string{"one-off": "One-off", "recurring": "Recurring"}},
		{Name: "startTime", Label: "Start time", Type: "select", Section: "Run details", ShowWhen: "runSchedule=recurring", Options: []string{"now", "on"}, Default: "now",
			OptionLabels: map[string]string{"now": "Immediately", "on": "On"}},
		{Name: "startAt", Label: "Start on", Type: "text", Section: "Run details", ShowWhen: "startTime=on", Pattern: `^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?Z)?$`, Help: "RFC 3339, in UTC: 2026-12-01T09:00:00Z"},
		{Name: "cron", Label: "Frequency", Type: "text", Section: "Run details", ShowWhen: "runSchedule=recurring", Default: "0 9 * * *",
			Pattern: `^((CRON_)?TZ=\S+ )?\S+ \S+ \S+ \S+ \S+$`, Help: "A cron expression: minute, hour, day of month, month, day of week."},
		{Name: "ends", Label: "Ends", Type: "select", Section: "Run details", ShowWhen: "runSchedule=recurring", Options: []string{"never", "on"}, Default: "never",
			OptionLabels: map[string]string{"never": "Never", "on": "On"}},
		{Name: "endAt", Label: "End on", Type: "text", Section: "Run details", ShowWhen: "ends=on", Pattern: `^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?Z)?$`, Help: "RFC 3339, in UTC."},
		{Name: "serviceAccount", Label: "Service account", Type: "text", Section: "Advanced options", Pattern: `^([^@\s]+@[^@\s]+)?$`,
			Help: "The run's identity; you need the Service Account User role on it. Empty: the Compute Engine default service account."},
		{Name: "output", Label: "Cloud storage location", Type: "text", Required: true, Section: "Runtime configuration", Pattern: `^(gs://)?[a-z0-9][\-a-z0-9._]{1,221}(/.*)?$`,
			Help: "gs://BUCKET/folder: the root output directory of the pipeline, where its artifacts are written."},
		{Name: "caching", Label: "Caching configuration", Type: "select", Section: "Runtime configuration", Options: []string{"default", "enabled", "disabled"}, Default: "default",
			OptionLabels: map[string]string{"default": "Use the pipeline's settings", "enabled": "Enable execution caching", "disabled": "Disable execution caching"},
			Help:         "Whether the run's steps reuse the outputs of earlier executions with the same inputs, as PipelineJob's enable_caching sets every step's caching option."},
		{Name: "failurePolicy", Label: "Failure policy", Type: "select", Section: "Runtime configuration", Options: []string{"slow", "fast"}, Default: "slow",
			OptionLabels: map[string]string{"slow": "Run all steps to completion", "fast": "Fail the pipeline run after one step fails"}},
		{Name: "parameters", Label: "Pipeline parameters", Type: "textarea", Section: "Runtime configuration",
			Help: `A JSON object of the pipeline's parameters: {"greeting": "hello", "times": 3}`},
	}
}

// pipelineSpecOf reads a compiled pipeline (JSON or YAML) as a Struct.
func pipelineSpecOf(text string) (*structpb.Struct, error) {
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		return nil, fmt.Errorf("the pipeline file is not JSON or YAML: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("the pipeline file is not a pipeline definition: %v", err)
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("the pipeline file is not a pipeline definition: %v", err)
	}
	return s, nil
}

// readPipelineFile is a gs:// pipeline file's contents.
func (p pipelineRunsProvider) readPipelineFile(ctx context.Context, uri string) (string, error) {
	bucket, object, ok := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if !ok || object == "" {
		return "", errors.New("Cloud Storage URL must be gs://BUCKET/OBJECT")
	}
	if p.storage == "" {
		return "", errors.New("Import from Cloud Storage needs Cloud Storage, which this instance does not serve")
	}
	sc, err := storageProvider{endpoint: p.storage}.storageClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = sc.Close() }()
	r, err := sc.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return "", fmt.Errorf("read gs://%s/%s: %v", bucket, object, err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, 8<<20))
	return string(b), err
}

func (p pipelineRunsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a run")
	}
	text := values["spec"]
	if values["source"] == "gcs" {
		var err error
		if text, err = p.readPipelineFile(ctx, values["specUri"]); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the run needs a pipeline file")
	}
	spec, err := pipelineSpecOf(text)
	if err != nil {
		return "", err
	}
	if c := values["caching"]; c == "enabled" || c == "disabled" {
		setPipelineCaching(spec, c == "enabled")
	}
	out := strings.TrimSpace(values["output"])
	if !strings.HasPrefix(out, "gs://") {
		out = "gs://" + out
	}
	rc := &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: out}
	if values["failurePolicy"] == "fast" {
		rc.FailurePolicy = aiplatformpb.PipelineFailurePolicy_PIPELINE_FAILURE_POLICY_FAIL_FAST
	}
	if ps := strings.TrimSpace(values["parameters"]); ps != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(ps), &m); err != nil {
			return "", fmt.Errorf("Pipeline parameters must be a JSON object: %v", err)
		}
		rc.ParameterValues = map[string]*structpb.Value{}
		for k, v := range m {
			sv, err := structpb.NewValue(v)
			if err != nil {
				return "", fmt.Errorf("Pipeline parameter %q: %v", k, err)
			}
			rc.ParameterValues[k] = sv
		}
	}
	job := &aiplatformpb.PipelineJob{DisplayName: values["displayName"], PipelineSpec: spec, RuntimeConfig: rc, ServiceAccount: strings.TrimSpace(values["serviceAccount"])}
	parent := colabParent(ctx, project)
	if values["runSchedule"] == "recurring" {
		sc := &aiplatformpb.Schedule{DisplayName: values["displayName"], TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: values["cron"]},
			MaxConcurrentRunCount: 1, Request: &aiplatformpb.Schedule_CreatePipelineJobRequest{CreatePipelineJobRequest: &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: job}}}
		for _, f := range []struct {
			on, at string
			dst    **timestamppb.Timestamp
		}{{values["startTime"], values["startAt"], &sc.StartTime}, {values["ends"], values["endAt"], &sc.EndTime}} {
			if f.on == "on" {
				t, err := time.Parse(time.RFC3339, f.at)
				if err != nil {
					return "", fmt.Errorf("%q is not an RFC 3339 time", f.at)
				}
				*f.dst = timestamppb.New(t)
			}
		}
		c, err := p.api.schedules(ctx)
		if err != nil {
			return "", err
		}
		defer func() { _ = c.Close() }()
		s, err := c.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: sc})
		if err != nil {
			return "", err
		}
		return vertexLast(s.GetName()), nil
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	j, err := c.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: job})
	if err != nil {
		return "", err
	}
	return vertexLast(j.GetName()), nil
}

func (p pipelineRunsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeletePipelineJob(ctx, &aiplatformpb.DeletePipelineJobRequest{Name: colabParent(ctx, project) + "/pipelineJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (pipelineRunsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p pipelineRunsProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.pipelines(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.CancelPipelineJob(ctx, &aiplatformpb.CancelPipelineJobRequest{Name: colabParent(ctx, project) + "/pipelineJobs/" + name})
}

// ---- Schedules

type pipelineSchedulesProvider struct{ colabSchedulesProvider }

func (pipelineSchedulesProvider) ID() string    { return "vertex-pipeline-schedules" }
func (pipelineSchedulesProvider) Title() string { return "Pipelines" }

func (p pipelineSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Schedule name", "Frequency", "Next run", "Runs started", "Created"},
		NameColumn: "ID", Noun: "schedules", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each run of a schedule is a pipeline run, listed on the Runs tab. Create one with Create pipeline run's Recurring run schedule."}
	if project == "" {
		l.Prompt = "Schedules are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.schedules(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListSchedules(ctx, &aiplatformpb.ListSchedulesRequest{Parent: colabParent(ctx, project), Filter: "create_pipeline_job_request:*"}); ; {
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

func (p pipelineSchedulesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
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
	j := s.GetCreatePipelineJobRequest().GetPipelineJob()
	last := "-"
	if r := s.GetLastScheduledRunResponse(); r != nil {
		last = vertexTime(r.GetScheduledRunTime()) + ": " + r.GetRunResponse()
	}
	return console.Detail{Title: "Schedule details", Summary: []console.Property{
		{Label: "Schedule name", Value: s.GetDisplayName()},
		{Label: "Name", Value: s.GetName(), Copy: s.GetName()},
		{Label: "Status", Value: colabScheduleState(s.GetState())},
		{Label: "Frequency", Value: s.GetCron()},
		{Label: "Start time", Value: colabOptTime(s.GetStartTime(), s.GetStartTime() != nil)},
		{Label: "End time", Value: colabOptTime(s.GetEndTime(), s.GetEndTime() != nil)},
		{Label: "Next run", Value: colabOptTime(s.GetNextRunTime(), s.GetNextRunTime() != nil)},
		{Label: "Pipeline", Value: dashIfEmpty(pipelineName(j))},
		{Label: "Cloud storage location", Value: dashIfEmpty(j.GetRuntimeConfig().GetGcsOutputDirectory())},
		{Label: "Service account", Value: dashIfEmpty(j.GetServiceAccount())},
		{Label: "Runs started", Value: fmt.Sprint(s.GetStartedRunCount())},
		{Label: "Last scheduled run", Value: last},
		{Label: "Created", Value: vertexTime(s.GetCreateTime())},
	}}, nil
}

// setPipelineCaching sets every task's cachingOptions.enableCache, as the
// Vertex AI SDK's PipelineJob(enable_caching=...) does.
func setPipelineCaching(spec *structpb.Struct, on bool) {
	comps := []*structpb.Struct{spec.GetFields()["root"].GetStructValue()}
	for _, c := range spec.GetFields()["components"].GetStructValue().GetFields() {
		comps = append(comps, c.GetStructValue())
	}
	for _, c := range comps {
		for _, t := range c.GetFields()["dag"].GetStructValue().GetFields()["tasks"].GetStructValue().GetFields() {
			if ts := t.GetStructValue(); ts != nil {
				ts.Fields["cachingOptions"] = structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{"enableCache": structpb.NewBoolValue(on)}})
			}
		}
	}
}
