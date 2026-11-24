package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataflow"
)

// The console's Dataflow pages (#2215), as Google's: the Jobs list (name,
// type, end time, elapsed time, start time, status, SDK version, job ID,
// region), Create job from template, and a job's page with its Job info
// beside the Job graph, Execution details, Job metrics, Autoscaling, Job
// logs and Worker logs (the job's dataflow_step entries in Cloud
// Logging), and Stop (Cancel, or Drain for a streaming job). Console names
// are REGION/JOB_ID.

const dataflowNotStarted = "Dataflow has not started"

var dataflowJobNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

type dataflowProvider struct {
	svc     *dataflowService
	logging *loggingService
}

func (dataflowProvider) ID() string    { return "dataflow" }
func (dataflowProvider) Title() string { return "Jobs" }

func (p dataflowProvider) api() (*dataflow.Server, error) {
	if s := p.svc.API(); s != nil {
		return s, nil
	}
	return nil, errors.New(dataflowNotStarted)
}

// dataflowState is Google's word for a job state: "Running", "Cancelled".
func dataflowState(st dataflowpb.JobState) string {
	s := strings.TrimPrefix(st.String(), "JOB_STATE_")
	if s == "UNKNOWN" || s == "" {
		return "Not started"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

func dataflowType(t dataflowpb.JobType) string {
	switch t {
	case dataflowpb.JobType_JOB_TYPE_STREAMING:
		return "Streaming"
	case dataflowpb.JobType_JOB_TYPE_BATCH:
		return "Batch"
	}
	return "—"
}

func dataflowTerminal(st dataflowpb.JobState) bool {
	switch st {
	case dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED,
		dataflowpb.JobState_JOB_STATE_UPDATED, dataflowpb.JobState_JOB_STATE_DRAINED:
		return true
	}
	return false
}

// dataflowElapsed is the time from the job's start to its end, or to now
// while it runs, as "1 hr 2 min 3 sec".
func dataflowElapsed(j *dataflowpb.Job, now time.Time) string {
	start := j.GetStartTime()
	if start == nil {
		start = j.GetCreateTime()
	}
	if start == nil {
		return "—"
	}
	end := now
	if dataflowTerminal(j.GetCurrentState()) && j.GetCurrentStateTime() != nil {
		end = j.GetCurrentStateTime().AsTime()
	}
	d := end.Sub(start.AsTime()).Round(time.Second)
	if d < 0 {
		d = 0
	}
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%d hr %d min %d sec", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%d min %d sec", m, sec)
	}
	return fmt.Sprintf("%d sec", sec)
}

func dataflowEnd(j *dataflowpb.Job) string {
	if dataflowTerminal(j.GetCurrentState()) {
		return kmsTime(j.GetCurrentStateTime())
	}
	return "—"
}

func dataflowSDK(j *dataflowpb.Job) string {
	v := j.GetJobMetadata().GetSdkVersion()
	if v.GetVersion() == "" {
		return "—"
	}
	if v.GetVersionDisplayName() != "" {
		return v.GetVersionDisplayName() + " " + v.GetVersion()
	}
	return v.GetVersion()
}

func (p dataflowProvider) List(_ context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Type", "End time", "Elapsed time", "Start time", "Status", "SDK version", "Job ID", "Region"},
		NameColumn: "Name", Noun: "jobs"}
	if project == "" {
		l.Prompt = "Dataflow lists jobs per project. Choose one in the toolbar."
		return l, nil
	}
	now := time.Now()
	for _, j := range s.Jobs(project) {
		st := dataflowState(j.GetCurrentState())
		l.Items = append(l.Items, console.Resource{Name: j.GetName(), Status: st, Opens: []string{j.GetLocation() + "/" + j.GetId()},
			Fields: map[string]string{"Type": dataflowType(j.GetType()), "End time": dataflowEnd(j), "Elapsed time": dataflowElapsed(j, now),
				"Start time": kmsTime(j.GetCreateTime()), "Status": st, "SDK version": dataflowSDK(j), "Job ID": j.GetId(), "Region": j.GetLocation()}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// CreateForm is Google's Create job from template page, for a classic
// template in Cloud Storage (Custom Template).
func (dataflowProvider) CreateForm() (string, []console.Field) {
	return "Create job from template", []console.Field{
		{Name: "jobName", Label: "Job name", Type: "text", Required: true, Pattern: dataflowJobNameRE.String(),
			Help: "Lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit. It must be unique among the project's running jobs."},
		{Name: "region", Label: "Regional endpoint", Type: "text", Required: true, Default: dataflow.DefaultLocation,
			Help: "The region the job runs in and its metadata is kept."},
		{Name: "template", Label: "Dataflow template", Type: "select", Default: "custom", Options: []string{"custom"},
			OptionLabels: map[string]string{"custom": "Custom Template"},
			Help:         "A classic template staged in Cloud Storage, such as one the Beam SDK writes with --template_location. Google's own templates are not built here yet."},
		{Name: "gcsPath", Label: "Template path", Type: "text", Required: true, Pattern: `^gs://[^/]+/.+$`, Section: "Required parameters",
			Help: "gs://BUCKET/PATH of the template file. Its metadata, if any, is read from PATH_metadata."},
		{Name: "tempLocation", Label: "Temporary location", Type: "text", Required: true, Pattern: `^gs://[^/]+(/.*)?$`, Section: "Required parameters",
			Help: "gs://BUCKET/PATH for the job's temporary files."},
		{Name: "parameters", Label: "Additional parameters", Type: "map", Section: "Optional parameters",
			Help: "The template's parameters, by name."},
		{Name: "maxWorkers", Label: "Max workers", Type: "number", Pattern: `^([1-9][0-9]{0,2}|1000)?$`, Section: "Optional parameters",
			Help: "The most workers autoscaling may use, 1 to 1000."},
		{Name: "numWorkers", Label: "Number of workers", Type: "number", Pattern: `^([1-9][0-9]{0,2}|1000)?$`, Section: "Optional parameters",
			Help: "The workers the job starts with."},
	}
}

func (dataflowProvider) CreateTitle() string  { return "Create job from template" }
func (dataflowProvider) CreateSubmit() string { return "Run job" }
func (dataflowProvider) CreateLayout() string { return console.CreateLayoutPage }
func (dataflowProvider) CreateOnPage() bool   { return true }

func (p dataflowProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	params, err := console.ParseMap(values["parameters"])
	if err != nil {
		return "", err
	}
	env := &dataflowpb.RuntimeEnvironment{TempLocation: strings.TrimSpace(values["tempLocation"])}
	for _, f := range []struct {
		key string
		to  *int32
	}{{"maxWorkers", &env.MaxWorkers}, {"numWorkers", &env.NumWorkers}} {
		if v := strings.TrimSpace(values[f.key]); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return "", fmt.Errorf("%s %q is not a number", f.key, v)
			}
			*f.to = int32(n)
		}
	}
	region := strings.TrimSpace(values["region"])
	resp, err := s.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: project, Location: region,
		Template:         &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: strings.TrimSpace(values["gcsPath"])},
		LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: strings.TrimSpace(values["jobName"]), Parameters: params, Environment: env}})
	if err != nil {
		return "", err
	}
	return resp.GetJob().GetLocation() + "/" + resp.GetJob().GetId(), nil
}

// job is the job a console name names, in full.
func (p dataflowProvider) job(ctx context.Context, project, name string) (*dataflow.Server, *dataflowpb.Job, error) {
	s, err := p.api()
	if err != nil {
		return nil, nil, err
	}
	loc, id, ok := strings.Cut(name, "/")
	if !ok || loc == "" || id == "" {
		return nil, nil, fmt.Errorf("%q is not REGION/JOB_ID", name)
	}
	j, err := s.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: loc, JobId: id, View: dataflowpb.JobView_JOB_VIEW_ALL})
	return s, j, err
}

func (p dataflowProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the job: " + consoleErr(err)}, nil
	}
	pid, loc, id := j.GetProjectId(), j.GetLocation(), j.GetId()
	metrics, _ := s.GetJobMetrics(ctx, &dataflowpb.GetJobMetricsRequest{ProjectId: pid, Location: loc, JobId: id})
	counts := dataflowElementCounts(metrics)
	msgs, _ := s.ListJobMessages(ctx, &dataflowpb.ListJobMessagesRequest{ProjectId: pid, Location: loc, JobId: id,
		MinimumImportance: dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED, PageSize: 1000})
	exec, _ := s.GetJobExecutionDetails(ctx, &dataflowpb.GetJobExecutionDetailsRequest{ProjectId: pid, Location: loc, JobId: id})

	workers := "0"
	if ev := msgs.GetAutoscalingEvents(); len(ev) > 0 {
		workers = strconv.FormatInt(ev[len(ev)-1].GetTargetNumWorkers(), 10)
	}
	info := []console.Property{
		{Label: "Job name", Value: j.GetName()}, {Label: "Job ID", Value: j.GetId(), Copy: "Copy job ID"},
		{Label: "Job type", Value: dataflowType(j.GetType())}, {Label: "Job status", Value: dataflowState(j.GetCurrentState())},
		{Label: "SDK version", Value: dataflowSDK(j)}, {Label: "Job region", Value: j.GetLocation()},
		{Label: "Worker location", Value: j.GetLocation()}, {Label: "Current workers", Value: workers},
		{Label: "Start time", Value: kmsTime(j.GetCreateTime())}, {Label: "Elapsed time", Value: dataflowElapsed(j, time.Now())},
		{Label: "Encryption type", Value: "Google-managed key"},
	}
	if j.GetReplacedByJobId() != "" {
		info = append(info, console.Property{Label: "Replaced by", Value: j.GetReplacedByJobId()})
	}
	if j.GetReplaceJobId() != "" {
		info = append(info, console.Property{Label: "Replaces", Value: j.GetReplaceJobId()})
	}
	var options []console.Property
	opts := j.GetEnvironment().GetSdkPipelineOptions().GetFields()["options"].GetStructValue().GetFields()
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := opts[k]
		text := v.GetStringValue()
		if text == "" {
			if b, err := v.MarshalJSON(); err == nil {
				text = string(b)
			}
		}
		options = append(options, console.Property{Label: k, Value: text})
	}
	groups := []console.PropertyGroup{{Heading: "Job info", Properties: info}}
	if len(options) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Pipeline options", Properties: options})
	}
	if labels := j.GetLabels(); len(labels) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: []console.Property{{Label: "Labels", Value: labelsText(labels)}}})
	}

	return console.Detail{
		Summary: info[:4],
		Sections: []console.Section{
			{ID: "graph", Label: "Job graph", Kind: console.KindGraph, Pipeline: dataflowGraph(j, counts)},
			{ID: "execution", Label: "Execution details", Listing: dataflowStages(j, exec)},
			{ID: "metrics", Label: "Job metrics", Listing: dataflowMetricsListing(metrics)},
			{ID: "autoscaling", Label: "Autoscaling", Listing: dataflowAutoscaling(msgs)},
			{ID: "info", Label: "Job info", Kind: console.KindProperties, Groups: groups},
			{ID: "joblogs", Label: "Job logs", Group: "Logs", Listing: dataflowJobLogs(msgs)},
			{ID: "workerlogs", Label: "Worker logs", Group: "Logs", Listing: p.workerLogs(ctx, j)},
		},
	}, nil
}

// dataflowElementCounts is each PCollection's ElementCount, by name.
func dataflowElementCounts(m *dataflowpb.JobMetrics) map[string]int64 {
	out := map[string]int64{}
	for _, u := range m.GetMetrics() {
		n := u.GetName()
		if n.GetName() != "ElementCount" || n.GetContext()["tentative"] != "" {
			continue
		}
		if pc, ok := strings.CutSuffix(n.GetContext()["original_name"], "-ElementCount"); ok {
			out[pc] += int64(u.GetScalar().GetNumberValue())
		}
	}
	return out
}

var dataflowKindLabels = map[dataflowpb.KindType]string{
	dataflowpb.KindType_PAR_DO_KIND: "ParDo", dataflowpb.KindType_GROUP_BY_KEY_KIND: "GroupByKey", dataflowpb.KindType_FLATTEN_KIND: "Flatten",
	dataflowpb.KindType_READ_KIND: "Read", dataflowpb.KindType_WRITE_KIND: "Write", dataflowpb.KindType_CONSTANT_KIND: "Constant",
	dataflowpb.KindType_SINGLETON_KIND: "Singleton", dataflowpb.KindType_SHUFFLE_KIND: "Shuffle", dataflowpb.KindType_UNKNOWN_KIND: "Composite",
}

// dataflowGraph is the job's top-level transforms (the user's steps; a
// composite's parts are named under it), each reading the steps that
// produce its inputs.
func dataflowGraph(j *dataflowpb.Job, counts map[string]int64) *console.PipelineGraph {
	ts := j.GetPipelineDescription().GetOriginalPipelineTransform()
	top := func(name string) string { t, _, _ := strings.Cut(name, "/"); return t }
	producer := map[string]string{}
	for _, t := range ts {
		for _, o := range t.GetOutputCollectionName() {
			if _, ok := producer[o]; !ok || !strings.Contains(t.GetName(), "/") {
				producer[o] = top(t.GetName())
			}
		}
	}
	state := dataflowState(j.GetCurrentState())
	g := &console.PipelineGraph{Steps: []console.PipelineStep{}}
	for _, t := range ts {
		if strings.Contains(t.GetName(), "/") {
			continue
		}
		var parts int
		for _, u := range ts {
			if strings.HasPrefix(u.GetName(), t.GetName()+"/") {
				parts++
			}
		}
		seen := map[string]bool{}
		var inputs []string
		for _, in := range t.GetInputCollectionName() {
			if p, ok := producer[in]; ok && p != t.GetName() && !seen[p] {
				seen[p] = true
				inputs = append(inputs, p)
			}
		}
		var out int64
		for _, o := range t.GetOutputCollectionName() {
			out += counts[o]
		}
		kind := dataflowKindLabels[t.GetKind()]
		if parts > 0 && t.GetKind() == dataflowpb.KindType_UNKNOWN_KIND {
			kind = "Composite"
		}
		props := []console.Property{{Label: "Elements added", Value: strconv.FormatInt(out, 10)},
			{Label: "Input collections", Value: strings.Join(t.GetInputCollectionName(), ", ")},
			{Label: "Output collections", Value: strings.Join(t.GetOutputCollectionName(), ", ")}}
		if parts > 0 {
			props = append(props, console.Property{Label: "Substeps", Value: strconv.Itoa(parts)})
		}
		g.Steps = append(g.Steps, console.PipelineStep{ID: t.GetName(), Name: t.GetName(), Kind: kind, State: state, Inputs: inputs, Properties: props})
	}
	return g
}

func dataflowExecState(st dataflowpb.ExecutionState) string {
	s := strings.TrimPrefix(st.String(), "EXECUTION_STATE_")
	return strings.ToUpper(s[:1]) + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

func dataflowStages(j *dataflowpb.Job, exec *dataflowpb.JobExecutionDetails) console.Listing {
	l := console.Listing{Columns: []string{"Stage name", "Status", "Start time", "End time", "Steps"}, NameColumn: "Stage ID", Noun: "stages"}
	names := map[string]*dataflowpb.ExecutionStageSummary{}
	for _, st := range j.GetPipelineDescription().GetExecutionPipelineStage() {
		names[st.GetId()] = st
	}
	for _, st := range exec.GetStages() {
		var steps []string
		for _, c := range names[st.GetStageId()].GetComponentTransform() {
			steps = append(steps, c.GetUserName())
		}
		l.Items = append(l.Items, console.Resource{Name: st.GetStageId(), Status: dataflowExecState(st.GetState()), Fields: map[string]string{
			"Stage name": names[st.GetStageId()].GetName(), "Status": dataflowExecState(st.GetState()),
			"Start time": kmsTime(st.GetStartTime()), "End time": dataflowTimeOrDash(st.GetEndTime()), "Steps": strings.Join(steps, ", ")}})
	}
	l.Total = len(l.Items)
	if l.Total == 0 {
		l.Note = "The job's stages are listed once its pipeline has been read."
	}
	return l
}

func dataflowTimeOrDash(t *timestamppb.Timestamp) string {
	if t == nil {
		return "—"
	}
	return kmsTime(t)
}

func dataflowMetricsListing(m *dataflowpb.JobMetrics) console.Listing {
	l := console.Listing{Columns: []string{"Origin", "Step", "Collection", "Value", "Updated"}, NameColumn: "Metric", Noun: "metrics"}
	for _, u := range m.GetMetrics() {
		n := u.GetName()
		if n.GetContext()["tentative"] != "" {
			continue
		}
		step := n.GetContext()["step"]
		if step == "" {
			step = strings.TrimSuffix(n.GetContext()["output_user_name"], ".out0")
		}
		value := strconv.FormatFloat(u.GetScalar().GetNumberValue(), 'f', -1, 64)
		if d := u.GetDistribution().GetStructValue().GetFields(); d != nil {
			value = fmt.Sprintf("count %v, min %v, max %v, sum %v", d["count"].GetNumberValue(), d["min"].GetNumberValue(),
				d["max"].GetNumberValue(), d["sum"].GetNumberValue())
		}
		l.Items = append(l.Items, console.Resource{Name: n.GetName(), Fields: map[string]string{"Origin": n.GetOrigin(), "Step": kmsOrDash(step),
			"Collection": kmsOrDash(strings.TrimSuffix(n.GetContext()["original_name"], "-ElementCount")), "Value": value,
			"Updated": dataflowTimeOrDash(u.GetUpdateTime())}})
	}
	l.Total = len(l.Items)
	if l.Total == 0 {
		l.Note = "The job has reported no metrics yet."
	}
	return l
}

func dataflowAutoscaling(msgs *dataflowpb.ListJobMessagesResponse) console.Listing {
	l := console.Listing{Columns: []string{"Event", "Workers", "Description"}, NameColumn: "Time", Noun: "autoscaling events"}
	for _, e := range msgs.GetAutoscalingEvents() {
		l.Items = append(l.Items, console.Resource{Name: kmsTime(e.GetTime()), Fields: map[string]string{
			"Event":   strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(e.GetEventType().String(), "TYPE_"), "_", " ")),
			"Workers": fmt.Sprintf("%d → %d", e.GetCurrentNumWorkers(), e.GetTargetNumWorkers()), "Description": e.GetDescription().GetMessageText()}})
	}
	l.Total = len(l.Items)
	return l
}

func dataflowImportance(i dataflowpb.JobMessageImportance) string {
	switch i {
	case dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR:
		return "Error"
	case dataflowpb.JobMessageImportance_JOB_MESSAGE_WARNING:
		return "Warning"
	case dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC:
		return "Info"
	}
	return "Detailed"
}

func dataflowJobLogs(msgs *dataflowpb.ListJobMessagesResponse) console.Listing {
	l := console.Listing{Columns: []string{"Severity", "Message"}, NameColumn: "Time", Noun: "job log entries"}
	ms := msgs.GetJobMessages()
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		l.Items = append(l.Items, console.Resource{Name: kmsTime(m.GetTime()), Fields: map[string]string{
			"Severity": dataflowImportance(m.GetMessageImportance()), "Message": m.GetMessageText()}})
	}
	l.Total = len(l.Items)
	return l
}

// workerLogs is the job's dataflow_step entries in the instance's Cloud
// Logging, newest first.
func (p dataflowProvider) workerLogs(ctx context.Context, j *dataflowpb.Job) console.Listing {
	l := console.Listing{Columns: []string{"Severity", "Message"}, NameColumn: "Time", Noun: "worker log entries"}
	if p.logging == nil || p.logging.api == nil {
		l.Unavailable = "Cloud Logging is not served, so the workers' lines are not kept."
		return l
	}
	resp, err := p.logging.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + j.GetProjectId()},
		Filter:  fmt.Sprintf(`resource.type="dataflow_step" AND resource.labels.job_id=%q`, j.GetId()),
		OrderBy: "timestamp desc", PageSize: 200})
	if err != nil {
		l.Unavailable = "cannot read the worker logs: " + consoleErr(err)
		return l
	}
	for _, e := range resp.GetEntries() {
		l.Items = append(l.Items, console.Resource{Name: kmsTime(e.GetTimestamp()), Fields: map[string]string{
			"Severity": consoleSeverityLabel(e.GetSeverity().String()), "Message": console.Redact(entryMessage(e))}})
	}
	l.Total = len(l.Items)
	l.Note = fmt.Sprintf(`The Logs Explorer shows them with resource.type="dataflow_step" AND resource.labels.job_id="%s".`, j.GetId())
	return l
}

func consoleSeverityLabel(s string) string {
	if s == "" || s == "DEFAULT" {
		return "Default"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// DetailActions is the job page's Stop: Cancel, and Drain for a streaming
// job, while the job runs.
func (p dataflowProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, j, err := p.job(ctx, project, path[0])
	if err != nil || dataflowTerminal(j.GetCurrentState()) || j.GetCurrentState() == dataflowpb.JobState_JOB_STATE_CANCELLING ||
		j.GetCurrentState() == dataflowpb.JobState_JOB_STATE_DRAINING {
		return nil
	}
	modes := []string{"cancel"}
	labels := map[string]string{"cancel": "Cancel", "drain": "Drain"}
	help := "Cancel stops the job at once; data in flight is lost."
	if j.GetType() == dataflowpb.JobType_JOB_TYPE_STREAMING {
		modes = append(modes, "drain")
		help += " Drain stops reading new data, finishes what has been read, closes the open windows, and then stops the job."
	}
	return []console.Action{{ID: "stop", Label: "Stop", Title: "Stop job", Submit: "Stop job", Destructive: true, Fields: []console.Field{
		{Name: "mode", Label: "Stop the job by", Type: "radio", Default: "cancel", Options: modes, OptionLabels: labels, Help: help}}}}
}

func (p dataflowProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 || action != "stop" {
		return errors.New("no such action")
	}
	s, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return err
	}
	st := dataflowpb.JobState_JOB_STATE_CANCELLED
	if values["mode"] == "drain" {
		st = dataflowpb.JobState_JOB_STATE_DRAINED
	}
	_, err = s.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: j.GetProjectId(), Location: j.GetLocation(), JobId: j.GetId(),
		Job: &dataflowpb.Job{RequestedState: st}})
	return err
}
