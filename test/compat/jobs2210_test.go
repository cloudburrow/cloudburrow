//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// trainerImage is a busybox-based training image (testdata/trainer): a
// custom job's replica writes its model with sh.
const trainerImage = "dev.local/cloudburrow-trainer:test"

// pushTrainer builds the trainer fixture and pushes it to a DOCKER
// repository of the instance's Artifact Registry, returning its path there.
func pushTrainer(t *testing.T, ctx context.Context, h *Harness) string {
	t.Helper()
	build := exec.CommandContext(ctx, "docker", "build", "-t", trainerImage, "testdata/trainer")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the trainer fixture: %v\n%s", err, out)
	}
	ar, arCtx := arClient(t, h)
	repo := "trainers-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	formatRepo(t, arCtx, ar, runParent(h), repo, &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER})
	tarPath := filepath.Join(t.TempDir(), "trainer.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", tarPath, trainerImage).CombinedOutput(); err != nil {
		t.Fatalf("docker save: %v\n%s", err, out)
	}
	tag, _ := name.NewTag(trainerImage)
	img, err := tarball.ImageFromPath(tarPath, &tag)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+h.Project()+"/"+repo+"/trainer:test", registryOptions(h)...); err != nil {
		t.Fatalf("push the trainer: %v", err)
	}
	return "us-central1-docker.pkg.dev/" + h.Project() + "/" + repo + "/trainer:test"
}

// vertexOpts dials the Vertex AI shard at endpoint.
func vertexOpts(endpoint string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func trainerPool(image, script string) *aiplatformpb.WorkerPoolSpec {
	return &aiplatformpb.WorkerPoolSpec{MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
		Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: image,
			Command: []string{"sh", "-c", script}}}}
}

func waitTrainJob(t *testing.T, ctx context.Context, get func() (aiplatformpb.JobState, error)) aiplatformpb.JobState {
	t.Helper()
	for {
		s, err := get()
		if err != nil {
			t.Fatal(err)
		}
		switch s {
		case aiplatformpb.JobState_JOB_STATE_SUCCEEDED, aiplatformpb.JobState_JOB_STATE_FAILED, aiplatformpb.JobState_JOB_STATE_CANCELLED:
			return s
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the job did not end: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// covers: google.cloud.aiplatform.v1.JobService/CreateCustomJob, google.cloud.aiplatform.v1.JobService/GetCustomJob, google.cloud.aiplatform.v1.JobService/ListCustomJobs, google.cloud.aiplatform.v1.JobService/CancelCustomJob, google.cloud.aiplatform.v1.JobService/DeleteCustomJob, google.cloud.aiplatform.v1.PipelineService/CreateTrainingPipeline, google.cloud.aiplatform.v1.PipelineService/GetTrainingPipeline, google.cloud.aiplatform.v1.PipelineService/ListTrainingPipelines, google.cloud.aiplatform.v1.PipelineService/DeleteTrainingPipeline, google.cloud.aiplatform.v1.JobService/CreateBatchPredictionJob, google.cloud.aiplatform.v1.JobService/GetBatchPredictionJob, google.cloud.aiplatform.v1.JobService/ListBatchPredictionJobs, google.cloud.aiplatform.v1.JobService/DeleteBatchPredictionJob
//
// TestVertexAICustomTraining (#2210), with the official Go client: a
// custom job whose image is in the instance's Artifact Registry writes its
// model under /gcs to AIP_MODEL_DIR and SUCCEEDS; one whose image is in
// Google's Container Registry FAILS without anything pulled; a long one is
// CANCELLED; a custom training pipeline runs its CustomJob and uploads
// model_to_upload (the model server fixture) from the job's model
// directory; a batch prediction job of that model writes
// prediction.results-00000-of-00001 with the model's answers; the list and delete
// methods.
func TestVertexAICustomTraining(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	gcs := dataformStorage(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	buildModelServer(t, ctx)
	server := pushModelServer(t, ctx, h)
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
	mc, err := aiplatform.NewModelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-train-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)

	// A custom job from the instance's Artifact Registry.
	job, err := jc.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: &aiplatformpb.CustomJob{DisplayName: "compat-train-" + suffix,
		Labels: map[string]string{"compat": "t2210"},
		JobSpec: &aiplatformpb.CustomJobSpec{BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/job"},
			WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{trainerPool(trainer,
				`d=/gcs/${AIP_MODEL_DIR#gs://}; mkdir -p "$d" && printf '{"weight":2,"bias":1}' > "$d/model.json" && echo "$CLUSTER_SPEC" > "$d/cluster.json"`)}}}})
	if err != nil {
		t.Fatalf("CreateCustomJob: %v", err)
	}
	if !strings.HasPrefix(job.GetName(), parent+"/customJobs/") || job.GetState() != aiplatformpb.JobState_JOB_STATE_QUEUED {
		t.Errorf("created = %s %v", job.GetName(), job.GetState())
	}
	var got *aiplatformpb.CustomJob
	state := waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
		got, err = jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: job.GetName()})
		return got.GetState(), err
	})
	if state != aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the custom job = %v %v", state, got.GetError())
	}
	r, err := gcs.Bucket(bucket).Object("job/model/model.json").NewReader(ctx)
	if err != nil {
		t.Fatalf("the job's model: %v", err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != `{"weight":2,"bias":1}` {
		t.Errorf("model.json = %q", b)
	}

	// An image in Google's registry is refused, never pulled.
	g, err := jc.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: &aiplatformpb.CustomJob{DisplayName: "compat-gcr-" + suffix,
		JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{trainerPool("gcr.io/cloudburrow-none/never:1", "true")}}}})
	if err != nil {
		t.Fatal(err)
	}
	state = waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
		got, err = jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: g.GetName()})
		return got.GetState(), err
	})
	if state != aiplatformpb.JobState_JOB_STATE_FAILED || !strings.Contains(got.GetError().GetMessage(), "never pulls from") {
		t.Errorf("a gcr.io image = %v %q", state, got.GetError().GetMessage())
	}

	// Cancel.
	long, err := jc.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: &aiplatformpb.CustomJob{DisplayName: "compat-long-" + suffix,
		JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{trainerPool(trainer, "sleep 600")}}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		got, _ = jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: long.GetName()})
		if got.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING {
			break
		}
		time.Sleep(time.Second)
	}
	if err := jc.CancelCustomJob(ctx, &aiplatformpb.CancelCustomJobRequest{Name: long.GetName()}); err != nil {
		t.Fatalf("CancelCustomJob: %v", err)
	}
	state = waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
		got, err = jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: long.GetName()})
		return got.GetState(), err
	})
	if state != aiplatformpb.JobState_JOB_STATE_CANCELLED || got.GetError().GetCode() != 1 {
		t.Errorf("the cancelled job = %v %v", state, got.GetError())
	}
	n := 0
	for it := jc.ListCustomJobs(ctx, &aiplatformpb.ListCustomJobsRequest{Parent: parent, Filter: `labels.compat="t2210"`}); ; n++ {
		if _, err := it.Next(); err != nil {
			break
		}
	}
	if n != 1 {
		t.Errorf("ListCustomJobs by label = %d jobs, want 1", n)
	}

	// A custom training pipeline that uploads its model.
	inputs, _ := structpb.NewValue(map[string]any{
		"baseOutputDirectory": map[string]any{"outputUriPrefix": "gs://" + bucket + "/tp"},
		"workerPoolSpecs": []any{map[string]any{"machineSpec": map[string]any{"machineType": "n1-standard-4"}, "replicaCount": "1",
			"containerSpec": map[string]any{"imageUri": trainer, "command": []any{"sh", "-c",
				`d=/gcs/${AIP_MODEL_DIR#gs://}; mkdir -p "$d" && printf '{"weight":2,"bias":1,"name":"trained"}' > "$d/model.json"`}}}}})
	modelID := "compat-trained-" + suffix
	tp, err := pc.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "compat-tp-" + suffix, TrainingTaskDefinition: "gs://google-cloud-aiplatform/schema/trainingjob/definition/custom_task_1.0.0.yaml",
		TrainingTaskInputs: inputs, ModelId: modelID,
		ModelToUpload: &aiplatformpb.Model{DisplayName: "compat-trained", ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: server,
			PredictRoute: "/v1/models/linear:predict", HealthRoute: "/v1/models/linear", Ports: []*aiplatformpb.Port{{ContainerPort: 7080}}}}}})
	if err != nil {
		t.Fatalf("CreateTrainingPipeline: %v", err)
	}
	defer func() {
		if op, err := mc.DeleteModel(context.Background(), &aiplatformpb.DeleteModelRequest{Name: parent + "/models/" + modelID}); err == nil {
			_ = op.Wait(context.Background())
		}
	}()
	var gotTP *aiplatformpb.TrainingPipeline
	for {
		gotTP, err = pc.GetTrainingPipeline(ctx, &aiplatformpb.GetTrainingPipelineRequest{Name: tp.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		s := gotTP.GetState()
		if s == aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || s == aiplatformpb.PipelineState_PIPELINE_STATE_FAILED || s == aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the training pipeline did not end")
		}
		time.Sleep(time.Second)
	}
	if gotTP.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the training pipeline = %v %v", gotTP.GetState(), gotTP.GetError())
	}
	model := gotTP.GetModelToUpload()
	if model.GetName() != parent+"/models/"+modelID || model.GetVersionId() != "1" || gotTP.GetModelId() != modelID {
		t.Errorf("model_to_upload = %s@%s (%s)", model.GetName(), model.GetVersionId(), gotTP.GetModelId())
	}
	if !strings.HasPrefix(gotTP.GetTrainingTaskMetadata().GetStructValue().GetFields()["backingCustomJob"].GetStringValue(), parent+"/customJobs/") {
		t.Errorf("training_task_metadata = %v", gotTP.GetTrainingTaskMetadata())
	}
	if m, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: model.GetName()}); err != nil || m.GetArtifactUri() != "gs://"+bucket+"/tp/model" {
		t.Errorf("the uploaded model = %v %v", m.GetArtifactUri(), err)
	}

	// Batch prediction of the trained model.
	w := gcs.Bucket(bucket).Object("in/instances.jsonl").NewWriter(ctx)
	_, _ = w.Write([]byte("1\n2\n3\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bp, err := jc.CreateBatchPredictionJob(ctx, &aiplatformpb.CreateBatchPredictionJobRequest{Parent: parent, BatchPredictionJob: &aiplatformpb.BatchPredictionJob{
		DisplayName: "compat-bp-" + suffix, Model: model.GetName(),
		InputConfig: &aiplatformpb.BatchPredictionJob_InputConfig{InstancesFormat: "jsonl",
			Source: &aiplatformpb.BatchPredictionJob_InputConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://" + bucket + "/in/*.jsonl"}}}},
		OutputConfig: &aiplatformpb.BatchPredictionJob_OutputConfig{PredictionsFormat: "jsonl",
			Destination: &aiplatformpb.BatchPredictionJob_OutputConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/out"}}},
		ManualBatchTuningParameters: &aiplatformpb.ManualBatchTuningParameters{BatchSize: 2}}})
	if err != nil {
		t.Fatalf("CreateBatchPredictionJob: %v", err)
	}
	var gotBP *aiplatformpb.BatchPredictionJob
	state = waitTrainJob(t, ctx, func() (aiplatformpb.JobState, error) {
		gotBP, err = jc.GetBatchPredictionJob(ctx, &aiplatformpb.GetBatchPredictionJobRequest{Name: bp.GetName()})
		return gotBP.GetState(), err
	})
	if state != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || gotBP.GetCompletionStats().GetSuccessfulCount() != 3 {
		t.Fatalf("the batch prediction job = %v %v %v", state, gotBP.GetError(), gotBP.GetCompletionStats())
	}
	dir := gotBP.GetOutputInfo().GetGcsOutputDirectory()
	if !strings.HasPrefix(dir, "gs://"+bucket+"/out/prediction-compat-trained-") {
		t.Fatalf("gcs_output_directory = %q", dir)
	}
	r, err = gcs.Bucket(bucket).Object(strings.TrimPrefix(dir, "gs://"+bucket+"/") + "/prediction.results-00000-of-00001").NewReader(ctx)
	if err != nil {
		t.Fatalf("the predictions: %v", err)
	}
	b, _ = io.ReadAll(r)
	_ = r.Close()
	if !strings.Contains(string(b), `{"instance":1,"prediction":3}`) || !strings.Contains(string(b), `{"instance":3,"prediction":7}`) {
		t.Errorf("predictions = %s", b)
	}
	if it := jc.ListBatchPredictionJobs(ctx, &aiplatformpb.ListBatchPredictionJobsRequest{Parent: parent}); it != nil {
		if _, err := it.Next(); err != nil {
			t.Errorf("ListBatchPredictionJobs: %v", err)
		}
	}
	if _, err := pc.ListTrainingPipelines(ctx, &aiplatformpb.ListTrainingPipelinesRequest{Parent: parent}).Next(); err != nil {
		t.Errorf("ListTrainingPipelines: %v", err)
	}
	for _, del := range []func() error{
		func() error {
			op, err := jc.DeleteBatchPredictionJob(ctx, &aiplatformpb.DeleteBatchPredictionJobRequest{Name: bp.GetName()})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		},
		func() error {
			op, err := pc.DeleteTrainingPipeline(ctx, &aiplatformpb.DeleteTrainingPipelineRequest{Name: tp.GetName()})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		},
		func() error {
			op, err := jc.DeleteCustomJob(ctx, &aiplatformpb.DeleteCustomJobRequest{Name: job.GetName()})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		},
	} {
		if err := del(); err != nil {
			t.Errorf("delete: %v", err)
		}
	}
	if _, err := jc.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: job.GetName()}); err == nil {
		t.Error("the deleted custom job is still there")
	}
}

// templateYAML is a one-task pipeline template, as kfp compiles one.
const templateYAML = `pipelineInfo:
  name: compat-template
schemaVersion: 2.1.0
sdkVersion: kfp-2.18.2
root:
  inputDefinitions:
    parameters:
      word:
        parameterType: STRING
        defaultValue: hello
  dag:
    tasks:
      echo:
        taskInfo:
          name: echo
        componentRef:
          name: comp-echo
        inputs:
          parameters:
            word:
              componentInputParameter: word
components:
  comp-echo:
    executorLabel: exec-echo
    inputDefinitions:
      parameters:
        word:
          parameterType: STRING
    outputDefinitions:
      parameters:
        Output:
          parameterType: STRING
deploymentSpec:
  executors:
    exec-echo:
      container:
        image: IMAGE
        command: [sh, -c]
        args: ["printf '%s' \"$0\" > \"$1\"", "{{$.inputs.parameters['word']}}", "{{$.outputs.parameters['Output'].output_file}}"]
`

// covers: google.cloud.aiplatform.v1.PipelineService/CreatePipelineJob
//
// TestPipelineTemplates (#2210): a PipelineJob's template_uri, as Google's
// "Use the template in a pipeline run" names it, from a KFP repository of
// the instance's Artifact Registry (uploaded with the KFP protocol's
// multipart POST, by tag) and from an https:// Cloud Storage URL
// (storage.googleapis.com, which is this instance's Cloud Storage). Both
// runs SUCCEED with the template's task; the registry's has
// template_metadata.version, the sha256 version the upload answered.
func TestPipelineTemplates(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	gcs := dataformStorage(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	trainer := pushTrainer(t, ctx, h)
	opts := vertexOpts(endpoint)
	pc, err := aiplatform.NewPipelineClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-tmpl-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)
	tmpl := strings.Replace(templateYAML, "IMAGE", trainer, 1)

	ar, arCtx := arClient(t, h)
	repo := "kfp-tmpl-" + suffix
	formatRepo(t, arCtx, ar, parent, repo, &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_KFP})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("content", "pipeline.yaml")
	_, _ = fw.Write([]byte(tmpl))
	_ = mw.WriteField("tags", "v1,latest")
	_ = mw.Close()
	_, port, _ := net.SplitHostPort(stripScheme(h.Endpoint(EnvRun)))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/us-central1-kfp.pkg.dev/"+h.Project()+"/"+repo, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload the template: %s %s", resp.Status, answer)
	}
	_, version, _ := strings.Cut(string(answer), "/")

	w := gcs.Bucket(bucket).Object("templates/pipeline.yaml").NewWriter(ctx)
	_, _ = w.Write([]byte(tmpl))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ uri, version string }{
		{fmt.Sprintf("https://us-central1-kfp.pkg.dev/%s/%s/compat-template/v1", h.Project(), repo), version},
		{"https://storage.googleapis.com/" + bucket + "/templates/pipeline.yaml", ""},
	} {
		j, err := pc.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
			TemplateUri: c.uri, RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://" + bucket + "/root",
				ParameterValues: map[string]*structpb.Value{"word": structpb.NewStringValue("templated")}}}})
		if err != nil {
			t.Errorf("CreatePipelineJob(%s): %v", c.uri, err)
			continue
		}
		if j.GetTemplateMetadata().GetVersion() != c.version {
			t.Errorf("%s: template_metadata.version = %q, want %q", c.uri, j.GetTemplateMetadata().GetVersion(), c.version)
		}
		for {
			j, err = pc.GetPipelineJob(ctx, &aiplatformpb.GetPipelineJobRequest{Name: j.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if s := j.GetState(); s == aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || s == aiplatformpb.PipelineState_PIPELINE_STATE_FAILED {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("the run did not end")
			}
			time.Sleep(time.Second)
		}
		if j.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
			t.Errorf("%s: run = %v %v", c.uri, j.GetState(), j.GetError())
			continue
		}
		for _, d := range j.GetJobDetail().GetTaskDetails() {
			if d.GetTaskName() == "echo" && taskOutput(d, "Output").GetStringValue() != "templated" {
				t.Errorf("%s: echo's output = %v", c.uri, taskOutput(d, "Output"))
			}
		}
	}
}
