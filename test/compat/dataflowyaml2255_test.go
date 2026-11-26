//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/api/iterator"
)

// yamlProbe is a Beam YAML pipeline whose one Python mapping reads, on
// the job's workers, what Google's YAML template's default dependencies
// install there: boto3 and azure-storage-blob from the instance's Python
// index, and job_builder_util_transforms (copy_files_to_gcs) from the
// tarball the template's main.py names on storage.googleapis.com.
func yamlProbe(out string) string {
	return fmt.Sprintf(`pipeline:
  type: chain
  transforms:
    - type: Create
      config:
        elements:
          - {name: kiwi}
          - {name: fig}
    - type: MapToFields
      config:
        language: python
        fields:
          line:
            callable: "lambda row: ' '.join([row.name, __import__('boto3').__version__, __import__('azure.storage.blob', fromlist=['BlobClient']).__name__, __import__('copy_files_to_gcs').__name__])"
    - type: WriteToText
      config:
        path: %s
`, out)
}

// waitYAMLJob waits for the YAML template's job to end: its launch
// builds the template's launcher on first use.
func waitYAMLJob(t *testing.T, ctx context.Context, c dataflowClients, project, id string) *dataflowpb.Job {
	t.Helper()
	for {
		j, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id})
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		switch j.CurrentState {
		case dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED:
			return j
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s is still %s:\n%s", id, j.CurrentState, jobMessages(t, context.Background(), c, project, id))
		case <-time.After(3 * time.Second):
		}
	}
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestGcloudDataflowYAMLRun (#2255): `gcloud dataflow yaml run` launches
// Google's YAML template (gs://dataflow-templates-REGION/latest/flex/
// Yaml_Template, docs: gcloud dataflow yaml run) with an inline Beam YAML
// pipeline. The template's main.py, given no requirements_file, has its
// default dependencies (boto3, botocore, the Azure packages and Google's
// job_builder_util_transforms tarball on storage.googleapis.com) staged
// for the workers; the job's network has no route but the instance, so
// they come from the packages the instance seeds (an Artifact Registry
// Python repository, and the tarball, built from its source, in its own
// gs://dataflow-templates). The job is DONE, its workers import all
// three, and its labels name the template. The seeded repository holds
// each pinned file, with its pinned SHA-256. The same template run with a
// pipeline file in Cloud Storage and Jinja variables (LaunchFlexTemplate,
// yaml_pipeline_file and jinja_variables) writes the pipeline's lines too.
func TestGcloudDataflowYAMLRun(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvRun)
	project := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-yaml2255-%s", project, suffix)
	sc := storageClient(t, h)
	dataflowBucket(t, ctx, sc, project, bucket)
	gcRun := gcloudDataflow(t, h, project)

	name := "yaml-run-" + suffix
	out, err := gcRun("dataflow", "yaml", "run", name, "--region=us-central1",
		"--yaml-pipeline="+yamlProbe("gs://"+bucket+"/out/inline"), "--temp-location=gs://"+bucket+"/tmp", "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow yaml run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	j := waitYAMLJob(t, ctx, c, project, id)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the YAML template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	if got := j.GetLabels()["goog-dataflow-provided-template-name"]; got != "yaml_template" {
		t.Errorf("the job's goog-dataflow-provided-template-name label is %q; want yaml_template (labels %v)", got, j.GetLabels())
	}
	got := readOutputs(t, ctx, sc, bucket, "out/inline")
	for _, want := range []string{"kiwi 1.44.3 azure.storage.blob copy_files_to_gcs", "fig 1.44.3 azure.storage.blob copy_files_to_gcs"} {
		if !strings.Contains(got, want) {
			t.Errorf("the job's output lacks %q:\n%s", want, got)
		}
	}

	// The seeded index: each pinned file, by its pinned digest.
	ar, actx := arClient(t, h)
	repo := "projects/dataflow-templates/locations/us/repositories/extra-python-packages"
	have := map[string]string{}
	it := ar.ListFiles(actx, &arpb.ListFilesRequest{Parent: repo})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFiles %s: %v", repo, err)
		}
		for _, hs := range f.GetHashes() {
			if hs.GetType() == arpb.Hash_SHA256 {
				have[f.GetName()[strings.LastIndex(f.GetName(), "/")+1:]] = fmt.Sprintf("%x", hs.GetValue())
			}
		}
	}
	for file, sum := range map[string]string{
		"boto3-1.44.3-py3-none-any.whl":               "8c9c4844c7ce184494b102983696d6cdb09f5307ec7f238ca4ad5849fb14e26b",
		"azure_identity-1.27.0-py3-none-any.whl":      "92fd2a93c0d2f777dac6c04e5fd7d5734f6643582b983544ef66b4575844e46d",
		"poetry_core-2.5.0-py3-none-any.whl":          "98ee465b66f7e14bb9fbe68fb5e00d0cff184f036993ec94610ae4b5612f3add",
		"azure_storage_blob-12.31.1-py3-none-any.whl": "9e012d5f4ab658b7cb8a4581329d7a7aa2bf5f39b4d63baeefaae7f9e90fe6f0",
	} {
		if have[file] != sum {
			t.Errorf("the seeded repository's %s has SHA-256 %q; want %s (files %v)", file, have[file], sum, have)
		}
	}

	// A pipeline file in Cloud Storage, with Jinja variables.
	put := bucketWriter(t, ctx, sc, bucket)
	put("pipelines/probe.yaml", strings.ReplaceAll(yamlProbe("{{ output }}"), "{name: kiwi}", "{name: '{{ fruit }}'}"))
	resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
		LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: name + "-file",
			Template: &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://dataflow-templates-us-central1/latest/flex/Yaml_Template"},
			Parameters: map[string]string{"yaml_pipeline_file": "gs://" + bucket + "/pipelines/probe.yaml",
				"jinja_variables": fmt.Sprintf(`{"output": "gs://%s/out/file", "fruit": "plum"}`, bucket)},
			Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
	if err != nil {
		t.Fatalf("LaunchFlexTemplate(Yaml_Template): %v", err)
	}
	id = resp.GetJob().GetId()
	if j := waitYAMLJob(t, ctx, c, project, id); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the YAML template's job from a pipeline file is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got = readOutputs(t, ctx, sc, bucket, "out/file")
	for _, want := range []string{"plum 1.44.3 azure.storage.blob copy_files_to_gcs", "fig 1.44.3"} {
		if !strings.Contains(got, want) {
			t.Errorf("the job's output lacks %q:\n%s", want, got)
		}
	}
}
