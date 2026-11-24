package vertexai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
)

// nasSpace is a PyGlove search space as to_json_str writes it: a model
// with a oneof of filters, a oneof of block kinds (one with a nested
// oneof), and a float.
const nasSpace = `{"_type": "model.Model", "filters": {"_type": "hyper.OneOf", "candidates": [16, 32, 64]},
 "block": {"_type": "hyper.OneOf", "candidates": ["plain", {"_type": "model.Residual", "depth": {"_type": "hyper.OneOf", "candidates": [1, 2]}}]},
 "dropout": {"_type": "hyper.Float", "min_value": 0.0, "max_value": 0.5}}`

// nasRunner is a trainer: its reward is filters/64 (+0.1 for a residual
// block), reported to CLOUD_ML_HP_METRIC_FILE under CLOUD_ML_HP_METRIC_TAG.
type nasRunner struct {
	*fakePipelineRunner
	mu   sync.Mutex
	args [][]string
	envs []map[string]string
	fail bool
}

func (r *nasRunner) RunContainer(_ context.Context, c PipelineContainer) error {
	r.mu.Lock()
	r.args = append(r.args, append([]string(nil), c.Args...))
	r.envs = append(r.envs, c.Env)
	r.mu.Unlock()
	if r.fail {
		return os.ErrInvalid
	}
	var params map[string]any
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, "--nas_params_str="); ok {
			if err := json.Unmarshal([]byte(v), &params); err != nil {
				return err
			}
		}
	}
	reward := 0.0
	if f, ok := params["filters"].(string); ok {
		n, _ := strconv.Atoi(f[strings.Index(f, "(")+1 : len(f)-1])
		reward = float64(n) / 64
	}
	if b, _ := params["block"].(string); strings.HasPrefix(b, "1/2") {
		reward += 0.1
	}
	dir := c.Mounts["/tmp/hypertune"]
	if dir == "" {
		return nil
	}
	line, _ := json.Marshal(map[string]any{"timestamp": float64(time.Now().UnixNano()) / 1e9, c.Env["CLOUD_ML_HP_METRIC_TAG"]: strconv.FormatFloat(reward, 'g', -1, 64), "global_step": "1"})
	return os.WriteFile(filepath.Join(dir, "output.metrics"), append(line, '\n'), 0o644)
}

func nasJob(algo aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MultiTrialAlgorithm, trials, parallel int32, space string) *aiplatformpb.NasJob {
	return &aiplatformpb.NasJob{DisplayName: "nas", NasJobSpec: &aiplatformpb.NasJobSpec{SearchSpaceSpec: space,
		NasAlgorithmSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_{MultiTrialAlgorithmSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec{
			MultiTrialAlgorithm: algo,
			Metric:              &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec{MetricId: "top_1_accuracy", Goal: aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MAXIMIZE},
			SearchTrialSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_SearchTrialSpec{MaxTrialCount: trials, MaxParallelTrialCount: parallel,
				SearchTrialJobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool("--epochs=1")},
					BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://b/nas"}}},
		}}}}
}

func waitNAS(t *testing.T, j *jobService, name string) *aiplatformpb.NasJob {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got, err := j.s.waitNASJob(ctx, name)
	if err != nil {
		t.Fatalf("NAS job %s did not end: %v", name, err)
	}
	return got
}

func newNASServer(t *testing.T) (*Server, *nasRunner) {
	t.Helper()
	s, base := newPipelineServer(t)
	r := &nasRunner{fakePipelineRunner: base}
	s.SetPipelineRunner(r)
	return s, r
}

func TestSearchSpaceParameters(t *testing.T) {
	space, err := parseSearchSpace(nasSpace)
	if err != nil {
		t.Fatal(err)
	}
	archs, err := gridArchs(space[0:1], 100) // block: plain, or residual of depth 1 or 2
	if err != nil || len(archs) != 3 {
		t.Fatalf("grid = %d, %v", len(archs), err)
	}
	if got := archs[2].params(); !strings.HasPrefix(got, `{"block": "1/2 (Residual(depth=`) || !strings.HasSuffix(got, `, "block[=1/2].depth": "1/2 (2)"}`) {
		t.Fatalf("params = %s", got)
	}
	if _, err := gridArchs(space, 100); err == nil {
		t.Fatal("a grid over a float was accepted")
	}
	if got := archFromParams(space, archs[2].params()); got != nil {
		t.Fatalf("a partial architecture was read back: %v", got)
	}
	if got := archFromParams(space[0:1], archs[2].params()); len(got) != 2 || got[1].picks[0] != 1 {
		t.Fatalf("architecture read back = %v", got)
	}
	for _, bad := range []string{`not json`, `{"a": 1}`, `{"x": {"_type": "hyper.OneOf", "candidates": []}}`,
		`{"x": {"_type": "hyper.Float", "min_value": 1, "max_value": 1}}`, `{"x": {"_type": "hyper.CustomHyper"}}`} {
		if _, err := parseSearchSpace(bad); err == nil {
			t.Errorf("search space %s was accepted", bad)
		}
	}
}

func TestNASJobValidation(t *testing.T) {
	s, _ := newNASServer(t)
	j := &jobService{s: s}
	parent := "projects/p/locations/us-central1"
	for name, mod := range map[string]func(*aiplatformpb.NasJob){
		"no display name": func(n *aiplatformpb.NasJob) { n.DisplayName = "" },
		"no spec":         func(n *aiplatformpb.NasJob) { n.NasJobSpec = nil },
		"no search space": func(n *aiplatformpb.NasJob) { n.NasJobSpec.SearchSpaceSpec = "" },
		"no metric":       func(n *aiplatformpb.NasJob) { n.NasJobSpec.GetMultiTrialAlgorithmSpec().Metric.MetricId = "" },
		"no trials":       func(n *aiplatformpb.NasJob) { n.NasJobSpec.GetMultiTrialAlgorithmSpec().SearchTrialSpec.MaxTrialCount = 0 },
		"no trial spec":   func(n *aiplatformpb.NasJob) { n.NasJobSpec.GetMultiTrialAlgorithmSpec().SearchTrialSpec.SearchTrialJobSpec = nil },
		"failed above max": func(n *aiplatformpb.NasJob) {
			n.NasJobSpec.GetMultiTrialAlgorithmSpec().SearchTrialSpec.MaxFailedTrialCount = 99
		},
		"train with no frequency": func(n *aiplatformpb.NasJob) {
			n.NasJobSpec.GetMultiTrialAlgorithmSpec().TrainTrialSpec = &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_TrainTrialSpec{
				TrainTrialJobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool()}}, MaxParallelTrialCount: 1}
		},
		"grid over a float": func(n *aiplatformpb.NasJob) {
			n.NasJobSpec.GetMultiTrialAlgorithmSpec().MultiTrialAlgorithm = aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH
		},
		"resume of a missing job": func(n *aiplatformpb.NasJob) { n.NasJobSpec.ResumeNasJobId = "404" },
	} {
		n := nasJob(0, 4, 2, nasSpace)
		mod(n)
		if _, err := j.CreateNasJob(context.Background(), &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: n}); codeOf(err) != codes.InvalidArgument && codeOf(err) != codes.NotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNASJobsSearchTrainAndResume(t *testing.T) {
	ctx := context.Background()
	s, r := newNASServer(t)
	j := &jobService{s: s}
	parent := "projects/p/locations/us-central1"
	n := nasJob(0, 12, 3, nasSpace)
	n.NasJobSpec.GetMultiTrialAlgorithmSpec().TrainTrialSpec = &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_TrainTrialSpec{
		TrainTrialJobSpec:     &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool("--train")}, BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://b/nas"}},
		MaxParallelTrialCount: 2, Frequency: 6}
	created, err := j.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: n})
	if err != nil {
		t.Fatal(err)
	}
	got := waitNAS(t, j, created.GetName())
	out := got.GetNasJobOutput().GetMultiTrialJobOutput()
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(out.GetSearchTrials()) != 12 {
		t.Fatalf("job %v %v, %d search trials", got.GetState(), got.GetError(), len(out.GetSearchTrials()))
	}
	// Two trainings of the best two, after 6 and 12 search trials.
	if len(out.GetTrainTrials()) != 4 {
		t.Fatalf("%d train trials, want 4", len(out.GetTrainTrials()))
	}
	for _, tr := range append(out.GetSearchTrials(), out.GetTrainTrials()...) {
		if tr.GetState() != aiplatformpb.NasTrial_SUCCEEDED || len(tr.GetFinalMeasurement().GetMetrics()) != 1 {
			t.Fatalf("trial %v", tr)
		}
	}
	r.mu.Lock()
	sawJobDir, sawRetrain := false, false
	for i, args := range r.args {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--nas_params_str={") {
			t.Errorf("trial args %q", args)
		}
		if strings.Contains(joined, "--job-dir=gs://b/nas/nas/search/"+r.envs[i]["CLOUD_ML_TRIAL_ID"]) {
			sawJobDir = true
		}
		if strings.Contains(joined, "--retrain_search_job_dir=gs://b/nas/nas/search") && strings.Contains(joined, "--retrain_search_job_trials=") {
			sawRetrain = true
		}
		if r.envs[i]["CLOUD_ML_HP_METRIC_TAG"] != "top_1_accuracy" {
			t.Errorf("trial env %v", r.envs[i])
		}
	}
	r.mu.Unlock()
	if !sawJobDir || !sawRetrain {
		t.Errorf("job dir %v, retrain args %v", sawJobDir, sawRetrain)
	}
	details, err := j.ListNasTrialDetails(ctx, &aiplatformpb.ListNasTrialDetailsRequest{Parent: created.GetName()})
	if err != nil || len(details.GetNasTrialDetails()) != 12 {
		t.Fatalf("details %v, %v", details, err)
	}
	withTrain := 0
	for _, d := range details.GetNasTrialDetails() {
		if !strings.HasPrefix(d.GetParameters(), "{") || d.GetSearchTrial() == nil {
			t.Errorf("detail %v", d)
		}
		if d.GetTrainTrial() != nil {
			withTrain++
		}
	}
	if withTrain != 4 {
		t.Errorf("%d details with a train trial, want 4", withTrain)
	}
	d, err := j.GetNasTrialDetail(ctx, &aiplatformpb.GetNasTrialDetailRequest{Name: created.GetName() + "/nasTrialDetails/1"})
	if err != nil || d.GetSearchTrial().GetId() != "1" {
		t.Fatalf("detail 1 = %v, %v", d, err)
	}
	if _, err := j.GetNasTrialDetail(ctx, &aiplatformpb.GetNasTrialDetailRequest{Name: created.GetName() + "/nasTrialDetails/99"}); codeOf(err) != codes.NotFound {
		t.Errorf("detail 99 = %v", err)
	}
	// Resume: the search space and algorithm come from the first job; a
	// resumed job cannot be resumed again, and a job is resumed once.
	resume := &aiplatformpb.NasJob{DisplayName: "nas-resume", NasJobSpec: &aiplatformpb.NasJobSpec{ResumeNasJobId: lastSegment(created.GetName())}}
	rj, err := j.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: resume})
	if err != nil {
		t.Fatal(err)
	}
	rgot := waitNAS(t, j, rj.GetName())
	if rgot.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || rgot.GetNasJobSpec().GetSearchSpaceSpec() != nasSpace {
		t.Fatalf("resumed job %v %v", rgot.GetState(), rgot.GetError())
	}
	if _, err := j.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: resume}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("a second resume = %v", err)
	}
	again := &aiplatformpb.NasJob{DisplayName: "x", NasJobSpec: &aiplatformpb.NasJobSpec{ResumeNasJobId: lastSegment(rj.GetName())}}
	if _, err := j.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: again}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("resuming a resume = %v", err)
	}
	list, err := j.ListNasJobs(ctx, &aiplatformpb.ListNasJobsRequest{Parent: parent})
	if err != nil || len(list.GetNasJobs()) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := j.CancelNasJob(ctx, &aiplatformpb.CancelNasJobRequest{Name: created.GetName()}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("cancel of an ended job = %v", err)
	}
	if _, err := j.DeleteNasJob(ctx, &aiplatformpb.DeleteNasJobRequest{Name: created.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.GetNasJob(ctx, &aiplatformpb.GetNasJobRequest{Name: created.GetName()}); codeOf(err) != codes.NotFound {
		t.Errorf("get after delete = %v", err)
	}
}

func TestNASGridSearchAndFailures(t *testing.T) {
	ctx := context.Background()
	s, r := newNASServer(t)
	j := &jobService{s: s}
	parent := "projects/p/locations/us-central1"
	grid := `{"filters": {"_type": "hyper.OneOf", "candidates": [16, 32, 64]}, "act": {"_type": "hyper.ManyOf", "num_choices": 2, "candidates": ["relu", "gelu"], "choices_sorted": true}}`
	created, err := j.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: nasJob(aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH, 10, 2, grid)})
	if err != nil {
		t.Fatal(err)
	}
	got := waitNAS(t, j, created.GetName())
	// 3 filters × 1 sorted pair of distinct activations: the grid ends at 3.
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetNasJobOutput().GetMultiTrialJobOutput().GetSearchTrials()) != 3 {
		t.Fatalf("grid job %v %v %d", got.GetState(), got.GetError(), len(got.GetNasJobOutput().GetMultiTrialJobOutput().GetSearchTrials()))
	}
	r.fail = true
	failing := nasJob(0, 6, 1, nasSpace)
	failing.NasJobSpec.GetMultiTrialAlgorithmSpec().SearchTrialSpec.MaxFailedTrialCount = 2
	created, err = j.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: failing})
	if err != nil {
		t.Fatal(err)
	}
	got = waitNAS(t, j, created.GetName())
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_FAILED || len(got.GetNasJobOutput().GetMultiTrialJobOutput().GetSearchTrials()) != 2 {
		t.Fatalf("failing job %v, %d trials", got.GetState(), len(got.GetNasJobOutput().GetMultiTrialJobOutput().GetSearchTrials()))
	}
}
