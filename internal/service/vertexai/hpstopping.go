package vertexai

// Automated early stopping of hyperparameter tuning trials and automated
// stopping of the study (#2210), as the StudySpec reference
// (https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/Shared.Types/StudySpec,
// read 2026-11-24) documents them.
//
// Trial rules (the automated_stopping_spec oneof), each evaluated while a
// trial runs, on its intermediate measurements (cloudml-hypertune's
// reports, read every hpPollInterval):
//
//   - median_automated_stopping_spec: "a pending Trial is stopped if its
//     best objective value is strictly below the median 'performance' of
//     all completed Trials reported up to the Trial's last measurement",
//     a completed Trial's performance being the running average of its
//     objective values up to that point; use_elapsed_duration measures
//     "up to" in elapsed_duration rather than step_count;
//   - decay_curve_stopping_spec: "a Gaussian Process Regressor is used to
//     predict the final objective value of a Trial based on the already
//     completed Trials and the intermediate measurements of the current
//     Trial. Early stopping is requested for the current Trial if there is
//     very low probability to exceed the optimal value found so far";
//     use_elapsed_duration makes elapsed_duration the curve's x-axis.
//
// Study rules (study_stopping_config), checked top to bottom, the first
// that applies deciding: minimum_runtime_constraint and min_num_trials
// keep the study going until they are met; maximum_runtime_constraint
// (max_duration since the job started, or end_time) and max_num_trials
// stop it; max_num_trials_no_progress and max_duration_no_progress stop it
// when the objective has not improved for that many consecutive trials or
// that long ("Effective only for single-objective studies", so ignored
// with several metrics). A stopped study suggests no more trials; with
// should_stop_asap its running trials are interrupted too ("interrupts
// ongoing evaluations of trials as soon as the stopping condition is
// met"), and otherwise they run to their end.
//
// UNVERIFIED (Google does not say; re-read 2026-11-24): that a trial the
// service stopped early ends SUCCEEDED with its final measurement chosen
// from the measurements it reported (INFEASIBLE, with an infeasible_reason
// saying so, when it reported none) and that it is not a failed trial
// toward max_failed_trial_count; how many completed trials the median and
// decay curve rules wait for (here 1 and 2); the decay curve regressor's
// kernel and its "very low probability" (here a squared-exponential kernel
// over the scaled (x, objective so far) of each measurement, predicting
// the trial's final objective, and a probability below
// decayStopProbability); that a job whose study was stopped SUCCEEDS when
// any trial did; that max_num_trials stops the study before it would
// create trial max_num_trials + 1 ("If there are more than this many
// trials, stop the study").
//
//   - convex_automated_stopping_spec: "When there are enough completed
//     trials (configured by min_measurement_count), for pending trials with
//     enough measurements and steps, the policy first computes an
//     overestimate of the objective value at max_num_steps according to
//     the slope of the incomplete objective value curve. No prediction can
//     be made if the curve is completely flat. If the overestimation is
//     worse than the best objective value of the completed trials, this
//     pending trial will be early-stopped, but a last measurement will be
//     added to the pending trial with max_num_steps and predicted
//     objective value from the autoregression model." max_step_count
//     defaults to what the completed trials learn (their longest curve),
//     min_step_count to a tenth of it, min_measurement_count to 5 (checks
//     wait for min_measurement_count + 1 completed trials and pending
//     trials of at least min_measurement_count measurements);
//     use_elapsed_duration reads the steps as elapsed seconds; with
//     update_all_stopped_trials each completed trial updates the final
//     measurements of the trials stopped before.
//
// UNVERIFIED for the convex rule (the page names neither): the slope is
// the last segment's, the overestimate never below the trial's best so
// far; the "autoregression model" is an AR(1) of the curve's increments
// (each increment phi times the one before, phi fitted by least squares
// and kept in [0, 1)), summed to max_step_count at the curve's mean step;
// learning_rate_parameter_name is accepted and not read.

import (
	"fmt"
	"math"
	"sort"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// hpPollInterval is how often a running trial's reports are read.
var hpPollInterval = time.Second

// decayStopProbability is the decay curve rule's "very low probability"
// of exceeding the best value so far, below which a trial stops.
const decayStopProbability = 0.05

// checkStopping validates a study spec's stopping fields.
func checkStopping(field string, ss *aiplatformpb.StudySpec) error {
	if r, ok := ss.GetAutomatedStoppingSpec().(*aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec_); ok {
		c := r.ConvexAutomatedStoppingSpec
		f := field + ".convex_automated_stopping_spec"
		switch {
		case c.GetMaxStepCount() < 0 || c.GetMinStepCount() < 0 || c.GetMinMeasurementCount() < 0:
			return apierror.InvalidArgument("%s's max_step_count, min_step_count and min_measurement_count must not be negative", f)
		case c.GetMaxStepCount() > 0 && c.GetMinStepCount() > c.GetMaxStepCount():
			return apierror.InvalidArgument("%s.min_step_count (%d) must not be more than max_step_count (%d)", f, c.GetMinStepCount(), c.GetMaxStepCount())
		}
	}
	c := ss.GetStudyStoppingConfig()
	if c == nil {
		return nil
	}
	f := field + ".study_stopping_config"
	for name, v := range map[string]interface{ GetValue() int32 }{"min_num_trials": c.GetMinNumTrials(), "max_num_trials": c.GetMaxNumTrials(), "max_num_trials_no_progress": c.GetMaxNumTrialsNoProgress()} {
		if v != nil && v.GetValue() < 0 {
			return apierror.InvalidArgument("%s.%s must not be negative", f, name)
		}
	}
	if c.GetMinNumTrials() != nil && c.GetMaxNumTrials() != nil && c.GetMaxNumTrials().GetValue() < c.GetMinNumTrials().GetValue() {
		return apierror.InvalidArgument("%s.max_num_trials (%d) must not be less than min_num_trials (%d)", f, c.GetMaxNumTrials().GetValue(), c.GetMinNumTrials().GetValue())
	}
	if d := c.GetMaxDurationNoProgress(); d != nil && (d.CheckValid() != nil || d.AsDuration() < 0) {
		return apierror.InvalidArgument("%s.max_duration_no_progress must be a non-negative duration", f)
	}
	for name, tc := range map[string]*aiplatformpb.StudyTimeConstraint{"minimum_runtime_constraint": c.GetMinimumRuntimeConstraint(), "maximum_runtime_constraint": c.GetMaximumRuntimeConstraint()} {
		if tc == nil {
			continue
		}
		if d := tc.GetMaxDuration(); d != nil && (d.CheckValid() != nil || d.AsDuration() < 0) {
			return apierror.InvalidArgument("%s.%s.max_duration must be a non-negative duration", f, name)
		}
		if e := tc.GetEndTime(); e != nil && e.CheckValid() != nil {
			return apierror.InvalidArgument("%s.%s.end_time is not a valid time", f, name)
		}
	}
	return nil
}

// curvePoint is one measurement of a trial's curve: x its step (or
// elapsed seconds), y its objective, maximized.
type curvePoint struct{ x, y float64 }

// trialCurve is a trial's measurements as a curve, by x.
func trialCurve(ss *aiplatformpb.StudySpec, ms []*aiplatformpb.Measurement, elapsed bool) []curvePoint {
	var out []curvePoint
	for _, m := range ms {
		y, ok := objective(ss, m)
		if !ok {
			continue
		}
		x := float64(m.GetStepCount())
		if elapsed {
			x = m.GetElapsedDuration().AsDuration().Seconds()
		}
		out = append(out, curvePoint{x, y})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].x < out[j].x })
	return out
}

// sortMeasurements orders measurements by (step_count, elapsed_duration),
// as a Trial's measurements are.
func sortMeasurements(ms []*aiplatformpb.Measurement) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].GetStepCount() != ms[j].GetStepCount() {
			return ms[i].GetStepCount() < ms[j].GetStepCount()
		}
		return ms[i].GetElapsedDuration().AsDuration() < ms[j].GetElapsedDuration().AsDuration()
	})
}

// shouldStopTrial is the automated stopping rule's verdict on a pending
// trial's measurements, given the completed trials' measurements, and why;
// last is the measurement the rule adds to a trial it stops (convex's).
func shouldStopTrial(ss *aiplatformpb.StudySpec, completed [][]*aiplatformpb.Measurement, pending []*aiplatformpb.Measurement) (stop bool, why string, last *aiplatformpb.Measurement) {
	switch r := ss.GetAutomatedStoppingSpec().(type) {
	case *aiplatformpb.StudySpec_MedianAutomatedStoppingSpec_:
		stop, why = medianStop(ss, completed, pending, r.MedianAutomatedStoppingSpec.GetUseElapsedDuration())
	case *aiplatformpb.StudySpec_DecayCurveStoppingSpec:
		stop, why = decayStop(ss, completed, pending, r.DecayCurveStoppingSpec.GetUseElapsedDuration())
	case *aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec_:
		return convexStop(ss, r.ConvexAutomatedStoppingSpec, completed, pending)
	}
	return stop, why, nil
}

// convexMaxSteps is the convex rule's max_step_count: the spec's, or the
// longest curve of the completed trials.
func convexMaxSteps(ss *aiplatformpb.StudySpec, c *aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec, completed [][]*aiplatformpb.Measurement) float64 {
	if c.GetMaxStepCount() > 0 {
		return float64(c.GetMaxStepCount())
	}
	hi := 0.0
	for _, ms := range completed {
		if cv := trialCurve(ss, ms, c.GetUseElapsedDuration()); len(cv) > 0 {
			hi = math.Max(hi, cv[len(cv)-1].x)
		}
	}
	return hi
}

func convexStop(ss *aiplatformpb.StudySpec, c *aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec, completed [][]*aiplatformpb.Measurement, pending []*aiplatformpb.Measurement) (bool, string, *aiplatformpb.Measurement) {
	minMeasurements := int(c.GetMinMeasurementCount())
	if minMeasurements == 0 {
		minMeasurements = 5
	}
	best, done := math.Inf(-1), 0
	for _, ms := range completed {
		if fm := finalMeasurement(ss, ms); fm != nil {
			if y, ok := objective(ss, fm); ok {
				best, done = math.Max(best, y), done+1
			}
		}
	}
	if done < minMeasurements+1 {
		return false, "", nil
	}
	maxX := convexMaxSteps(ss, c, completed)
	minX := float64(c.GetMinStepCount())
	if minX == 0 {
		minX = maxX / 10
	}
	p := trialCurve(ss, pending, c.GetUseElapsedDuration())
	if len(p) < minMeasurements || len(p) < 2 {
		return false, "", nil
	}
	q := p[len(p)-1]
	if q.x < minX || q.x >= maxX {
		return false, "", nil
	}
	flat := true
	for i := 1; i < len(p); i++ {
		flat = flat && p[i].y == p[i-1].y
	}
	prev := p[len(p)-2]
	if flat || q.x == prev.x {
		return false, "", nil
	}
	top := math.Inf(-1)
	for _, pt := range p {
		top = math.Max(top, pt.y)
	}
	over := math.Max(top, q.y+(q.y-prev.y)/(q.x-prev.x)*(maxX-q.x))
	if over >= best {
		return false, "", nil
	}
	pred := convexPredict(p, maxX)
	m := &aiplatformpb.Measurement{Metrics: []*aiplatformpb.Measurement_Metric{{MetricId: ss.GetMetrics()[0].GetMetricId(), Value: signed(ss, pred)}}}
	if c.GetUseElapsedDuration() {
		m.ElapsedDuration = durationpb.New(time.Duration(maxX * float64(time.Second)))
	} else {
		m.StepCount = int64(maxX)
	}
	return true, fmt.Sprintf("its objective value, overestimated at %g steps from its curve's slope, is %g, worse than the best value %g of the completed trials (convex automated stopping)",
		maxX, signed(ss, over), signed(ss, best)), m
}

// convexPredict is the curve's objective at maxX by an AR(1) of its
// increments, summed at the curve's mean step.
func convexPredict(p []curvePoint, maxX float64) float64 {
	q := p[len(p)-1]
	var d []float64
	for i := 1; i < len(p); i++ {
		d = append(d, p[i].y-p[i-1].y)
	}
	h := (q.x - p[0].x) / float64(len(p)-1)
	steps := 0.0
	if h > 0 {
		steps = (maxX - q.x) / h
	}
	num, den := 0.0, 0.0
	for i := 1; i < len(d); i++ {
		num += d[i] * d[i-1]
		den += d[i-1] * d[i-1]
	}
	phi := 0.0
	if den > 0 {
		phi = math.Max(0, math.Min(0.999, num/den))
	}
	last := d[len(d)-1]
	if phi == 0 || steps <= 0 {
		return q.y
	}
	return q.y + last*phi*(1-math.Pow(phi, steps))/(1-phi)
}

func medianStop(ss *aiplatformpb.StudySpec, completed [][]*aiplatformpb.Measurement, pending []*aiplatformpb.Measurement, elapsed bool) (bool, string) {
	p := trialCurve(ss, pending, elapsed)
	if len(p) == 0 {
		return false, ""
	}
	last := p[len(p)-1].x
	best := math.Inf(-1)
	for _, pt := range p {
		best = math.Max(best, pt.y)
	}
	var perf []float64
	for _, ms := range completed {
		sum, n := 0.0, 0
		for _, pt := range trialCurve(ss, ms, elapsed) {
			if pt.x <= last {
				sum += pt.y
				n++
			}
		}
		if n > 0 {
			perf = append(perf, sum/float64(n))
		}
	}
	if len(perf) == 0 {
		return false, ""
	}
	sort.Float64s(perf)
	med := perf[len(perf)/2]
	if len(perf)%2 == 0 {
		med = (perf[len(perf)/2-1] + perf[len(perf)/2]) / 2
	}
	if best < med {
		return true, fmt.Sprintf("its best objective value %g is below the median performance %g of the %d completed trials up to its last measurement (median automated stopping)",
			signed(ss, best), signed(ss, med), len(perf))
	}
	return false, ""
}

// signed is a maximized objective in the metric's own sign.
func signed(ss *aiplatformpb.StudySpec, y float64) float64 {
	if ss.GetMetrics()[0].GetGoal() == aiplatformpb.StudySpec_MetricSpec_MINIMIZE {
		return -y
	}
	return y
}

// decayStop fits a Gaussian process to (x, objective at x) -> final
// objective over the completed trials' measurements, and predicts the
// pending trial's final objective from its last measurement.
func decayStop(ss *aiplatformpb.StudySpec, completed [][]*aiplatformpb.Measurement, pending []*aiplatformpb.Measurement, elapsed bool) (bool, string) {
	p := trialCurve(ss, pending, elapsed)
	if len(p) == 0 {
		return false, ""
	}
	type sample struct{ x, y, final float64 }
	var data []sample
	best := math.Inf(-1)
	trials := 0
	for _, ms := range completed {
		c := trialCurve(ss, ms, elapsed)
		fm := finalMeasurement(ss, ms)
		if len(c) == 0 || fm == nil {
			continue
		}
		final, ok := objective(ss, fm)
		if !ok {
			continue
		}
		trials++
		best = math.Max(best, final)
		for _, pt := range c {
			data = append(data, sample{pt.x, pt.y, final})
		}
	}
	if trials < 2 {
		return false, ""
	}
	// Scale x and y to [0, 1] over the data and the query, and centre the
	// targets.
	q := p[len(p)-1]
	xlo, xhi, ylo, yhi := q.x, q.x, q.y, q.y
	mean := 0.0
	for _, d := range data {
		xlo, xhi, ylo, yhi = math.Min(xlo, d.x), math.Max(xhi, d.x), math.Min(ylo, math.Min(d.y, d.final)), math.Max(yhi, math.Max(d.y, d.final))
		mean += d.final
	}
	mean /= float64(len(data))
	sx, sy := xhi-xlo, yhi-ylo
	if sx == 0 {
		sx = 1
	}
	if sy == 0 {
		sy = 1
	}
	const ls, noise = 0.3, 1e-4
	k := func(ax, ay, bx, by float64) float64 {
		dx, dy := (ax-bx)/sx, (ay-by)/sy
		return math.Exp(-(dx*dx + dy*dy) / (2 * ls * ls))
	}
	n := len(data)
	K := make([][]float64, n)
	yv := make([]float64, n)
	ks := make([]float64, n)
	for i := range data {
		K[i] = make([]float64, n)
		for j := range data {
			K[i][j] = k(data[i].x, data[i].y, data[j].x, data[j].y)
		}
		K[i][i] += noise
		yv[i] = (data[i].final - mean) / sy
		ks[i] = k(data[i].x, data[i].y, q.x, q.y)
	}
	L, ok := cholesky(K)
	if !ok {
		return false, ""
	}
	alpha := cholSolve(L, yv)
	mu := 0.0
	for i := range ks {
		mu += ks[i] * alpha[i]
	}
	v := forwardSub(L, ks)
	vv := 0.0
	for _, x := range v {
		vv += x * x
	}
	sd := math.Sqrt(math.Max(1+noise-vv, 1e-12))
	mu = mu*sy + mean
	sd *= sy
	prob := 1 - normCDF((best-mu)/sd)
	if prob < decayStopProbability {
		return true, fmt.Sprintf("the decay curve predicts its final objective value %g, with probability %.3g of exceeding the best value %g so far (decay curve automated stopping)",
			signed(ss, mu), prob, signed(ss, best))
	}
	return false, ""
}

// studyProgress is what the study stopping rules read.
type studyProgress struct {
	start     time.Time
	now       time.Time
	completed int // trials SUCCEEDED
	trials    int // trials created, with the one about to be
	// sinceBest is the number of trials ended since the objective last
	// improved; bestAt when it did (the start before any).
	sinceBest int
	bestAt    time.Time
}

func constraintPassed(tc *aiplatformpb.StudyTimeConstraint, p studyProgress) bool {
	if d := tc.GetMaxDuration(); d != nil {
		return p.now.Sub(p.start) >= d.AsDuration()
	}
	if e := tc.GetEndTime(); e != nil {
		return !p.now.Before(e.AsTime())
	}
	return false
}

// studyShouldStop applies study_stopping_config's rules top to bottom.
func studyShouldStop(ss *aiplatformpb.StudySpec, p studyProgress) (bool, string) {
	c := ss.GetStudyStoppingConfig()
	if c == nil {
		return false, ""
	}
	if tc := c.GetMinimumRuntimeConstraint(); tc != nil && !constraintPassed(tc, p) {
		return false, ""
	}
	if v := c.GetMinNumTrials(); v != nil && p.completed < int(v.GetValue()) {
		return false, ""
	}
	if tc := c.GetMaximumRuntimeConstraint(); tc != nil && constraintPassed(tc, p) {
		return true, "the study's maximum_runtime_constraint has passed"
	}
	if v := c.GetMaxNumTrials(); v != nil && p.trials > int(v.GetValue()) {
		return true, fmt.Sprintf("another trial would make more than max_num_trials (%d)", v.GetValue())
	}
	if len(ss.GetMetrics()) == 1 {
		if v := c.GetMaxNumTrialsNoProgress(); v != nil && v.GetValue() > 0 && p.sinceBest >= int(v.GetValue()) {
			return true, fmt.Sprintf("the objective has not improved for %d consecutive trials (max_num_trials_no_progress)", p.sinceBest)
		}
		if d := c.GetMaxDurationNoProgress(); d != nil && d.AsDuration() > 0 && p.now.Sub(p.bestAt) >= d.AsDuration() {
			return true, "the objective has not improved for max_duration_no_progress"
		}
	}
	return false, ""
}

// updateConvexStopped predicts trial id's last measurement again from
// before, what it reported, and makes it the trial's final measurement.
func (s *Server) updateConvexStopped(e *hpJobEntry, ss *aiplatformpb.StudySpec, c *aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec, completed [][]*aiplatformpb.Measurement, id string, before []*aiplatformpb.Measurement) {
	p := trialCurve(ss, before, c.GetUseElapsedDuration())
	maxX := convexMaxSteps(ss, c, completed)
	if len(p) < 2 || maxX <= p[len(p)-1].x {
		return
	}
	m := &aiplatformpb.Measurement{Metrics: []*aiplatformpb.Measurement_Metric{{MetricId: ss.GetMetrics()[0].GetMetricId(), Value: signed(ss, convexPredict(p, maxX))}}}
	if c.GetUseElapsedDuration() {
		m.ElapsedDuration = durationpb.New(time.Duration(maxX * float64(time.Second)))
	} else {
		m.StepCount = int64(maxX)
	}
	ms := append(append([]*aiplatformpb.Measurement(nil), before...), m)
	s.notebooks().setHPJob(e, func(j *aiplatformpb.HyperparameterTuningJob) {
		for _, t := range j.GetTrials() {
			if t.GetId() == id && t.GetState() == aiplatformpb.Trial_SUCCEEDED {
				t.Measurements, t.FinalMeasurement = ms, finalMeasurement(ss, ms)
			}
		}
	})
}
