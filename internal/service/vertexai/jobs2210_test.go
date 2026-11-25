package vertexai

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
)

// richRunner is a fakePipelineRunner that also serves templates and a
// fake BigQuery REST API.
type richRunner struct {
	*fakePipelineRunner
	templates map[string]string
	bqMu      sync.Mutex
	bqCalls   []string
	bqBodies  []string
}

func (r *richRunner) FetchTemplate(_ context.Context, uri string) ([]byte, error) {
	t, ok := r.templates[uri]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return []byte(t), nil
}

func (r *richRunner) BigQueryREST(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	r.bqMu.Lock()
	r.bqCalls = append(r.bqCalls, method+" "+path)
	r.bqBodies = append(r.bqBodies, string(body))
	r.bqMu.Unlock()
	switch {
	case method == "POST" && strings.HasSuffix(path, "/jobs"):
		var j map[string]any
		_ = json.Unmarshal(body, &j)
		q := j["configuration"].(map[string]any)["query"].(map[string]any)
		ref := map[string]any{"projectId": "p", "jobId": "job1", "location": "US"}
		out, _ := json.Marshal(map[string]any{"jobReference": ref, "selfLink": "http://local/bigquery/v2/projects/p/jobs/job1", "configuration": map[string]any{"query": q}})
		return 200, out, nil
	case method == "GET" && strings.Contains(path, "/jobs/job1"):
		r.bqMu.Lock()
		last := ""
		for i := len(r.bqBodies) - 1; i >= 0; i-- {
			if strings.HasPrefix(r.bqCalls[i], "POST") {
				last = r.bqBodies[i]
				break
			}
		}
		r.bqMu.Unlock()
		var j map[string]any
		_ = json.Unmarshal([]byte(last), &j)
		q := j["configuration"].(map[string]any)["query"].(map[string]any)
		stats := map[string]any{"statementType": "SELECT"}
		if strings.HasPrefix(strings.ToUpper(q["query"].(string)), "CREATE MODEL") {
			stats = map[string]any{"statementType": "CREATE_MODEL", "ddlTargetTable": map[string]any{"projectId": "p", "datasetId": "d", "tableId": "m"}}
		}
		if _, ok := q["destinationTable"]; !ok {
			q["destinationTable"] = map[string]any{"projectId": "p", "datasetId": "_anon", "tableId": "anon1"}
		}
		out, _ := json.Marshal(map[string]any{"id": "p:US.job1", "status": map[string]any{"state": "DONE"},
			"configuration": map[string]any{"query": q}, "statistics": map[string]any{"query": stats}})
		return 200, out, nil
	case method == "GET" && strings.Contains(path, "/queries/job1"):
		return 200, []byte(`{"schema": {"fields": [{"name": "r2_score", "type": "FLOAT"}]}, "rows": [{"f": [{"v": "0.9"}]}]}`), nil
	}
	return 404, []byte(`{"error": "not found"}`), nil
}

func newRichServer(t *testing.T) (*Server, *richRunner) {
	t.Helper()
	s, base := newPipelineServer(t)
	r := &richRunner{fakePipelineRunner: base, templates: map[string]string{}}
	s.SetPipelineRunner(r)
	return s, r
}

func waitCustom(t *testing.T, j *jobService, name string) *aiplatformpb.CustomJob {
	t.Helper()
	for i := 0; i < 500; i++ {
		got, err := j.GetCustomJob(context.Background(), &aiplatformpb.GetCustomJobRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if !jobRunning(got.GetState()) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("custom job %s did not end", name)
	return nil
}

func containerPool(args ...string) *aiplatformpb.WorkerPoolSpec {
	return &aiplatformpb.WorkerPoolSpec{MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
		Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: "local/train:1", Args: args}}}
}

func TestCustomJobs(t *testing.T) {
	ctx := context.Background()
	s, r := newRichServer(t)
	j := &jobService{s: s}
	parent := "projects/p/locations/us-central1"
	for name, bad := range map[string]*aiplatformpb.CustomJob{
		"no display name": {JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool()}}},
		"no pools":        {DisplayName: "x", JobSpec: &aiplatformpb.CustomJobSpec{}},
		"two primaries": {DisplayName: "x", JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{
			{MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1"}, ReplicaCount: 2, Task: containerPool().Task}}}},
		"no machine": {DisplayName: "x", JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{
			{ReplicaCount: 1, Task: containerPool().Task}}}},
	} {
		if _, err := j.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: bad}); codeOf(err) != codes.InvalidArgument {
			t.Errorf("create %s = %v, want INVALID_ARGUMENT", name, err)
		}
	}
	r.objects["data/in.csv"] = []byte("1,2")
	job, err := j.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: &aiplatformpb.CustomJob{DisplayName: "train",
		JobSpec: &aiplatformpb.CustomJobSpec{BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://out/run"},
			WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool("--data=gs://data/in.csv", "write:/gcs/out/run/model/model.txt=weights")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(job.GetName(), parent+"/customJobs/") {
		t.Errorf("name = %s", job.GetName())
	}
	done := waitCustom(t, j, job.GetName())
	if done.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || done.GetStartTime() == nil || done.GetEndTime() == nil {
		t.Fatalf("job = %v %v", done.GetState(), done.GetError())
	}
	if got := string(r.objects["out/run/model/model.txt"]); got != "weights" {
		t.Errorf("the model written under /gcs = %q", got)
	}
	run := r.runs[len(r.runs)-1]
	if run.Env["AIP_MODEL_DIR"] != "gs://out/run/model/" || run.Env["AIP_CHECKPOINT_DIR"] != "gs://out/run/checkpoints/" ||
		!strings.Contains(run.Env["CLUSTER_SPEC"], `"workerpool0"`) || !strings.Contains(run.Env["TF_CONFIG"], `"chief"`) {
		t.Errorf("env = %v", run.Env)
	}

	bad, _ := j.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: &aiplatformpb.CustomJob{DisplayName: "boom",
		JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{containerPool("fail")}}}})
	failed := waitCustom(t, j, bad.GetName())
	if failed.GetState() != aiplatformpb.JobState_JOB_STATE_FAILED || !strings.HasPrefix(failed.GetError().GetMessage(), "The replica workerpool0-0 exited with a non-zero status of 1.") {
		t.Errorf("failed job = %v %q", failed.GetState(), failed.GetError().GetMessage())
	}
	list, err := j.ListCustomJobs(ctx, &aiplatformpb.ListCustomJobsRequest{Parent: parent, Filter: `display_name="train"`})
	if err != nil || len(list.GetCustomJobs()) != 1 {
		t.Errorf("list = %v %v", list, err)
	}
	if _, err := j.CancelCustomJob(ctx, &aiplatformpb.CancelCustomJobRequest{Name: job.GetName()}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("cancel a finished job = %v", err)
	}
	if _, err := j.DeleteCustomJob(ctx, &aiplatformpb.DeleteCustomJobRequest{Name: job.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: job.GetName()}); codeOf(err) != codes.NotFound {
		t.Errorf("get deleted = %v", err)
	}
}

func TestTrainingPipelines(t *testing.T) {
	ctx := context.Background()
	s, _ := newRichServer(t)
	p := &pipelineService{s: s}
	parent := "projects/p/locations/us-central1"
	inputs, _ := structpb.NewValue(map[string]any{"workerPoolSpecs": []any{map[string]any{"machineSpec": map[string]any{"machineType": "n1-standard-4"},
		"replicaCount": "1", "containerSpec": map[string]any{"imageUri": "local/train:1"}}}})
	if _, err := p.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "automl", TrainingTaskDefinition: "gs://google-cloud-aiplatform/schema/trainingjob/definition/automl_tabular_1.0.0.yaml", TrainingTaskInputs: inputs}}); codeOf(err) != codes.Unimplemented {
		t.Errorf("an AutoML pipeline = %v", err)
	}
	tp, err := p.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "custom", TrainingTaskDefinition: customTaskDefinition, TrainingTaskInputs: inputs}})
	if err != nil {
		t.Fatal(err)
	}
	var got *aiplatformpb.TrainingPipeline
	for i := 0; i < 500; i++ {
		got, _ = p.GetTrainingPipeline(ctx, &aiplatformpb.GetTrainingPipelineRequest{Name: tp.GetName()})
		if !trainingRunning(got.GetState()) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("pipeline = %v %v", got.GetState(), got.GetError())
	}
	backing := got.GetTrainingTaskMetadata().GetStructValue().GetFields()["backingCustomJob"].GetStringValue()
	if !strings.HasPrefix(backing, parent+"/customJobs/") {
		t.Errorf("backingCustomJob = %q", backing)
	}
	list, err := p.ListTrainingPipelines(ctx, &aiplatformpb.ListTrainingPipelinesRequest{Parent: parent})
	if err != nil || len(list.GetTrainingPipelines()) != 1 {
		t.Errorf("list = %v %v", list, err)
	}
	if _, err := p.DeleteTrainingPipeline(ctx, &aiplatformpb.DeleteTrainingPipelineRequest{Name: tp.GetName()}); err != nil {
		t.Error(err)
	}
}

// gcpcSpec is a pipeline of a GCPC CustomTrainingJobOp and a
// BigqueryQueryJobOp, as kfp compiles them (trimmed).
const gcpcSpec = `{
 "pipelineInfo": {"name": "gcpc"}, "schemaVersion": "2.1.0",
 "root": {"dag": {"tasks": {
   "custom": {"taskInfo": {"name": "custom"}, "componentRef": {"name": "comp-custom"},
     "inputs": {"parameters": {"display_name": {"runtimeValue": {"constant": "trainer"}},
       "worker_pool_specs": {"runtimeValue": {"constant": [{"machine_spec": {"machine_type": "n1-standard-4"}, "replica_count": 1, "container_spec": {"image_uri": "local/train:1", "args": ["ok"]}}]}}}}},
   "query": {"taskInfo": {"name": "query"}, "componentRef": {"name": "comp-query"},
     "inputs": {"parameters": {"query": {"runtimeValue": {"constant": "SELECT 1"}}}}}
 }}},
 "components": {
  "comp-custom": {"executorLabel": "exec-custom",
    "inputDefinitions": {"parameters": {"display_name": {"parameterType": "STRING"}, "worker_pool_specs": {"parameterType": "LIST"},
      "project": {"parameterType": "STRING", "defaultValue": "{{$.pipeline_google_cloud_project_id}}", "isOptional": true},
      "location": {"parameterType": "STRING", "defaultValue": "{{$.pipeline_google_cloud_location}}", "isOptional": true},
      "service_account": {"parameterType": "STRING", "defaultValue": "", "isOptional": true}}},
    "outputDefinitions": {"parameters": {"gcp_resources": {"parameterType": "STRING"}}}},
  "comp-query": {"executorLabel": "exec-query",
    "inputDefinitions": {"parameters": {"query": {"parameterType": "STRING"},
      "project": {"parameterType": "STRING", "defaultValue": "{{$.pipeline_google_cloud_project_id}}", "isOptional": true}}},
    "outputDefinitions": {"parameters": {"gcp_resources": {"parameterType": "STRING"}}, "artifacts": {"destination_table": {"artifactType": {"schemaTitle": "google.BQTable", "schemaVersion": "0.0.1"}}}}}
 },
 "deploymentSpec": {"executors": {
  "exec-custom": {"container": {"image": "gcr.io/ml-pipeline/google-cloud-pipeline-components:2.23.0",
    "command": ["python3", "-u", "-m", "google_cloud_pipeline_components.container.v1.custom_job.launcher"],
    "args": ["--type", "CustomJob", "--payload", "{\"display_name\": \"{{$.inputs.parameters['display_name']}}\", \"job_spec\": {\"worker_pool_specs\": {{$.inputs.parameters['worker_pool_specs']}}, \"service_account\": \"{{$.inputs.parameters['service_account']}}\", \"scheduling\": {\"strategy\": \"STANDARD\", \"max_wait_duration\": \"86400s\"}}}",
      "--project", "{{$.inputs.parameters['project']}}", "--location", "{{$.inputs.parameters['location']}}", "--gcp_resources", "{{$.outputs.parameters['gcp_resources'].output_file}}"]}},
  "exec-query": {"container": {"image": "gcr.io/ml-pipeline/google-cloud-pipeline-components:2.23.0",
    "command": ["python3", "-u", "-m", "google_cloud_pipeline_components.container.v1.bigquery.query_job.launcher"],
    "args": ["--type", "BigqueryQueryJob", "--project", "{{$.inputs.parameters['project']}}", "--location", "US", "--payload", "{\"configuration\": {\"query\": {}}}",
      "--job_configuration_query_override", "{\"query\": \"{{$.inputs.parameters['query']}}\", \"query_parameters\": [], \"destination_encryption_configuration\": {\"kmsKeyName\": \"\"}}",
      "--gcp_resources", "{{$.outputs.parameters['gcp_resources'].output_file}}", "--executor_input", "{{$}}"]}}
 }}
}`

func TestGCPCLaunchers(t *testing.T) {
	ctx := context.Background()
	s, r := newRichServer(t)
	p := &pipelineService{s: s}
	parent := "projects/p/locations/us-central1"
	j, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
		PipelineSpec: testSpec(t, gcpcSpec), RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://root/runs"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitPipeline(t, p, j.GetName())
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("job = %v %v %v", done.GetState(), done.GetError(), done.GetJobDetail())
	}
	d := map[string]*aiplatformpb.PipelineTaskDetail{}
	for _, t := range done.GetJobDetail().GetTaskDetails() {
		d[t.GetTaskName()] = t
	}
	res := d["custom"].GetExecution().GetMetadata().GetFields()["output:gcp_resources"].GetStringValue()
	if !strings.Contains(res, `"resourceType": "CustomJob"`) || !strings.Contains(res, "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/customJobs/") {
		t.Errorf("custom's gcp_resources = %s", res)
	}
	for _, run := range r.runs {
		if strings.Contains(run.Image, "google-cloud-pipeline-components") {
			t.Errorf("the GCPC image was run: %v", run)
		}
	}
	jobs, _ := (&jobService{s: s}).ListCustomJobs(ctx, &aiplatformpb.ListCustomJobsRequest{Parent: parent})
	if len(jobs.GetCustomJobs()) != 1 || jobs.GetCustomJobs()[0].GetDisplayName() != "trainer" {
		t.Errorf("custom jobs = %v", jobs)
	}
	q := d["query"]
	res = q.GetExecution().GetMetadata().GetFields()["output:gcp_resources"].GetStringValue()
	if !strings.Contains(res, `"resourceType": "BigQueryJob"`) || !strings.Contains(res, "https://www.googleapis.com/bigquery/v2/projects/p/jobs/job1?location=US") {
		t.Errorf("query's gcp_resources = %s", res)
	}
	tables := q.GetOutputs()["destination_table"].GetArtifacts()
	if len(tables) != 1 || tables[0].GetSchemaTitle() != "google.BQTable" || tables[0].GetUri() != "https://www.googleapis.com/bigquery/v2/projects/p/datasets/_anon/tables/anon1" ||
		tables[0].GetMetadata().GetFields()["tableId"].GetStringValue() != "anon1" {
		t.Errorf("destination_table = %v", tables)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(r.bqBodies[0]), &sent)
	qc := sent["configuration"].(map[string]any)["query"].(map[string]any)
	if qc["query"] != "SELECT 1" || qc["useLegacySql"] != false || qc["query_parameters"] != nil {
		t.Errorf("the job sent = %v", sent)
	}
}

func TestPipelineTemplates(t *testing.T) {
	ctx := context.Background()
	s, r := newRichServer(t)
	p := &pipelineService{s: s}
	parent := "projects/p/locations/us-central1"
	uri := "https://us-central1-kfp.pkg.dev/p/templates/hello-pipeline/v1"
	r.templates[uri] = testPipelineSpec
	j, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{TemplateUri: uri,
		RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://root/runs",
			ParameterValues: map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(j.GetTemplateMetadata().GetVersion(), "sha256:") || pipelineInfoName(j.GetPipelineSpec()) != "hello-pipeline" {
		t.Errorf("template job = %v %v", j.GetTemplateMetadata(), pipelineInfoName(j.GetPipelineSpec()))
	}
	if done := waitPipeline(t, p, j.GetName()); done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Errorf("template run = %v %v", done.GetState(), done.GetError())
	}
	if _, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
		TemplateUri: "https://us-central1-kfp.pkg.dev/p/templates/missing/v1"}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a missing template = %v", err)
	}
}

func TestBigQueryLauncherQueries(t *testing.T) {
	for _, c := range []struct {
		kind string
		args map[string]string
		want string
	}{
		{"BigqueryPredictModelJob", map[string]string{"model_name": "p.d.m", "table_name": "p.d.t", "threshold": "0.5"}, "SELECT * FROM ML.PREDICT(MODEL `p.d.m`, TABLE `p.d.t`, STRUCT(0.5 AS threshold))"},
		{"BigqueryEvaluateModelJob", map[string]string{"model_name": "p.d.m", "query_statement": "SELECT 1"}, "SELECT * FROM ML.EVALUATE(MODEL `p.d.m`, (SELECT 1))"},
		{"BigqueryMLForecastJob", map[string]string{"model_name": "p.d.m", "horizon": "3", "confidence_level": "0.9"}, "SELECT * FROM ML.FORECAST(MODEL `p.d.m` , STRUCT(3 AS horizon,0.9 AS confidence_level))"},
		{"BigqueryDropModelJob", map[string]string{"model_name": "p.d.m"}, "DROP MODEL `p.d.m`"},
		{"BigqueryMLCentroidsJob", map[string]string{"model_name": "p.d.m", "standardize": "true"}, "SELECT * FROM ML.CENTROIDS(MODEL `p.d.m`, STRUCT(True AS standardize))"},
	} {
		got, _, err := bqQuery(c.kind, c.args)
		if err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.kind, got, err, c.want)
		}
	}
	if _, _, err := bqQuery("BigqueryPredictModelJob", map[string]string{"model_name": "m"}); err == nil {
		t.Error("predict with neither table nor query: no error")
	}
	got, _ := json.Marshal(removeEmpty(map[string]any{"a": "", "b": 0.0, "c": map[string]any{"d": ""}, "e": []any{"x", map[string]any{}}, "f": true}))
	if string(got) != `{"e":["x"],"f":true}` {
		t.Errorf("removeEmpty = %s", got)
	}
}
