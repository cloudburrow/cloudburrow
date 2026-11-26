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

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// kfpPyImage is the Kubeflow Pipelines SDK fixture (testdata/kfppy): kfp
// and google-cloud-aiplatform, pinned and hash-locked; its pipelines'
// tasks run in it too.
const kfpPyImage = "dev.local/cloudburrow-kfppy:test"

func buildKFPPy(t *testing.T, ctx context.Context) {
	t.Helper()
	build := exec.CommandContext(ctx, "docker", "build", "-t", kfpPyImage, "testdata/kfppy")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the kfp fixture: %v\n%s", err, out)
	}
}

func detailsByName(j *aiplatformpb.PipelineJob) map[string][]*aiplatformpb.PipelineTaskDetail {
	out := map[string][]*aiplatformpb.PipelineTaskDetail{}
	for _, d := range j.GetJobDetail().GetTaskDetails() {
		out[d.GetTaskName()] = append(out[d.GetTaskName()], d)
	}
	return out
}

func taskOutput(d *aiplatformpb.PipelineTaskDetail, key string) *structpb.Value {
	return d.GetExecution().GetMetadata().GetFields()["output:"+key]
}

// covers: google.cloud.aiplatform.v1.PipelineService/CreatePipelineJob, google.cloud.aiplatform.v1.PipelineService/GetPipelineJob
//
// TestPythonKFPControlFlow (#2205): Google's Python SDKs, as Google's
// "Build a pipeline" guide uses them: kfp compiles a pipeline with an
// importer, a nested pipeline, dsl.If/Elif/Else on a task's output,
// dsl.ParallelFor (parallelism 2) with dsl.Collected and a component
// writing a Model and logging Metrics, and a pipeline with a
// dsl.ExitHandler around a task retried twice; aiplatform.PipelineJob
// runs them. The first run SUCCEEDS with the Elif branch taken, the other
// branches NOT_TRIGGERED, three loop iterations collected into total, the
// nested pipeline's output, and the Metrics in ML Metadata; the second
// (enable_caching=True) has every container task SKIPPED with a CACHED
// execution and the first run's outputs; with enable_caching=False the
// Else branch runs and nothing is cached; the exit-handler run FAILS
// naming exit-handler-1 after three attempts of the failing task, and the
// exit task SUCCEEDS with the FAILED final status. aiplatform.Artifact
// and aiplatform.Execution list them from ML Metadata. The SDK runs on an
// internal Docker network with no route out, and the tasks with no
// network.
func TestPythonKFPControlFlow(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	buildKFPPy(t, ctx)
	project := h.Project()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "kfp-cf-" + suffix
	dataformBucket(ctx, t, gcs, project, bucket)
	w := gcs.Bucket(bucket).Object("data/rows.csv").NewWriter(ctx)
	_, _ = w.Write([]byte("a,1\nb,2\n"))
	if err := w.Close(); err != nil {
		t.Fatalf("write the dataset: %v", err)
	}
	_, port, err := net.SplitHostPort(stripScheme(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	network := internalNetwork(t, ctx, kfpPyImage, port)
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(moduleRoot(t), "testdata", "kfppy", "flow.py")+":/flow/flow.py:ro",
		"-v", creds+":/creds/adc.json:ro",
		"-e", "CB_ENDPOINT=http://host.docker.internal:"+port,
		"-e", "CB_PROJECT="+project, "-e", "CB_LOCATION=us-central1",
		"-e", "CB_CREDENTIALS=/creds/adc.json", "-e", "CB_TASK_IMAGE="+kfpPyImage,
		"-e", "CB_PIPELINE_ROOT=gs://"+bucket+"/root", "-e", "CB_DATA_URI=gs://"+bucket+"/data/rows.csv",
		"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=cloudburrow.localhost",
		kfpPyImage, "/flow/flow.py")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the kfp flow: %v\n%s", err, out)
	}
	var res struct {
		First, Cached, Uncached, Exit json.RawMessage
		Metrics                       []struct {
			Name     string
			Metadata map[string]any
		}
		Models           []string
		CachedExecutions int `json:"cached_executions"`
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
	first, cached, uncached, exitRun := job("first", res.First), job("cached", res.Cached), job("uncached", res.Uncached), job("exit", res.Exit)
	if first.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the first run = %v %v\n%s", first.GetState(), first.GetError(), out)
	}
	ft := detailsByName(first)
	state := func(d map[string][]*aiplatformpb.PipelineTaskDetail, name string) aiplatformpb.PipelineTaskDetail_State {
		if len(d[name]) == 0 {
			return aiplatformpb.PipelineTaskDetail_STATE_UNSPECIFIED
		}
		return d[name][0].GetState()
	}
	for name, want := range map[string]aiplatformpb.PipelineTaskDetail_State{
		"importer": aiplatformpb.PipelineTaskDetail_SUCCEEDED, "flip": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"condition-branches-1": aiplatformpb.PipelineTaskDetail_SUCCEEDED, "condition-2": aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED,
		"condition-3": aiplatformpb.PipelineTaskDetail_SUCCEEDED, "say-tails": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"condition-4": aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED, "for-loop-5": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"total": aiplatformpb.PipelineTaskDetail_SUCCEEDED, "train": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"cb-inner": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
	} {
		if got := state(ft, name); got != want {
			t.Errorf("first run: task %s = %v, want %v", name, got, want)
		}
	}
	if n := len(ft["square"]); n != 4 {
		t.Errorf("first run: %d square tasks, want 3 loop iterations and the nested pipeline's", n)
	}
	if got := taskOutput(ft["total"][0], "Output").GetNumberValue(); got != 14 {
		t.Errorf("total = %v, want 14 (the loop's squares, collected)", got)
	}
	if got := taskOutput(ft["cb-inner"][0], "Output").GetNumberValue(); got != 1 {
		t.Errorf("the nested pipeline's output = %v, want 1", got)
	}
	if got := taskOutput(ft["condition-branches-1"][0], "pipelinechannel--say-2-Output"); got != nil && got.GetStringValue() != "T" {
		t.Errorf("the branches' output = %v", got)
	}
	metrics := ft["train"][0].GetOutputs()["metrics"].GetArtifacts()
	if len(metrics) != 1 || metrics[0].GetSchemaTitle() != "system.Metrics" || metrics[0].GetMetadata().GetFields()["accuracy"].GetNumberValue() != 0.9 {
		t.Errorf("train's metrics = %v", metrics)
	}
	if first.GetJobDetail().GetPipelineRunContext().GetSchemaTitle() != "system.PipelineRun" {
		t.Errorf("the run's context = %v", first.GetJobDetail().GetPipelineRunContext())
	}

	ct := detailsByName(cached)
	if cached.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Errorf("the cached run = %v %v", cached.GetState(), cached.GetError())
	}
	for _, name := range []string{"flip", "say-tails", "total", "train"} {
		if len(ct[name]) == 0 || ct[name][0].GetState() != aiplatformpb.PipelineTaskDetail_SKIPPED || ct[name][0].GetExecution().GetState() != aiplatformpb.Execution_CACHED {
			t.Errorf("cached run: task %s = %v, want SKIPPED with a CACHED execution", name, ct[name])
		}
	}
	if len(ct["train"]) > 0 && ct["train"][0].GetOutputs()["model"].GetArtifacts()[0].GetName() != ft["train"][0].GetOutputs()["model"].GetArtifacts()[0].GetName() {
		t.Errorf("the cached train's model is not the first run's")
	}

	ut := detailsByName(uncached)
	if uncached.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || state(ut, "condition-4") != aiplatformpb.PipelineTaskDetail_SUCCEEDED ||
		state(ut, "say-edge") != aiplatformpb.PipelineTaskDetail_SUCCEEDED || state(ut, "condition-3") != aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED {
		t.Errorf("the uncached run (seed 2, Else) = %v, edge %v, tails %v", uncached.GetState(), state(ut, "say-edge"), state(ut, "condition-3"))
	}
	for _, d := range uncached.GetJobDetail().GetTaskDetails() {
		if d.GetState() == aiplatformpb.PipelineTaskDetail_SKIPPED {
			t.Errorf("enable_caching=False: task %s was SKIPPED", d.GetTaskName())
		}
	}

	et := detailsByName(exitRun)
	if exitRun.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED ||
		exitRun.GetError().GetMessage() != "The DAG failed because some tasks failed. The failed tasks are: [exit-handler-1]." {
		t.Errorf("the exit-handler run = %v %q", exitRun.GetState(), exitRun.GetError().GetMessage())
	}
	if len(et["boom"]) == 1 {
		failures := 0
		for _, s := range et["boom"][0].GetPipelineTaskStatus() {
			if s.GetState() == aiplatformpb.PipelineTaskDetail_FAILED {
				failures++
			}
		}
		if failures != 3 || et["boom"][0].GetState() != aiplatformpb.PipelineTaskDetail_FAILED {
			t.Errorf("boom = %v with %d failed attempts, want FAILED after 3", et["boom"][0].GetState(), failures)
		}
	} else {
		t.Errorf("the exit-handler run has no boom task")
	}
	if state(et, "notify") != aiplatformpb.PipelineTaskDetail_SUCCEEDED || !strings.HasPrefix(taskOutput(et["notify"][0], "Output").GetStringValue(), "FAILED exit-handler-1") {
		t.Errorf("notify = %v %v, want SUCCEEDED with the handler's FAILED status", state(et, "notify"), et["notify"])
	}

	found := false
	for _, m := range res.Metrics {
		if m.Metadata["accuracy"] == 0.9 {
			found = true
		}
	}
	if !found || len(res.Models) == 0 || res.CachedExecutions < 4 {
		t.Errorf("ML Metadata through the SDK: metrics %v, models %v, %d cached executions", res.Metrics, res.Models, res.CachedExecutions)
	}
}

// resolverSpec2205 is a hand-written KFP IR (the kfp SDK has no resolver
// DSL): a resolver of the newest system.Model with a label, a task that
// takes it and the pipeline's own input artifact, and a loop over that
// artifact list.
func resolverSpec2205(image, label string) *structpb.Struct {
	raw := `{
 "pipelineInfo": {"name": "cb-resolver"}, "schemaVersion": "2.1.0",
 "root": {
  "inputDefinitions": {"artifacts": {"seed": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}},
  "dag": {"tasks": {
   "resolve": {"taskInfo": {"name": "resolve"}, "componentRef": {"name": "comp-resolve"}},
   "use": {"taskInfo": {"name": "use"}, "componentRef": {"name": "comp-use"},
     "inputs": {"artifacts": {"m": {"taskOutputArtifact": {"producerTask": "resolve", "outputArtifactKey": "model"}}, "d": {"componentInputArtifact": "seed"}}}}
  }}
 },
 "components": {
  "comp-resolve": {"executorLabel": "exec-resolve", "outputDefinitions": {"artifacts": {"model": {"artifactType": {"schemaTitle": "system.Model"}}}}},
  "comp-use": {"executorLabel": "exec-use",
   "inputDefinitions": {"artifacts": {"m": {"artifactType": {"schemaTitle": "system.Model"}}, "d": {"artifactType": {"schemaTitle": "system.Dataset"}}}},
   "outputDefinitions": {"parameters": {"seen": {"parameterType": "STRING"}}}}
 },
 "deploymentSpec": {"executors": {
  "exec-resolve": {"resolver": {"outputArtifactQueries": {"model": {"filter": "schema_title=\"system.Model\" AND display_name=\"LABEL\"", "limit": 1}}}},
  "exec-use": {"container": {"image": "IMAGE", "command": ["sh", "-c", "printf '%s %s' \"$0\" \"$1\" > \"$2\""],
    "args": ["{{$.inputs.artifacts['m'].uri}}", "{{$.inputs.artifacts['d'].uri}}", "{{$.outputs.parameters['seen'].output_file}}"]}}
 }}
}`
	s := &structpb.Struct{}
	_ = protojson.Unmarshal([]byte(strings.NewReplacer("IMAGE", image, "LABEL", label).Replace(raw)), s)
	return s
}

// covers: google.cloud.aiplatform.v1.PipelineService/CreatePipelineJob, google.cloud.aiplatform.v1.PipelineService/GetPipelineJob, google.cloud.aiplatform.v1.PipelineService/CancelPipelineJob, google.cloud.aiplatform.v1.MetadataService/CreateArtifact, google.cloud.aiplatform.v1.MetadataService/QueryExecutionInputsAndOutputs
//
// TestVertexAIPipelineResolverAndLimits (#2205), with the official Go
// PipelineClient and MetadataClient: a resolver answers the newest
// matching artifact in ML Metadata and a task takes it with the run's own
// input artifact (runtime_config.input_artifacts, by artifact ID); the
// task's execution in ML Metadata has both as INPUT events; a resolver
// matching nothing FAILS its task; a loop cancelled mid-run is CANCELLED;
// a customJob executor, a bad trigger condition, an exit-task input on a
// task that is not one, and a loop of more than 10,000 tasks are refused
// or FAIL as Google documents.
func TestVertexAIPipelineResolverAndLimits(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	gcs := dataformStorage(t, h)
	image := pipelineImage(t)
	c := pipelineClient(t, h, endpoint, "")
	md, err := aiplatform.NewMetadataClient(h.Context(), option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	store := parent + "/metadataStores/default"
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "kfp-rs-" + suffix
	dataformBucket(ctx, t, gcs, project, bucket)
	if _, err := md.GetMetadataStore(ctx, &aiplatformpb.GetMetadataStoreRequest{Name: store}); err != nil {
		t.Fatalf("the default metadata store: %v", err)
	}
	label := "model-" + suffix
	var model, seed *aiplatformpb.Artifact
	for i, uri := range []string{"gs://" + bucket + "/models/old", "gs://" + bucket + "/models/new"} {
		a, err := md.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, Artifact: &aiplatformpb.Artifact{
			DisplayName: label, SchemaTitle: "system.Model", Uri: uri, State: aiplatformpb.Artifact_LIVE}})
		if err != nil {
			t.Fatalf("CreateArtifact %d: %v", i, err)
		}
		model = a
		time.Sleep(20 * time.Millisecond)
	}
	seed, err = md.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, Artifact: &aiplatformpb.Artifact{
		DisplayName: "seed", SchemaTitle: "system.Dataset", Uri: "gs://" + bucket + "/data/seed", State: aiplatformpb.Artifact_LIVE}})
	if err != nil {
		t.Fatal(err)
	}
	create := func(spec *structpb.Struct, inputs map[string]*aiplatformpb.PipelineJob_RuntimeConfig_InputArtifact) (*aiplatformpb.PipelineJob, error) {
		return c.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
			DisplayName: "resolver " + suffix, PipelineSpec: spec,
			RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://" + bucket + "/root", InputArtifacts: inputs}}})
	}
	seedIn := map[string]*aiplatformpb.PipelineJob_RuntimeConfig_InputArtifact{"seed": {Kind: &aiplatformpb.PipelineJob_RuntimeConfig_InputArtifact_ArtifactId{
		ArtifactId: seed.GetName()[strings.LastIndex(seed.GetName(), "/")+1:]}}}
	j, err := create(resolverSpec2205(image, label), seedIn)
	if err != nil {
		t.Fatalf("CreatePipelineJob: %v", err)
	}
	done := waitPipelineJob(ctx, t, c, j.GetName())
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the resolver run = %v %v", done.GetState(), done.GetError())
	}
	use := detailsByName(done)["use"][0]
	if got := taskOutput(use, "seen").GetStringValue(); got != model.GetUri()+" "+seed.GetUri() {
		t.Errorf("use saw %q, want the newest model %s and the seed %s", got, model.GetUri(), seed.GetUri())
	}
	io, err := md.QueryExecutionInputsAndOutputs(ctx, &aiplatformpb.QueryExecutionInputsAndOutputsRequest{Execution: use.GetExecution().GetName()})
	if err != nil {
		t.Fatalf("QueryExecutionInputsAndOutputs: %v", err)
	}
	inputs := map[string]bool{}
	for _, e := range io.GetEvents() {
		if e.GetType() == aiplatformpb.Event_INPUT {
			inputs[e.GetArtifact()] = true
		}
	}
	if !inputs[model.GetName()] || !inputs[seed.GetName()] {
		t.Errorf("use's INPUT events = %v, want %s and %s", io.GetEvents(), model.GetName(), seed.GetName())
	}

	j, err = create(resolverSpec2205(image, "nothing-"+suffix), seedIn)
	if err != nil {
		t.Fatal(err)
	}
	if failed := waitPipelineJob(ctx, t, c, j.GetName()); failed.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED ||
		!strings.Contains(detailsByName(failed)["resolve"][0].GetError().GetMessage(), "no artifact matches") {
		t.Errorf("a resolver matching nothing = %v %v", failed.GetState(), failed.GetJobDetail())
	}
	if _, err := create(resolverSpec2205(image, label), map[string]*aiplatformpb.PipelineJob_RuntimeConfig_InputArtifact{
		"seed": {Kind: &aiplatformpb.PipelineJob_RuntimeConfig_InputArtifact_ArtifactId{ArtifactId: "none"}}}); err == nil {
		if failed := waitPipelineJob(ctx, t, c, j.GetName()); failed.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED {
			t.Errorf("an input artifact that does not exist = %v", failed.GetState())
		}
	}

	// A slow loop, cancelled: the run and the loop are CANCELLED.
	loop := strings.NewReplacer("IMAGE", image).Replace(`{
 "pipelineInfo": {"name": "cb-slow-loop"}, "schemaVersion": "2.1.0",
 "root": {"dag": {"tasks": {"loop": {"taskInfo": {"name": "loop"}, "componentRef": {"name": "comp-loop"},
   "parameterIterator": {"items": {"raw": "[1, 2, 3, 4]"}, "itemInput": "item"}, "iteratorPolicy": {"parallelismLimit": 1}}}}},
 "components": {
  "comp-loop": {"inputDefinitions": {"parameters": {"item": {"parameterType": "NUMBER_INTEGER"}}},
   "dag": {"tasks": {"wait": {"taskInfo": {"name": "wait"}, "componentRef": {"name": "comp-wait"}, "inputs": {"parameters": {"n": {"componentInputParameter": "item"}}}}}}},
  "comp-wait": {"executorLabel": "exec-wait", "inputDefinitions": {"parameters": {"n": {"parameterType": "NUMBER_INTEGER"}}}}
 },
 "deploymentSpec": {"executors": {"exec-wait": {"container": {"image": "IMAGE", "command": ["sleep", "30"]}}}}
}`)
	ls := &structpb.Struct{}
	if err := protojson.Unmarshal([]byte(loop), ls); err != nil {
		t.Fatal(err)
	}
	j, err = create(ls, nil)
	if err != nil {
		t.Fatalf("CreatePipelineJob of the loop: %v", err)
	}
	for i := 0; i < 60; i++ {
		got, _ := c.GetPipelineJob(ctx, &aiplatformpb.GetPipelineJobRequest{Name: j.GetName()})
		if len(detailsByName(got)["wait"]) > 0 && detailsByName(got)["wait"][0].GetState() == aiplatformpb.PipelineTaskDetail_RUNNING {
			if n := len(detailsByName(got)["wait"]); n != 1 {
				t.Errorf("with parallelism 1, %d iterations started at once", n)
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := c.CancelPipelineJob(ctx, &aiplatformpb.CancelPipelineJobRequest{Name: j.GetName()}); err != nil {
		t.Fatalf("CancelPipelineJob: %v", err)
	}
	cancelled := waitPipelineJob(ctx, t, c, j.GetName())
	if cancelled.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED || detailsByName(cancelled)["loop"][0].GetState() != aiplatformpb.PipelineTaskDetail_CANCELLED {
		t.Errorf("the cancelled loop = %v, loop %v", cancelled.GetState(), detailsByName(cancelled)["loop"])
	}

	bad := func(from, to string) *structpb.Struct {
		s := &structpb.Struct{}
		_ = protojson.Unmarshal([]byte(strings.Replace(loop, from, to, 1)), s)
		return s
	}
	for what, spec := range map[string]*structpb.Struct{
		"a customJob executor": bad(`{"container": {"image": "`+image+`", "command": ["sleep", "30"]}}`, `{"customJob": {"customJob": {"displayName": "x"}}}`),
		"a bad condition":      bad(`"iteratorPolicy"`, `"triggerPolicy": {"condition": "inputs.parameter_values['x'] =="}, "iteratorPolicy"`),
		"a final status input": bad(`"inputs": {"parameters": {"n": {"componentInputParameter": "item"}}}`, `"inputs": {"parameters": {"n": {"taskFinalStatus": {"producerTask": "wait"}}}}`),
	} {
		if _, err := create(spec, nil); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreatePipelineJob with %s = %v, want INVALID_ARGUMENT", what, err)
		}
	}
	items, _ := json.Marshal(make([]int, 10001))
	quoted, _ := json.Marshal(string(items))
	j, err = create(bad(`"[1, 2, 3, 4]"`, string(quoted)), nil)
	if err != nil {
		t.Fatalf("CreatePipelineJob of a 10,001-item loop: %v", err)
	}
	if big := waitPipelineJob(ctx, t, c, j.GetName()); big.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED ||
		!strings.Contains(detailsByName(big)["loop"][0].GetError().GetMessage(), "10000 pipeline tasks") {
		t.Errorf("a loop past 10,000 tasks = %v %v", big.GetState(), detailsByName(big)["loop"])
	}
}
