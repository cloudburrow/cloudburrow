//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// hpTrainScript is a trial's training code in the busybox trainer: it
// reads --lr and --opt, writes its model to AIP_MODEL_DIR, and reports
// loss = (lr-0.3)^2 (+1 for sgd) at steps 1 and 2 as cloudml-hypertune
// writes its reports, JSON lines in /tmp/hypertune/output.metrics.
const hpTrainScript = `lr=0.5; opt=adam
for a in "$@"; do case "$a" in --lr=*) lr=${a#--lr=};; --opt=*) opt=${a#--opt=};; esac; done
loss=$(awk -v l="$lr" -v o="$opt" 'BEGIN { v = (l - 0.3) * (l - 0.3); if (o == "sgd") v += 1; printf "%.12g", v }')
first=$(awk -v v="$loss" 'BEGIN { printf "%.12g", v + 0.5 }')
d=/gcs/${AIP_MODEL_DIR#gs://}; mkdir -p "$d" && echo "$lr $opt" > "$d/params.txt"
mkdir -p /tmp/hypertune
now=$(date +%s)
printf '{"checkpoint_path": "", "global_step": "1", "loss": "%s", "timestamp": %s, "trial": "%s"}\n' "$first" "$now" "$CLOUD_ML_TRIAL_ID" > /tmp/hypertune/output.metrics
printf '{"checkpoint_path": "", "global_step": "2", "loss": "%s", "timestamp": %s, "trial": "%s"}\n' "$loss" "$now" "$CLOUD_ML_TRIAL_ID" >> /tmp/hypertune/output.metrics`

func hpTrainerPool(image string) *aiplatformpb.WorkerPoolSpec {
	return &aiplatformpb.WorkerPoolSpec{MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
		Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: image,
			Command: []string{"sh", "-c", hpTrainScript, "sh"}}}}
}

// Not yet annotated covers: (it has not run); once it passes it covers the
// five JobService HyperparameterTuningJob methods.
//
// TestVertexAIHyperparameterTuning (#2210), with the official Go client:
// a hyperparameter tuning job of the default algorithm over a log-scaled
// DOUBLE lr and a CATEGORICAL opt runs each trial as a CustomJob of the
// trainer (its image in the instance's Artifact Registry), which gets
// --lr=VALUE and --opt=VALUE and CLOUD_ML_TRIAL_ID, writes its model to
// BASE/TRIAL_ID/model/ and reports its loss as cloudml-hypertune does;
// every trial SUCCEEDS with its last report as final_measurement and the
// job SUCCEEDS. A GRID_SEARCH job of a DOUBLE parameter is
// INVALID_ARGUMENT; a job whose trials fail is FAILED with its trials
// INFEASIBLE; a running job is CANCELLED with its trials' custom jobs;
// a hyperparameter tuning training pipeline names its backing job and
// best trial; list and delete.
// covers: google.cloud.aiplatform.v1.JobService/CreateHyperparameterTuningJob, google.cloud.aiplatform.v1.JobService/GetHyperparameterTuningJob, google.cloud.aiplatform.v1.JobService/ListHyperparameterTuningJobs, google.cloud.aiplatform.v1.JobService/CancelHyperparameterTuningJob, google.cloud.aiplatform.v1.JobService/DeleteHyperparameterTuningJob
func TestVertexAIHyperparameterTuning(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	gcs := dataformStorage(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	trainer := pushTrainer(t, ctx, h)
	opts := vertexOpts(endpoint)
	jc, err := aiplatform.NewJobClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	pc, err := aiplatform.NewPipelineClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-hptune-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)

	metrics := []*aiplatformpb.StudySpec_MetricSpec{{MetricId: "loss", Goal: aiplatformpb.StudySpec_MetricSpec_MINIMIZE}}
	lr := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "lr", ScaleType: aiplatformpb.StudySpec_ParameterSpec_UNIT_LOG_SCALE,
		ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_{DoubleValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec{MinValue: 0.01, MaxValue: 1}}}
	opt := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "opt", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_{
		CategoricalValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec{Values: []string{"adam", "sgd"}}}}
	spec := func(prefix string) *aiplatformpb.CustomJobSpec {
		return &aiplatformpb.CustomJobSpec{BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/" + prefix},
			WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{hpTrainerPool(trainer)}}
	}
	wait := func(name string) *aiplatformpb.HyperparameterTuningJob {
		var got *aiplatformpb.HyperparameterTuningJob
		waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
			got, err = jc.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: name})
			return got.GetState(), err
		})
		return got
	}

	// Validation: grid search takes no DOUBLE.
	if _, err := jc.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
		HyperparameterTuningJob: &aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-grid-" + suffix, MaxTrialCount: 2, ParallelTrialCount: 1,
			StudySpec:    &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{lr}, Algorithm: aiplatformpb.StudySpec_GRID_SEARCH},
			TrialJobSpec: spec("grid")}}); grpcCode(err) != codes.InvalidArgument {
		t.Errorf("a grid search of a DOUBLE = %v, want INVALID_ARGUMENT", err)
	}

	// The default algorithm, two trials at a time.
	job, err := jc.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
		HyperparameterTuningJob: &aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-hptune-" + suffix, Labels: map[string]string{"compat": "hp2210-" + suffix},
			MaxTrialCount: 6, ParallelTrialCount: 2,
			StudySpec:    &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{lr, opt}},
			TrialJobSpec: spec("tune")}})
	if err != nil {
		t.Fatalf("CreateHyperparameterTuningJob: %v", err)
	}
	if !strings.HasPrefix(job.GetName(), parent+"/hyperparameterTuningJobs/") || job.GetState() != aiplatformpb.JobState_JOB_STATE_QUEUED {
		t.Errorf("created = %s %v", job.GetName(), job.GetState())
	}
	got := wait(job.GetName())
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 6 {
		t.Fatalf("the job = %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
	}
	for _, tr := range got.GetTrials() {
		vals := map[string]any{}
		for _, p := range tr.GetParameters() {
			vals[p.GetParameterId()] = p.GetValue().AsInterface()
		}
		l, _ := vals["lr"].(float64)
		o, _ := vals["opt"].(string)
		want := (l - 0.3) * (l - 0.3)
		if o == "sgd" {
			want++
		}
		fm := tr.GetFinalMeasurement()
		if tr.GetState() != aiplatformpb.Trial_SUCCEEDED || l < 0.01 || l > 1 || (o != "adam" && o != "sgd") ||
			fm.GetStepCount() != 2 || len(fm.GetMetrics()) != 1 || math.Abs(fm.GetMetrics()[0].GetValue()-want) > 1e-6 || len(tr.GetMeasurements()) != 2 {
			t.Errorf("trial %s = %v %v, final %v", tr.GetId(), tr.GetState(), vals, fm)
		}
		cj, err := jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: tr.GetCustomJob()})
		if err != nil || cj.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
			t.Errorf("trial %s's custom job %s = %v %v", tr.GetId(), tr.GetCustomJob(), cj.GetState(), err)
			continue
		}
		r, err := gcs.Bucket(bucket).Object("tune/" + tr.GetId() + "/model/params.txt").NewReader(ctx)
		if err != nil {
			t.Errorf("trial %s wrote no model to BASE/TRIAL_ID/model: %v", tr.GetId(), err)
			continue
		}
		b, _ := io.ReadAll(r)
		_ = r.Close()
		if f := strings.Fields(string(b)); len(f) != 2 || f[1] != o {
			t.Errorf("trial %s's arguments reached the code as %q, want opt %s", tr.GetId(), b, o)
		} else if v, _ := strconv.ParseFloat(f[0], 64); math.Abs(v-l) > 1e-12 {
			t.Errorf("trial %s's --lr = %s, its parameter %v", tr.GetId(), f[0], l)
		}
	}

	// Failing trials.
	bad := spec("bad")
	bad.WorkerPoolSpecs[0].GetContainerSpec().Command = []string{"sh", "-c", "exit 3"}
	fj, err := jc.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
		HyperparameterTuningJob: &aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-hpfail-" + suffix, MaxTrialCount: 4, ParallelTrialCount: 1,
			MaxFailedTrialCount: 2, StudySpec: &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{opt}, Algorithm: aiplatformpb.StudySpec_RANDOM_SEARCH},
			TrialJobSpec: bad}})
	if err != nil {
		t.Fatal(err)
	}
	failed := wait(fj.GetName())
	if failed.GetState() != aiplatformpb.JobState_JOB_STATE_FAILED || len(failed.GetTrials()) != 2 {
		t.Errorf("a job whose trials fail = %v, %d trials", failed.GetState(), len(failed.GetTrials()))
	}
	for _, tr := range failed.GetTrials() {
		if tr.GetState() != aiplatformpb.Trial_INFEASIBLE || !strings.Contains(tr.GetInfeasibleReason(), "non-zero status of 3") {
			t.Errorf("failed trial %s = %v %q", tr.GetId(), tr.GetState(), tr.GetInfeasibleReason())
		}
	}

	// Cancel.
	slow := spec("slow")
	slow.WorkerPoolSpecs[0].GetContainerSpec().Command = []string{"sh", "-c", "sleep 600"}
	cjob, err := jc.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
		HyperparameterTuningJob: &aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-hpcancel-" + suffix, MaxTrialCount: 2, ParallelTrialCount: 2,
			StudySpec: &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{opt}}, TrialJobSpec: slow}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		g, err := jc.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: cjob.GetName()})
		if err == nil && g.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING && len(g.GetTrials()) == 2 {
			break
		}
	}
	if _, err := jc.DeleteHyperparameterTuningJob(ctx, &aiplatformpb.DeleteHyperparameterTuningJobRequest{Name: cjob.GetName()}); err == nil {
		t.Errorf("deleting a running job succeeded")
	}
	if err := jc.CancelHyperparameterTuningJob(ctx, &aiplatformpb.CancelHyperparameterTuningJobRequest{Name: cjob.GetName()}); err != nil {
		t.Fatalf("CancelHyperparameterTuningJob: %v", err)
	}
	cancelled := wait(cjob.GetName())
	if cancelled.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED || cancelled.GetError().GetCode() != 1 {
		t.Errorf("the cancelled job = %v %v", cancelled.GetState(), cancelled.GetError())
	}
	for _, tr := range cancelled.GetTrials() {
		cj, _ := jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: tr.GetCustomJob()})
		if tr.GetState() != aiplatformpb.Trial_INFEASIBLE || cj.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED {
			t.Errorf("cancelled trial %s = %v, its custom job %v", tr.GetId(), tr.GetState(), cj.GetState())
		}
	}

	// A hyperparameter tuning training pipeline.
	inputs, err := hpTaskStruct(&aiplatformpb.HyperparameterTuningJob{MaxTrialCount: 2, ParallelTrialCount: 2,
		StudySpec:    &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{opt}, Algorithm: aiplatformpb.StudySpec_GRID_SEARCH},
		TrialJobSpec: spec("pipeline")})
	if err != nil {
		t.Fatal(err)
	}
	tp, err := pc.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName:            "compat-hptp-" + suffix,
		TrainingTaskDefinition: "gs://google-cloud-aiplatform/schema/trainingjob/definition/hyperparameter_tuning_task_1.0.0.yaml",
		TrainingTaskInputs:     inputs}})
	if err != nil {
		t.Fatalf("CreateTrainingPipeline: %v", err)
	}
	var gotTP *aiplatformpb.TrainingPipeline
	for deadline := time.Now().Add(10 * time.Minute); ; time.Sleep(time.Second) {
		gotTP, err = pc.GetTrainingPipeline(ctx, &aiplatformpb.GetTrainingPipelineRequest{Name: tp.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if s := gotTP.GetState(); s == aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || s == aiplatformpb.PipelineState_PIPELINE_STATE_FAILED ||
			s == aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED || time.Now().After(deadline) {
			break
		}
	}
	md := gotTP.GetTrainingTaskMetadata().GetStructValue().GetFields()
	if gotTP.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED ||
		!strings.HasPrefix(md["backingHyperparameterTuningJob"].GetStringValue(), parent+"/hyperparameterTuningJobs/") ||
		!strings.HasPrefix(md["bestTrialBackingCustomJob"].GetStringValue(), parent+"/customJobs/") {
		t.Errorf("the tuning pipeline = %v %v, metadata %v", gotTP.GetState(), gotTP.GetError(), md)
	}

	// List (a filter by label) and delete.
	n := 0
	for it := jc.ListHyperparameterTuningJobs(ctx, &aiplatformpb.ListHyperparameterTuningJobsRequest{Parent: parent, Filter: `labels.compat="hp2210-` + suffix + `"`}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if j.GetName() != job.GetName() {
			t.Errorf("the label filter listed %s", j.GetName())
		}
		n++
	}
	if n != 1 {
		t.Errorf("the label filter listed %d jobs, want 1", n)
	}
	op, err := jc.DeleteHyperparameterTuningJob(ctx, &aiplatformpb.DeleteHyperparameterTuningJobRequest{Name: job.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := jc.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: job.GetName()}); grpcCode(err) != codes.NotFound {
		t.Errorf("get after delete = %v", err)
	}
}

// hpStepScript reports loss at steps 1 to 8, one a second, as
// cloudml-hypertune appends its reports: 0.1/step for --opt=good, 5/step
// for --opt=bad.
const hpStepScript = `opt=good
for a in "$@"; do case "$a" in --opt=*) opt=${a#--opt=};; esac; done
base=0.1; [ "$opt" = bad ] && base=5
mkdir -p /tmp/hypertune
for s in 1 2 3 4 5 6 7 8; do
  loss=$(awk -v b="$base" -v s="$s" 'BEGIN { printf "%.12g", b / s }')
  printf '{"checkpoint_path": "", "global_step": "%s", "loss": "%s", "timestamp": %s, "trial": "%s"}\n' "$s" "$loss" "$(date +%s)" "$CLOUD_ML_TRIAL_ID" >> /tmp/hypertune/output.metrics
  sleep 1
done`

// TestVertexAIHyperparameterTuningStopping (#2210), with the official Go
// client, as the StudySpec reference documents automated stopping: with
// median_automated_stopping_spec, a GRID_SEARCH job runs --opt=good to
// its end and then stops --opt=bad early, its best objective below the
// completed trial's: the stopped trial SUCCEEDS with the measurements it
// reported so far (fewer than 8) and its custom job is CANCELLED; with
// study_stopping_config max_num_trials 2, a job of max_trial_count 5 ends
// after 2 trials; with convex_automated_stopping_spec, the bad trial
// stops with a last measurement at max_step_count.
// covers: google.cloud.aiplatform.v1.JobService/CreateHyperparameterTuningJob, google.cloud.aiplatform.v1.JobService/GetHyperparameterTuningJob
func TestVertexAIHyperparameterTuningStopping(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	gcs := dataformStorage(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	trainer := pushTrainer(t, ctx, h)
	jc, err := aiplatform.NewJobClient(ctx, vertexOpts(endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-hpstop-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)
	metrics := []*aiplatformpb.StudySpec_MetricSpec{{MetricId: "loss", Goal: aiplatformpb.StudySpec_MetricSpec_MINIMIZE}}
	opt := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "opt", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_{
		CategoricalValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec{Values: []string{"good", "bad"}}}}
	spec := func(prefix string) *aiplatformpb.CustomJobSpec {
		pool := hpTrainerPool(trainer)
		pool.GetContainerSpec().Command = []string{"sh", "-c", hpStepScript, "sh"}
		return &aiplatformpb.CustomJobSpec{BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/" + prefix},
			WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{pool}}
	}
	run := func(j *aiplatformpb.HyperparameterTuningJob) *aiplatformpb.HyperparameterTuningJob {
		created, err := jc.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent, HyperparameterTuningJob: j})
		if err != nil {
			t.Fatalf("CreateHyperparameterTuningJob %s: %v", j.GetDisplayName(), err)
		}
		var got *aiplatformpb.HyperparameterTuningJob
		waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
			got, err = jc.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: created.GetName()})
			return got.GetState(), err
		})
		return got
	}

	// Convex stopping: two trials of --opt=good and --opt=fine complete
	// (min_measurement_count 1 needs 2), and --opt=bad, whose loss 5/step
	// extrapolated from its slope to step 8 stays worse than 0.1/8, stops
	// with a last measurement at step 8, the completed trials' longest.
	cat := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "opt", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec_{
		CategoricalValueSpec: &aiplatformpb.StudySpec_ParameterSpec_CategoricalValueSpec{Values: []string{"fine", "good", "bad"}}}}
	got := run(&aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-hpconvex-" + suffix, MaxTrialCount: 3, ParallelTrialCount: 1,
		StudySpec: &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{cat}, Algorithm: aiplatformpb.StudySpec_GRID_SEARCH,
			AutomatedStoppingSpec: &aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec_{ConvexAutomatedStoppingSpec: &aiplatformpb.StudySpec_ConvexAutomatedStoppingSpec{MinMeasurementCount: 1}}},
		TrialJobSpec: spec("convex")})
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 3 {
		t.Fatalf("the convex-stopping job = %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
	}
	for _, tr := range got.GetTrials() {
		v := tr.GetParameters()[0].GetValue().GetStringValue()
		ms := tr.GetMeasurements()
		switch {
		case tr.GetState() != aiplatformpb.Trial_SUCCEEDED:
			t.Errorf("convex: trial --opt=%s = %v %s", v, tr.GetState(), tr.GetInfeasibleReason())
		case v != "bad" && len(ms) != 8:
			t.Errorf("convex: trial --opt=%s was stopped: %d measurements", v, len(ms))
		case v == "bad" && (len(ms) < 2 || len(ms) > 8 || ms[len(ms)-1].GetStepCount() != 8 || ms[len(ms)-2].GetStepCount() >= 7 ||
			tr.GetFinalMeasurement().GetStepCount() != 8):
			t.Errorf("convex: trial --opt=bad was not stopped early with a last measurement at step 8: %v, final %v", ms, tr.GetFinalMeasurement())
		}
	}

	// Median stopping.
	got = run(&aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-hpmedian-" + suffix, MaxTrialCount: 2, ParallelTrialCount: 1,
		StudySpec: &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{opt}, Algorithm: aiplatformpb.StudySpec_GRID_SEARCH,
			AutomatedStoppingSpec: &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec_{MedianAutomatedStoppingSpec: &aiplatformpb.StudySpec_MedianAutomatedStoppingSpec{}}},
		TrialJobSpec: spec("median")})
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 2 {
		t.Fatalf("the median-stopping job = %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
	}
	good, bad := got.GetTrials()[0], got.GetTrials()[1]
	if good.GetState() != aiplatformpb.Trial_SUCCEEDED || len(good.GetMeasurements()) != 8 || good.GetFinalMeasurement().GetStepCount() != 8 {
		t.Errorf("the good trial = %v, %d measurements, final %v", good.GetState(), len(good.GetMeasurements()), good.GetFinalMeasurement())
	}
	if bad.GetState() != aiplatformpb.Trial_SUCCEEDED || len(bad.GetMeasurements()) == 0 || len(bad.GetMeasurements()) >= 8 || bad.GetFinalMeasurement() == nil {
		t.Errorf("the bad trial was not stopped early: %v, %d measurements, final %v", bad.GetState(), len(bad.GetMeasurements()), bad.GetFinalMeasurement())
	}
	if cj, err := jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: bad.GetCustomJob()}); err != nil || cj.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("the stopped trial's custom job = %v %v", cj.GetState(), err)
	}

	// The study stops at max_num_trials.
	lr := &aiplatformpb.StudySpec_ParameterSpec{ParameterId: "lr", ScaleType: aiplatformpb.StudySpec_ParameterSpec_UNIT_LINEAR_SCALE,
		ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_{DoubleValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec{MinValue: 0.01, MaxValue: 1}}}
	quick := spec("max")
	quick.WorkerPoolSpecs[0].GetContainerSpec().Command = []string{"sh", "-c", hpTrainScript, "sh"}
	got = run(&aiplatformpb.HyperparameterTuningJob{DisplayName: "compat-hpmax-" + suffix, MaxTrialCount: 5, ParallelTrialCount: 1,
		StudySpec: &aiplatformpb.StudySpec{Metrics: metrics, Parameters: []*aiplatformpb.StudySpec_ParameterSpec{lr}, Algorithm: aiplatformpb.StudySpec_RANDOM_SEARCH,
			StudyStoppingConfig: &aiplatformpb.StudySpec_StudyStoppingConfig{MaxNumTrials: wrapperspb.Int32(2)}},
		TrialJobSpec: quick})
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(got.GetTrials()) != 2 {
		t.Errorf("the max_num_trials job = %v %v, %d trials", got.GetState(), got.GetError(), len(got.GetTrials()))
	}
}

// hpTaskStruct is a HyperparameterTuningJobSpec as training_task_inputs.
func hpTaskStruct(j *aiplatformpb.HyperparameterTuningJob) (*structpb.Value, error) {
	b, err := protojson.Marshal(j)
	if err != nil {
		return nil, err
	}
	v := &structpb.Value{}
	return v, protojson.Unmarshal(b, v)
}

// Not yet annotated covers: (it has not run); once it passes it covers
// JobService Create and GetHyperparameterTuningJob.
//
// TestPythonHyperparameterTuning (#2210): Google's Python SDKs, pinned
// (testdata/gcpcpy: google-cloud-aiplatform 1.165.1,
// google-cloud-pipeline-components 2.23.0, cloudml-hypertune 0.1.0.dev6),
// as Google's guide uses them (testdata/gcpcpy/hptune.py):
// aiplatform.HyperparameterTuningJob(...).run() over an aiplatform.CustomJob
// whose code parses its arguments with argparse and reports loss with
// cloudml-hypertune SUCCEEDS, each of its 4 trials with a final
// measurement of the loss it reported; a pipeline of GCPC's
// HyperparameterTuningJobRunOp (GRID_SEARCH over an integer 1-3) SUCCEEDS
// with 3 trials, its gcp_resources naming the HyperparameterTuningJob.
// The SDK runs on an internal Docker network whose only route is a relay
// to the instance's Vertex AI port, and the trials with no network.
func TestPythonHyperparameterTuning(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	gcs := dataformStorage(t, h)
	creds := os.Getenv("CLOUDBURROW_TEST_CREDENTIALS")
	if creds == "" {
		t.Skip("CLOUDBURROW_TEST_CREDENTIALS is not set: the SDK needs the instance's ADC fixture")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "docker", "build", "-t", gcpcPyImage, "testdata/gcpcpy")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the GCPC fixture: %v\n%s", err, out)
	}
	project := h.Project()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "hptune-py-" + suffix
	dataformBucket(ctx, t, gcs, project, bucket)
	_, port, err := net.SplitHostPort(stripScheme(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	network := internalNetwork(t, ctx, gcpcPyImage, port)
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(moduleRoot(t), "testdata", "gcpcpy", "hptune.py")+":/flow/hptune.py:ro",
		"-v", creds+":/creds/adc.json:ro",
		"-e", "CB_ENDPOINT=http://host.docker.internal:"+port,
		"-e", "CB_PROJECT="+project, "-e", "CB_LOCATION=us-central1",
		"-e", "CB_CREDENTIALS=/creds/adc.json", "-e", "CB_TASK_IMAGE="+gcpcPyImage,
		"-e", "CB_PIPELINE_ROOT=gs://"+bucket+"/root", "-e", "CB_STAGING=gs://"+bucket+"/staging",
		"-e", "CB_BASE=gs://"+bucket+"/tune",
		"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=cloudburrow.localhost",
		gcpcPyImage, "/flow/hptune.py")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the hyperparameter tuning flow: %v\n%s", err, out)
	}
	var res struct{ SDK, Pipeline json.RawMessage }
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(l, "RESULT "); ok {
			line = s
		}
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("the flow printed no result (%v):\n%s", err, out)
	}
	hj := &aiplatformpb.HyperparameterTuningJob{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(res.SDK, hj); err != nil {
		t.Fatalf("the SDK's job: %v", err)
	}
	if hj.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(hj.GetTrials()) != 4 {
		t.Fatalf("the SDK's job = %v %v, %d trials\n%s", hj.GetState(), hj.GetError(), len(hj.GetTrials()), out)
	}
	for _, tr := range hj.GetTrials() {
		vals := map[string]any{}
		for _, p := range tr.GetParameters() {
			vals[p.GetParameterId()] = p.GetValue().AsInterface()
		}
		l, _ := vals["lr"].(float64)
		want := (l-0.3)*(l-0.3) + 0.01
		if vals["opt"] == "sgd" {
			want++
		}
		// The SDK's default measurement_selection is "best": step 2's.
		fm := tr.GetFinalMeasurement()
		if tr.GetState() != aiplatformpb.Trial_SUCCEEDED || len(fm.GetMetrics()) != 1 || fm.GetMetrics()[0].GetMetricId() != "loss" ||
			math.Abs(fm.GetMetrics()[0].GetValue()-want) > 1e-6 || fm.GetStepCount() != 2 {
			t.Errorf("trial %s = %v %v, final %v", tr.GetId(), tr.GetState(), vals, fm)
		}
	}
	pj := &aiplatformpb.PipelineJob{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(res.Pipeline, pj); err != nil {
		t.Fatalf("the pipeline run: %v", err)
	}
	if pj.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the HyperparameterTuningJobRunOp run = %v %v\n%s", pj.GetState(), pj.GetError(), out)
	}
	r := gcpcResources(t, gcpcTask(t, pj, "tune"))
	prefix := "https://us-central1-aiplatform.googleapis.com/v1/projects/" + project + "/locations/us-central1/hyperparameterTuningJobs/"
	if len(r) != 1 || r[0].ResourceType != "HyperparameterTuningJob" || !strings.HasPrefix(r[0].ResourceURI, prefix) {
		t.Fatalf("tune's gcp_resources = %v, want one HyperparameterTuningJob under %s", r, prefix)
	}
	jc, err := aiplatform.NewJobClient(ctx, vertexOpts(endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	g, err := jc.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: strings.TrimPrefix(r[0].ResourceURI, "https://us-central1-aiplatform.googleapis.com/v1/")})
	if err != nil {
		t.Fatal(err)
	}
	var layers []string
	for _, tr := range g.GetTrials() {
		if tr.GetState() == aiplatformpb.Trial_SUCCEEDED && len(tr.GetParameters()) == 1 {
			layers = append(layers, strconv.FormatFloat(tr.GetParameters()[0].GetValue().GetNumberValue(), 'g', -1, 64))
		}
	}
	if g.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || strings.Join(layers, ",") != "1,2,3" {
		t.Errorf("the GCPC job = %v, trials' layers %v, want the grid 1,2,3", g.GetState(), layers)
	}
}
