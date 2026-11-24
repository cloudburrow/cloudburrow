//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
)

// gcpcPyImage is the Google Cloud Pipeline Components fixture
// (testdata/gcpcpy): google-cloud-pipeline-components, kfp and
// google-cloud-aiplatform 1.x, pinned and hash-locked; the custom jobs'
// containers and the pipelines' lightweight tasks run in it too.
const gcpcPyImage = "dev.local/cloudburrow-gcpcpy:test"

// extraRelay starts a second relay on network, reached there as alias, to
// the host's port (internalNetwork's relay is host.docker.internal), and
// removes it when the test ends.
func extraRelay(t *testing.T, ctx context.Context, network, alias, image, port string) {
	t.Helper()
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(forwarderSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module relay\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", "relay", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0", "GOFLAGS=")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the relay: %v\n%s", err, b)
	}
	relay := fmt.Sprintf("cb-relay-%s-%d", alias, time.Now().UnixNano()%1e9)
	args := append([]string{"run", "-d", "--name", relay, "-v", dir + ":/relay:ro", "--entrypoint", "/relay/relay"},
		hostRelayArgs(t, ctx)...)
	args = append(args, image, ":"+port, "host.docker.internal:"+port)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the %s relay: %v\n%s", alias, err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	for deadline := time.Now().Add(30 * time.Second); ; {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", relay).CombinedOutput()
		if strings.Contains(string(logs), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s relay did not start:\n%s", alias, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", alias, network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the %s relay: %v\n%s", alias, err, b)
	}
}

// gcpcTask is the one detail of the task named name.
func gcpcTask(t *testing.T, j *aiplatformpb.PipelineJob, name string) *aiplatformpb.PipelineTaskDetail {
	t.Helper()
	if d := detailsByName(j)[name]; len(d) > 0 {
		return d[0]
	}
	t.Errorf("the run has no task %s", name)
	return nil
}

// gcpcResources is a task's gcp_resources output parameter: GCPC's
// GcpResources JSON.
func gcpcResources(t *testing.T, d *aiplatformpb.PipelineTaskDetail) []struct{ ResourceType, ResourceURI string } {
	t.Helper()
	if d == nil {
		return nil
	}
	v := taskOutput(d, "gcp_resources")
	var r struct {
		Resources []struct {
			ResourceType string `json:"resourceType"`
			ResourceURI  string `json:"resourceUri"`
		} `json:"resources"`
	}
	if v == nil || json.Unmarshal([]byte(v.GetStringValue()), &r) != nil {
		t.Errorf("task %s: gcp_resources = %v, want GCPC's GcpResources JSON", d.GetTaskName(), v)
		return nil
	}
	var out []struct{ ResourceType, ResourceURI string }
	for _, x := range r.Resources {
		out = append(out, struct{ ResourceType, ResourceURI string }{x.ResourceType, x.ResourceURI})
	}
	return out
}

// gcpcArtifact is the task's one output artifact key.
func gcpcArtifact(t *testing.T, d *aiplatformpb.PipelineTaskDetail, key, schema string) map[string]any {
	t.Helper()
	if d == nil {
		return nil
	}
	l := d.GetOutputs()[key].GetArtifacts()
	if len(l) != 1 || l[0].GetSchemaTitle() != schema {
		t.Errorf("task %s: output %s = %v, want one %s", d.GetTaskName(), key, l, schema)
		return nil
	}
	return l[0].GetMetadata().AsMap()
}

// covers: google.cloud.aiplatform.v1.PipelineService/CreatePipelineJob, google.cloud.aiplatform.v1.PipelineService/GetPipelineJob, google.cloud.aiplatform.v1.PipelineService/CreateTrainingPipeline, google.cloud.aiplatform.v1.PipelineService/GetTrainingPipeline
//
// TestPythonGCPC (#2210): Google's Python SDKs, pinned (testdata/gcpcpy:
// google-cloud-pipeline-components 2.23.0, kfp, google-cloud-aiplatform
// 1.x), as Google's guides use them. kfp's RegistryClient uploads a
// pipeline template to a KFP repository of the instance's Artifact
// Registry, and aiplatform.PipelineJob runs it from its
// https://LOCATION-kfp.pkg.dev URL: the job SUCCEEDS with that template_uri
// and the uploaded sha256 version as its template_metadata.version. A
// pipeline of GCPC's CustomTrainingJobOp, BigqueryQueryJobOp,
// BigqueryCreateModelJobOp, BigqueryEvaluateModelJobOp and
// BigqueryPredictModelJobOp SUCCEEDS on the instance's own Vertex AI and
// BigQuery, each task's gcp_resources naming its CustomJob or BigQuery
// job, with the artifacts GCPC's launchers produce (google.BQTable,
// google.BQMLModel, the evaluation's system.Artifact), and the custom
// job's model file in Cloud Storage. aiplatform.CustomContainerTrainingJob
// runs a TrainingPipeline that SUCCEEDS and uploads a Model with the
// trained artifacts. The SDK runs on an internal Docker network whose only
// routes are relays to the instance's Vertex AI and Cloud Run ports, and
// the containers with no network.
func TestPythonGCPC(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	runEndpoint := h.Endpoint(EnvRun)
	gcs := dataformStorage(t, h)
	bq, bqProject := bigqueryClient(t, h)
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

	// The KFP repository the template is uploaded to.
	ar, arCtx := arClient(t, h)
	repoID := "kfp-gcpc-" + suffix
	formatRepo(t, arCtx, ar, runParent(h), repoID, &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_KFP})

	bucket := "kfp-gcpc-" + suffix
	dataformBucket(ctx, t, gcs, project, bucket)

	// The BigQuery table the components read.
	dataset := "gcpc_" + suffix
	ds := bq.Dataset(dataset)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create dataset %s: %v", dataset, err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	q := bq.Query("CREATE TABLE `" + bqProject + "." + dataset + ".train` AS " +
		"SELECT * FROM UNNEST([STRUCT(1.0 AS x, 3.0 AS y), (2.0, 5.0), (3.0, 7.0), (4.0, 9.0), (5.0, 11.0), (6.0, 13.0)])")
	if job, err := q.Run(ctx); err != nil {
		t.Fatalf("create the training table: %v", err)
	} else if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("create the training table: %v %v", err, st)
	}

	_, port, err := net.SplitHostPort(stripScheme(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	_, runPort, err := net.SplitHostPort(stripScheme(runEndpoint))
	if err != nil {
		t.Fatal(err)
	}
	network := internalNetwork(t, ctx, gcpcPyImage, port)
	extraRelay(t, ctx, network, "cb-run.localhost", gcpcPyImage, runPort)
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(moduleRoot(t), "testdata", "gcpcpy", "flow.py")+":/flow/flow.py:ro",
		"-v", creds+":/creds/adc.json:ro",
		"-e", "CB_ENDPOINT=http://host.docker.internal:"+port,
		"-e", "CB_RUN_ENDPOINT=http://cb-run.localhost:"+runPort,
		"-e", "CB_PROJECT="+project, "-e", "CB_LOCATION=us-central1",
		"-e", "CB_CREDENTIALS=/creds/adc.json", "-e", "CB_TASK_IMAGE="+gcpcPyImage,
		"-e", "CB_KFP_REPO="+repoID,
		"-e", "CB_PIPELINE_ROOT=gs://"+bucket+"/root", "-e", "CB_STAGING=gs://"+bucket+"/staging",
		"-e", "CB_MODEL_DIR=gs://"+bucket+"/custom", "-e", "CB_EXPORT_DIR=gs://"+bucket+"/exported",
		"-e", "CB_BQ_PROJECT="+bqProject, "-e", "CB_BQ_DATASET="+dataset,
		"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=cloudburrow.localhost",
		gcpcPyImage, "/flow/flow.py")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the GCPC flow: %v\n%s", err, out)
	}
	var res struct {
		Uploaded struct {
			Package, Version string
		}
		Template, Components json.RawMessage
		Training             struct {
			Pipeline json.RawMessage
			Model    string
			ModelURI string `json:"model_uri"`
		}
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(l, "RESULT "); ok {
			line = s
		}
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("the flow printed no result (%v):\n%s", err, out)
	}
	job := func(what string, raw json.RawMessage) *aiplatformpb.PipelineJob {
		j := &aiplatformpb.PipelineJob{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, j); err != nil {
			t.Fatalf("the %s run: %v", what, err)
		}
		return j
	}

	// The template from Artifact Registry.
	tmpl := job("template", res.Template)
	wantURI := "https://us-central1-kfp.pkg.dev/" + project + "/" + repoID + "/cb-gcpc-template/v1"
	if res.Uploaded.Package != "cb-gcpc-template" || !strings.HasPrefix(res.Uploaded.Version, "sha256:") {
		t.Errorf("RegistryClient.upload_pipeline = %+v", res.Uploaded)
	}
	if tmpl.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || tmpl.GetTemplateUri() != wantURI ||
		tmpl.GetTemplateMetadata().GetVersion() != res.Uploaded.Version {
		t.Errorf("the template run = %v %v, template_uri %q, template_metadata %v; want SUCCEEDED, %s, version %s",
			tmpl.GetState(), tmpl.GetError(), tmpl.GetTemplateUri(), tmpl.GetTemplateMetadata(), wantURI, res.Uploaded.Version)
	}
	if d := gcpcTask(t, tmpl, "hello"); d != nil && taskOutput(d, "Output").GetStringValue() != "hello registry" {
		t.Errorf("the template's hello = %v", taskOutput(d, "Output"))
	}

	// Google Cloud Pipeline Components.
	comp := job("components", res.Components)
	if comp.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the components run = %v %v\n%s", comp.GetState(), comp.GetError(), out)
	}
	for _, name := range []string{"custom-job", "query", "create-model", "evaluate", "predict", "export-model", "create-transform-model", "export-transform-model"} {
		if d := gcpcTask(t, comp, name); d != nil && d.GetState() != aiplatformpb.PipelineTaskDetail_SUCCEEDED {
			t.Errorf("task %s = %v %v", name, d.GetState(), d.GetError())
		}
	}
	cj := gcpcResources(t, gcpcTask(t, comp, "custom-job"))
	prefix := "https://us-central1-aiplatform.googleapis.com/v1/projects/" + project + "/locations/us-central1/customJobs/"
	if len(cj) != 1 || cj[0].ResourceType != "CustomJob" || !strings.HasPrefix(cj[0].ResourceURI, prefix) {
		t.Errorf("custom-job's gcp_resources = %v, want one CustomJob under %s", cj, prefix)
	}
	for _, name := range []string{"query", "create-model", "evaluate", "predict", "export-model"} {
		r := gcpcResources(t, gcpcTask(t, comp, name))
		bqPrefix := "https://www.googleapis.com/bigquery/v2/projects/" + bqProject + "/jobs/"
		if len(r) != 1 || r[0].ResourceType != "BigQueryJob" || !strings.HasPrefix(r[0].ResourceURI, bqPrefix) {
			t.Errorf("%s's gcp_resources = %v, want one BigQueryJob under %s", name, r, bqPrefix)
		}
	}
	if m := gcpcArtifact(t, gcpcTask(t, comp, "query"), "destination_table", "google.BQTable"); m != nil &&
		(m["projectId"] != bqProject || m["datasetId"] != dataset || m["tableId"] != "doubled") {
		t.Errorf("query's destination_table = %v", m)
	}
	if m := gcpcArtifact(t, gcpcTask(t, comp, "create-model"), "model", "google.BQMLModel"); m != nil &&
		(m["projectId"] != bqProject || m["datasetId"] != dataset || m["modelId"] != "lin") {
		t.Errorf("create-model's model = %v", m)
	}
	if m := gcpcArtifact(t, gcpcTask(t, comp, "evaluate"), "evaluation_metrics", "system.Artifact"); m != nil && len(m) == 0 {
		t.Errorf("evaluate's evaluation_metrics has no metadata")
	}
	if m := gcpcArtifact(t, gcpcTask(t, comp, "predict"), "destination_table", "google.BQTable"); m != nil &&
		(m["datasetId"] != dataset || m["tableId"] != "predicted") {
		t.Errorf("predict's destination_table = %v", m)
	}
	// BigqueryExportModelJobOp: EXPORT MODEL wrote the linear regression's
	// TF SavedModel, and exported_model_path names where.
	if d := gcpcTask(t, comp, "export-model"); d != nil && taskOutput(d, "exported_model_path").GetStringValue() != "gs://"+bucket+"/exported" {
		t.Errorf("export-model's exported_model_path = %v", taskOutput(d, "exported_model_path"))
	}
	saved := map[string]bool{}
	for objs := gcs.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "exported/"}); ; {
		o, err := objs.Next()
		if err != nil {
			break
		}
		saved[strings.TrimPrefix(o.Name, "exported/")] = true
	}
	if !saved["saved_model.pb"] || !saved["variables/variables.index"] {
		t.Errorf("EXPORT MODEL wrote %v under gs://%s/exported/, want a TF SavedModel", saved, bucket)
	}
	// The TRANSFORM model's export: the model's SavedModel, and its
	// preprocessing SavedModel in transform/.
	saved = map[string]bool{}
	for objs := gcs.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "exported-transform/"}); ; {
		o, err := objs.Next()
		if err != nil {
			break
		}
		saved[strings.TrimPrefix(o.Name, "exported-transform/")] = true
	}
	if !saved["saved_model.pb"] || !saved["transform/saved_model.pb"] || !saved["transform/variables/variables.index"] {
		t.Errorf("EXPORT MODEL of the TRANSFORM model wrote %v under gs://%s/exported-transform/, want a SavedModel and transform/", saved, bucket)
	}
	it := bq.Dataset(dataset).Table("predicted").Read(ctx)
	rows := 0
	for {
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			break
		}
		rows++
	}
	if rows != 6 {
		t.Errorf("the predicted table has %d rows, want the 6 of train", rows)
	}
	found := false
	for objs := gcs.Bucket(bucket).Objects(ctx, nil); ; {
		o, err := objs.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(o.Name, "custom/") && strings.HasSuffix(o.Name, "/model.txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("the custom job wrote no model.txt under gs://%s/custom/ (AIP_MODEL_DIR)", bucket)
	}

	// A TrainingPipeline, through aiplatform.CustomContainerTrainingJob.
	tp := &aiplatformpb.TrainingPipeline{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(res.Training.Pipeline, tp); err != nil {
		t.Fatalf("the training pipeline: %v", err)
	}
	if tp.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED ||
		tp.GetTrainingTaskDefinition() != "gs://google-cloud-aiplatform/schema/trainingjob/definition/custom_task_1.0.0.yaml" {
		t.Errorf("the training pipeline = %v %v, definition %q", tp.GetState(), tp.GetError(), tp.GetTrainingTaskDefinition())
	}
	if res.Training.Model == "" || tp.GetModelToUpload().GetName() == "" || !strings.HasPrefix(res.Training.Model, tp.GetModelToUpload().GetName()) ||
		!strings.HasPrefix(res.Training.ModelURI, "gs://") {
		t.Errorf("the training pipeline's model = %q (uri %q), model_to_upload %v", res.Training.Model, res.Training.ModelURI, tp.GetModelToUpload())
	}
}
