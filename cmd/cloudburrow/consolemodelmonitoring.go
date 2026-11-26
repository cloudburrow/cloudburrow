package main

// Agent Platform's Monitoring page (#2214), in Google's layout ("Set up
// model monitoring", https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/set-up-model-monitoring;
// "Run a monitoring job", .../run-monitoring-job; read 2026-11-24):
//
//   - Monitoring lists the model monitors; Configure monitoring selects a
//     model and its version, the schema of its input features and
//     prediction outputs, an optional training dataset, Advanced options'
//     Cloud Storage bucket for the results, then the objectives, each
//     with its metric and alert threshold, and the alerts' email
//     addresses and notification channels; Set up creates the monitor;
//   - a monitor's page shows its runs, each run's target-versus-baseline
//     feature statistics and its alerts; Run now runs a job on a target
//     dataset; a schedule (name, cron, time zone, end date) runs one on
//     each tick, with Pause, Resume and Delete;
//   - v1's model deployment monitoring jobs watch an endpoint's deployed
//     models for skew and drift, with Pause, Resume and Delete ("You must
//     pause a job before you can delete it").
//
// UNVERIFIED (re-read 2026-11-24): the lists' columns, the tabs' names
// (Model monitors, Schedules, Deployment monitoring jobs) and the monitor
// page's sections (Google's docs show none of them); the schema is entered
// as one NAME:TYPE per line.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	aiplatformbeta "cloud.google.com/go/aiplatform/apiv1beta1"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (v vertexAPI) monitoring(ctx context.Context) (*aiplatformbeta.ModelMonitoringClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatformbeta.NewModelMonitoringClient(ctx, v.opts()...)
}

func (v vertexAPI) betaSchedules(ctx context.Context) (*aiplatformbeta.ScheduleClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatformbeta.NewScheduleClient(ctx, v.opts()...)
}

var monitorTabs = []string{"vertex-model-monitors", "vertex-model-monitoring-schedules", "vertex-model-deployment-monitoring"}

// monitorStateText is a v1 or v1beta1 job's state as the lists word it.
func monitorStateText(s fmt.Stringer) string {
	t := strings.TrimPrefix(s.String(), "JOB_STATE_")
	if t == "" || t == "UNSPECIFIED" {
		return "Unknown"
	}
	t = strings.ReplaceAll(strings.ToLower(t), "_", " ")
	return strings.ToUpper(t[:1]) + t[1:]
}

// parseSchemaFields reads NAME:TYPE lines.
func parseSchemaFields(text string) ([]*betapb.ModelMonitoringSchema_FieldSchema, error) {
	var out []*betapb.ModelMonitoringSchema_FieldSchema
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, typ, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%q is not NAME:TYPE (TYPE is float, integer, boolean, string or categorical)", line)
		}
		out = append(out, &betapb.ModelMonitoringSchema_FieldSchema{Name: strings.TrimSpace(name), DataType: strings.TrimSpace(typ)})
	}
	return out, nil
}

func monitorInput(uri, format string) *betapb.ModelMonitoringInput {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil
	}
	if strings.HasPrefix(uri, "bq://") {
		return &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: &betapb.ModelMonitoringInput_ModelMonitoringDataset{
			DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_BigquerySource{BigquerySource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringBigQuerySource{
				Connection: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringBigQuerySource_TableUri{TableUri: uri}}}}}}
	}
	if strings.Contains(uri, "/endpoints/") && !strings.HasPrefix(uri, "gs://") {
		return &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_VertexEndpointLogs_{VertexEndpointLogs: &betapb.ModelMonitoringInput_VertexEndpointLogs{Endpoints: []string{uri}}}}
	}
	f := betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_CSV
	if format == "jsonl" {
		f = betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_JSONL
	}
	return &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: &betapb.ModelMonitoringInput_ModelMonitoringDataset{
		DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_GcsSource{GcsSource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource{GcsUri: uri, Format: f}}}}}
}

func inputText(in *betapb.ModelMonitoringInput) string {
	switch {
	case in.GetColumnizedDataset().GetGcsSource() != nil:
		return in.GetColumnizedDataset().GetGcsSource().GetGcsUri()
	case in.GetColumnizedDataset().GetBigquerySource() != nil:
		return in.GetColumnizedDataset().GetBigquerySource().GetTableUri()
	case in.GetColumnizedDataset().GetVertexDataset() != "":
		return in.GetColumnizedDataset().GetVertexDataset()
	case in.GetVertexEndpointLogs() != nil:
		return strings.Join(in.GetVertexEndpointLogs().GetEndpoints(), ", ")
	case in.GetBatchPredictionOutput() != nil:
		return in.GetBatchPredictionOutput().GetBatchPredictionJob()
	}
	return "-"
}

func objectivesText(o *betapb.ModelMonitoringObjectiveSpec_TabularObjective) string {
	var out []string
	if o.GetFeatureDriftSpec() != nil {
		out = append(out, "Input feature drift")
	}
	if o.GetPredictionOutputDriftSpec() != nil {
		out = append(out, "Prediction output drift")
	}
	if o.GetFeatureAttributionSpec() != nil {
		out = append(out, "Feature attribution")
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// ---- Model monitors

type modelMonitorsProvider struct{ api vertexAPI }

func (modelMonitorsProvider) ID() string           { return "vertex-model-monitors" }
func (modelMonitorsProvider) Title() string        { return "Monitoring" }
func (modelMonitorsProvider) ScopeKey() string     { return colabRegionKey }
func (modelMonitorsProvider) CreateTitle() string  { return "Configure monitoring" }
func (modelMonitorsProvider) CreateSubmit() string { return "Set up" }

func (p modelMonitorsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Monitor name", "Model", "Version", "Objectives", "Training dataset", "Created"},
		NameColumn: "ID", Noun: "model monitors", Scope: colabScope(ctx),
		Note: "A monitoring run reads its datasets (Cloud Storage CSV or JSONL files, or the predictions an endpoint logged) and computes each feature's distance on this machine."}
	if project == "" {
		l.Prompt = "Model monitors are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.monitoring(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListModelMonitors(ctx, &betapb.ListModelMonitorsRequest{Parent: colabParent(ctx, project)}); ; {
		m, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		vm := m.GetModelMonitoringTarget().GetVertexModel()
		l.Items = append(l.Items, console.Resource{Name: vertexLast(m.GetName()), Fields: map[string]string{
			"Monitor name": dashIfEmpty(m.GetDisplayName()), "Model": dashIfEmpty(vertexLast(vm.GetModel())), "Version": dashIfEmpty(vm.GetModelVersionId()),
			"Objectives": objectivesText(m.GetTabularObjective()), "Training dataset": inputText(m.GetTrainingDataset()), "Created": vertexTime(m.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p modelMonitorsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.monitoring(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	name := colabParent(ctx, project) + "/modelMonitors/" + path[0]
	m, err := c.GetModelMonitor(ctx, &betapb.GetModelMonitorRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	vm := m.GetModelMonitoringTarget().GetVertexModel()
	var feats []string
	for _, f := range m.GetModelMonitoringSchema().GetFeatureFields() {
		feats = append(feats, f.GetName()+" ("+f.GetDataType()+")")
	}
	var preds []string
	for _, f := range m.GetModelMonitoringSchema().GetPredictionFields() {
		preds = append(preds, f.GetName()+" ("+f.GetDataType()+")")
	}
	d := console.Detail{Title: dashIfEmpty(m.GetDisplayName()), Summary: []console.Property{
		{Label: "Monitor name", Value: dashIfEmpty(m.GetDisplayName())},
		{Label: "Name", Value: m.GetName(), Copy: m.GetName()},
		{Label: "Model", Value: dashIfEmpty(vm.GetModel())},
		{Label: "Version", Value: dashIfEmpty(vm.GetModelVersionId())},
		{Label: "Input features", Value: dashIfEmpty(strings.Join(feats, ", "))},
		{Label: "Prediction outputs", Value: dashIfEmpty(strings.Join(preds, ", "))},
		{Label: "Training dataset", Value: inputText(m.GetTrainingDataset())},
		{Label: "Objectives", Value: objectivesText(m.GetTabularObjective())},
		{Label: "Results bucket", Value: dashIfEmpty(m.GetOutputSpec().GetGcsBaseDirectory().GetOutputUriPrefix())},
		{Label: "Alert emails", Value: dashIfEmpty(strings.Join(m.GetNotificationSpec().GetEmailConfig().GetUserEmails(), ", "))},
		{Label: "Created", Value: vertexTime(m.GetCreateTime())},
	}}
	runs := console.Listing{Columns: []string{"Run name", "Status", "Target dataset", "Schedule", "Created", "Error"}, NameColumn: "ID", Noun: "runs"}
	for it := c.ListModelMonitoringJobs(ctx, &betapb.ListModelMonitoringJobsRequest{Parent: name}); ; {
		j, err := it.Next()
		if err != nil {
			break
		}
		msg := j.GetJobExecutionDetail().GetError().GetMessage()
		for _, k := range sortedKeysOf(j.GetJobExecutionDetail().GetObjectiveStatus()) {
			if s := j.GetJobExecutionDetail().GetObjectiveStatus()[k]; s.GetCode() != 0 {
				msg = strings.TrimPrefix(msg+"; ", "; ") + k + ": " + s.GetMessage()
			}
		}
		runs.Items = append(runs.Items, console.Resource{Name: vertexLast(j.GetName()), Fields: map[string]string{
			"Run name": dashIfEmpty(j.GetDisplayName()), "Status": monitorStateText(j.GetState()),
			"Target dataset": inputText(j.GetModelMonitoringSpec().GetObjectiveSpec().GetTargetDataset()),
			"Schedule":       dashIfEmpty(vertexLast(j.GetSchedule())), "Created": vertexTime(j.GetCreateTime()), "Error": dashIfEmpty(msg)}})
	}
	runs.Total = len(runs.Items)
	stats := console.Listing{Columns: []string{"Feature", "Objective", "Algorithm", "Distance", "Threshold", "Anomaly", "Run"}, NameColumn: "Feature", Noun: "feature statistics"}
	for it := c.SearchModelMonitoringStats(ctx, &betapb.SearchModelMonitoringStatsRequest{ModelMonitor: name}); ; {
		s, err := it.Next()
		if err != nil {
			break
		}
		ts := s.GetTabularStats()
		if len(ts.GetDataPoints()) == 0 {
			continue
		}
		pt := ts.GetDataPoints()[0]
		stats.Items = append(stats.Items, console.Resource{Name: ts.GetStatsName() + " " + ts.GetObjectiveType(), Fields: map[string]string{
			"Feature": ts.GetStatsName(), "Objective": ts.GetObjectiveType(), "Algorithm": pt.GetAlgorithm(),
			"Distance":  strconv.FormatFloat(pt.GetCurrentStats().GetDistributionValue().GetDistributionDeviation(), 'g', 6, 64),
			"Threshold": strconv.FormatFloat(pt.GetThresholdValue(), 'g', 6, 64), "Anomaly": map[bool]string{true: "Yes", false: "No"}[pt.GetHasAnomaly()],
			"Run": vertexLast(pt.GetModelMonitoringJob())}})
	}
	stats.Total = len(stats.Items)
	alerts := console.Listing{Columns: []string{"Alert time", "Feature", "Objective", "Summary"}, NameColumn: "Alert time", Noun: "alerts"}
	for it := c.SearchModelMonitoringAlerts(ctx, &betapb.SearchModelMonitoringAlertsRequest{ModelMonitor: name}); ; {
		a, err := it.Next()
		if err != nil {
			break
		}
		alerts.Items = append(alerts.Items, console.Resource{Name: vertexTime(a.GetAlertTime()) + " " + a.GetStatsName(), Fields: map[string]string{
			"Alert time": vertexTime(a.GetAlertTime()), "Feature": a.GetStatsName(), "Objective": a.GetObjectiveType(),
			"Summary": a.GetAnomaly().GetTabularAnomaly().GetSummary()}})
	}
	alerts.Total = len(alerts.Items)
	d.Sections = []console.Section{{ID: "runs", Label: "Runs", Listing: runs}, {ID: "stats", Label: "Feature statistics", Listing: stats}, {ID: "alerts", Label: "Alerts", Listing: alerts}}
	return d, nil
}

func targetFields(section string) []console.Field {
	return []console.Field{
		{Name: "target", Label: "Target dataset", Type: "text", Required: true, Section: section,
			Help: "gs://BUCKET/path/data.csv (wildcards allowed), bq://PROJECT.DATASET.TABLE, or an endpoint, projects/P/locations/L/endpoints/E, whose logged predictions are read."},
		{Name: "targetFormat", Label: "Data format", Type: "select", Section: section, Options: []string{"csv", "jsonl"}, Default: "csv",
			OptionLabels: map[string]string{"csv": "CSV", "jsonl": "JSONL"}},
		{Name: "baseline", Label: "Baseline dataset", Type: "text", Section: section, Help: "Empty: the monitor's training dataset."},
		{Name: "baselineFormat", Label: "Baseline data format", Type: "select", Section: section, Options: []string{"csv", "jsonl"}, Default: "csv",
			OptionLabels: map[string]string{"csv": "CSV", "jsonl": "JSONL"}},
	}
}

func (modelMonitorsProvider) CreateForm() (string, []console.Field) {
	return "Configure monitoring", []console.Field{
		{Name: "displayName", Label: "Monitor name", Type: "text", Section: "Model", Pattern: `^.{0,128}$`},
		{Name: "model", Label: "Model", Type: "text", Required: true, Section: "Model", Help: "The Model Registry model's ID."},
		{Name: "version", Label: "Version", Type: "text", Section: "Model", Default: "1"},
		{Name: "features", Label: "Input features", Type: "textarea", Section: "Schema", Help: "One NAME:TYPE per line; TYPE is float, integer, boolean, string or categorical."},
		{Name: "predictions", Label: "Prediction outputs", Type: "textarea", Section: "Schema", Help: "One NAME:TYPE per line."},
		{Name: "training", Label: "Training dataset", Type: "text", Section: "Schema", Help: "Optional: gs://BUCKET/path/train.csv, to monitor drift against it."},
		{Name: "trainingFormat", Label: "Data format", Type: "select", Section: "Schema", Options: []string{"csv", "jsonl"}, Default: "csv",
			OptionLabels: map[string]string{"csv": "CSV", "jsonl": "JSONL"}},
		{Name: "output", Label: "Cloud Storage bucket", Type: "text", Section: "Advanced options", Pattern: `^(gs://[a-z0-9][\-a-z0-9._]+(/.*)?)?$`,
			Help: "gs://BUCKET/folder for the results."},
		{Name: "featureDrift", Label: "Input feature drift", Type: "checkbox", Section: "Objectives", Default: "true"},
		{Name: "predictionDrift", Label: "Prediction output drift", Type: "checkbox", Section: "Objectives"},
		{Name: "categoricalMetric", Label: "Categorical metric", Type: "select", Section: "Objectives", Options: []string{"l_infinity", "jensen_shannon_divergence"}, Default: "l_infinity",
			OptionLabels: map[string]string{"l_infinity": "L-infinity", "jensen_shannon_divergence": "Jensen-Shannon divergence"}},
		{Name: "threshold", Label: "Alert threshold", Type: "text", Section: "Objectives", Default: "0.3", Pattern: `^([0-9]*\.?[0-9]+)?$`},
		{Name: "emails", Label: "Email addresses", Type: "text", Section: "Notifications", Help: "Comma-separated."},
		{Name: "channels", Label: "Notification channels", Type: "text", Section: "Notifications", Help: "Comma-separated projects/P/notificationChannels/C."},
	}
}

func (p modelMonitorsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before configuring monitoring")
	}
	feats, err := parseSchemaFields(values["features"])
	if err != nil {
		return "", err
	}
	preds, err := parseSchemaFields(values["predictions"])
	if err != nil {
		return "", err
	}
	parent := colabParent(ctx, project)
	model := strings.TrimSpace(values["model"])
	if !strings.HasPrefix(model, "projects/") {
		model = parent + "/models/" + model
	}
	th := 0.3
	if t := strings.TrimSpace(values["threshold"]); t != "" {
		if th, err = strconv.ParseFloat(t, 64); err != nil {
			return "", fmt.Errorf("Alert threshold %q is not a number", t)
		}
	}
	cond := &betapb.ModelMonitoringAlertCondition{Condition: &betapb.ModelMonitoringAlertCondition_Threshold{Threshold: th}}
	drift := func() *betapb.ModelMonitoringObjectiveSpec_DataDriftSpec {
		return &betapb.ModelMonitoringObjectiveSpec_DataDriftSpec{CategoricalMetricType: values["categoricalMetric"], NumericMetricType: "jensen_shannon_divergence",
			DefaultCategoricalAlertCondition: cond, DefaultNumericAlertCondition: cond}
	}
	obj := &betapb.ModelMonitoringObjectiveSpec_TabularObjective{}
	if values["featureDrift"] == "true" {
		obj.FeatureDriftSpec = drift()
	}
	if values["predictionDrift"] == "true" {
		obj.PredictionOutputDriftSpec = drift()
	}
	m := &betapb.ModelMonitor{DisplayName: values["displayName"],
		ModelMonitoringTarget: &betapb.ModelMonitor_ModelMonitoringTarget{Source: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModel{
			VertexModel: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModelSource{Model: model, ModelVersionId: strings.TrimSpace(values["version"])}}},
		ModelMonitoringSchema: &betapb.ModelMonitoringSchema{FeatureFields: feats, PredictionFields: preds},
		TrainingDataset:       monitorInput(values["training"], values["trainingFormat"]),
		DefaultObjective:      &betapb.ModelMonitor_TabularObjective{TabularObjective: obj},
	}
	if o := strings.TrimSpace(values["output"]); o != "" {
		m.OutputSpec = &betapb.ModelMonitoringOutputSpec{GcsBaseDirectory: &betapb.GcsDestination{OutputUriPrefix: o}}
	}
	ns := &betapb.ModelMonitoringNotificationSpec{}
	for _, e := range strings.Split(values["emails"], ",") {
		if e = strings.TrimSpace(e); e != "" {
			if ns.EmailConfig == nil {
				ns.EmailConfig = &betapb.ModelMonitoringNotificationSpec_EmailConfig{}
			}
			ns.EmailConfig.UserEmails = append(ns.EmailConfig.UserEmails, e)
		}
	}
	for _, ch := range strings.Split(values["channels"], ",") {
		if ch = strings.TrimSpace(ch); ch != "" {
			ns.NotificationChannelConfigs = append(ns.NotificationChannelConfigs, &betapb.ModelMonitoringNotificationSpec_NotificationChannelConfig{NotificationChannel: ch})
		}
	}
	if ns.EmailConfig != nil || len(ns.NotificationChannelConfigs) > 0 {
		m.NotificationSpec = ns
	}
	c, err := p.api.monitoring(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: m})
	if err != nil {
		return "", err
	}
	got, err := op.Wait(ctx)
	if err != nil {
		return "", err
	}
	return vertexLast(got.GetName()), nil
}

func (p modelMonitorsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.monitoring(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteModelMonitor(ctx, &betapb.DeleteModelMonitorRequest{Name: colabParent(ctx, project) + "/modelMonitors/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (modelMonitorsProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	sched := append([]console.Field{
		{Name: "displayName", Label: "Schedule name", Type: "text", Required: true, Pattern: `^.{1,128}$`},
		{Name: "cron", Label: "Frequency", Type: "text", Required: true, Default: "0 * * * *", Pattern: `^((CRON_)?TZ=\S+ )?\S+ \S+ \S+ \S+ \S+$`,
			Help: "A cron expression; prefix TZ=AREA/CITY for a time zone."},
		{Name: "endAt", Label: "End date", Type: "text", Pattern: `^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?Z)?$`, Help: "Optional, RFC 3339 in UTC."},
		{Name: "window", Label: "Target window", Type: "text", Pattern: `^([0-9]+[wWdDhH])?$`, Help: "Optional: the latest hours (1h), days (1d) or weeks (1w) of a timestamped dataset."},
	}, targetFields("")...)
	return []console.Action{
		{ID: "run", Label: "Run now", Title: "Run a monitoring job", Submit: "Run", Fields: append([]console.Field{
			{Name: "displayName", Label: "Run name", Type: "text", Pattern: `^.{0,128}$`}}, targetFields("")...)},
		{ID: "schedule", Label: "Schedule", Title: "Schedule monitoring runs", Submit: "Create", Fields: sched},
	}
}

func (p modelMonitorsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("choose a model monitor")
	}
	mon := colabParent(ctx, project) + "/modelMonitors/" + path[0]
	target := monitorInput(values["target"], values["targetFormat"])
	if target == nil {
		return errors.New("Target dataset is required")
	}
	if w := strings.TrimSpace(values["window"]); w != "" {
		target.TimeSpec = &betapb.ModelMonitoringInput_TimeOffset_{TimeOffset: &betapb.ModelMonitoringInput_TimeOffset{Window: w}}
	}
	job := &betapb.ModelMonitoringJob{ModelMonitoringSpec: &betapb.ModelMonitoringSpec{ObjectiveSpec: &betapb.ModelMonitoringObjectiveSpec{
		TargetDataset: target, BaselineDataset: monitorInput(values["baseline"], values["baselineFormat"])}}}
	switch action {
	case "run":
		job.DisplayName = values["displayName"]
		c, err := p.api.monitoring(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.CreateModelMonitoringJob(ctx, &betapb.CreateModelMonitoringJobRequest{Parent: mon, ModelMonitoringJob: job})
		return err
	case "schedule":
		sc := &betapb.Schedule{DisplayName: values["displayName"], TimeSpecification: &betapb.Schedule_Cron{Cron: values["cron"]}, MaxConcurrentRunCount: 1,
			Request: &betapb.Schedule_CreateModelMonitoringJobRequest{CreateModelMonitoringJobRequest: &betapb.CreateModelMonitoringJobRequest{Parent: mon, ModelMonitoringJob: job}}}
		if e := strings.TrimSpace(values["endAt"]); e != "" {
			t, err := time.Parse(time.RFC3339, e)
			if err != nil {
				return fmt.Errorf("%q is not an RFC 3339 time", e)
			}
			sc.EndTime = timestamppb.New(t)
		}
		c, err := p.api.betaSchedules(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.CreateSchedule(ctx, &betapb.CreateScheduleRequest{Parent: colabParent(ctx, project), Schedule: sc})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Schedules

type monitoringSchedulesProvider struct{ colabSchedulesProvider }

func (monitoringSchedulesProvider) ID() string    { return "vertex-model-monitoring-schedules" }
func (monitoringSchedulesProvider) Title() string { return "Monitoring" }

func (p monitoringSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Schedule name", "Model monitor", "Frequency", "Next run", "Runs started", "Created"},
		NameColumn: "ID", Noun: "schedules", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each run of a schedule is a monitoring job, listed on its model monitor's page. Create one with a model monitor's Schedule."}
	if project == "" {
		l.Prompt = "Schedules are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.betaSchedules(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListSchedules(ctx, &betapb.ListSchedulesRequest{Parent: colabParent(ctx, project), Filter: "create_model_monitoring_job_request:*"}); ; {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		state := aiplatformpb.Schedule_State(s.GetState())
		l.Items = append(l.Items, console.Resource{Name: vertexLast(s.GetName()), Status: colabScheduleState(state), Actions: colabScheduleActions(state),
			Fields: map[string]string{"Schedule name": s.GetDisplayName(), "Model monitor": vertexLast(s.GetCreateModelMonitoringJobRequest().GetParent()),
				"Frequency": s.GetCron(), "Next run": colabOptTime(s.GetNextRunTime(), s.GetNextRunTime() != nil),
				"Runs started": fmt.Sprint(s.GetStartedRunCount()), "Created": vertexTime(s.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p monitoringSchedulesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.betaSchedules(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	s, err := c.GetSchedule(ctx, &betapb.GetScheduleRequest{Name: colabParent(ctx, project) + "/schedules/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	r := s.GetCreateModelMonitoringJobRequest()
	last := "-"
	if lr := s.GetLastScheduledRunResponse(); lr != nil {
		last = vertexTime(lr.GetScheduledRunTime()) + ": " + lr.GetRunResponse()
	}
	return console.Detail{Title: "Schedule details", Summary: []console.Property{
		{Label: "Schedule name", Value: s.GetDisplayName()},
		{Label: "Name", Value: s.GetName(), Copy: s.GetName()},
		{Label: "Status", Value: colabScheduleState(aiplatformpb.Schedule_State(s.GetState()))},
		{Label: "Model monitor", Value: dashIfEmpty(r.GetParent())},
		{Label: "Target dataset", Value: inputText(r.GetModelMonitoringJob().GetModelMonitoringSpec().GetObjectiveSpec().GetTargetDataset())},
		{Label: "Frequency", Value: s.GetCron()},
		{Label: "End time", Value: colabOptTime(s.GetEndTime(), s.GetEndTime() != nil)},
		{Label: "Next run", Value: colabOptTime(s.GetNextRunTime(), s.GetNextRunTime() != nil)},
		{Label: "Runs started", Value: fmt.Sprint(s.GetStartedRunCount())},
		{Label: "Last scheduled run", Value: last},
		{Label: "Created", Value: vertexTime(s.GetCreateTime())},
	}}, nil
}

// ---- v1 deployment monitoring jobs

type mdmJobsProvider struct{ api vertexAPI }

func (mdmJobsProvider) ID() string           { return "vertex-model-deployment-monitoring" }
func (mdmJobsProvider) Title() string        { return "Monitoring" }
func (mdmJobsProvider) ScopeKey() string     { return colabRegionKey }
func (mdmJobsProvider) CreateTitle() string  { return "Configure model monitoring" }
func (mdmJobsProvider) CreateSubmit() string { return "Create" }

func mdmActions(s aiplatformpb.JobState) []console.Action {
	switch s {
	case aiplatformpb.JobState_JOB_STATE_RUNNING, aiplatformpb.JobState_JOB_STATE_PENDING:
		return []console.Action{{ID: "pause", Label: "Pause"}}
	case aiplatformpb.JobState_JOB_STATE_PAUSED:
		return []console.Action{{ID: "resume", Label: "Resume"}}
	}
	return nil
}

func (p mdmJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Job name", "Endpoint", "Monitoring frequency", "Last run", "Next run", "Created"},
		NameColumn: "ID", Noun: "monitoring jobs", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "A job logs its endpoint's predictions and, each monitoring frequency, compares them with the training data (skew) and the window before (drift). Pause a job before you delete it."}
	if project == "" {
		l.Prompt = "Monitoring jobs are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListModelDeploymentMonitoringJobs(ctx, &aiplatformpb.ListModelDeploymentMonitoringJobsRequest{Parent: colabParent(ctx, project)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		last := "-"
		if m := j.GetLatestMonitoringPipelineMetadata(); m != nil {
			last = vertexTime(m.GetRunTime())
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: monitorStateText(j.GetState()), Actions: mdmActions(j.GetState()), Fields: map[string]string{
			"Job name": j.GetDisplayName(), "Endpoint": vertexLast(j.GetEndpoint()),
			"Monitoring frequency": j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration().String(),
			"Last run":             last, "Next run": colabOptTime(j.GetNextScheduleTime(), j.GetNextScheduleTime() != nil), "Created": vertexTime(j.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p mdmJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	j, err := c.GetModelDeploymentMonitoringJob(ctx, &aiplatformpb.GetModelDeploymentMonitoringJobRequest{Name: colabParent(ctx, project) + "/modelDeploymentMonitoringJobs/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	var tables []string
	for _, t := range j.GetBigqueryTables() {
		tables = append(tables, t.GetBigqueryTablePath())
	}
	status := "-"
	if m := j.GetLatestMonitoringPipelineMetadata(); m != nil {
		status = "OK"
		if m.GetStatus().GetCode() != 0 {
			status = m.GetStatus().GetMessage()
		}
	}
	d := console.Detail{Title: j.GetDisplayName(), Summary: []console.Property{
		{Label: "Job name", Value: j.GetDisplayName()},
		{Label: "Name", Value: j.GetName(), Copy: j.GetName()},
		{Label: "Status", Value: monitorStateText(j.GetState())},
		{Label: "Endpoint", Value: j.GetEndpoint()},
		{Label: "Monitoring frequency", Value: j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration().String()},
		{Label: "Sampling rate", Value: dashIfEmpty(strconv.FormatFloat(j.GetLoggingSamplingStrategy().GetRandomSampleConfig().GetSampleRate(), 'g', -1, 64))},
		{Label: "Logging tables", Value: dashIfEmpty(strings.Join(tables, ", "))},
		{Label: "Alert emails", Value: dashIfEmpty(strings.Join(j.GetModelMonitoringAlertConfig().GetEmailAlertConfig().GetUserEmails(), ", "))},
		{Label: "Last run status", Value: status},
		{Label: "Next run", Value: colabOptTime(j.GetNextScheduleTime(), j.GetNextScheduleTime() != nil)},
		{Label: "Created", Value: vertexTime(j.GetCreateTime())},
	}}
	anom := console.Listing{Columns: []string{"Deployed model", "Objective", "Feature", "Score", "Threshold", "Anomaly", "Window end"}, NameColumn: "Feature", Noun: "feature statistics"}
	for _, oc := range j.GetModelDeploymentMonitoringObjectiveConfigs() {
		req := &aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest{ModelDeploymentMonitoringJob: j.GetName(), DeployedModelId: oc.GetDeployedModelId(),
			Objectives: []*aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest_StatsAnomaliesObjective{
				{Type: aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW}, {Type: aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_DRIFT}}}
		for it := c.SearchModelDeploymentMonitoringStatsAnomalies(ctx, req); ; {
			s, err := it.Next()
			if err != nil {
				break
			}
			for _, f := range s.GetFeatureStats() {
				if len(f.GetPredictionStats()) == 0 {
					continue
				}
				p := f.GetPredictionStats()[0]
				anom.Items = append(anom.Items, console.Resource{Name: oc.GetDeployedModelId() + " " + s.GetObjective().String() + " " + f.GetFeatureDisplayName(), Fields: map[string]string{
					"Deployed model": oc.GetDeployedModelId(), "Objective": map[bool]string{true: "Skew", false: "Drift"}[s.GetObjective() == aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW],
					"Feature": f.GetFeatureDisplayName(), "Score": strconv.FormatFloat(p.GetScore(), 'g', 6, 64),
					"Threshold": strconv.FormatFloat(f.GetThreshold().GetValue(), 'g', 6, 64), "Anomaly": map[bool]string{true: "Yes", false: "No"}[p.GetScore() > f.GetThreshold().GetValue()],
					"Window end": vertexTime(p.GetEndTime())}})
			}
		}
	}
	sort.SliceStable(anom.Items, func(a, b int) bool { return anom.Items[a].Name < anom.Items[b].Name })
	anom.Total = len(anom.Items)
	d.Sections = []console.Section{{ID: "stats", Label: "Feature statistics", Listing: anom}}
	return d, nil
}

func (mdmJobsProvider) CreateForm() (string, []console.Field) {
	return "Configure model monitoring", []console.Field{
		{Name: "displayName", Label: "Monitoring job name", Type: "text", Required: true, Section: "Basics", Pattern: `^.{1,128}$`},
		{Name: "endpoint", Label: "Endpoint", Type: "text", Required: true, Section: "Basics", Help: "The endpoint's ID; every model deployed on it is monitored."},
		{Name: "interval", Label: "Monitoring frequency (hours)", Type: "text", Section: "Basics", Default: "24", Pattern: `^[1-9][0-9]*$`},
		{Name: "sampleRate", Label: "Sampling rate", Type: "text", Section: "Basics", Default: "1", Pattern: `^(0?\.[0-9]+|1(\.0*)?)$`},
		{Name: "emails", Label: "Alert emails", Type: "text", Section: "Basics", Help: "Comma-separated."},
		{Name: "objective", Label: "Monitoring objective", Type: "select", Section: "Monitoring objectives", Options: []string{"skew", "drift"}, Default: "skew",
			OptionLabels: map[string]string{"skew": "Training-serving skew detection", "drift": "Prediction drift detection"}},
		{Name: "training", Label: "Training data source", Type: "text", Section: "Monitoring objectives", ShowWhen: "objective=skew", Help: "gs://BUCKET/path/train.csv"},
		{Name: "trainingFormat", Label: "Data format", Type: "select", Section: "Monitoring objectives", ShowWhen: "objective=skew", Options: []string{"csv", "jsonl"}, Default: "csv",
			OptionLabels: map[string]string{"csv": "CSV", "jsonl": "JSONL"}},
		{Name: "targetField", Label: "Target column", Type: "text", Section: "Monitoring objectives", ShowWhen: "objective=skew"},
		{Name: "threshold", Label: "Alert threshold", Type: "text", Section: "Monitoring objectives", Default: "0.3", Pattern: `^([0-9]*\.?[0-9]+)?$`},
	}
}

func (p mdmJobsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before configuring monitoring")
	}
	parent := colabParent(ctx, project)
	ep := strings.TrimSpace(values["endpoint"])
	if !strings.HasPrefix(ep, "projects/") {
		ep = parent + "/endpoints/" + ep
	}
	hours, err := strconv.Atoi(strings.TrimSpace(values["interval"]))
	if err != nil {
		return "", errors.New("Monitoring frequency must be a whole number of hours")
	}
	rate, _ := strconv.ParseFloat(strings.TrimSpace(values["sampleRate"]), 64)
	th, _ := strconv.ParseFloat(strings.TrimSpace(values["threshold"]), 64)
	if th == 0 {
		th = 0.3
	}
	thr := &aiplatformpb.ThresholdConfig{Threshold: &aiplatformpb.ThresholdConfig_Value{Value: th}}
	ecs, err := p.api.endpoints(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = ecs.Close() }()
	e, err := ecs.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: ep})
	if err != nil {
		return "", err
	}
	j := &aiplatformpb.ModelDeploymentMonitoringJob{DisplayName: values["displayName"], Endpoint: ep,
		ModelDeploymentMonitoringScheduleConfig: &aiplatformpb.ModelDeploymentMonitoringScheduleConfig{MonitorInterval: durationpb.New(time.Duration(hours) * time.Hour)}}
	if rate > 0 {
		j.LoggingSamplingStrategy = &aiplatformpb.SamplingStrategy{RandomSampleConfig: &aiplatformpb.SamplingStrategy_RandomSampleConfig{SampleRate: rate}}
	}
	for _, dm := range e.GetDeployedModels() {
		oc := &aiplatformpb.ModelMonitoringObjectiveConfig{}
		if values["objective"] == "drift" {
			oc.PredictionDriftDetectionConfig = &aiplatformpb.ModelMonitoringObjectiveConfig_PredictionDriftDetectionConfig{DefaultDriftThreshold: thr}
		} else {
			oc.TrainingDataset = &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset{DataFormat: values["trainingFormat"], TargetField: values["targetField"],
				DataSource: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{strings.TrimSpace(values["training"])}}}}
			oc.TrainingPredictionSkewDetectionConfig = &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingPredictionSkewDetectionConfig{DefaultSkewThreshold: thr}
		}
		j.ModelDeploymentMonitoringObjectiveConfigs = append(j.ModelDeploymentMonitoringObjectiveConfigs,
			&aiplatformpb.ModelDeploymentMonitoringObjectiveConfig{DeployedModelId: dm.GetId(), ObjectiveConfig: oc})
	}
	var emails []string
	for _, m := range strings.Split(values["emails"], ",") {
		if m = strings.TrimSpace(m); m != "" {
			emails = append(emails, m)
		}
	}
	if len(emails) > 0 {
		j.ModelMonitoringAlertConfig = &aiplatformpb.ModelMonitoringAlertConfig{Alert: &aiplatformpb.ModelMonitoringAlertConfig_EmailAlertConfig_{
			EmailAlertConfig: &aiplatformpb.ModelMonitoringAlertConfig_EmailAlertConfig{UserEmails: emails}}}
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	got, err := c.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: j})
	if err != nil {
		return "", err
	}
	return vertexLast(got.GetName()), nil
}

func (p mdmJobsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteModelDeploymentMonitoringJob(ctx, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: colabParent(ctx, project) + "/modelDeploymentMonitoringJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (mdmJobsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p mdmJobsProvider) Act(ctx context.Context, project, name, action string) error {
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	full := colabParent(ctx, project) + "/modelDeploymentMonitoringJobs/" + name
	switch action {
	case "pause":
		return c.PauseModelDeploymentMonitoringJob(ctx, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: full})
	case "resume":
		return c.ResumeModelDeploymentMonitoringJob(ctx, &aiplatformpb.ResumeModelDeploymentMonitoringJobRequest{Name: full})
	}
	return fmt.Errorf("unknown action %q", action)
}
