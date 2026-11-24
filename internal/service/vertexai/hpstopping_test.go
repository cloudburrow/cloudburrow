package vertexai

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func lossSpec() *aiplatformpb.StudySpec {
	return &aiplatformpb.StudySpec{Metrics: []*aiplatformpb.StudySpec_MetricSpec{{MetricId: "loss", Goal: aiplatformpb.StudySpec_MetricSpec_MINIMIZE}}}
}

func lossCurve(vals ...float64) []*aiplatformpb.Measurement {
	var out []*aiplatformpb.Measurement
	for i, v := range vals {
		out = append(out, &aiplatformpb.Measurement{StepCount: int64(i + 1), ElapsedDuration: durationpb.New(time.Duration(i+1) * time.Second),
			Metrics: []*aiplatformpb.Measurement_Metric{{MetricId: "loss", Value: v}}})
	}
	return out
}

func TestMedianStoppingRule(t *testing.T) {
	ss := lossSpec()
	ss.AutomatedStoppingSpec = &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec_{MedianAutomatedStoppingSpec: &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec{}}
	completed := [][]*aiplatformpb.Measurement{lossCurve(1, 0.5, 0.2), lossCurve(1.25, 0.75, 0.3), lossCurve(2, 1.5, 1)}
	// Up to step 2 the performances (running average of -loss) are -0.75,
	// -1 and -1.75: median -1, so a best loss of 1 (not strictly below)
	// goes on and a best loss of 1.25 stops.
	if stop, _, _ := shouldStopTrial(ss, completed, lossCurve(1.5, 1)); stop {
		t.Error("a trial at the median was stopped")
	}
	if stop, why, _ := shouldStopTrial(ss, completed, lossCurve(1.5, 1.25)); !stop || !strings.Contains(why, "median") {
		t.Errorf("a trial below the median: %v %q", stop, why)
	}
	if stop, _, _ := shouldStopTrial(ss, nil, lossCurve(9)); stop {
		t.Error("stopped with no completed trials")
	}
	// use_elapsed_duration: the completed curves up to 1 s.
	ss.GetMedianAutomatedStoppingSpec().UseElapsedDuration = true
	p := lossCurve(1.5)
	if stop, _, _ := shouldStopTrial(ss, completed, p); !stop {
		t.Error("elapsed: a trial whose 1 s loss 1.5 is above the median 1.25 went on")
	}
}

// The convex rule: once min_measurement_count + 1 trials completed, a
// pending trial whose slope-extrapolated objective at max_step_count is
// still worse than the best stops, with a last measurement at
// max_step_count; a flat curve, a short curve and too few completed
// trials go on.
func TestConvexStoppingRule(t *testing.T) {
	ss := lossSpec()
	cs := &aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec{MinMeasurementCount: 2}
	ss.AutomatedStoppingSpec = &aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec_{ConvexAutomatedStoppingSpec: cs}
	completed := [][]*aiplatformpb.Measurement{lossCurve(1, 0.5, 0.3, 0.2, 0.15, 0.1, 0.08, 0.06, 0.05, 0.04),
		lossCurve(1.2, 0.7, 0.5, 0.4, 0.3, 0.25, 0.2, 0.18, 0.16, 0.15), lossCurve(2, 1.5, 1.2, 1, 0.9, 0.8, 0.75, 0.7, 0.68, 0.66)}
	// 3, 2.9: the slope -0.1 a step reaches 2.1 at step 10, worse than 0.04.
	stop, why, last := shouldStopTrial(ss, completed, lossCurve(3, 2.9))
	if !stop || !strings.Contains(why, "convex") || last == nil || last.GetStepCount() != 10 {
		t.Fatalf("a trial far from the best: %v %q %v", stop, why, last)
	}
	if v := last.GetMetrics()[0].GetValue(); v > 2.9 || v < 2.0 {
		t.Errorf("the predicted loss at step 10 is %g", v)
	}
	// 3, 1: the slope reaches below the best at step 10.
	if stop, _, _ := shouldStopTrial(ss, completed, lossCurve(3, 1)); stop {
		t.Error("a trial whose overestimate beats the best was stopped")
	}
	if stop, _, _ := shouldStopTrial(ss, completed, lossCurve(3, 3, 3)); stop {
		t.Error("a flat curve was stopped")
	}
	if stop, _, _ := shouldStopTrial(ss, completed, lossCurve(3)); stop {
		t.Error("a trial of fewer than min_measurement_count measurements was stopped")
	}
	if stop, _, _ := shouldStopTrial(ss, completed[:2], lossCurve(3, 2.9)); stop {
		t.Error("stopped with min_measurement_count completed trials")
	}
	// min_step_count: not before it.
	cs.MinStepCount = 5
	if stop, _, _ := shouldStopTrial(ss, completed, lossCurve(3, 2.9)); stop {
		t.Error("stopped before min_step_count")
	}
	// The AR(1) of the increments: halving increments sum to their limit.
	p := []curvePoint{{1, 0}, {2, 8}, {3, 12}, {4, 14}}
	if got := convexPredict(p, 1000); math.Abs(got-16) > 1e-6 {
		t.Errorf("convexPredict = %g, want 16", got)
	}
}

func TestDecayCurveStoppingRule(t *testing.T) {
	ss := lossSpec()
	ss.AutomatedStoppingSpec = &aiplatformpb.StudySpec_DecayCurveStoppingSpec{DecayCurveStoppingSpec: &aiplatformpb.StudySpec_DecayCurveAutomatedStoppingSpec{}}
	completed := [][]*aiplatformpb.Measurement{lossCurve(1, 0.5, 0.25, 0.1), lossCurve(1.1, 0.6, 0.3, 0.15), lossCurve(3, 2.6, 2.3, 2.1), lossCurve(3.2, 2.8, 2.5, 2.2)}
	if stop, why, _ := shouldStopTrial(ss, completed, lossCurve(3.1, 2.7)); !stop || !strings.Contains(why, "decay curve") {
		t.Errorf("a trial on the bad curves went on: %q", why)
	}
	if stop, why, _ := shouldStopTrial(ss, completed, lossCurve(1, 0.55)); stop {
		t.Errorf("a trial on the best curve was stopped: %q", why)
	}
	if stop, _, _ := shouldStopTrial(ss, completed[:1], lossCurve(3.1, 2.7)); stop {
		t.Error("stopped with one completed trial")
	}
}

func TestStudyStoppingRules(t *testing.T) {
	ss := lossSpec()
	start := time.Unix(1000, 0)
	p := studyProgress{start: start, now: start.Add(time.Minute), completed: 1, trials: 3, sinceBest: 2, bestAt: start}
	for name, c := range map[string]struct {
		cfg  *aiplatformpb.StudySpec_StudyStoppingConfig
		stop bool
	}{
		"none":                {&aiplatformpb.StudySpec_StudyStoppingConfig{}, false},
		"more than max":       {&aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrials: wrapperspb.Int32(2)}, true},
		"at max":              {&aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrials: wrapperspb.Int32(3)}, false},
		"min trials first":    {&aiplatformpb.StudySpec_StudyStoppingConfig{MinNumTrials: wrapperspb.Int32(2), MaxNumTrials: wrapperspb.Int32(2)}, false},
		"no progress":         {&aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrialsNoProgress: wrapperspb.Int32(2)}, true},
		"some progress":       {&aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrialsNoProgress: wrapperspb.Int32(3)}, false},
		"no progress for 30s": {&aiplatformpb.StudySpec_StudyStoppingConfig{MaxDurationNoProgress: durationpb.New(30 * time.Second)}, true},
		"max runtime":         {&aiplatformpb.StudySpec_StudyStoppingConfig{MaximumRuntimeConstraint: &aiplatformpb.StudyTimeConstraint{Constraint: &aiplatformpb.StudyTimeConstraint_MaxDuration{MaxDuration: durationpb.New(30 * time.Second)}}}, true},
		"min runtime first": {&aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrials: wrapperspb.Int32(1),
			MinimumRuntimeConstraint: &aiplatformpb.StudyTimeConstraint{Constraint: &aiplatformpb.StudyTimeConstraint_MaxDuration{MaxDuration: durationpb.New(time.Hour)}}}, false},
	} {
		ss.StudyStoppingConfig = c.cfg
		if got, why := studyShouldStop(ss, p); got != c.stop {
			t.Errorf("%s: %v %q, want %v", name, got, why, c.stop)
		}
	}
	// Several metrics: the no-progress rules do not apply.
	ss.Metrics = append(ss.Metrics, &aiplatformpb.StudySpec_MetricSpec{MetricId: "acc", Goal: aiplatformpb.StudySpec_MetricSpec_MAXIMIZE})
	ss.StudyStoppingConfig = &aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrialsNoProgress: wrapperspb.Int32(1)}
	if got, _ := studyShouldStop(ss, p); got {
		t.Error("max_num_trials_no_progress stopped a multi-objective study")
	}
}

// stepRunner's containers report loss at steps 1..steps, one every delay,
// as cloudml-hypertune appends them: 0.1 a step for --opt=good, 5 for bad.
type stepRunner struct {
	*fakePipelineRunner
	steps int
	delay time.Duration
}

func (r *stepRunner) RunContainer(ctx context.Context, c PipelineContainer) error {
	loss := 0.1
	for _, a := range c.Args {
		if a == "--opt=bad" {
			loss = 5
		}
	}
	dir := c.Mounts["/tmp/hypertune"]
	for step := 1; step <= r.steps; step++ {
		b, _ := json.Marshal(map[string]any{"timestamp": float64(time.Now().UnixNano()) / 1e9, "trial": c.Env["CLOUD_ML_TRIAL_ID"],
			"loss": strconv.FormatFloat(loss/float64(step), 'g', -1, 64), "global_step": strconv.Itoa(step), "checkpoint_path": ""})
		f, err := os.OpenFile(filepath.Join(dir, "output.metrics"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func optParam(vals ...string) *aiplatformpb.StudySpec_ParameterSpec {
	return &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "opt", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_{
		CategoricalValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec{Values: vals}}}
}

func TestHyperparameterTuningAutomatedStopping(t *testing.T) {
	old := hpPollInterval
	hpPollInterval = 20 * time.Millisecond
	defer func() { hpPollInterval = old }()
	ctx := context.Background()
	parent := "projects/p/locations/us-central1"
	newServer := func(steps int, delay time.Duration) *jobService {
		s, base := newPipelineServer(t)
		s.SetPipelineRunner(&stepRunner{fakePipelineRunner: base, steps: steps, delay: delay})
		return &jobService{s: s}
	}

	t.Run("median", func(t *testing.T) {
		j := newServer(20, 30*time.Millisecond)
		h := hpJob(aiplatformpb.StudySpec_GRID_SEARCH, 2, 1, optParam("good", "bad"))
		h.StudySpec.AutomatedStoppingSpec = &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec_{MedianAutomatedStoppingSpec: &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec{}}
		job, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent, HyperparameterTuningJob: h})
		if err != nil {
			t.Fatal(err)
		}
		got := waitHP(t, j, job.GetName())
		if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 2 {
			t.Fatalf("job %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
		}
		good, bad := got.GetTrials()[0], got.GetTrials()[1]
		if good.GetState() != aiplatformpb.Trial_SUCCEEDED || len(good.GetMeasurements()) != 20 {
			t.Errorf("the good trial: %v, %d measurements", good.GetState(), len(good.GetMeasurements()))
		}
		if bad.GetState() != aiplatformpb.Trial_SUCCEEDED || len(bad.GetMeasurements()) == 0 || len(bad.GetMeasurements()) >= 20 || bad.GetFinalMeasurement() == nil {
			t.Errorf("the bad trial was not stopped early: %v, %d measurements, final %v", bad.GetState(), len(bad.GetMeasurements()), bad.GetFinalMeasurement())
		}
		cj, err := j.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: bad.GetCustomJob()})
		if err != nil || cj.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED {
			t.Errorf("the stopped trial's custom job: %v %v", cj.GetState(), err)
		}
	})

	t.Run("max_num_trials", func(t *testing.T) {
		j := newServer(2, time.Millisecond)
		h := hpJob(aiplatformpb.StudySpec_RANDOM_SEARCH, 6, 1, lrParam(0))
		h.StudySpec.StudyStoppingConfig = &aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrials: wrapperspb.Int32(2)}
		job, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent, HyperparameterTuningJob: h})
		if err != nil {
			t.Fatal(err)
		}
		got := waitHP(t, j, job.GetName())
		if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 2 {
			t.Fatalf("job %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
		}
	})

	t.Run("should_stop_asap", func(t *testing.T) {
		j := newServer(1000, 20*time.Millisecond)
		h := hpJob(aiplatformpb.StudySpec_RANDOM_SEARCH, 4, 2, lrParam(0))
		h.StudySpec.StudyStoppingConfig = &aiplatformpb.StudySpec_StudyStoppingConfig{ShouldStopAsap: wrapperspb.Bool(true),
			MaximumRuntimeConstraint: &aiplatformpb.StudyTimeConstraint{Constraint: &aiplatformpb.StudyTimeConstraint_MaxDuration{MaxDuration: durationpb.New(300 * time.Millisecond)}}}
		job, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent, HyperparameterTuningJob: h})
		if err != nil {
			t.Fatal(err)
		}
		got := waitHP(t, j, job.GetName())
		if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 2 {
			t.Fatalf("job %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
		}
		for _, tr := range got.GetTrials() {
			if tr.GetState() != aiplatformpb.Trial_SUCCEEDED || tr.GetFinalMeasurement() == nil || len(tr.GetMeasurements()) >= 1000 {
				t.Errorf("trial %s: %v, final %v", tr.GetId(), tr.GetState(), tr.GetFinalMeasurement())
			}
		}
	})
}
