//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nasTrialScript is a NAS trial in the busybox trainer: it reads
// --nas_params_str ({"filters": "i/3 (N)", ...}) and reports the reward
// N/64 as cloudml-hypertune writes its reports, under the tag
// CLOUD_ML_HP_METRIC_TAG, to /tmp/hypertune/output.metrics. A train trial
// (--retrain_search_job_trials) reports the reward plus one.
const nasTrialScript = `p=; train=0
for a in "$@"; do case "$a" in --nas_params_str=*) p=${a#--nas_params_str=};; --retrain_search_job_trials=*) train=1;; esac; done
n=$(echo "$p" | sed 's/.*"filters": "[0-9]*\/[0-9]* (\([0-9]*\))".*/\1/')
r=$(awk -v n="$n" -v t="$train" 'BEGIN { printf "%.12g", n / 64 + t }')
mkdir -p /tmp/hypertune
printf '{"global_step": "1", "%s": "%s", "timestamp": %s}\n' "$CLOUD_ML_HP_METRIC_TAG" "$r" "$(date +%s)" > /tmp/hypertune/output.metrics`

// Not yet annotated covers: (it has not run); once it passes it covers
// the seven JobService NasJob and NasTrialDetail methods.
//
// TestVertexAINeuralArchitectureSearch (#2214), with the official Go
// client: a multi-trial NAS job over a PyGlove search space (a OneOf of
// three filter counts), its trials CustomJobs of the trainer in the
// instance's Artifact Registry given --nas_params_str, runs a grid search
// of 3 search trials and trains the best 2 (--retrain_search_job_trials)
// and SUCCEEDS; its NasTrialDetails pair each search trial's parameters
// with its train trial; a bad search space and a grid search over a float
// are INVALID_ARGUMENT; a running job is CANCELLED; list, REST and delete.
func TestVertexAINeuralArchitectureSearch(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if strings.TrimSpace(endpoint) == "" {
		t.Fatal("no Agent Platform endpoint")
	}
	gcs := dataformStorage(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	trainer := pushTrainer(t, ctx, h)
	jc, err := aiplatform.NewJobClient(ctx, vertexOpts(endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-nas-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)

	pool := func(script string) *aiplatformpb.CustomJobSpec {
		return &aiplatformpb.CustomJobSpec{BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/nas"},
			WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{{MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
				Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: trainer, Command: []string{"sh", "-c", script, "sh"}}}}}}
	}
	nas := func(display, space string, algo aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MultiTrialAlgorithm, trials int32, script string) *aiplatformpb.NasJob {
		return &aiplatformpb.NasJob{DisplayName: display, NasJobSpec: &aiplatformpb.NasJobSpec{SearchSpaceSpec: space,
			NasAlgorithmSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_{MultiTrialAlgorithmSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec{
				MultiTrialAlgorithm: algo,
				Metric:              &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec{MetricId: "top_1_accuracy", Goal: aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MAXIMIZE},
				SearchTrialSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_SearchTrialSpec{MaxTrialCount: trials, MaxParallelTrialCount: trials,
					SearchTrialJobSpec: pool(script)}}}}}
	}
	wait := func(name string) *aiplatformpb.NasJob {
		var got *aiplatformpb.NasJob
		waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
			got, err = jc.GetNasJob(ctx, &aiplatformpb.GetNasJobRequest{Name: name})
			return got.GetState(), err
		})
		return got
	}
	space := `{"_type": "model.Model", "filters": {"_type": "hyper.OneOf", "candidates": [16, 32, 64]}}`

	// Refusals.
	if _, err := jc.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: nas("bad-"+suffix, `{"x": {"_type": "hyper.Nope"}}`,
		aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH, 1, nasTrialScript)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bad search space = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := jc.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: nas("grid-"+suffix,
		`{"lr": {"_type": "hyper.Float", "min_value": 0.1, "max_value": 1.0}}`, aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH, 1, nasTrialScript)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a grid search over a float = %v, want INVALID_ARGUMENT", err)
	}

	// The search, and training of its best two.
	job := nas("compat-nas-"+suffix, space, aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH, 3, nasTrialScript)
	job.NasJobSpec.GetMultiTrialAlgorithmSpec().TrainTrialSpec = &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_TrainTrialSpec{
		TrainTrialJobSpec: pool(nasTrialScript), MaxParallelTrialCount: 2, Frequency: 3}
	created, err := jc.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: job})
	if err != nil {
		t.Fatalf("CreateNasJob: %v", err)
	}
	t.Cleanup(func() {
		if op, err := jc.DeleteNasJob(context.Background(), &aiplatformpb.DeleteNasJobRequest{Name: created.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	got := wait(created.GetName())
	out := got.GetNasJobOutput().GetMultiTrialJobOutput()
	if got.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || len(out.GetSearchTrials()) != 3 || len(out.GetTrainTrials()) != 2 {
		t.Fatalf("NAS job %v (%v): %d search and %d train trials", got.GetState(), got.GetError(), len(out.GetSearchTrials()), len(out.GetTrainTrials()))
	}
	rewards := map[float64]bool{}
	for _, tr := range out.GetSearchTrials() {
		if tr.GetState() != aiplatformpb.NasTrial_SUCCEEDED || len(tr.GetFinalMeasurement().GetMetrics()) != 1 {
			t.Fatalf("search trial %v", tr)
		}
		rewards[tr.GetFinalMeasurement().GetMetrics()[0].GetValue()] = true
	}
	if !rewards[0.25] || !rewards[0.5] || !rewards[1] {
		t.Errorf("search rewards %v; want 0.25, 0.5 and 1, one for each filter count", rewards)
	}
	for _, tr := range out.GetTrainTrials() {
		if v := tr.GetFinalMeasurement().GetMetrics()[0].GetValue(); tr.GetState() != aiplatformpb.NasTrial_SUCCEEDED || (v != 2 && v != 1.5) {
			t.Errorf("train trial %v; want the best two (1+1 and 0.5+1)", tr)
		}
	}
	trained := 0
	for it := jc.ListNasTrialDetails(ctx, &aiplatformpb.ListNasTrialDetailsRequest{Parent: created.GetName()}); ; {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListNasTrialDetails: %v", err)
		}
		var params map[string]string
		if err := json.Unmarshal([]byte(d.GetParameters()), &params); err != nil || !strings.Contains(params["filters"], "/3 (") {
			t.Errorf("trial detail %s parameters %q", d.GetName(), d.GetParameters())
		}
		if d.GetTrainTrial() != nil {
			trained++
		}
	}
	if trained != 2 {
		t.Errorf("%d trial details have a train trial, want 2", trained)
	}
	if d, err := jc.GetNasTrialDetail(ctx, &aiplatformpb.GetNasTrialDetailRequest{Name: created.GetName() + "/nasTrialDetails/1"}); err != nil || d.GetSearchTrial().GetId() != "1" {
		t.Errorf("GetNasTrialDetail = %v, %v", d, err)
	}
	// REST: the job as JSON.
	resp, err := http.Get("http://" + stripScheme(endpoint) + "/v1/" + created.GetName())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"nasJobSpec"`) {
		t.Errorf("GET /v1/%s = %d: %.300s", created.GetName(), resp.StatusCode, body)
	}

	// A running job, cancelled.
	sleeper, err := jc.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: nas("compat-nas-sleeper-"+suffix, space,
		aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_REINFORCEMENT_LEARNING, 1, "sleep 600")})
	if err != nil {
		t.Fatalf("CreateNasJob: %v", err)
	}
	t.Cleanup(func() {
		if op, err := jc.DeleteNasJob(context.Background(), &aiplatformpb.DeleteNasJobRequest{Name: sleeper.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
		g, err := jc.GetNasJob(ctx, &aiplatformpb.GetNasJobRequest{Name: sleeper.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if g.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING && len(g.GetNasJobOutput().GetMultiTrialJobOutput().GetSearchTrials()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sleeper is %v", g.GetState())
		}
	}
	if _, err := jc.DeleteNasJob(ctx, &aiplatformpb.DeleteNasJobRequest{Name: sleeper.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Delete of a running job = %v, want FAILED_PRECONDITION", err)
	}
	if err := jc.CancelNasJob(ctx, &aiplatformpb.CancelNasJobRequest{Name: sleeper.GetName()}); err != nil {
		t.Fatalf("CancelNasJob: %v", err)
	}
	if g := wait(sleeper.GetName()); g.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("cancelled job = %v", g.GetState())
	}

	listed := map[string]bool{}
	for it := jc.ListNasJobs(ctx, &aiplatformpb.ListNasJobsRequest{Parent: parent}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListNasJobs: %v", err)
		}
		listed[j.GetName()] = true
	}
	if !listed[created.GetName()] || !listed[sleeper.GetName()] {
		t.Errorf("ListNasJobs = %v", listed)
	}
	op, err := jc.DeleteNasJob(ctx, &aiplatformpb.DeleteNasJobRequest{Name: created.GetName()})
	if err != nil {
		t.Fatalf("DeleteNasJob: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteNasJob operation: %v", err)
	}
	if _, err := jc.GetNasJob(ctx, &aiplatformpb.GetNasJobRequest{Name: created.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetNasJob after delete = %v, want NOT_FOUND", err)
	}
}
