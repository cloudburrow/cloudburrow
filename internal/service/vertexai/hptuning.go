package vertexai

// Vertex AI hyperparameter tuning (#2210): JobService's
// HyperparameterTuningJob methods over gRPC and REST. Each Trial is a real
// CustomJob on this instance (customjobs.go) whose replicas get the
// trial's parameter values as command-line arguments, and whose primary
// replica's cloudml-hypertune reports are the trial's measurements.
//
// Documented behaviour (the HyperparameterTuningJob and Study protos,
// cloud.google.com/go/aiplatform v1.127.0;
// https://docs.cloud.google.com/vertex-ai/docs/training/hyperparameter-tuning-overview
// and .../training/using-hyperparameter-tuning, read 2026-11-24):
//   - display_name (up to 128 characters), study_spec, max_trial_count,
//     parallel_trial_count and trial_job_spec are required;
//     "The maxFailedTrialCount value must be less than or equal to
//     maxTrialCount";
//   - parameters are DOUBLE and INTEGER (inclusive min and max), CATEGORICAL
//     (a list of strings) and DISCRETE (an increasing list of numbers, at
//     most 1,000); a scale is linear, log or reverse log, the log scales
//     needing a strictly positive feasible space, and is left unset for
//     CATEGORICAL; a conditional parameter is active when its parent's
//     value is one of its parent_*_values; parameter ids are unique and
//     have no whitespace;
//   - algorithms: the default (Bayesian optimization), GRID_SEARCH ("all
//     parameters must be INTEGER, CATEGORICAL, or DISCRETE") and
//     RANDOM_SEARCH ("A random search within the feasible space");
//   - "The training service includes command-line arguments using these
//     names when it runs your application": each trial's replicas get
//     --PARAMETER_ID=VALUE;
//   - metrics are reported with the cloudml-hypertune package; a trial
//     "done but missing the final_measurement" is INFEASIBLE;
//     measurement_selection_type LAST_MEASUREMENT (the default) or
//     BEST_MEASUREMENT picks the final measurement;
//   - max_failed_trial_count failed trials fail the job; with 0, "If the
//     first trial fails, the job ends immediately";
//   - a Trial's custom_job names its CustomJob; trial_job_spec's
//     base_output_directory is the trial's BASE/TRIAL_ID, so its
//     AIP_MODEL_DIR is BASE/TRIAL_ID/model/.
//
// cloudml-hypertune (0.1.0.dev6, Google's, Apache-2.0, read from its PyPI
// sdist) writes the last 100 reports as JSON lines to
// CLOUD_ML_HP_METRIC_FILE, default /tmp/hypertune/output.metrics, each
// {"timestamp": T, "trial": ID, TAG: "VALUE", "global_step": "N",
// "checkpoint_path": ""}, TAG training/hptuning/metric when none is
// given, the trial id from CLOUD_ML_TRIAL_ID. The primary replica's
// /tmp/hypertune is a folder of the trial's, read when its job ends, and
// every replica gets CLOUD_ML_TRIAL_ID.
//
// UNVERIFIED (Google does not document them; re-read 2026-11-24,
// https://cloud.google.com/vertex-ai/docs/training/hyperparameter-tuning-overview): the default algorithm's
// method beyond "Bayesian optimization" (here a Gaussian process with
// expected improvement over the scaled space, after random first trials,
// seeded by the job's name); a grid's order (the parameters' cartesian
// product, the first parameter slowest) and that a job whose grid is
// exhausted ends with fewer trials; how many failed trials fail a job
// whose max_failed_trial_count is 0 beyond the first (here: more failed
// than succeeded); the trial CustomJobs' display names
// (DISPLAY_NAME-trial-ID); how a DISCRETE value is written in the
// argument (Go's shortest decimal form); the failed job's error message.
//
// Not run (UNIMPLEMENTED, naming why): the automated stopping specs and
// study_stopping_config, which stop trials early from their intermediate
// measurements; trial_job_spec's persistent resources and reserved IPs.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// hpJobEntry is a hyperparameter tuning job and its run.
type hpJobEntry struct {
	Job    *aiplatformpb.HyperparameterTuningJob
	cancel context.CancelFunc
	done   chan struct{}
}

// hypertuneDefaultTag is cloudml-hypertune's tag for a report with none.
const hypertuneDefaultTag = "training/hptuning/metric"

// trialRunKey carries a trial's run in its CustomJob's context.
type trialRunKey struct{}

type trialRun struct {
	id string
	// metricsDir is the host folder that is the primary replica's
	// /tmp/hypertune.
	metricsDir string
}

// trialEnv gives a trial's replica CLOUD_ML_TRIAL_ID and, on the primary,
// its /tmp/hypertune folder.
func trialEnv(ctx context.Context, c *PipelineContainer, primary bool) {
	tr, _ := ctx.Value(trialRunKey{}).(*trialRun)
	if tr == nil {
		return
	}
	c.Env["CLOUD_ML_TRIAL_ID"] = tr.id
	if primary {
		c.Mounts["/tmp/hypertune"] = tr.metricsDir
	}
}

// ---- validation

func paramKind(p *aiplatformpb.StudySpec_ParameterSpec) string {
	switch p.GetParameterValueSpec().(type) {
	case *aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_:
		return "DOUBLE"
	case *aiplatformpb.StudySpec_ParameterSpec_IntegerValueSpec_:
		return "INTEGER"
	case *aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_:
		return "CATEGORICAL"
	case *aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec_:
		return "DISCRETE"
	}
	return ""
}

func checkParam(field string, p *aiplatformpb.StudySpec_ParameterSpec, seen map[string]bool, grid bool) error {
	id := p.GetParameterId()
	if id == "" {
		return apierror.InvalidArgument("%s.parameter_id is required", field)
	}
	if strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return apierror.InvalidArgument("%s.parameter_id %q must not contain whitespace", field, id)
	}
	if seen[id] {
		return apierror.InvalidArgument("%s.parameter_id %q is not unique amongst the study's parameters", field, id)
	}
	seen[id] = true
	logScale := p.GetScaleType() == aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE ||
		p.GetScaleType() == aiplatformpb.StudySpec_ParameterSpec_UNIT_REVERSE_LOG_SCALE
	switch kind := paramKind(p); kind {
	case "DOUBLE":
		v := p.GetDoubleValueSpec()
		if grid {
			return apierror.InvalidArgument("%s (%s) is DOUBLE: to use grid search, all parameters must be INTEGER, CATEGORICAL, or DISCRETE", field, id)
		}
		if math.IsNaN(v.GetMinValue()) || math.IsNaN(v.GetMaxValue()) || math.IsInf(v.GetMinValue(), 0) || math.IsInf(v.GetMaxValue(), 0) {
			return apierror.InvalidArgument("%s (%s): min_value and max_value must be finite", field, id)
		}
		if v.GetMinValue() > v.GetMaxValue() {
			return apierror.InvalidArgument("%s (%s): min_value %v is greater than max_value %v", field, id, v.GetMinValue(), v.GetMaxValue())
		}
		if logScale && v.GetMinValue() <= 0 {
			return apierror.InvalidArgument("%s (%s): a log scale needs a strictly positive feasible space, and min_value is %v", field, id, v.GetMinValue())
		}
	case "INTEGER":
		v := p.GetIntegerValueSpec()
		if v.GetMinValue() > v.GetMaxValue() {
			return apierror.InvalidArgument("%s (%s): min_value %d is greater than max_value %d", field, id, v.GetMinValue(), v.GetMaxValue())
		}
		if logScale && v.GetMinValue() <= 0 {
			return apierror.InvalidArgument("%s (%s): a log scale needs a strictly positive feasible space, and min_value is %d", field, id, v.GetMinValue())
		}
	case "CATEGORICAL":
		vals := p.GetCategoricalValueSpec().GetValues()
		if len(vals) == 0 {
			return apierror.InvalidArgument("%s (%s).categorical_value_spec.values is required", field, id)
		}
		if p.GetScaleType() != aiplatformpb.StudySpec_ParameterSpec_SCALE_TYPE_UNSPECIFIED {
			return apierror.InvalidArgument("%s (%s): leave scale_type unset for CATEGORICAL parameters", field, id)
		}
		u := map[string]bool{}
		for _, v := range vals {
			if u[v] {
				return apierror.InvalidArgument("%s (%s): the category %q is listed twice", field, id, v)
			}
			u[v] = true
		}
	case "DISCRETE":
		vals := p.GetDiscreteValueSpec().GetValues()
		if len(vals) == 0 {
			return apierror.InvalidArgument("%s (%s).discrete_value_spec.values is required", field, id)
		}
		if len(vals) > 1000 {
			return apierror.InvalidArgument("%s (%s).discrete_value_spec.values: at most 1,000 values, got %d", field, id, len(vals))
		}
		for i := 1; i < len(vals); i++ {
			if !(vals[i]-vals[i-1] >= 1e-10) {
				return apierror.InvalidArgument("%s (%s).discrete_value_spec.values must be in increasing order and at least 1e-10 apart", field, id)
			}
		}
		if logScale && vals[0] <= 0 {
			return apierror.InvalidArgument("%s (%s): a log scale needs a strictly positive feasible space, and the least value is %v", field, id, vals[0])
		}
	default:
		return apierror.InvalidArgument("%s (%s) needs a double_value_spec, integer_value_spec, categorical_value_spec or discrete_value_spec", field, id)
	}
	for i, c := range p.GetConditionalParameterSpecs() {
		f := fmt.Sprintf("%s.conditional_parameter_specs[%d]", field, i)
		want := map[string]string{"INTEGER": "parent_int_values", "CATEGORICAL": "parent_categorical_values", "DISCRETE": "parent_discrete_values"}[paramKind(p)]
		got := ""
		var n int
		switch v := c.GetParentValueCondition().(type) {
		case *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentIntValues:
			got, n = "parent_int_values", len(v.ParentIntValues.GetValues())
		case *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentCategoricalValues:
			got, n = "parent_categorical_values", len(v.ParentCategoricalValues.GetValues())
		case *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentDiscreteValues:
			got, n = "parent_discrete_values", len(v.ParentDiscreteValues.GetValues())
		}
		if want == "" {
			return apierror.InvalidArgument("%s: a DOUBLE parameter (%s) cannot have conditional parameters", f, id)
		}
		if got != want {
			return apierror.InvalidArgument("%s: the parent %s is %s, so its condition is %s", f, id, paramKind(p), want)
		}
		if n == 0 {
			return apierror.InvalidArgument("%s.%s.values is required", f, want)
		}
		if c.GetParameterSpec() == nil {
			return apierror.InvalidArgument("%s.parameter_spec is required", f)
		}
		// A conditional parameter's id may repeat under disjoint conditions.
		child := map[string]bool{}
		for k, v := range seen {
			child[k] = v
		}
		delete(child, c.GetParameterSpec().GetParameterId())
		if err := checkParam(f+".parameter_spec", c.GetParameterSpec(), child, grid); err != nil {
			return err
		}
		seen[c.GetParameterSpec().GetParameterId()] = true
	}
	return nil
}

func checkStudySpec(field string, ss *aiplatformpb.StudySpec) error {
	if ss == nil {
		return apierror.InvalidArgument("%s is required", field)
	}
	if len(ss.GetMetrics()) == 0 {
		return apierror.InvalidArgument("%s.metrics is required: at least one metric", field)
	}
	ids := map[string]bool{}
	for i, m := range ss.GetMetrics() {
		if m.GetMetricId() == "" {
			return apierror.InvalidArgument("%s.metrics[%d].metric_id is required", field, i)
		}
		if ids[m.GetMetricId()] {
			return apierror.InvalidArgument("%s.metrics[%d].metric_id %q is listed twice", field, i, m.GetMetricId())
		}
		ids[m.GetMetricId()] = true
		if m.GetGoal() == aiplatformpb.StudySpec_MetricSpec_GOAL_TYPE_UNSPECIFIED {
			return apierror.InvalidArgument("%s.metrics[%d].goal is required: MAXIMIZE or MINIMIZE", field, i)
		}
	}
	if len(ss.GetParameters()) == 0 {
		return apierror.InvalidArgument("%s.parameters is required: at least one parameter", field)
	}
	grid := ss.GetAlgorithm() == aiplatformpb.StudySpec_GRID_SEARCH
	seen := map[string]bool{}
	for i, p := range ss.GetParameters() {
		if err := checkParam(fmt.Sprintf("%s.parameters[%d]", field, i), p, seen, grid); err != nil {
			return err
		}
	}
	switch ss.GetAutomatedStoppingSpec().(type) {
	case *aiplatformpb.StudySpec_DecayCurveStoppingSpec, *aiplatformpb.StudySpec_MedianAutomatedStoppingSpec_, *aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec_:
		return apierror.Unimplemented("%s's automated stopping spec is not run on this instance: trials are not stopped early from their intermediate measurements", field)
	}
	if ss.StudyStoppingConfig != nil {
		return apierror.Unimplemented("%s.study_stopping_config is not run on this instance: the study is not stopped early", field)
	}
	return nil
}

// checkHPSpec validates a HyperparameterTuningJob's (or a tuning task's)
// spec at field.
func checkHPSpec(field string, j *aiplatformpb.HyperparameterTuningJob) error {
	if err := checkStudySpec(field+".study_spec", j.GetStudySpec()); err != nil {
		return err
	}
	if j.GetMaxTrialCount() <= 0 {
		return apierror.InvalidArgument("%s.max_trial_count is required and must be positive", field)
	}
	if j.GetParallelTrialCount() <= 0 {
		return apierror.InvalidArgument("%s.parallel_trial_count is required and must be positive", field)
	}
	if j.GetMaxFailedTrialCount() < 0 {
		return apierror.InvalidArgument("%s.max_failed_trial_count must not be negative", field)
	}
	if j.GetMaxFailedTrialCount() > j.GetMaxTrialCount() {
		return apierror.InvalidArgument("%s.max_failed_trial_count (%d) must be less than or equal to max_trial_count (%d)", field, j.GetMaxFailedTrialCount(), j.GetMaxTrialCount())
	}
	if err := checkCustomJobSpec(field+".trial_job_spec", j.GetTrialJobSpec()); err != nil {
		return err
	}
	if j.GetTrialJobSpec().GetPersistentResourceId() != "" {
		return apierror.Unimplemented("%s.trial_job_spec.persistent_resource_id: persistent resources are not served on this instance", field)
	}
	return nil
}

// ---- the search space

// hpDim is one parameter, flattened with its conditional parameters.
type hpDim struct {
	spec *aiplatformpb.StudySpec_ParameterSpec
	// parent is the index of the dim this one is conditional on, -1 none;
	// cond its parent_*_values.
	parent int
	cond   *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec
}

func flattenParams(specs []*aiplatformpb.StudySpec_ParameterSpec) []hpDim {
	var out []hpDim
	var add func(p *aiplatformpb.StudySpec_ParameterSpec, parent int, c *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec)
	add = func(p *aiplatformpb.StudySpec_ParameterSpec, parent int, c *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec) {
		i := len(out)
		out = append(out, hpDim{spec: p, parent: parent, cond: c})
		for _, cc := range p.GetConditionalParameterSpecs() {
			add(cc.GetParameterSpec(), i, cc)
		}
	}
	for _, p := range specs {
		add(p, -1, nil)
	}
	return out
}

// hpValue is a parameter's value: a number, or a category.
type hpValue struct {
	num float64
	cat string
	isC bool
}

func (v hpValue) proto() *structpb.Value {
	if v.isC {
		return structpb.NewStringValue(v.cat)
	}
	return structpb.NewNumberValue(v.num)
}

// arg is the value as the trial's --ID=VALUE argument writes it.
func (v hpValue) arg(kind string) string {
	switch {
	case v.isC:
		return v.cat
	case kind == "INTEGER":
		return strconv.FormatInt(int64(v.num), 10)
	}
	return strconv.FormatFloat(v.num, 'g', -1, 64)
}

// active reports whether dim d's condition holds for vals.
func (d hpDim) active(vals []*hpValue) bool {
	if d.parent < 0 {
		return true
	}
	pv := vals[d.parent]
	if pv == nil {
		return false
	}
	switch c := d.cond.GetParentValueCondition().(type) {
	case *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentIntValues:
		for _, x := range c.ParentIntValues.GetValues() {
			if float64(x) == pv.num {
				return true
			}
		}
	case *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentCategoricalValues:
		for _, x := range c.ParentCategoricalValues.GetValues() {
			if x == pv.cat {
				return true
			}
		}
	case *aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentDiscreteValues:
		for _, x := range c.ParentDiscreteValues.GetValues() {
			if math.Abs(x-pv.num) < 1e-10 {
				return true
			}
		}
	}
	return false
}

// fromUnit is the value at u in [0, 1] of the parameter's scaled space.
func fromUnit(p *aiplatformpb.StudySpec_ParameterSpec, u float64) hpValue {
	u = math.Min(math.Max(u, 0), 1)
	scale := func(lo, hi float64) float64 {
		switch p.GetScaleType() {
		case aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE:
			return math.Exp(math.Log(lo) + u*(math.Log(hi)-math.Log(lo)))
		case aiplatformpb.StudySpec_ParameterSpec_UNIT_REVERSE_LOG_SCALE:
			return lo + hi - math.Exp(math.Log(lo)+(1-u)*(math.Log(hi)-math.Log(lo)))
		}
		return lo + u*(hi-lo)
	}
	pick := func(n int) int { return min(int(u*float64(n)), n-1) }
	switch v := p.GetParameterValueSpec().(type) {
	case *aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_:
		x := scale(v.DoubleValueSpec.GetMinValue(), v.DoubleValueSpec.GetMaxValue())
		return hpValue{num: math.Min(math.Max(x, v.DoubleValueSpec.GetMinValue()), v.DoubleValueSpec.GetMaxValue())}
	case *aiplatformpb.StudySpec_ParameterSpec_IntegerValueSpec_:
		lo, hi := float64(v.IntegerValueSpec.GetMinValue()), float64(v.IntegerValueSpec.GetMaxValue())
		// Each integer gets an equal share of the scaled space.
		x := math.Floor(scale(lo, hi+1))
		return hpValue{num: math.Min(math.Max(x, lo), hi)}
	case *aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_:
		vals := v.CategoricalValueSpec.GetValues()
		return hpValue{cat: vals[pick(len(vals))], isC: true}
	case *aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec_:
		vals := v.DiscreteValueSpec.GetValues()
		return hpValue{num: vals[pick(len(vals))]}
	}
	return hpValue{}
}

// gridPoints are a grid-searched parameter's values.
func gridPoints(p *aiplatformpb.StudySpec_ParameterSpec, limit int) []hpValue {
	var out []hpValue
	switch v := p.GetParameterValueSpec().(type) {
	case *aiplatformpb.StudySpec_ParameterSpec_IntegerValueSpec_:
		for x := v.IntegerValueSpec.GetMinValue(); x <= v.IntegerValueSpec.GetMaxValue() && len(out) < limit; x++ {
			out = append(out, hpValue{num: float64(x)})
		}
	case *aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_:
		for _, c := range v.CategoricalValueSpec.GetValues() {
			out = append(out, hpValue{cat: c, isC: true})
		}
	case *aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec_:
		for _, x := range v.DiscreteValueSpec.GetValues() {
			out = append(out, hpValue{num: x})
		}
	}
	return out
}

// gridAssignments are the first limit points of the grid: the cartesian
// product of the active parameters' values, the first parameter slowest.
func gridAssignments(dims []hpDim, limit int) [][]*hpValue {
	var out [][]*hpValue
	var walk func(i int, vals []*hpValue)
	walk = func(i int, vals []*hpValue) {
		if len(out) >= limit {
			return
		}
		if i == len(dims) {
			out = append(out, append([]*hpValue(nil), vals...))
			return
		}
		if !dims[i].active(vals) {
			walk(i+1, vals)
			return
		}
		for _, v := range gridPoints(dims[i].spec, limit) {
			v := v
			vals[i] = &v
			walk(i+1, vals)
			vals[i] = nil
			if len(out) >= limit {
				return
			}
		}
	}
	walk(0, make([]*hpValue, len(dims)))
	return out
}

// assign is the values a point u of the unit cube gives, inactive
// parameters nil.
func assign(dims []hpDim, u []float64) []*hpValue {
	vals := make([]*hpValue, len(dims))
	for i, d := range dims {
		if d.active(vals) {
			v := fromUnit(d.spec, u[i])
			vals[i] = &v
		}
	}
	return vals
}

// toUnit is where a value lies in the parameter's scaled space (the
// middle of its share for an integer, category or discrete value).
func toUnit(p *aiplatformpb.StudySpec_ParameterSpec, v *hpValue) float64 {
	if v == nil {
		return 0.5
	}
	unscale := func(x, lo, hi float64) float64 {
		if hi == lo {
			return 0.5
		}
		switch p.GetScaleType() {
		case aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE:
			return (math.Log(x) - math.Log(lo)) / (math.Log(hi) - math.Log(lo))
		case aiplatformpb.StudySpec_ParameterSpec_UNIT_REVERSE_LOG_SCALE:
			return 1 - (math.Log(lo+hi-x)-math.Log(lo))/(math.Log(hi)-math.Log(lo))
		}
		return (x - lo) / (hi - lo)
	}
	share := func(i, n int) float64 { return (float64(i) + 0.5) / float64(n) }
	switch s := p.GetParameterValueSpec().(type) {
	case *aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_:
		return unscale(v.num, s.DoubleValueSpec.GetMinValue(), s.DoubleValueSpec.GetMaxValue())
	case *aiplatformpb.StudySpec_ParameterSpec_IntegerValueSpec_:
		lo, hi := float64(s.IntegerValueSpec.GetMinValue()), float64(s.IntegerValueSpec.GetMaxValue())
		a, b := unscale(v.num, lo, hi+1), unscale(v.num+1, lo, hi+1)
		return (a + b) / 2
	case *aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_:
		vals := s.CategoricalValueSpec.GetValues()
		for i, c := range vals {
			if c == v.cat {
				return share(i, len(vals))
			}
		}
	case *aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec_:
		vals := s.DiscreteValueSpec.GetValues()
		for i, x := range vals {
			if math.Abs(x-v.num) < 1e-10 {
				return share(i, len(vals))
			}
		}
	}
	return 0.5
}

// hpObservation is a finished trial's point and objective (its first
// metric, as the goal is maximized: a MINIMIZE metric negated).
type hpObservation struct {
	u []float64
	y float64
}

// suggester proposes trials' values.
type suggester struct {
	algo aiplatformpb.StudySpec_Algorithm
	dims []hpDim
	rng  *rand.Rand
	grid [][]*hpValue
	next int
	// seen are the suggested points' keys, so a discrete space is not
	// tried twice while untried points remain.
	seen map[string]bool
}

func newSuggester(name string, ss *aiplatformpb.StudySpec, maxTrials int) *suggester {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	s := &suggester{algo: ss.GetAlgorithm(), dims: flattenParams(ss.GetParameters()), rng: rand.New(rand.NewSource(int64(h.Sum64()))), seen: map[string]bool{}}
	if s.algo == aiplatformpb.StudySpec_GRID_SEARCH {
		s.grid = gridAssignments(s.dims, maxTrials)
	}
	return s
}

func valuesKey(vals []*hpValue) string {
	var b strings.Builder
	for _, v := range vals {
		switch {
		case v == nil:
			b.WriteString("-|")
		case v.isC:
			b.WriteString("c" + v.cat + "|")
		default:
			b.WriteString(strconv.FormatFloat(v.num, 'g', -1, 64) + "|")
		}
	}
	return b.String()
}

func (s *suggester) randomPoint() []float64 {
	u := make([]float64, len(s.dims))
	for i := range u {
		u[i] = s.rng.Float64()
	}
	return u
}

// fresh is a point whose values were not suggested before, if one of n
// draws of draw is.
func (s *suggester) fresh(draw func() []float64, n int) []float64 {
	var u []float64
	for i := 0; i < n; i++ {
		u = draw()
		if !s.seen[valuesKey(assign(s.dims, u))] {
			return u
		}
	}
	return u
}

// suggest is the next trial's values; false when a grid is exhausted.
func (s *suggester) suggest(obs []hpObservation) ([]*hpValue, bool) {
	var vals []*hpValue
	switch s.algo {
	case aiplatformpb.StudySpec_GRID_SEARCH:
		if s.next >= len(s.grid) {
			return nil, false
		}
		vals = s.grid[s.next]
		s.next++
	case aiplatformpb.StudySpec_RANDOM_SEARCH:
		vals = assign(s.dims, s.fresh(s.randomPoint, 50))
	default:
		vals = assign(s.dims, s.bayesian(obs))
	}
	s.seen[valuesKey(vals)] = true
	return vals, true
}

// bayesian is the default algorithm's next point: random until there are
// as many observations as dimensions plus one (at least 2), then the
// candidate of most expected improvement under a Gaussian process fitted
// to the observations (an RBF kernel on the scaled space, the objective
// standardized).
func (s *suggester) bayesian(obs []hpObservation) []float64 {
	d := len(s.dims)
	if len(obs) < max(2, d+1) {
		return s.fresh(s.randomPoint, 50)
	}
	if len(obs) > 200 {
		obs = obs[len(obs)-200:]
	}
	n := len(obs)
	mean, sd := 0.0, 0.0
	for _, o := range obs {
		mean += o.y
	}
	mean /= float64(n)
	for _, o := range obs {
		sd += (o.y - mean) * (o.y - mean)
	}
	sd = math.Sqrt(sd / float64(n))
	if sd == 0 {
		sd = 1
	}
	y := make([]float64, n)
	best := math.Inf(-1)
	bestI := 0
	for i, o := range obs {
		y[i] = (o.y - mean) / sd
		if y[i] > best {
			best, bestI = y[i], i
		}
	}
	ls := 0.25 * math.Sqrt(float64(d))
	kern := func(a, b []float64) float64 {
		s := 0.0
		for i := range a {
			s += (a[i] - b[i]) * (a[i] - b[i])
		}
		return math.Exp(-s / (2 * ls * ls))
	}
	K := make([][]float64, n)
	for i := range K {
		K[i] = make([]float64, n)
		for j := range K[i] {
			K[i][j] = kern(obs[i].u, obs[j].u)
		}
		K[i][i] += 1e-4
	}
	L, ok := cholesky(K)
	if !ok {
		return s.fresh(s.randomPoint, 50)
	}
	alpha := cholSolve(L, y)
	ei := func(u []float64) float64 {
		k := make([]float64, n)
		for i := range k {
			k[i] = kern(u, obs[i].u)
		}
		mu := 0.0
		for i := range k {
			mu += k[i] * alpha[i]
		}
		v := forwardSub(L, k)
		varr := 1.0
		for _, x := range v {
			varr -= x * x
		}
		if varr < 1e-12 {
			return 0
		}
		sig := math.Sqrt(varr)
		z := (mu - best - 0.01) / sig
		return (mu-best-0.01)*normCDF(z) + sig*normPDF(z)
	}
	var cands [][]float64
	for i := 0; i < 400; i++ {
		cands = append(cands, s.randomPoint())
	}
	for i := 0; i < 100; i++ {
		c := append([]float64(nil), obs[bestI].u...)
		for j := range c {
			c[j] = math.Min(math.Max(c[j]+s.rng.NormFloat64()*0.08, 0), 1)
		}
		cands = append(cands, c)
	}
	var pick []float64
	top := math.Inf(-1)
	for _, c := range cands {
		if s.seen[valuesKey(assign(s.dims, c))] {
			continue
		}
		if e := ei(c); e > top {
			top, pick = e, c
		}
	}
	if pick == nil {
		return s.fresh(s.randomPoint, 50)
	}
	return pick
}

func cholesky(a [][]float64) ([][]float64, bool) {
	n := len(a)
	L := make([][]float64, n)
	for i := range L {
		L[i] = make([]float64, n)
		for j := 0; j <= i; j++ {
			s := a[i][j]
			for k := 0; k < j; k++ {
				s -= L[i][k] * L[j][k]
			}
			if i == j {
				if s <= 0 {
					return nil, false
				}
				L[i][i] = math.Sqrt(s)
			} else {
				L[i][j] = s / L[j][j]
			}
		}
	}
	return L, true
}

func forwardSub(L [][]float64, b []float64) []float64 {
	x := make([]float64, len(b))
	for i := range b {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= L[i][k] * x[k]
		}
		x[i] = s / L[i][i]
	}
	return x
}

func cholSolve(L [][]float64, b []float64) []float64 {
	z := forwardSub(L, b)
	n := len(b)
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := z[i]
		for k := i + 1; k < n; k++ {
			s -= L[k][i] * x[k]
		}
		x[i] = s / L[i][i]
	}
	return x
}

func normPDF(z float64) float64 { return math.Exp(-z*z/2) / math.Sqrt(2*math.Pi) }
func normCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }

// ---- metrics

// readHypertune reads the measurements cloudml-hypertune wrote in dir, for
// the study's metrics, each with its step and elapsed time since start.
func readHypertune(dir string, metrics []*aiplatformpb.StudySpec_MetricSpec, start time.Time) []*aiplatformpb.Measurement {
	f, err := os.Open(dir + "/output.metrics")
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []*aiplatformpb.Measurement
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		m := &aiplatformpb.Measurement{}
		if gs, ok := line["global_step"].(string); ok {
			m.StepCount, _ = strconv.ParseInt(gs, 10, 64)
		}
		if ts, ok := line["timestamp"].(float64); ok {
			el := time.Unix(0, int64(ts*1e9)).Sub(start)
			if el < 0 {
				el = 0
			}
			m.ElapsedDuration = durationpb.New(el)
		}
		for _, ms := range metrics {
			raw, ok := line[ms.GetMetricId()]
			if !ok && len(metrics) == 1 {
				raw, ok = line[hypertuneDefaultTag]
			}
			if !ok {
				continue
			}
			var v float64
			switch x := raw.(type) {
			case string:
				v, err = strconv.ParseFloat(x, 64)
				if err != nil {
					continue
				}
			case float64:
				v = x
			default:
				continue
			}
			m.Metrics = append(m.Metrics, &aiplatformpb.Measurement_Metric{MetricId: ms.GetMetricId(), Value: v})
		}
		if len(m.Metrics) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// objective is a measurement's first metric, maximized, and whether it
// has it.
func objective(ss *aiplatformpb.StudySpec, m *aiplatformpb.Measurement) (float64, bool) {
	first := ss.GetMetrics()[0]
	for _, x := range m.GetMetrics() {
		if x.GetMetricId() == first.GetMetricId() {
			if first.GetGoal() == aiplatformpb.StudySpec_MetricSpec_MINIMIZE {
				return -x.GetValue(), true
			}
			return x.GetValue(), true
		}
	}
	return 0, false
}

// finalMeasurement picks the trial's final measurement as
// measurement_selection_type says.
func finalMeasurement(ss *aiplatformpb.StudySpec, ms []*aiplatformpb.Measurement) *aiplatformpb.Measurement {
	if len(ms) == 0 {
		return nil
	}
	if ss.GetMeasurementSelectionType() != aiplatformpb.StudySpec_BEST_MEASUREMENT {
		return ms[len(ms)-1]
	}
	var best *aiplatformpb.Measurement
	top := math.Inf(-1)
	for _, m := range ms {
		if y, ok := objective(ss, m); ok && y > top {
			top, best = y, m
		}
	}
	if best == nil {
		return ms[len(ms)-1]
	}
	return best
}

// ---- the methods

func (j *jobService) CreateHyperparameterTuningJob(_ context.Context, req *aiplatformpb.CreateHyperparameterTuningJobRequest) (*aiplatformpb.HyperparameterTuningJob, error) {
	out, err := j.s.createHPJob(req.GetParent(), req.GetHyperparameterTuningJob(), nil)
	if err != nil {
		return nil, grpcError(err)
	}
	return out, nil
}

// createHPJob records a hyperparameter tuning job and starts it; onEnd,
// when set, is called with the job once it ends.
func (s *Server) createHPJob(parent string, in *aiplatformpb.HyperparameterTuningJob, onEnd func(*aiplatformpb.HyperparameterTuningJob)) (*aiplatformpb.HyperparameterTuningJob, error) {
	if _, _, err := parseParent("parent", parent); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("hyperparameter_tuning_job is required")
	}
	if in.GetDisplayName() == "" {
		return nil, apierror.InvalidArgument("hyperparameter_tuning_job.display_name is required")
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		return nil, apierror.InvalidArgument("hyperparameter_tuning_job.display_name can be up to 128 characters long")
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	if err := checkHPSpec("hyperparameter_tuning_job", in); err != nil {
		return nil, err
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pipelineRunner == nil {
		return nil, apierror.FailedPrecondition("Vertex AI hyperparameter tuning runs each trial as a custom job, which needs Docker and this instance's Cloud Storage (start with --services including storage)")
	}
	if st.hpJobs == nil {
		st.hpJobs = map[string]*hpJobEntry{}
	}
	name := parent + "/hyperparameterTuningJobs/" + regNumericID()
	for st.hpJobs[name] != nil {
		name = parent + "/hyperparameterTuningJobs/" + regNumericID()
	}
	j := proto.Clone(in).(*aiplatformpb.HyperparameterTuningJob)
	now := timestamppb.New(st.now())
	j.Name, j.CreateTime, j.UpdateTime, j.StartTime, j.EndTime, j.Error, j.Trials = name, now, now, nil, nil, nil, nil
	j.State = aiplatformpb.JobState_JOB_STATE_QUEUED
	ctx, cancel := context.WithCancel(context.Background())
	e := &hpJobEntry{Job: j, cancel: cancel, done: make(chan struct{})}
	st.hpJobs[name] = e
	st.save()
	go s.runHPJob(ctx, e, parent, onEnd)
	return proto.Clone(j).(*aiplatformpb.HyperparameterTuningJob), nil
}

func (st *notebookStore) setHPJob(e *hpJobEntry, f func(j *aiplatformpb.HyperparameterTuningJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hpJobs[e.Job.GetName()] != e {
		return
	}
	f(e.Job)
	e.Job.UpdateTime = timestamppb.New(st.now())
	st.save()
}

// trialJob is a trial's CustomJob: the trial job spec, its replicas given
// --ID=VALUE for each active parameter, its output under BASE/TRIAL_ID.
func trialJob(j *aiplatformpb.HyperparameterTuningJob, id string, dims []hpDim, vals []*hpValue) *aiplatformpb.CustomJob {
	spec := proto.Clone(j.GetTrialJobSpec()).(*aiplatformpb.CustomJobSpec)
	var args []string
	for i, d := range dims {
		if vals[i] != nil {
			args = append(args, "--"+d.spec.GetParameterId()+"="+vals[i].arg(paramKind(d.spec)))
		}
	}
	for _, p := range spec.GetWorkerPoolSpecs() {
		switch t := p.GetTask().(type) {
		case *aiplatformpb.WorkerPoolSpec_ContainerSpec:
			t.ContainerSpec.Args = append(t.ContainerSpec.Args, args...)
		case *aiplatformpb.WorkerPoolSpec_PythonPackageSpec:
			t.PythonPackageSpec.Args = append(t.PythonPackageSpec.Args, args...)
		}
	}
	if b := spec.GetBaseOutputDirectory().GetOutputUriPrefix(); b != "" {
		spec.BaseOutputDirectory.OutputUriPrefix = strings.TrimSuffix(b, "/") + "/" + id
	}
	return &aiplatformpb.CustomJob{DisplayName: truncateUTF8(j.GetDisplayName()+"-trial-"+id, 128), JobSpec: spec,
		Labels: j.GetLabels(), EncryptionSpec: j.GetEncryptionSpec()}
}

func truncateUTF8(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

type trialDone struct {
	id  string
	job *aiplatformpb.CustomJob
	dir string
}

func (s *Server) runHPJob(ctx context.Context, e *hpJobEntry, parent string, onEnd func(*aiplatformpb.HyperparameterTuningJob)) {
	st := s.notebooks()
	defer close(e.done)
	st.setHPJob(e, func(j *aiplatformpb.HyperparameterTuningJob) { j.State = aiplatformpb.JobState_JOB_STATE_PENDING })
	st.setHPJob(e, func(j *aiplatformpb.HyperparameterTuningJob) {
		j.State, j.StartTime = aiplatformpb.JobState_JOB_STATE_RUNNING, timestamppb.New(st.now())
	})
	st.mu.Lock()
	spec := proto.Clone(e.Job).(*aiplatformpb.HyperparameterTuningJob)
	st.mu.Unlock()
	ss := spec.GetStudySpec()
	maxTrials, parallel, maxFailed := int(spec.GetMaxTrialCount()), int(spec.GetParallelTrialCount()), int(spec.GetMaxFailedTrialCount())
	sug := newSuggester(spec.GetName(), ss, maxTrials)
	results := make(chan trialDone, parallel)
	running := map[string]string{} // trial id -> custom job
	var obs []hpObservation
	started, failed, succeeded := 0, 0, 0
	var firstEnded *bool
	var lastErr *status.Status
	stop, exhausted := false, false
	setTrial := func(id string, f func(t *aiplatformpb.Trial)) {
		st.setHPJob(e, func(j *aiplatformpb.HyperparameterTuningJob) {
			for _, t := range j.Trials {
				if t.GetId() == id {
					f(t)
				}
			}
		})
	}
	tooManyFailed := func() bool {
		if maxFailed > 0 {
			return failed >= maxFailed
		}
		return (firstEnded != nil && !*firstEnded) || (failed > 0 && failed > succeeded)
	}
	cancelRunning := func() {
		for _, cj := range running {
			_ = s.cancelCustomJob(cj)
		}
	}
	for {
		for !stop && !exhausted && ctx.Err() == nil && len(running) < parallel && started < maxTrials {
			vals, ok := sug.suggest(obs)
			if !ok {
				exhausted = true
				break
			}
			started++
			id := strconv.Itoa(started)
			trial := &aiplatformpb.Trial{Id: id, State: aiplatformpb.Trial_ACTIVE, StartTime: timestamppb.New(st.now())}
			for i, d := range sug.dims {
				if vals[i] != nil {
					trial.Parameters = append(trial.Parameters, &aiplatformpb.Trial_Parameter{ParameterId: d.spec.GetParameterId(), Value: vals[i].proto()})
				}
			}
			st.setHPJob(e, func(j *aiplatformpb.HyperparameterTuningJob) { j.Trials = append(j.Trials, trial) })
			dir, err := os.MkdirTemp("", "cb-hptrial-")
			if err == nil {
				_ = os.Chmod(dir, 0o777)
			}
			var cj *aiplatformpb.CustomJob
			if err == nil {
				tctx := context.WithValue(context.Background(), trialRunKey{}, &trialRun{id: id, metricsDir: dir})
				cj, err = s.startCustomJob(tctx, parent, trialJob(spec, id, sug.dims, vals), func(done *aiplatformpb.CustomJob) {
					results <- trialDone{id: id, job: done, dir: dir}
				})
			}
			if err != nil {
				_ = os.RemoveAll(dir)
				failed++
				lastErr = &status.Status{Code: int32(apierror.From(err).Code), Message: apierror.From(err).Message}
				setTrial(id, func(t *aiplatformpb.Trial) {
					t.State, t.EndTime, t.InfeasibleReason = aiplatformpb.Trial_INFEASIBLE, timestamppb.New(st.now()), lastErr.GetMessage()
				})
				if firstEnded == nil {
					f := false
					firstEnded = &f
				}
				if tooManyFailed() {
					stop = true
				}
				continue
			}
			running[id] = cj.GetName()
			setTrial(id, func(t *aiplatformpb.Trial) { t.CustomJob = cj.GetName() })
		}
		if len(running) == 0 {
			break
		}
		var r trialDone
		select {
		case r = <-results:
		case <-ctx.Done():
			cancelRunning()
			r = <-results
		}
		delete(running, r.id)
		var start time.Time
		if r.job.GetStartTime() != nil {
			start = r.job.GetStartTime().AsTime()
		}
		ms := readHypertune(r.dir, ss.GetMetrics(), start)
		_ = os.RemoveAll(r.dir)
		final := finalMeasurement(ss, ms)
		ok := r.job.GetState() == aiplatformpb.JobState_JOB_STATE_SUCCEEDED && final != nil
		reason := ""
		switch {
		case r.job.GetState() == aiplatformpb.JobState_JOB_STATE_CANCELLED:
			reason = "The trial's custom job " + r.job.GetName() + " was cancelled."
		case r.job.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED:
			reason = r.job.GetError().GetMessage()
			lastErr = r.job.GetError()
		case final == nil:
			reason = fmt.Sprintf("The trial ended without reporting the metric %s (cloudml-hypertune's report_hyperparameter_tuning_metric), so it has no final measurement.", ss.GetMetrics()[0].GetMetricId())
			lastErr = &status.Status{Code: int32(codes.InvalidArgument), Message: reason}
		}
		setTrial(r.id, func(t *aiplatformpb.Trial) {
			t.EndTime, t.Measurements = timestamppb.New(st.now()), ms
			if ok {
				t.State, t.FinalMeasurement = aiplatformpb.Trial_SUCCEEDED, final
			} else {
				t.State, t.InfeasibleReason = aiplatformpb.Trial_INFEASIBLE, reason
			}
		})
		if ctx.Err() != nil {
			continue
		}
		if firstEnded == nil {
			firstEnded = &ok
		}
		if ok {
			succeeded++
			st.mu.Lock()
			var tp *aiplatformpb.Trial
			for _, t := range e.Job.GetTrials() {
				if t.GetId() == r.id {
					tp = proto.Clone(t).(*aiplatformpb.Trial)
				}
			}
			st.mu.Unlock()
			if y, has := objective(ss, final); has && tp != nil {
				obs = append(obs, hpObservation{u: trialPoint(sug.dims, tp), y: y})
			}
		} else {
			failed++
			if tooManyFailed() && !stop {
				stop = true
				cancelRunning()
			}
		}
	}
	st.setHPJob(e, func(j *aiplatformpb.HyperparameterTuningJob) {
		j.EndTime = timestamppb.New(st.now())
		switch {
		case ctx.Err() != nil:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_CANCELLED, &status.Status{Code: int32(codes.Canceled), Message: "CANCELED"}
		case stop || succeeded == 0:
			code := int32(codes.Internal)
			msg := ""
			if lastErr != nil {
				code, msg = lastErr.GetCode(), lastErr.GetMessage()
			}
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_FAILED, &status.Status{Code: code,
				Message: fmt.Sprintf("%d of the %d trials run failed. The last failure: %s", failed, started, msg)}
		default:
			j.State = aiplatformpb.JobState_JOB_STATE_SUCCEEDED
		}
	})
	if onEnd != nil {
		st.mu.Lock()
		out := proto.Clone(e.Job).(*aiplatformpb.HyperparameterTuningJob)
		st.mu.Unlock()
		onEnd(out)
	}
}

// trialPoint is a trial's parameters as a point of the unit cube.
func trialPoint(dims []hpDim, t *aiplatformpb.Trial) []float64 {
	byID := map[string]*structpb.Value{}
	for _, p := range t.GetParameters() {
		byID[p.GetParameterId()] = p.GetValue()
	}
	vals := make([]*hpValue, len(dims))
	u := make([]float64, len(dims))
	for i, d := range dims {
		if !d.active(vals) {
			u[i] = 0.5
			continue
		}
		v, ok := byID[d.spec.GetParameterId()]
		if !ok {
			u[i] = 0.5
			continue
		}
		hv := hpValue{num: v.GetNumberValue()}
		if _, isS := v.GetKind().(*structpb.Value_StringValue); isS {
			hv = hpValue{cat: v.GetStringValue(), isC: true}
		}
		vals[i] = &hv
		u[i] = toUnit(d.spec, &hv)
	}
	return u
}

// cancelCustomJob cancels a running custom job (a trial's).
func (s *Server) cancelCustomJob(name string) error {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.customJob(name)
	if err != nil {
		return err
	}
	if !jobRunning(e.Job.GetState()) {
		return nil
	}
	e.Job.State, e.Job.UpdateTime = aiplatformpb.JobState_JOB_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return nil
}

func (st *notebookStore) hpJob(name string) (*hpJobEntry, error) {
	if _, _, _, err := parseChild("name", "hyperparameterTuningJobs", name); err != nil {
		return nil, err
	}
	e, ok := st.hpJobs[name]
	if !ok {
		return nil, apierror.NotFound("HyperparameterTuningJob %s not found", name)
	}
	return e, nil
}

func (j *jobService) GetHyperparameterTuningJob(_ context.Context, req *aiplatformpb.GetHyperparameterTuningJobRequest) (*aiplatformpb.HyperparameterTuningJob, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.hpJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(e.Job).(*aiplatformpb.HyperparameterTuningJob), nil
}

func (j *jobService) ListHyperparameterTuningJobs(_ context.Context, req *aiplatformpb.ListHyperparameterTuningJobsRequest) (*aiplatformpb.ListHyperparameterTuningJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := j.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.HyperparameterTuningJob
	for _, k := range sortedNames(st.hpJobs) {
		if strings.HasPrefix(k, req.GetParent()+"/hyperparameterTuningJobs/") {
			all = append(all, proto.Clone(st.hpJobs[k].Job).(*aiplatformpb.HyperparameterTuningJob))
		}
	}
	st.mu.Unlock()
	items, next, err := listJobs("hyperparameterTuningJobs", req.GetParent(), req.GetFilter(), "", req.GetPageToken(), req.GetPageSize(), all)
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetReadMask() != nil {
		keep := map[string]bool{}
		for _, p := range req.GetReadMask().GetPaths() {
			keep[camelToSnake(p)] = true
		}
		for _, it := range items {
			m := it.ProtoReflect()
			fields := m.Descriptor().Fields()
			for i := 0; i < fields.Len(); i++ {
				if fd := fields.Get(i); !keep[string(fd.Name())] && fd.Name() != "name" {
					m.Clear(fd)
				}
			}
		}
	}
	return &aiplatformpb.ListHyperparameterTuningJobsResponse{HyperparameterTuningJobs: items, NextPageToken: next}, nil
}

func (j *jobService) DeleteHyperparameterTuningJob(_ context.Context, req *aiplatformpb.DeleteHyperparameterTuningJobRequest) (*longrunningpb.Operation, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	e, err := st.hpJob(req.GetName())
	if err == nil && jobRunning(e.Job.GetState()) {
		err = apierror.FailedPrecondition("HyperparameterTuningJob %s is %s: cancel it before deleting it", req.GetName(), e.Job.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.hpJobs, req.GetName())
	st.save()
	st.mu.Unlock()
	return j.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (j *jobService) CancelHyperparameterTuningJob(_ context.Context, req *aiplatformpb.CancelHyperparameterTuningJobRequest) (*emptypb.Empty, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.hpJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if !jobRunning(e.Job.GetState()) {
		return nil, grpcError(apierror.FailedPrecondition("HyperparameterTuningJob %s is %s and cannot be cancelled", req.GetName(), e.Job.GetState()))
	}
	e.Job.State, e.Job.UpdateTime = aiplatformpb.JobState_JOB_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return &emptypb.Empty{}, nil
}

// waitHPJob waits for a hyperparameter tuning job's run to end and
// returns it.
func (s *Server) waitHPJob(ctx context.Context, name string) (*aiplatformpb.HyperparameterTuningJob, error) {
	st := s.notebooks()
	st.mu.Lock()
	e, err := st.hpJob(name)
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		st.mu.Lock()
		if jobRunning(e.Job.GetState()) && e.cancel != nil {
			e.cancel()
		}
		st.mu.Unlock()
		<-e.done
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return proto.Clone(e.Job).(*aiplatformpb.HyperparameterTuningJob), nil
}

// bestTrial is a finished job's best succeeded trial by its first metric.
func bestTrial(j *aiplatformpb.HyperparameterTuningJob) *aiplatformpb.Trial {
	var best *aiplatformpb.Trial
	top := math.Inf(-1)
	for _, t := range j.GetTrials() {
		if t.GetState() != aiplatformpb.Trial_SUCCEEDED {
			continue
		}
		if y, ok := objective(j.GetStudySpec(), t.GetFinalMeasurement()); ok && y > top {
			top, best = y, t
		}
	}
	return best
}

// loadHPJobs restores saved hyperparameter tuning jobs: one running when
// the instance stopped failed with it, its active trials infeasible.
func (st *notebookStore) loadHPJobs(raw []json.RawMessage) {
	st.hpJobs = map[string]*hpJobEntry{}
	closed := make(chan struct{})
	close(closed)
	for _, b := range raw {
		j := &aiplatformpb.HyperparameterTuningJob{}
		if lenient.Unmarshal(b, j) != nil || j.GetName() == "" {
			continue
		}
		if jobRunning(j.GetState()) {
			j.State, j.Error, j.EndTime = aiplatformpb.JobState_JOB_STATE_FAILED, stoppedWithInstance, timestamppb.New(time.Now())
			for _, t := range j.GetTrials() {
				if t.GetState() == aiplatformpb.Trial_ACTIVE || t.GetState() == aiplatformpb.Trial_STOPPING || t.GetState() == aiplatformpb.Trial_REQUESTED {
					t.State, t.InfeasibleReason, t.EndTime = aiplatformpb.Trial_INFEASIBLE, stoppedWithInstance.GetMessage(), j.EndTime
				}
			}
		}
		st.hpJobs[j.GetName()] = &hpJobEntry{Job: j, done: closed}
	}
}

// ---- the hyperparameter tuning training task

// hpTaskDefinition is the hyperparameter tuning training task's schema,
// named as the custom task's is. Its inputs are a HyperparameterTuningJobSpec
// (study_spec, max_trial_count, parallel_trial_count,
// max_failed_trial_count, trial_job_spec) and its metadata a
// HyperparameterTuningJobMetadata (backingHyperparameterTuningJob,
// bestTrialBackingCustomJob), as the HyperparameterTuningTask reference
// page documents them (read 2026-11-24).
// UNVERIFIED (re-read 2026-11-24, https://cloud.google.com/vertex-ai/docs/reference/rest/v1/projects.locations.trainingPipelines):
// the schema URI, which no page names: the custom task's,
// custom_task_1.0.0.yaml, with this task's name.
const hpTaskDefinition = "gs://google-cloud-aiplatform/schema/trainingjob/definition/hyperparameter_tuning_task_1.0.0.yaml"

// hpTaskInputs reads a hyperparameter tuning task's training_task_inputs.
func hpTaskInputs(v *structpb.Value) (*aiplatformpb.HyperparameterTuningJob, error) {
	if v.GetStructValue() == nil {
		return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs is required: the hyperparameter tuning task's HyperparameterTuningJobSpec")
	}
	for k := range v.GetStructValue().GetFields() {
		switch k {
		case "studySpec", "study_spec", "maxTrialCount", "max_trial_count", "parallelTrialCount", "parallel_trial_count",
			"maxFailedTrialCount", "max_failed_trial_count", "trialJobSpec", "trial_job_spec":
		default:
			return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs: %q is not a field of a HyperparameterTuningJobSpec", k)
		}
	}
	b, err := protojson.Marshal(v)
	if err != nil {
		return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs: %v", err)
	}
	j := &aiplatformpb.HyperparameterTuningJob{}
	if err := protojson.Unmarshal(b, j); err != nil {
		return nil, apierror.InvalidArgument("training_pipeline.training_task_inputs is not a HyperparameterTuningJobSpec: %v", err)
	}
	if err := checkHPSpec("training_pipeline.training_task_inputs", j); err != nil {
		return nil, err
	}
	return j, nil
}

// runHPTraining runs a hyperparameter tuning pipeline: its
// HyperparameterTuningJob, then model_to_upload from the best trial's model
// directory (BASE/TRIAL_ID/model).
func (s *Server) runHPTraining(ctx context.Context, e *trainingEntry, parent string) {
	st := s.notebooks()
	st.mu.Lock()
	tp := proto.Clone(e.TP).(*aiplatformpb.TrainingPipeline)
	in := proto.Clone(e.hp).(*aiplatformpb.HyperparameterTuningJob)
	runner := st.pipelineRunner
	st.mu.Unlock()
	fail := func(code codes.Code, msg string) {
		st.setTraining(e, func(t *aiplatformpb.TrainingPipeline) {
			t.State, t.Error, t.EndTime = aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, &status.Status{Code: int32(code), Message: msg}, timestamppb.New(st.now())
			if code == codes.Canceled {
				t.State = aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED
			}
		})
	}
	if c := tp.GetInputDataConfig(); c != nil {
		// A managed dataset's data, exported once for every trial
		// (datasetsplit.go), as for a custom training pipeline.
		env, err := s.exportTrainingData(ctx, runner, parent, c, tp.GetCreateTime().AsTime())
		if err != nil {
			ae := apierror.From(err)
			fail(ae.Code, "Exporting the training data from Dataset "+parent+"/datasets/"+c.GetDatasetId()+" failed: "+ae.Message)
			return
		}
		withDataEnv(in.GetTrialJobSpec(), env)
	}
	in.DisplayName, in.Labels, in.EncryptionSpec = truncateUTF8(tp.GetDisplayName()+"-hyperparameter-tuning-job", 128), tp.GetLabels(), tp.GetEncryptionSpec()
	job, err := s.createHPJob(parent, in, nil)
	if err != nil {
		ae := apierror.From(err)
		fail(ae.Code, ae.Message)
		return
	}
	meta, _ := structpb.NewValue(map[string]any{"backingHyperparameterTuningJob": job.GetName()})
	st.setTraining(e, func(t *aiplatformpb.TrainingPipeline) {
		t.State, t.StartTime, t.TrainingTaskMetadata = aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, timestamppb.New(st.now()), meta
	})
	done, err := s.waitHPJob(ctx, job.GetName())
	switch {
	case ctx.Err() != nil:
		fail(codes.Canceled, "CANCELED")
		return
	case err != nil:
		ae := apierror.From(err)
		fail(ae.Code, ae.Message)
		return
	case done.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED:
		fail(codes.Code(done.GetError().GetCode()), fmt.Sprintf("Job failed. The backing HyperparameterTuningJob %s %s: %s", job.GetName(),
			strings.ToLower(strings.TrimPrefix(done.GetState().String(), "JOB_STATE_")), done.GetError().GetMessage()))
		return
	}
	best := bestTrial(done)
	m := map[string]any{"backingHyperparameterTuningJob": job.GetName()}
	if best != nil {
		m["bestTrialBackingCustomJob"] = best.GetCustomJob()
	}
	meta, _ = structpb.NewValue(m)
	var uploaded *aiplatformpb.Model
	if mu := tp.GetModelToUpload(); mu != nil && best != nil {
		r := s.registry()
		model := proto.Clone(mu).(*aiplatformpb.Model)
		if model.ArtifactUri == "" {
			model.ArtifactUri = strings.TrimSuffix(in.GetTrialJobSpec().GetBaseOutputDirectory().GetOutputUriPrefix(), "/") + "/" + best.GetId() + "/model"
		}
		resp, err := r.upload(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, Model: model, ModelId: tp.GetModelId(), ParentModel: tp.GetParentModel()})
		if err != nil {
			ae := apierror.From(err)
			fail(ae.Code, "Model upload failed: "+ae.Message)
			return
		}
		if uploaded, err = r.get(resp.GetModel() + "@" + resp.GetModelVersionId()); err != nil {
			uploaded = &aiplatformpb.Model{Name: resp.GetModel(), VersionId: resp.GetModelVersionId()}
		}
	}
	st.setTraining(e, func(t *aiplatformpb.TrainingPipeline) {
		t.State, t.EndTime, t.TrainingTaskMetadata = aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED, timestamppb.New(st.now()), meta
		if uploaded != nil {
			t.ModelToUpload = uploaded
			_, _, t.ModelId, _, _ = parseModelName("model", uploaded.GetName())
		}
	})
}
