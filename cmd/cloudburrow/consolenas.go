package main

// Vertex AI Training's Neural Architecture Search tab (#2214), beside the
// Training pipelines, Custom jobs and Hyperparameter tuning jobs tabs: the
// list of NAS jobs, and a job's page with its details, its search and
// train trial worker pools, a chart of the search trials' rewards, and its
// trials, each a NasTrialDetail (its architecture's parameters, the search
// trial and, once trained, its train trial)
// (https://docs.cloud.google.com/vertex-ai/docs/training/neural-architecture-search/overview,
// read 2026-11-24). Google creates NAS jobs from the vertex-nas-cli and the
// API, not a console pane.
//
// UNVERIFIED (no screenshots of the tab; re-read 2026-11-24): the
// list's columns, the job page's fields, and the Trials table's columns.

import (
	"context"
	"fmt"
	"strconv"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

type nasJobsProvider struct{ api vertexAPI }

func (nasJobsProvider) ID() string       { return "vertex-nas-jobs" }
func (nasJobsProvider) Title() string    { return "Training" }
func (nasJobsProvider) ScopeKey() string { return colabRegionKey }

func nasTrialStateText(s aiplatformpb.NasTrial_State) string {
	switch s {
	case aiplatformpb.NasTrial_REQUESTED:
		return "Requested"
	case aiplatformpb.NasTrial_ACTIVE:
		return "Active"
	case aiplatformpb.NasTrial_STOPPING:
		return "Stopping"
	case aiplatformpb.NasTrial_SUCCEEDED:
		return "Succeeded"
	case aiplatformpb.NasTrial_INFEASIBLE:
		return "Infeasible"
	}
	return "Unknown"
}

func nasAlgorithmText(a aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MultiTrialAlgorithm) string {
	if a == aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH {
		return "Grid search"
	}
	return "Reinforcement learning"
}

func (p nasJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Name", "Algorithm", "Search trials", "Train trials", "Created", "Elapsed time", "Labels"},
		NameColumn: "ID", Noun: "NAS jobs", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each trial of a Neural Architecture Search job is a custom job on this machine's Docker, given --nas_params_str; its code reports the reward with cloudml-hypertune. Create one from the API or vertex-nas-cli."}
	if project == "" {
		l.Prompt = "NAS jobs are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListNasJobs(ctx, &aiplatformpb.ListNasJobsRequest{Parent: colabParent(ctx, project)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		if jobActive(j.GetState()) {
			actions = append(actions, cancelAction("NAS job"))
		}
		mt := j.GetNasJobSpec().GetMultiTrialAlgorithmSpec()
		out := j.GetNasJobOutput().GetMultiTrialJobOutput()
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: jobStateText(j.GetState()), Actions: actions, Fields: map[string]string{
			"Name": dashIfEmpty(j.GetDisplayName()), "Algorithm": nasAlgorithmText(mt.GetMultiTrialAlgorithm()),
			"Search trials": fmt.Sprintf("%d of %d", len(out.GetSearchTrials()), mt.GetSearchTrialSpec().GetMaxTrialCount()),
			"Train trials":  strconv.Itoa(len(out.GetTrainTrials())),
			"Created":       optVertexTime(j.GetCreateTime()), "Elapsed time": elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime()),
			"Labels": pipelineLabels(j.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func nasReward(t *aiplatformpb.NasTrial) (float64, bool) {
	if t == nil || t.GetState() != aiplatformpb.NasTrial_SUCCEEDED || len(t.GetFinalMeasurement().GetMetrics()) == 0 {
		return 0, false
	}
	return t.GetFinalMeasurement().GetMetrics()[0].GetValue(), true
}

func (p nasJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	name := colabParent(ctx, project) + "/nasJobs/" + path[0]
	j, err := c.GetNasJob(ctx, &aiplatformpb.GetNasJobRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	spec := j.GetNasJobSpec()
	mt := spec.GetMultiTrialAlgorithmSpec()
	props := []console.Property{
		{Label: "Name", Value: dashIfEmpty(j.GetDisplayName())},
		{Label: "ID", Value: vertexLast(j.GetName()), Copy: j.GetName()},
		{Label: "Status", Value: jobStateText(j.GetState())},
		{Label: "Region", Value: vertexLocation(j.GetName())},
		{Label: "Created", Value: optVertexTime(j.GetCreateTime())},
		{Label: "Start time", Value: optVertexTime(j.GetStartTime())},
		{Label: "End time", Value: optVertexTime(j.GetEndTime())},
		{Label: "Elapsed time", Value: elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime())},
		{Label: "Labels", Value: pipelineLabels(j.GetLabels())},
	}
	if r := spec.GetResumeNasJobId(); r != "" {
		props = append(props, console.Property{Label: "Resumed from", Value: r})
	}
	if e := jobErrorText(j.GetError().GetCode(), j.GetError().GetMessage()); e != "" {
		props = append(props, console.Property{Label: "Error", Value: e})
	}
	goal := map[aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_GoalType]string{
		aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MAXIMIZE: "Maximize", aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MINIMIZE: "Minimize"}[mt.GetMetric().GetGoal()]
	st := mt.GetSearchTrialSpec()
	search := []console.Property{
		{Label: "Algorithm", Value: nasAlgorithmText(mt.GetMultiTrialAlgorithm())},
		{Label: "Reward metric", Value: dashIfEmpty(mt.GetMetric().GetMetricId()) + " (" + goal + ")"},
		{Label: "Maximum number of search trials", Value: strconv.Itoa(int(st.GetMaxTrialCount()))},
		{Label: "Maximum number of parallel search trials", Value: strconv.Itoa(int(st.GetMaxParallelTrialCount()))},
		{Label: "Maximum number of failed search trials", Value: map[bool]string{true: "Decided by Vertex AI", false: strconv.Itoa(int(st.GetMaxFailedTrialCount()))}[st.GetMaxFailedTrialCount() == 0]},
		{Label: "Search space", Value: dashIfEmpty(spec.GetSearchSpaceSpec()), Copy: spec.GetSearchSpaceSpec()},
	}
	groups := []console.PropertyGroup{{Heading: "NAS job", Properties: props}, {Heading: "Search", Properties: search}}
	for _, g := range workerPoolProps(st.GetSearchTrialJobSpec()) {
		g.Heading = "Search trial " + g.Heading
		groups = append(groups, g)
	}
	if tt := mt.GetTrainTrialSpec(); tt != nil {
		groups = append(groups, console.PropertyGroup{Heading: "Train", Properties: []console.Property{
			{Label: "Train every", Value: fmt.Sprintf("%d search trials", tt.GetFrequency())},
			{Label: "Maximum number of parallel train trials", Value: strconv.Itoa(int(tt.GetMaxParallelTrialCount()))}}})
		for _, g := range workerPoolProps(tt.GetTrainTrialJobSpec()) {
			g.Heading = "Train trial " + g.Heading
			groups = append(groups, g)
		}
	}

	metric := dashIfEmpty(mt.GetMetric().GetMetricId())
	trials := console.Listing{Columns: []string{"State", "Search " + metric, "Train state", "Train " + metric, "Parameters", "Elapsed time", "Custom job"},
		NameColumn: "Trial ID", Noun: "trials"}
	var points []console.ChartPoint
	for it := c.ListNasTrialDetails(ctx, &aiplatformpb.ListNasTrialDetailsRequest{Parent: name}); ; {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return console.Detail{Unavailable: err.Error()}, nil
		}
		s, tr := d.GetSearchTrial(), d.GetTrainTrial()
		f := map[string]string{"State": nasTrialStateText(s.GetState()), "Search " + metric: "—", "Train state": "—", "Train " + metric: "—",
			"Parameters": dashIfEmpty(d.GetParameters()), "Elapsed time": elapsed(s.GetStartTime(), s.GetStartTime(), s.GetEndTime()), "Custom job": "—"}
		if v, ok := nasReward(s); ok {
			f["Search "+metric] = strconv.FormatFloat(v, 'g', 6, 64)
			v := v
			points = append(points, console.ChartPoint{At: s.GetEndTime().AsTime().UTC().Format("2006-01-02T15:04:05.000Z"), Value: &v})
		}
		if tr != nil {
			f["Train state"] = nasTrialStateText(tr.GetState())
			if v, ok := nasReward(tr); ok {
				f["Train "+metric] = strconv.FormatFloat(v, 'g', 6, 64)
			}
		}
		trials.Items = append(trials.Items, console.Resource{Name: s.GetId(), Fields: f})
	}
	trials.Total = len(trials.Items)
	sections := []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups}}
	if len(points) > 0 {
		sections = append(sections, console.Section{ID: "rewards", Label: "Rewards", Kind: console.KindChart,
			Series: []console.ChartSeries{{Label: "Search trial " + metric, Points: points}}})
	}
	sections = append(sections, console.Section{ID: "trials", Label: "Trials", Listing: trials})
	d := console.Detail{Title: dashIfEmpty(j.GetDisplayName()), Sections: sections}
	if jobActive(j.GetState()) {
		d.Actions = []console.Action{cancelAction("NAS job")}
	}
	return d, nil
}

func (p nasJobsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteNasJob(ctx, &aiplatformpb.DeleteNasJobRequest{Name: colabParent(ctx, project) + "/nasJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (nasJobsProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p nasJobsProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.CancelNasJob(ctx, &aiplatformpb.CancelNasJobRequest{Name: colabParent(ctx, project) + "/nasJobs/" + name})
}
