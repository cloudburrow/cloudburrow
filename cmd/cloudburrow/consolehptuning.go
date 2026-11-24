package main

// Vertex AI Training's Hyperparameter tuning jobs tab (#2210), beside the
// Training pipelines and Custom jobs tabs: the list, and a job's page with
// its details and its Trials table, and the Train new model pane's
// Hyperparameter tuning step:
//
//   - "Enable hyperparameter tuning", "Add a hyperparameter" (its name,
//     Type: Double, Integer, Categorical or Discrete, its values or range
//     and Scale), "Metric to optimize" with its Goal (Maximize or
//     Minimize), "Maximum number of trials", "Maximum number of parallel
//     trials", "Search algorithm" (Default, Grid search, Random search),
//     and "Enable early stopping", which "has no effect"
//     (https://docs.cloud.google.com/vertex-ai/docs/training/using-hyperparameter-tuning,
//     Console tab, read 2026-11-24); the pane then creates a training
//     pipeline with the hyperparameter tuning task.
//
// UNVERIFIED (the page above shows no screenshots): the list's columns, the
// job page's fields, the Trials table's columns (Trial ID, State, each
// metric, each parameter, Elapsed time); and the hyperparameters' entry, one
// per line ("NAME TYPE ..."), where Google's pane adds them one at a time.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// hpTaskDefinition is the hyperparameter tuning training task's schema
// (internal/service/vertexai/hptuning.go).
const hpTaskDefinition = "gs://google-cloud-aiplatform/schema/trainingjob/definition/hyperparameter_tuning_task_1.0.0.yaml"

type hpTuningProvider struct{ api vertexAPI }

func (hpTuningProvider) ID() string       { return "vertex-hyperparameter-tuning-jobs" }
func (hpTuningProvider) Title() string    { return "Training" }
func (hpTuningProvider) ScopeKey() string { return colabRegionKey }

func trialStateText(s aiplatformpb.Trial_State) string {
	switch s {
	case aiplatformpb.Trial_REQUESTED:
		return "Requested"
	case aiplatformpb.Trial_ACTIVE:
		return "Active"
	case aiplatformpb.Trial_STOPPING:
		return "Stopping"
	case aiplatformpb.Trial_SUCCEEDED:
		return "Succeeded"
	case aiplatformpb.Trial_INFEASIBLE:
		return "Infeasible"
	}
	return "Unknown"
}

func (p hpTuningProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Name", "Trials", "Created", "Elapsed time", "Labels"},
		NameColumn: "ID", Noun: "hyperparameter tuning jobs", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each trial of a hyperparameter tuning job is a custom job on this machine's Docker; its code reports metrics with cloudml-hypertune. Create one from the API, the SDK's HyperparameterTuningJob, HyperparameterTuningJobRunOp, or Train new model with hyperparameter tuning enabled."}
	if project == "" {
		l.Prompt = "Hyperparameter tuning jobs are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListHyperparameterTuningJobs(ctx, &aiplatformpb.ListHyperparameterTuningJobsRequest{Parent: colabParent(ctx, project)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		var actions []console.Action
		if jobActive(j.GetState()) {
			actions = append(actions, cancelAction("hyperparameter tuning job"))
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(j.GetName()), Status: jobStateText(j.GetState()), Actions: actions, Fields: map[string]string{
			"Name": dashIfEmpty(j.GetDisplayName()), "Trials": fmt.Sprintf("%d of %d", len(j.GetTrials()), j.GetMaxTrialCount()),
			"Created": optVertexTime(j.GetCreateTime()), "Elapsed time": elapsed(j.GetCreateTime(), j.GetStartTime(), j.GetEndTime()),
			"Labels": pipelineLabels(j.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func algorithmText(a aiplatformpb.StudySpec_Algorithm) string {
	switch a {
	case aiplatformpb.StudySpec_GRID_SEARCH:
		return "Grid search"
	case aiplatformpb.StudySpec_RANDOM_SEARCH:
		return "Random search"
	}
	return "Default (Bayesian optimization)"
}

func paramSpecText(p *aiplatformpb.StudySpec_ParameterSpec) string {
	scale := map[aiplatformpb.StudySpec_ParameterSpec_ScaleType]string{aiplatformpb.StudySpec_ParameterSpec_UNIT_LINEAR_SCALE: ", linear scale",
		aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE: ", log scale", aiplatformpb.StudySpec_ParameterSpec_UNIT_REVERSE_LOG_SCALE: ", reverse log scale"}[p.GetScaleType()]
	switch {
	case p.GetDoubleValueSpec() != nil:
		return fmt.Sprintf("Double, %v to %v%s", p.GetDoubleValueSpec().GetMinValue(), p.GetDoubleValueSpec().GetMaxValue(), scale)
	case p.GetIntegerValueSpec() != nil:
		return fmt.Sprintf("Integer, %d to %d%s", p.GetIntegerValueSpec().GetMinValue(), p.GetIntegerValueSpec().GetMaxValue(), scale)
	case p.GetCategoricalValueSpec() != nil:
		return "Categorical: " + strings.Join(p.GetCategoricalValueSpec().GetValues(), ", ")
	case p.GetDiscreteValueSpec() != nil:
		var v []string
		for _, x := range p.GetDiscreteValueSpec().GetValues() {
			v = append(v, strconv.FormatFloat(x, 'g', -1, 64))
		}
		return "Discrete: " + strings.Join(v, ", ") + scale
	}
	return "—"
}

// hpParamIDs are a study's parameter ids, conditional ones after their
// parent.
func hpParamIDs(specs []*aiplatformpb.StudySpec_ParameterSpec) []string {
	var out []string
	for _, p := range specs {
		out = append(out, p.GetParameterId())
		for _, c := range p.GetConditionalParameterSpecs() {
			for _, id := range hpParamIDs([]*aiplatformpb.StudySpec_ParameterSpec{c.GetParameterSpec()}) {
				dup := false
				for _, o := range out {
					dup = dup || o == id
				}
				if !dup {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

func (p hpTuningProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	j, err := c.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: colabParent(ctx, project) + "/hyperparameterTuningJobs/" + path[0]})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	ss := j.GetStudySpec()
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
	if e := jobErrorText(j.GetError().GetCode(), j.GetError().GetMessage()); e != "" {
		props = append(props, console.Property{Label: "Error", Value: e})
	}
	var metrics []string
	for _, m := range ss.GetMetrics() {
		metrics = append(metrics, m.GetMetricId()+" ("+map[aiplatformpb.StudySpec_MetricSpec_GoalType]string{
			aiplatformpb.StudySpec_MetricSpec_MAXIMIZE: "Maximize", aiplatformpb.StudySpec_MetricSpec_MINIMIZE: "Minimize"}[m.GetGoal()]+")")
	}
	study := []console.Property{
		{Label: "Metric to optimize", Value: dashIfEmpty(strings.Join(metrics, ", "))},
		{Label: "Search algorithm", Value: algorithmText(ss.GetAlgorithm())},
		{Label: "Maximum number of trials", Value: strconv.Itoa(int(j.GetMaxTrialCount()))},
		{Label: "Maximum number of parallel trials", Value: strconv.Itoa(int(j.GetParallelTrialCount()))},
		{Label: "Maximum number of failed trials", Value: map[bool]string{true: "Decided by Vertex AI", false: strconv.Itoa(int(j.GetMaxFailedTrialCount()))}[j.GetMaxFailedTrialCount() == 0]},
	}
	var flat []*aiplatformpb.StudySpec_ParameterSpec
	var walk func([]*aiplatformpb.StudySpec_ParameterSpec)
	walk = func(ps []*aiplatformpb.StudySpec_ParameterSpec) {
		for _, p := range ps {
			flat = append(flat, p)
			for _, c := range p.GetConditionalParameterSpecs() {
				walk([]*aiplatformpb.StudySpec_ParameterSpec{c.GetParameterSpec()})
			}
		}
	}
	walk(ss.GetParameters())
	for _, ps := range flat {
		study = append(study, console.Property{Label: "Hyperparameter " + ps.GetParameterId(), Value: paramSpecText(ps)})
	}
	groups := append([]console.PropertyGroup{{Heading: "Hyperparameter tuning job", Properties: props}, {Heading: "Study", Properties: study}},
		workerPoolProps(j.GetTrialJobSpec())...)

	ids := hpParamIDs(ss.GetParameters())
	cols := []string{"State"}
	for _, m := range ss.GetMetrics() {
		cols = append(cols, m.GetMetricId())
	}
	cols = append(cols, ids...)
	cols = append(cols, "Elapsed time", "Custom job")
	trials := console.Listing{Columns: cols, NameColumn: "Trial ID", Noun: "trials"}
	var best *aiplatformpb.Trial
	top := math.Inf(-1)
	for _, t := range j.GetTrials() {
		f := map[string]string{"State": trialStateText(t.GetState()), "Elapsed time": elapsed(t.GetStartTime(), t.GetStartTime(), t.GetEndTime()),
			"Custom job": dashIfEmpty(vertexLast(t.GetCustomJob()))}
		for _, m := range ss.GetMetrics() {
			f[m.GetMetricId()] = "—"
			for _, x := range t.GetFinalMeasurement().GetMetrics() {
				if x.GetMetricId() == m.GetMetricId() {
					f[m.GetMetricId()] = strconv.FormatFloat(x.GetValue(), 'g', 6, 64)
				}
			}
		}
		for _, id := range ids {
			f[id] = "—"
		}
		for _, pv := range t.GetParameters() {
			switch v := pv.GetValue().AsInterface().(type) {
			case string:
				f[pv.GetParameterId()] = v
			case float64:
				f[pv.GetParameterId()] = strconv.FormatFloat(v, 'g', -1, 64)
			}
		}
		if t.GetInfeasibleReason() != "" {
			f["State"] += ": " + t.GetInfeasibleReason()
		}
		row := console.Resource{Name: t.GetId(), Fields: f}
		if cj := t.GetCustomJob(); cj != "" {
			row.Link = "/agent-platform/training/custom-jobs/" + vertexLast(cj) + "?project=" + project + "&region=" + vertexLocation(cj)
		}
		trials.Items = append(trials.Items, row)
		if t.GetState() == aiplatformpb.Trial_SUCCEEDED && len(ss.GetMetrics()) > 0 {
			for _, x := range t.GetFinalMeasurement().GetMetrics() {
				if x.GetMetricId() == ss.GetMetrics()[0].GetMetricId() {
					y := x.GetValue()
					if ss.GetMetrics()[0].GetGoal() == aiplatformpb.StudySpec_MetricSpec_MINIMIZE {
						y = -y
					}
					if y > top {
						top, best = y, t
					}
				}
			}
		}
	}
	trials.Total = len(trials.Items)
	if best != nil {
		groups[1].Properties = append(groups[1].Properties, console.Property{Label: "Best trial", Value: best.GetId()})
	}
	d := console.Detail{Title: dashIfEmpty(j.GetDisplayName()), Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups},
		{ID: "trials", Label: "Trials", Listing: trials},
	}}
	if jobActive(j.GetState()) {
		d.Actions = []console.Action{cancelAction("hyperparameter tuning job")}
	}
	return d, nil
}

func (p hpTuningProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteHyperparameterTuningJob(ctx, &aiplatformpb.DeleteHyperparameterTuningJobRequest{Name: colabParent(ctx, project) + "/hyperparameterTuningJobs/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (hpTuningProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p hpTuningProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.jobs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.CancelHyperparameterTuningJob(ctx, &aiplatformpb.CancelHyperparameterTuningJobRequest{Name: colabParent(ctx, project) + "/hyperparameterTuningJobs/" + name})
}

// ---- the Train new model pane's Hyperparameter tuning step

// hpFields are the step's fields.
func hpFields() []console.Field {
	const sec = "Hyperparameter tuning"
	return []console.Field{
		{Name: "hpEnable", Label: "Enable hyperparameter tuning", Type: "checkbox", Section: sec},
		{Name: "hpParams", Label: "Hyperparameters", Type: "textarea", Section: sec, ShowWhen: "hpEnable",
			Help: "One per line, its name (the argument your code reads as --NAME), then its type: NAME DOUBLE MIN MAX [linear|log|reverse-log], " +
				"NAME INTEGER MIN MAX [linear|log|reverse-log], NAME CATEGORICAL A,B,C or NAME DISCRETE 1,2,4 [linear|log|reverse-log]."},
		{Name: "hpMetric", Label: "Metric to optimize", Type: "text", Section: sec, ShowWhen: "hpEnable", Pattern: `^\S*$`,
			Help: "The tag your code reports with cloudml-hypertune's report_hyperparameter_tuning_metric."},
		{Name: "hpGoal", Label: "Goal", Type: "radio", Section: sec, ShowWhen: "hpEnable", Options: []string{"MAXIMIZE", "MINIMIZE"}, Default: "MAXIMIZE",
			OptionLabels: map[string]string{"MAXIMIZE": "Maximize", "MINIMIZE": "Minimize"}},
		{Name: "hpMaxTrials", Label: "Maximum number of trials", Type: "text", Section: sec, ShowWhen: "hpEnable", Default: "10", Pattern: `^[1-9][0-9]*$`},
		{Name: "hpParallel", Label: "Maximum number of parallel trials", Type: "text", Section: sec, ShowWhen: "hpEnable", Default: "2", Pattern: `^[1-9][0-9]*$`},
		{Name: "hpAlgorithm", Label: "Search algorithm", Type: "select", Section: sec, ShowWhen: "hpEnable",
			Options: []string{"ALGORITHM_UNSPECIFIED", "GRID_SEARCH", "RANDOM_SEARCH"}, Default: "ALGORITHM_UNSPECIFIED",
			OptionLabels: map[string]string{"ALGORITHM_UNSPECIFIED": "Default", "GRID_SEARCH": "Grid search", "RANDOM_SEARCH": "Random search"}},
		{Name: "hpEarlyStopping", Label: "Enable early stopping", Type: "checkbox", Section: sec, ShowWhen: "hpEnable",
			Help: "As on Google's pane, this has no effect."},
	}
}

var hpScales = map[string]string{"": "", "linear": "UNIT_LINEAR_SCALE", "log": "UNIT_LOG_SCALE", "reverse-log": "UNIT_REVERSE_LOG_SCALE"}

// hpParamsOf reads the step's hyperparameter lines as ParameterSpecs (in
// the REST JSON form).
func hpParamsOf(text string) ([]any, error) {
	var out []any
	for n, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		bad := func(why string) error {
			return fmt.Errorf("hyperparameter line %d (%q): %s", n+1, strings.TrimSpace(line), why)
		}
		if len(f) < 3 {
			return nil, bad("give its name, type and values")
		}
		p := map[string]any{"parameterId": f[0]}
		scaleAt := -1
		switch strings.ToUpper(f[1]) {
		case "DOUBLE", "INTEGER":
			if len(f) < 4 {
				return nil, bad("give its minimum and maximum")
			}
			lo, e1 := strconv.ParseFloat(f[2], 64)
			hi, e2 := strconv.ParseFloat(f[3], 64)
			if e1 != nil || e2 != nil {
				return nil, bad("the minimum and maximum must be numbers")
			}
			if strings.EqualFold(f[1], "DOUBLE") {
				p["doubleValueSpec"] = map[string]any{"minValue": lo, "maxValue": hi}
			} else {
				if lo != math.Trunc(lo) || hi != math.Trunc(hi) {
					return nil, bad("an integer's minimum and maximum must be integers")
				}
				p["integerValueSpec"] = map[string]any{"minValue": strconv.FormatInt(int64(lo), 10), "maxValue": strconv.FormatInt(int64(hi), 10)}
			}
			scaleAt = 4
		case "CATEGORICAL":
			p["categoricalValueSpec"] = map[string]any{"values": strings.Split(f[2], ",")}
			if len(f) > 3 {
				return nil, bad("a categorical hyperparameter has no scale")
			}
		case "DISCRETE":
			var vals []any
			for _, s := range strings.Split(f[2], ",") {
				v, err := strconv.ParseFloat(s, 64)
				if err != nil {
					return nil, bad("its values must be numbers")
				}
				vals = append(vals, v)
			}
			p["discreteValueSpec"] = map[string]any{"values": vals}
			scaleAt = 3
		default:
			return nil, bad("its type is DOUBLE, INTEGER, CATEGORICAL or DISCRETE")
		}
		if scaleAt >= 0 && len(f) > scaleAt {
			s, ok := hpScales[strings.ToLower(f[scaleAt])]
			if !ok || len(f) > scaleAt+1 {
				return nil, bad("its scale is linear, log or reverse-log")
			}
			p["scaleType"] = s
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("hyperparameter tuning needs at least one hyperparameter")
	}
	return out, nil
}

// hpTaskInputsOf wraps a custom task's CustomJobSpec (as REST JSON) in the
// step's HyperparameterTuningJobSpec.
func hpTaskInputsOf(values map[string]string, spec map[string]any) (map[string]any, error) {
	params, err := hpParamsOf(values["hpParams"])
	if err != nil {
		return nil, err
	}
	metric := strings.TrimSpace(values["hpMetric"])
	if metric == "" {
		return nil, errors.New("hyperparameter tuning needs the metric to optimize")
	}
	goal := values["hpGoal"]
	if goal == "" {
		goal = "MAXIMIZE"
	}
	trials, _ := strconv.Atoi(strings.TrimSpace(values["hpMaxTrials"]))
	parallel, _ := strconv.Atoi(strings.TrimSpace(values["hpParallel"]))
	if trials < 1 || parallel < 1 {
		return nil, errors.New("the maximum numbers of trials and of parallel trials must be at least 1")
	}
	algo := values["hpAlgorithm"]
	if algo == "" {
		algo = "ALGORITHM_UNSPECIFIED"
	}
	return map[string]any{
		"studySpec":     map[string]any{"metrics": []any{map[string]any{"metricId": metric, "goal": goal}}, "parameters": params, "algorithm": algo},
		"maxTrialCount": trials, "parallelTrialCount": parallel, "trialJobSpec": spec,
	}, nil
}
