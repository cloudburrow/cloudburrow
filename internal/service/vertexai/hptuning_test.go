package vertexai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// hypertuneRunner is a fakePipelineRunner whose containers act as a
// training program with cloudml-hypertune: from its --lr=X and --opt=Y
// arguments it reports loss (lr-0.3)^2 (+1 for opt=sgd) at steps 1 and 2
// to /tmp/hypertune/output.metrics, as the library writes them. An lr
// above failAbove exits 1; noReport writes nothing.
type hypertuneRunner struct {
	*fakePipelineRunner
	failAbove float64
	noReport  bool
	mu        sync.Mutex
	args      [][]string
	envs      []map[string]string
	block     chan struct{}
}

func (r *hypertuneRunner) RunContainer(ctx context.Context, c PipelineContainer) error {
	r.mu.Lock()
	r.args = append(r.args, append([]string(nil), c.Args...))
	r.envs = append(r.envs, c.Env)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lr, opt := 0.0, ""
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, "--lr="); ok {
			lr, _ = strconv.ParseFloat(v, 64)
		}
		if v, ok := strings.CutPrefix(a, "--opt="); ok {
			opt = v
		}
	}
	if r.failAbove > 0 && lr > r.failAbove {
		return errors.New("exit status 1: diverged")
	}
	dir, ok := c.Mounts["/tmp/hypertune"]
	if !ok || r.noReport {
		return nil
	}
	loss := (lr - 0.3) * (lr - 0.3)
	if opt == "sgd" {
		loss++
	}
	var lines []string
	for step, v := range []float64{loss + 0.5, loss} {
		b, _ := json.Marshal(map[string]any{"timestamp": float64(time.Now().UnixNano()) / 1e9, "trial": c.Env["CLOUD_ML_TRIAL_ID"],
			"loss": strconv.FormatFloat(v, 'g', -1, 64), "global_step": strconv.Itoa(step + 1), "checkpoint_path": ""})
		lines = append(lines, string(b))
	}
	return os.WriteFile(filepath.Join(dir, "output.metrics"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func newHPServer(t *testing.T) (*Server, *hypertuneRunner) {
	t.Helper()
	s, base := newPipelineServer(t)
	r := &hypertuneRunner{fakePipelineRunner: base}
	s.SetPipelineRunner(r)
	return s, r
}

func waitHP(t *testing.T, j *jobService, name string) *aiplatformpb.HyperparameterTuningJob {
	t.Helper()
	for i := 0; i < 1000; i++ {
		got, err := j.GetHyperparameterTuningJob(context.Background(), &aiplatformpb.GetHyperparameterTuningJobRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if !jobRunning(got.GetState()) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hyperparameter tuning job %s did not end", name)
	return nil
}

func lrParam(scale aiplatformpb.StudySpec_ParameterSpec_ScaleType) *aiplatformpb.StudySpec_ParameterSpec {
	return &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "lr", ScaleType: scale, ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_{
		DoubleValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec{MinValue: 0.01, MaxValue: 1}}}
}

func hpJob(algo aiplatformpb.StudySpec_Algorithm, trials, parallel int32, params ...*aiplatformpb.StudySpec_ParameterSpec) *aiplatformpb.HyperparameterTuningJob {
	return &aiplatformpb.HyperparameterTuningJob{DisplayName: "tune", MaxTrialCount: trials, ParallelTrialCount: parallel,
		StudySpec: &aiplatformpb.StudySpec{Algorithm: algo, Parameters: params,
			Metrics: []*aiplatformpb.StudySpec_MetricSpec{{MetricId: "loss", Goal: aiplatformpb.StudySpec_MetricSpec_MINIMIZE}}},
		TrialJobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool("--epochs=3")},
			BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://b/out"}}}
}

func TestHyperparameterTuningValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newHPServer(t)
	j := &jobService{s: s}
	parent := "projects/p/locations/us-central1"
	cat := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "opt", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_{
		CategoricalValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec{Values: []string{"adam", "sgd"}}}}
	for name, c := range map[string]struct {
		job  func(*aiplatformpb.HyperparameterTuningJob)
		code codes.Code
	}{
		"no display name":  {func(h *aiplatformpb.HyperparameterTuningJob) { h.DisplayName = "" }, codes.InvalidArgument},
		"no trials":        {func(h *aiplatformpb.HyperparameterTuningJob) { h.MaxTrialCount = 0 }, codes.InvalidArgument},
		"no parallel":      {func(h *aiplatformpb.HyperparameterTuningJob) { h.ParallelTrialCount = 0 }, codes.InvalidArgument},
		"failed above max": {func(h *aiplatformpb.HyperparameterTuningJob) { h.MaxFailedTrialCount = 9 }, codes.InvalidArgument},
		"no metrics":       {func(h *aiplatformpb.HyperparameterTuningJob) { h.StudySpec.Metrics = nil }, codes.InvalidArgument},
		"no goal":          {func(h *aiplatformpb.HyperparameterTuningJob) { h.StudySpec.Metrics[0].Goal = 0 }, codes.InvalidArgument},
		"no parameters":    {func(h *aiplatformpb.HyperparameterTuningJob) { h.StudySpec.Parameters = nil }, codes.InvalidArgument},
		"grid with a double": {func(h *aiplatformpb.HyperparameterTuningJob) {
			h.StudySpec.Algorithm = aiplatformpb.StudySpec_GRID_SEARCH
		}, codes.InvalidArgument},
		"duplicate parameter": {func(h *aiplatformpb.HyperparameterTuningJob) {
			h.StudySpec.Parameters = append(h.StudySpec.Parameters, lrParam(0))
		}, codes.InvalidArgument},
		"log scale from 0": {func(h *aiplatformpb.HyperparameterTuningJob) {
			h.StudySpec.Parameters[0].GetDoubleValueSpec().MinValue = 0
			h.StudySpec.Parameters[0].ScaleType = aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE
		}, codes.InvalidArgument},
		"scaled categorical": {func(h *aiplatformpb.HyperparameterTuningJob) {
			c := proto.Clone(cat).(*aiplatformpb.StudySpec_ParameterSpec)
			c.ScaleType = aiplatformpb.StudySpec_ParameterSpec_UNIT_LINEAR_SCALE
			h.StudySpec.Parameters = append(h.StudySpec.Parameters, c)
		}, codes.InvalidArgument},
		"no trial job spec": {func(h *aiplatformpb.HyperparameterTuningJob) { h.TrialJobSpec = nil }, codes.InvalidArgument},
		"early stopping": {func(h *aiplatformpb.HyperparameterTuningJob) {
			h.StudySpec.AutomatedStoppingSpec = &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec_{MedianAutomatedStoppingSpec: &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec{}}
		}, codes.Unimplemented},
	} {
		h := hpJob(0, 4, 2, lrParam(0))
		c.job(h)
		if _, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent, HyperparameterTuningJob: h}); codeOf(err) != c.code {
			t.Errorf("%s: %v, want %v", name, err, c.code)
		}
	}
}

func TestHyperparameterTuningJobs(t *testing.T) {
	ctx := context.Background()
	parent := "projects/p/locations/us-central1"
	for _, algo := range []aiplatformpb.StudySpec_Algorithm{aiplatformpb.StudySpec_ALGORITHM_UNSPECIFIED, aiplatformpb.StudySpec_RANDOM_SEARCH} {
		t.Run(algo.String(), func(t *testing.T) {
			s, r := newHPServer(t)
			j := &jobService{s: s}
			h, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
				HyperparameterTuningJob: hpJob(algo, 12, 3, lrParam(aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE))})
			if err != nil {
				t.Fatal(err)
			}
			got := waitHP(t, j, h.GetName())
			if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 12 {
				t.Fatalf("job %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
			}
			best := math.Inf(1)
			for _, tr := range got.GetTrials() {
				if tr.GetState() != aiplatformpb.Trial_SUCCEEDED || !strings.HasPrefix(tr.GetCustomJob(), parent+"/customJobs/") {
					t.Fatalf("trial %v", tr)
				}
				lr := tr.GetParameters()[0].GetValue().GetNumberValue()
				if lr < 0.01 || lr > 1 {
					t.Errorf("lr %v outside its range", lr)
				}
				fm := tr.GetFinalMeasurement()
				// LAST_MEASUREMENT: step 2, loss (lr-0.3)^2.
				if fm.GetStepCount() != 2 || math.Abs(fm.GetMetrics()[0].GetValue()-(lr-0.3)*(lr-0.3)) > 1e-9 || len(tr.GetMeasurements()) != 2 {
					t.Errorf("trial %s measurement %v", tr.GetId(), fm)
				}
				best = math.Min(best, fm.GetMetrics()[0].GetValue())
				cj, err := j.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: tr.GetCustomJob()})
				if err != nil {
					t.Fatal(err)
				}
				if o := cj.GetJobSpec().GetBaseOutputDirectory().GetOutputUriPrefix(); o != "gs://b/out/"+tr.GetId() {
					t.Errorf("trial %s output %q", tr.GetId(), o)
				}
				args := cj.GetJobSpec().GetWorkerPoolSpecs()[0].GetContainerSpec().GetArgs()
				if len(args) != 2 || args[0] != "--epochs=3" || args[1] != "--lr="+strconv.FormatFloat(lr, 'g', -1, 64) {
					t.Errorf("trial args %q", args)
				}
			}
			if algo == aiplatformpb.StudySpec_ALGORITHM_UNSPECIFIED && best > 0.01 {
				t.Errorf("the default search's best loss %v: it did not approach lr=0.3", best)
			}
			r.mu.Lock()
			for _, e := range r.envs {
				if e["CLOUD_ML_TRIAL_ID"] == "" || !strings.HasSuffix(e["AIP_MODEL_DIR"], "/"+e["CLOUD_ML_TRIAL_ID"]+"/model/") {
					t.Errorf("trial env %v", e)
				}
			}
			r.mu.Unlock()
			list, err := j.ListHyperparameterTuningJobs(ctx, &aiplatformpb.ListHyperparameterTuningJobsRequest{Parent: parent})
			if err != nil || len(list.GetHyperparameterTuningJobs()) != 1 {
				t.Fatalf("list %v %v", list, err)
			}
			if _, err := j.DeleteHyperparameterTuningJob(ctx, &aiplatformpb.DeleteHyperparameterTuningJobRequest{Name: h.GetName()}); err != nil {
				t.Fatal(err)
			}
			if _, err := j.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: h.GetName()}); codeOf(err) != codes.NotFound {
				t.Errorf("get after delete %v", err)
			}
		})
	}
}

func TestHyperparameterTuningGridAndConditions(t *testing.T) {
	ctx := context.Background()
	s, _ := newHPServer(t)
	j := &jobService{s: s}
	parent := "projects/p/locations/us-central1"
	opt := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "opt", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_{
		CategoricalValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec{Values: []string{"adam", "sgd"}}},
		ConditionalParameterSpecs: []*aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec{{
			ParentValueCondition: &aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_ParentCategoricalValues{
				ParentCategoricalValues: &aiplatformpb.StudySpec_ParameterSpec_ConditionalParameterSpec_CategoricalValueCondition{Values: []string{"sgd"}}},
			ParameterSpec: &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "momentum", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec_{
				DiscreteValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec{Values: []float64{0.5, 0.9}}}}}}}
	lr := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "lr", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec_{
		DiscreteValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DiscreteValueSpec{Values: []float64{0.1, 0.3}}}}
	// The grid: lr {0.1, 0.3} x (adam | sgd x momentum {0.5, 0.9}) = 6
	// points; 10 trials asked, so the grid is exhausted after 6.
	h, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
		HyperparameterTuningJob: hpJob(aiplatformpb.StudySpec_GRID_SEARCH, 10, 2, lr, opt)})
	if err != nil {
		t.Fatal(err)
	}
	got := waitHP(t, j, h.GetName())
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 6 {
		t.Fatalf("grid job %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
	}
	var seen []string
	for _, tr := range got.GetTrials() {
		var p []string
		for _, x := range tr.GetParameters() {
			p = append(p, fmt.Sprintf("%s=%v", x.GetParameterId(), x.GetValue().AsInterface()))
		}
		seen = append(seen, strings.Join(p, ","))
	}
	want := "lr=0.1,opt=adam|lr=0.1,opt=sgd,momentum=0.5|lr=0.1,opt=sgd,momentum=0.9|lr=0.3,opt=adam|lr=0.3,opt=sgd,momentum=0.5|lr=0.3,opt=sgd,momentum=0.9"
	if strings.Join(seen, "|") != want {
		t.Errorf("grid trials\n%s\nwant\n%s", strings.Join(seen, "|"), want)
	}
	if b := bestTrial(got); b == nil || b.GetParameters()[0].GetValue().GetNumberValue() != 0.3 || b.GetParameters()[1].GetValue().GetStringValue() != "adam" {
		t.Errorf("best trial %v", b)
	}
}

func TestHyperparameterTuningFailures(t *testing.T) {
	ctx := context.Background()
	parent := "projects/p/locations/us-central1"
	t.Run("max failed", func(t *testing.T) {
		s, r := newHPServer(t)
		r.failAbove = 0.0001
		j := &jobService{s: s}
		h := hpJob(aiplatformpb.StudySpec_RANDOM_SEARCH, 10, 1, lrParam(0))
		h.MaxFailedTrialCount = 3
		created, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent, HyperparameterTuningJob: h})
		if err != nil {
			t.Fatal(err)
		}
		got := waitHP(t, j, created.GetName())
		if got.GetState() != aiplatformpb.JobState_JOB_STATE_FAILED || len(got.GetTrials()) != 3 {
			t.Fatalf("job %v, %d trials", got.GetState(), len(got.GetTrials()))
		}
		for _, tr := range got.GetTrials() {
			if tr.GetState() != aiplatformpb.Trial_INFEASIBLE || !strings.Contains(tr.GetInfeasibleReason(), "non-zero status") {
				t.Errorf("trial %v", tr)
			}
		}
	})
	t.Run("no report", func(t *testing.T) {
		s, r := newHPServer(t)
		r.noReport = true
		j := &jobService{s: s}
		created, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
			HyperparameterTuningJob: hpJob(aiplatformpb.StudySpec_RANDOM_SEARCH, 4, 1, lrParam(0))})
		if err != nil {
			t.Fatal(err)
		}
		got := waitHP(t, j, created.GetName())
		// With max_failed_trial_count 0, the first trial's failure ends the job.
		if got.GetState() != aiplatformpb.JobState_JOB_STATE_FAILED || len(got.GetTrials()) != 1 || got.GetTrials()[0].GetState() != aiplatformpb.Trial_INFEASIBLE {
			t.Fatalf("job %v %v", got.GetState(), got.GetTrials())
		}
	})
	t.Run("cancel", func(t *testing.T) {
		s, r := newHPServer(t)
		r.block = make(chan struct{})
		j := &jobService{s: s}
		created, err := j.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
			HyperparameterTuningJob: hpJob(aiplatformpb.StudySpec_RANDOM_SEARCH, 4, 2, lrParam(0))})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 500; i++ {
			g, _ := j.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: created.GetName()})
			if len(g.GetTrials()) == 2 && g.GetTrials()[1].GetCustomJob() != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := j.DeleteHyperparameterTuningJob(ctx, &aiplatformpb.DeleteHyperparameterTuningJobRequest{Name: created.GetName()}); codeOf(err) != codes.FailedPrecondition {
			t.Errorf("delete running %v", err)
		}
		if _, err := j.CancelHyperparameterTuningJob(ctx, &aiplatformpb.CancelHyperparameterTuningJobRequest{Name: created.GetName()}); err != nil {
			t.Fatal(err)
		}
		got := waitHP(t, j, created.GetName())
		if got.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED || got.GetError().GetCode() != int32(codes.Canceled) {
			t.Fatalf("job %v %v", got.GetState(), got.GetError())
		}
		for _, tr := range got.GetTrials() {
			cj, _ := j.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: tr.GetCustomJob()})
			if tr.GetState() != aiplatformpb.Trial_INFEASIBLE || cj.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED {
				t.Errorf("trial %v, its job %v", tr.GetState(), cj.GetState())
			}
		}
	})
}

func TestHyperparameterTuningTrainingPipeline(t *testing.T) {
	ctx := context.Background()
	s, _ := newHPServer(t)
	p := &pipelineService{s: s}
	parent := "projects/p/locations/us-central1"
	inputs, _ := structpb.NewValue(map[string]any{
		"studySpec": map[string]any{"metrics": []any{map[string]any{"metricId": "loss", "goal": "MINIMIZE"}},
			"parameters": []any{map[string]any{"parameterId": "lr", "discreteValueSpec": map[string]any{"values": []any{0.1, 0.3, 0.5}}}},
			"algorithm":  "GRID_SEARCH"},
		"maxTrialCount": 3, "parallelTrialCount": 1,
		"trialJobSpec": map[string]any{"workerPoolSpecs": []any{map[string]any{"machineSpec": map[string]any{"machineType": "n1-standard-4"},
			"replicaCount": "1", "containerSpec": map[string]any{"imageUri": "local/train:1"}}}},
	})
	tp, err := p.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "tuned", TrainingTaskDefinition: hpTaskDefinition, TrainingTaskInputs: inputs}})
	if err != nil {
		t.Fatal(err)
	}
	var got *aiplatformpb.TrainingPipeline
	for i := 0; i < 1000; i++ {
		got, _ = p.GetTrainingPipeline(ctx, &aiplatformpb.GetTrainingPipelineRequest{Name: tp.GetName()})
		if !trainingRunning(got.GetState()) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("pipeline %v %v", got.GetState(), got.GetError())
	}
	md := got.GetTrainingTaskMetadata().GetStructValue().GetFields()
	hp := md["backingHyperparameterTuningJob"].GetStringValue()
	j := &jobService{s: s}
	job, err := j.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: hp})
	if err != nil {
		t.Fatal(err)
	}
	if b := bestTrial(job); b == nil || md["bestTrialBackingCustomJob"].GetStringValue() != b.GetCustomJob() || b.GetParameters()[0].GetValue().GetNumberValue() != 0.3 {
		t.Errorf("best %v, metadata %v", b, md)
	}
	bad, _ := structpb.NewValue(map[string]any{"workerPoolSpecs": []any{}})
	if _, err := p.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "bad", TrainingTaskDefinition: hpTaskDefinition, TrainingTaskInputs: bad}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a custom task's inputs on the tuning task: %v", err)
	}
}

func TestScaledSpace(t *testing.T) {
	logp := lrParam(aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE)
	if v := fromUnit(logp, 0.5).num; math.Abs(v-0.1) > 1e-9 {
		t.Errorf("log scale midpoint %v, want 0.1", v)
	}
	rev := lrParam(aiplatformpb.StudySpec_ParameterSpec_UNIT_REVERSE_LOG_SCALE)
	if lo, hi := fromUnit(rev, 0).num, fromUnit(rev, 1).num; math.Abs(lo-0.01) > 1e-9 || math.Abs(hi-1) > 1e-9 {
		t.Errorf("reverse log ends %v %v", lo, hi)
	}
	for _, u := range []float64{0.1, 0.4, 0.77} {
		for _, p := range []*aiplatformpb.StudySpec_ParameterSpec{logp, rev, lrParam(0)} {
			v := fromUnit(p, u)
			if back := toUnit(p, &v); math.Abs(back-u) > 1e-9 {
				t.Errorf("%v at %v round trips to %v", p.GetScaleType(), u, back)
			}
		}
	}
	ip := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "n", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_IntegerValueSpec_{
		IntegerValueSpec: &aiplatformpb.StudySpec_ParameterSpec_IntegerValueSpec{MinValue: 1, MaxValue: 4}}}
	counts := map[float64]int{}
	for i := 0; i < 400; i++ {
		counts[fromUnit(ip, (float64(i)+0.5)/400).num]++
	}
	if len(counts) != 4 || counts[1] != 100 || counts[4] != 100 {
		t.Errorf("integers' shares %v", counts)
	}
	if a := (hpValue{num: 8}).arg("DISCRETE"); a != "8" {
		t.Errorf("discrete arg %q", a)
	}
}

func TestReadHypertune(t *testing.T) {
	dir := t.TempDir()
	// As cloudml-hypertune writes it, the second with no tag given.
	text := `{"checkpoint_path": "", "global_step": "10", "loss": "0.5", "timestamp": 1700000001.5, "trial": "3"}
{"checkpoint_path": "", "global_step": "20", "loss": "0.25", "timestamp": 1700000003.0, "trial": "3"}
{"checkpoint_path": "", "global_step": "30", "training/hptuning/metric": "0.4", "timestamp": 1700000004.0, "trial": "3"}
`
	if err := os.WriteFile(filepath.Join(dir, "output.metrics"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	ss := &aiplatformpb.StudySpec{Metrics: []*aiplatformpb.StudySpec_MetricSpec{{MetricId: "loss", Goal: aiplatformpb.StudySpec_MetricSpec_MINIMIZE}}}
	ms := readHypertune(dir, ss.GetMetrics(), time.Unix(1700000000, 0))
	if len(ms) != 3 || ms[0].GetStepCount() != 10 || ms[0].GetElapsedDuration().AsDuration() != 1500*time.Millisecond || ms[2].GetMetrics()[0].GetValue() != 0.4 {
		t.Fatalf("measurements %v", ms)
	}
	if f := finalMeasurement(ss, ms); f.GetStepCount() != 30 {
		t.Errorf("LAST_MEASUREMENT %v", f)
	}
	ss.MeasurementSelectionType = aiplatformpb.StudySpec_BEST_MEASUREMENT
	if f := finalMeasurement(ss, ms); f.GetStepCount() != 20 {
		t.Errorf("BEST_MEASUREMENT %v", f)
	}
}
