package main

import (
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestTrainNewModelForm (#2210): Train new model's values make the
// TrainingPipeline the Vertex AI SDK's CustomTrainingJob would: the
// custom task definition, its CustomJobSpec's worker pool 0 (a custom
// container, or a prebuilt one with the Python package), the model output
// directory, and model_to_upload from the prediction container.
func TestTrainNewModelForm(t *testing.T) {
	tp, err := trainingPipelineOf("p", "us-central1", map[string]string{"displayName": "m1", "container": "custom",
		"image": "us-central1-docker.pkg.dev/p/r/train:1", "args": "--epochs=2\n\n--lr=0.1", "outputDir": "gs://b/out",
		"machineType": "e2-standard-4", "replicaCount": "1", "prediction": "custom", "predImage": "us-central1-docker.pkg.dev/p/r/serve:1",
		"predRoute": "/predict", "healthRoute": "/health", "predPort": "8080"})
	if err != nil {
		t.Fatal(err)
	}
	spec := &aiplatformpb.CustomJobSpec{}
	b, _ := protojson.Marshal(tp.GetTrainingTaskInputs())
	if err := protojson.Unmarshal(b, spec); err != nil {
		t.Fatalf("the task inputs are not a CustomJobSpec: %v (%s)", err, b)
	}
	wp := spec.GetWorkerPoolSpecs()[0]
	if tp.GetTrainingTaskDefinition() != customTaskDefinition || wp.GetContainerSpec().GetImageUri() != "us-central1-docker.pkg.dev/p/r/train:1" ||
		strings.Join(wp.GetContainerSpec().GetArgs(), " ") != "--epochs=2 --lr=0.1" || wp.GetReplicaCount() != 1 ||
		wp.GetMachineSpec().GetMachineType() != "e2-standard-4" || spec.GetBaseOutputDirectory().GetOutputUriPrefix() != "gs://b/out" {
		t.Errorf("the pipeline = %v", tp)
	}
	cs := tp.GetModelToUpload().GetContainerSpec()
	if tp.GetModelToUpload().GetDisplayName() != "m1" || cs.GetPredictRoute() != "/predict" || cs.GetPorts()[0].GetContainerPort() != 8080 {
		t.Errorf("model_to_upload = %v", tp.GetModelToUpload())
	}

	tp, err = trainingPipelineOf("p", "us-central1", map[string]string{"displayName": "m2", "container": "prebuilt", "frameworkVersion": "scikit-learn-1.6",
		"packageUri": "gs://b/trainer.tar.gz", "pythonModule": "trainer.task", "prediction": "prebuilt", "predFrameworkVersion": "scikit-learn-1.6",
		"modelMode": "version", "parentModel": "m1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = protojson.Marshal(tp.GetTrainingTaskInputs())
	spec = &aiplatformpb.CustomJobSpec{}
	_ = protojson.Unmarshal(b, spec)
	ps := spec.GetWorkerPoolSpecs()[0].GetPythonPackageSpec()
	if ps.GetExecutorImageUri() != "us-docker.pkg.dev/vertex-ai/training/sklearn-cpu.1-6:latest" || ps.GetPythonModule() != "trainer.task" ||
		tp.GetParentModel() != "projects/p/locations/us-central1/models/m1" ||
		tp.GetModelToUpload().GetContainerSpec().GetImageUri() != "us-docker.pkg.dev/vertex-ai/prediction/sklearn-cpu.1-6:latest" {
		t.Errorf("the prebuilt pipeline = %v", tp)
	}
	for _, bad := range []map[string]string{
		{"container": "custom"},
		{"displayName": "x", "container": "custom"},
		{"displayName": "x", "container": "prebuilt", "frameworkVersion": "xgboost-2.1"},
		{"displayName": "x", "container": "custom", "image": "i", "modelMode": "version", "parentModel": "m"},
	} {
		if _, err := trainingPipelineOf("p", "us-central1", bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

// TestNewBatchPredictionForm (#2210): New batch prediction's sources map
// to the API's instances formats, and BigQuery input writes to BigQuery.
func TestNewBatchPredictionForm(t *testing.T) {
	j, err := batchPredictionOf(map[string]string{"displayName": "b", "model": "projects/p/locations/us-central1/models/1", "source": "gcs",
		"sourcePath": "gs://b/in.jsonl", "destination": "gs://b/out", "nodes": "2", "machineType": "n1-standard-4"})
	if err != nil {
		t.Fatal(err)
	}
	if j.GetInputConfig().GetInstancesFormat() != "jsonl" || j.GetOutputConfig().GetPredictionsFormat() != "jsonl" ||
		j.GetOutputConfig().GetGcsDestination().GetOutputUriPrefix() != "gs://b/out" || j.GetDedicatedResources().GetStartingReplicaCount() != 2 {
		t.Errorf("the job = %v", j)
	}
	j, err = batchPredictionOf(map[string]string{"displayName": "b", "model": "m", "source": "bigquery", "bqSource": "bq://p.d.t"})
	if err != nil {
		t.Fatal(err)
	}
	if j.GetInputConfig().GetInstancesFormat() != "bigquery" || j.GetOutputConfig().GetBigqueryDestination().GetOutputUri() != "bq://p" {
		t.Errorf("the BigQuery job = %v", j)
	}
	for in, want := range map[string]string{"gs://b/x.csv": "csv", "gs://b/x.tfrecord": "tf-record", "gs://b/x.tfrecord.gz": "tf-record-gzip"} {
		if got, _ := instancesFormat(in); got != want {
			t.Errorf("instancesFormat(%s) = %q, want %q", in, got, want)
		}
	}
	if _, err := instancesFormat("gs://b/x.parquet"); err == nil {
		t.Error("a .parquet source was accepted")
	}
	if _, err := batchPredictionOf(map[string]string{"displayName": "b", "model": "m", "source": "gcs-list"}); err == nil {
		t.Error("an empty file list was accepted")
	}
}

// TestSelectFromExistingPipelines (#2210): the run source's template_uri
// is the KFP repository's.
func TestSelectFromExistingPipelines(t *testing.T) {
	got, err := pipelineTemplateURI("us-central1", "p", map[string]string{"repository": "kfp-repo", "template": "hello-world"})
	if err != nil || got != "https://us-central1-kfp.pkg.dev/p/kfp-repo/hello-world/latest" {
		t.Errorf("template_uri = %q, %v", got, err)
	}
	if _, err := pipelineTemplateURI("us-central1", "p", map[string]string{"repository": "r"}); err == nil {
		t.Error("a template with no pipeline was accepted")
	}
}
