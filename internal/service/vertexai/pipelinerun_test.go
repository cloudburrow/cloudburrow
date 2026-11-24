package vertexai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
)

// controlFlowSpec is a KFP IR as kfp 2's compiler writes it for a
// pipeline with an ExitHandler around an importer, an If/Else
// (condition-branches with a OneOf output), a ParallelFor (parallelism 2,
// its outputs Collected), a retried task and a cached task, and an exit
// task taking the handler's final status.
const controlFlowSpec = `{
 "pipelineInfo": {"name": "control-flow"},
 "schemaVersion": "2.1.0", "sdkVersion": "kfp-2.14.3",
 "root": {
  "inputDefinitions": {"parameters": {"flip": {"parameterType": "STRING", "defaultValue": "heads"}, "xs": {"parameterType": "LIST", "defaultValue": [1, 2, 3]}}},
  "dag": {"tasks": {
   "exit-handler-1": {"taskInfo": {"name": "exit-handler-1"}, "componentRef": {"name": "comp-exit-handler-1"},
     "inputs": {"parameters": {"pipelinechannel--flip": {"componentInputParameter": "flip"}, "pipelinechannel--xs": {"componentInputParameter": "xs"}}}},
   "notify": {"taskInfo": {"name": "notify"}, "componentRef": {"name": "comp-notify"}, "dependentTasks": ["exit-handler-1"],
     "triggerPolicy": {"strategy": "ALL_UPSTREAM_TASKS_COMPLETED"},
     "inputs": {"parameters": {"status": {"taskFinalStatus": {"producerTask": "exit-handler-1"}}}}}
  }}
 },
 "components": {
  "comp-exit-handler-1": {
   "inputDefinitions": {"parameters": {"pipelinechannel--flip": {"parameterType": "STRING"}, "pipelinechannel--xs": {"parameterType": "LIST"}}},
   "dag": {"tasks": {
    "importer": {"taskInfo": {"name": "importer"}, "componentRef": {"name": "comp-importer"},
      "inputs": {"parameters": {"uri": {"runtimeValue": {"constant": "gs://root/data/seed.csv"}}}}},
    "condition-branches-1": {"taskInfo": {"name": "condition-branches-1"}, "componentRef": {"name": "comp-condition-branches-1"},
      "inputs": {"parameters": {"pipelinechannel--flip": {"componentInputParameter": "pipelinechannel--flip"}}}},
    "for-loop-2": {"taskInfo": {"name": "for-loop-2"}, "componentRef": {"name": "comp-for-loop-2"},
      "parameterIterator": {"items": {"inputParameter": "pipelinechannel--xs"}, "itemInput": "pipelinechannel--xs-loop-item"},
      "iteratorPolicy": {"parallelismLimit": 2},
      "inputs": {"parameters": {"pipelinechannel--xs": {"componentInputParameter": "pipelinechannel--xs"}}}},
    "sum": {"taskInfo": {"name": "sum"}, "componentRef": {"name": "comp-sum"}, "dependentTasks": ["for-loop-2"],
      "inputs": {"parameters": {"vals": {"taskOutputParameter": {"producerTask": "for-loop-2", "outputParameterKey": "pipelinechannel--echo-Output"}}}}},
    "flaky": {"taskInfo": {"name": "flaky"}, "componentRef": {"name": "comp-flaky"},
      "retryPolicy": {"maxRetryCount": 2, "backoffDuration": "0s", "backoffFactor": 2, "backoffMaxDuration": "3600s"}},
    "train": {"taskInfo": {"name": "train"}, "componentRef": {"name": "comp-train"}, "dependentTasks": ["importer"],
      "cachingOptions": {"enableCache": true},
      "inputs": {"artifacts": {"data": {"taskOutputArtifact": {"producerTask": "importer", "outputArtifactKey": "artifact"}}}}}
   }}
  },
  "comp-importer": {"executorLabel": "exec-importer",
   "inputDefinitions": {"parameters": {"uri": {"parameterType": "STRING"}}},
   "outputDefinitions": {"artifacts": {"artifact": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}}},
  "comp-condition-branches-1": {
   "inputDefinitions": {"parameters": {"pipelinechannel--flip": {"parameterType": "STRING"}}},
   "outputDefinitions": {"parameters": {"pipelinechannel--branch": {"parameterType": "STRING"}}},
   "dag": {
    "outputs": {"parameters": {"pipelinechannel--branch": {"valueFromOneof": {"parameterSelectors": [
      {"producerSubtask": "condition-2", "outputParameterKey": "pipelinechannel--heads-Output"},
      {"producerSubtask": "condition-3", "outputParameterKey": "pipelinechannel--tails-Output"}]}}}},
    "tasks": {
     "condition-2": {"taskInfo": {"name": "condition-2"}, "componentRef": {"name": "comp-condition-2"},
       "triggerPolicy": {"condition": "inputs.parameter_values['pipelinechannel--flip'] == 'heads'"},
       "inputs": {"parameters": {"pipelinechannel--flip": {"componentInputParameter": "pipelinechannel--flip"}}}},
     "condition-3": {"taskInfo": {"name": "condition-3"}, "componentRef": {"name": "comp-condition-3"},
       "triggerPolicy": {"condition": "!(inputs.parameter_values['pipelinechannel--flip'] == 'heads')"},
       "inputs": {"parameters": {"pipelinechannel--flip": {"componentInputParameter": "pipelinechannel--flip"}}}}
    }}
  },
  "comp-condition-2": {
   "inputDefinitions": {"parameters": {"pipelinechannel--flip": {"parameterType": "STRING"}}},
   "outputDefinitions": {"parameters": {"pipelinechannel--heads-Output": {"parameterType": "STRING"}}},
   "dag": {"outputs": {"parameters": {"pipelinechannel--heads-Output": {"valueFromParameter": {"producerSubtask": "heads", "outputParameterKey": "Output"}}}},
    "tasks": {"heads": {"taskInfo": {"name": "heads"}, "componentRef": {"name": "comp-say"}, "inputs": {"parameters": {"msg": {"runtimeValue": {"constant": "H"}}}}}}}
  },
  "comp-condition-3": {
   "inputDefinitions": {"parameters": {"pipelinechannel--flip": {"parameterType": "STRING"}}},
   "outputDefinitions": {"parameters": {"pipelinechannel--tails-Output": {"parameterType": "STRING"}}},
   "dag": {"outputs": {"parameters": {"pipelinechannel--tails-Output": {"valueFromParameter": {"producerSubtask": "tails", "outputParameterKey": "Output"}}}},
    "tasks": {"tails": {"taskInfo": {"name": "tails"}, "componentRef": {"name": "comp-fail"}}}}
  },
  "comp-for-loop-2": {
   "inputDefinitions": {"parameters": {"pipelinechannel--xs": {"parameterType": "LIST"}, "pipelinechannel--xs-loop-item": {"parameterType": "NUMBER_INTEGER"}}},
   "outputDefinitions": {"parameters": {"pipelinechannel--echo-Output": {"parameterType": "LIST"}}},
   "dag": {"outputs": {"parameters": {"pipelinechannel--echo-Output": {"valueFromParameter": {"producerSubtask": "echo", "outputParameterKey": "Output"}}}},
    "tasks": {"echo": {"taskInfo": {"name": "echo"}, "componentRef": {"name": "comp-echo"},
      "inputs": {"parameters": {"x": {"componentInputParameter": "pipelinechannel--xs-loop-item"}}}}}}
  },
  "comp-say": {"executorLabel": "exec-say", "inputDefinitions": {"parameters": {"msg": {"parameterType": "STRING"}}},
   "outputDefinitions": {"parameters": {"Output": {"parameterType": "STRING"}}}},
  "comp-fail": {"executorLabel": "exec-fail", "outputDefinitions": {"parameters": {"Output": {"parameterType": "STRING"}}}},
  "comp-echo": {"executorLabel": "exec-echo", "inputDefinitions": {"parameters": {"x": {"parameterType": "NUMBER_INTEGER"}}},
   "outputDefinitions": {"parameters": {"Output": {"parameterType": "NUMBER_INTEGER"}}}},
  "comp-sum": {"executorLabel": "exec-sum", "inputDefinitions": {"parameters": {"vals": {"parameterType": "LIST"}}},
   "outputDefinitions": {"parameters": {"Output": {"parameterType": "STRING"}}}},
  "comp-flaky": {"executorLabel": "exec-flaky"},
  "comp-train": {"executorLabel": "exec-train",
   "inputDefinitions": {"artifacts": {"data": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}},
   "outputDefinitions": {"artifacts": {"model": {"artifactType": {"schemaTitle": "system.Model", "schemaVersion": "0.0.1"}},
     "metrics": {"artifactType": {"schemaTitle": "system.Metrics", "schemaVersion": "0.0.1"}}}}},
  "comp-notify": {"executorLabel": "exec-notify", "inputDefinitions": {"parameters": {"status": {"parameterType": "TASK_FINAL_STATUS"}}},
   "outputDefinitions": {"parameters": {"Output": {"parameterType": "STRING"}}}}
 },
 "deploymentSpec": {"executors": {
  "exec-importer": {"importer": {"artifactUri": {"runtimeParameter": "uri"}, "typeSchema": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}},
  "exec-say": {"container": {"image": "local/tool:1", "args": ["write:{{$.outputs.parameters['Output'].output_file}}={{$.inputs.parameters['msg']}}"]}},
  "exec-fail": {"container": {"image": "local/tool:1", "args": ["fail"]}},
  "exec-echo": {"container": {"image": "local/tool:1", "args": ["write:{{$.outputs.parameters['Output'].output_file}}={{$.inputs.parameters['x']}}"]}},
  "exec-sum": {"container": {"image": "local/tool:1", "args": ["write:{{$.outputs.parameters['Output'].output_file}}={{$.inputs.parameters['vals']}}"]}},
  "exec-flaky": {"container": {"image": "local/tool:1", "args": ["flaky:f1:2"]}},
  "exec-train": {"container": {"image": "local/tool:1", "args": ["write:{{$.outputs.artifacts['model'].path}}=weights",
    "write:/cb-outputs/executor_output.json={\"artifacts\": {\"metrics\": {\"artifacts\": [{\"metadata\": {\"accuracy\": 0.9}}]}}}"]}},
  "exec-notify": {"container": {"image": "local/tool:1", "args": ["write:{{$.outputs.parameters['Output'].output_file}}={{$.inputs.parameters['status']}}"]}}
 }}
}`

func detailsByUnique(j *aiplatformpb.PipelineJob) map[string]*aiplatformpb.PipelineTaskDetail {
	out := map[string]*aiplatformpb.PipelineTaskDetail{}
	for _, d := range j.GetJobDetail().GetTaskDetails() {
		out[d.GetTaskUniqueName()] = d
	}
	return out
}

func TestPipelineControlFlow(t *testing.T) {
	ctx := context.Background()
	s, r := newPipelineServer(t)
	p := &pipelineService{s: s}
	md := &metadataService{s: s}
	parent := "projects/p/locations/us-central1"
	run := func(flip string) *aiplatformpb.PipelineJob {
		t.Helper()
		j, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
			PipelineSpec: testSpec(t, controlFlowSpec), RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://root/runs",
				ParameterValues: map[string]*structpb.Value{"flip": structpb.NewStringValue(flip)}}}})
		if err != nil {
			t.Fatal(err)
		}
		return waitPipeline(t, p, j.GetName())
	}
	done := run("heads")
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the run = %v %v\n%v", done.GetState(), done.GetError(), done.GetJobDetail())
	}
	d := detailsByUnique(done)
	want := map[string]aiplatformpb.PipelineTaskDetail_State{
		"exit-handler-1": aiplatformpb.PipelineTaskDetail_SUCCEEDED, "notify": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.importer": aiplatformpb.PipelineTaskDetail_SUCCEEDED, "exit-handler-1.condition-branches-1": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.condition-branches-1.condition-2":       aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.condition-branches-1.condition-2.heads": aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.condition-branches-1.condition-3":       aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED,
		"exit-handler-1.for-loop-2":                             aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.for-loop-2.0.echo":                      aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.for-loop-2.2.echo":                      aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.sum":                                    aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.flaky":                                  aiplatformpb.PipelineTaskDetail_SUCCEEDED,
		"exit-handler-1.train":                                  aiplatformpb.PipelineTaskDetail_SUCCEEDED,
	}
	for n, st := range want {
		if d[n].GetState() != st {
			t.Errorf("task %s = %v, want %v", n, d[n].GetState(), st)
		}
	}
	if d["exit-handler-1.for-loop-2.0.echo"].GetParentTaskId() != d["exit-handler-1.for-loop-2.0"].GetTaskId() ||
		d["exit-handler-1.for-loop-2.0"].GetParentTaskId() != d["exit-handler-1.for-loop-2"].GetTaskId() ||
		d["exit-handler-1.for-loop-2"].GetParentTaskId() != d["exit-handler-1"].GetTaskId() {
		t.Errorf("the loop's details are not nested")
	}
	if got := d["exit-handler-1.sum"].GetExecution().GetMetadata().GetFields()["input:vals"]; got.GetListValue() == nil || len(got.GetListValue().GetValues()) != 3 {
		t.Errorf("sum took %v, want the loop's three collected outputs", got)
	}
	if got := d["exit-handler-1.condition-branches-1"].GetExecution().GetMetadata().GetFields()["output:pipelinechannel--branch"].GetStringValue(); got != "H" {
		t.Errorf("the OneOf output = %q, want H", got)
	}
	var status map[string]any
	out := d["notify"].GetExecution().GetMetadata().GetFields()["output:Output"].GetStringValue()
	if err := json.Unmarshal([]byte(out), &status); err != nil || status["state"] != "SUCCEEDED" || status["pipelineTaskName"] != "exit-handler-1" ||
		status["pipelineJobResourceName"] != done.GetName() {
		t.Errorf("notify's final status = %s (%v)", out, err)
	}
	flaky := d["exit-handler-1.flaky"]
	var failedAttempts int
	for _, st := range flaky.GetPipelineTaskStatus() {
		if st.GetState() == aiplatformpb.PipelineTaskDetail_FAILED {
			failedAttempts++
		}
	}
	if failedAttempts != 2 {
		t.Errorf("flaky's status history has %d failed attempts, want 2: %v", failedAttempts, flaky.GetPipelineTaskStatus())
	}
	train := d["exit-handler-1.train"]
	metrics := train.GetOutputs()["metrics"].GetArtifacts()
	if len(metrics) != 1 || metrics[0].GetSchemaTitle() != "system.Metrics" || metrics[0].GetMetadata().GetFields()["accuracy"].GetNumberValue() != 0.9 ||
		!strings.Contains(metrics[0].GetName(), "/metadataStores/default/artifacts/") {
		t.Errorf("train's metrics = %v", metrics)
	}
	if train.GetExecution().GetState() != aiplatformpb.Execution_COMPLETE {
		t.Errorf("train's execution = %v", train.GetExecution())
	}
	if done.GetJobDetail().GetPipelineRunContext().GetSchemaTitle() != "system.PipelineRun" || done.GetJobDetail().GetPipelineContext().GetSchemaTitle() != "system.Pipeline" {
		t.Errorf("the run's contexts = %v", done.GetJobDetail())
	}
	if _, err := md.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: metrics[0].GetName()}); err != nil {
		t.Errorf("the metrics artifact is not in ML Metadata: %v", err)
	}
	countRuns := func(task string) int {
		r.mu.Lock()
		defer r.mu.Unlock()
		n := 0
		for _, c := range r.runs {
			if c.Labels["cloudburrow.pipeline-task"] == task {
				n++
			}
		}
		return n
	}
	if n := countRuns("train"); n != 1 {
		t.Errorf("train ran %d times", n)
	}

	// The second run: the importer reuses its artifact, so train's cache
	// key matches and it is SKIPPED, its execution CACHED, its outputs the
	// first run's.
	again := run("heads")
	d2 := detailsByUnique(again)
	if again.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || d2["exit-handler-1.train"].GetState() != aiplatformpb.PipelineTaskDetail_SKIPPED ||
		d2["exit-handler-1.train"].GetExecution().GetState() != aiplatformpb.Execution_CACHED {
		t.Errorf("the second run's train = %v, %v", d2["exit-handler-1.train"].GetState(), d2["exit-handler-1.train"].GetExecution())
	}
	if got := d2["exit-handler-1.train"].GetOutputs()["model"].GetArtifacts(); len(got) != 1 || got[0].GetName() != train.GetOutputs()["model"].GetArtifacts()[0].GetName() {
		t.Errorf("the cached train's model = %v", got)
	}
	if n := countRuns("train"); n != 1 {
		t.Errorf("train ran %d times; the second run should have used the cache", n)
	}
	// Deleting the cached execution from ML Metadata drops the entry.
	if _, err := md.DeleteExecution(ctx, &aiplatformpb.DeleteExecutionRequest{Name: train.GetExecution().GetName()}); err != nil {
		t.Fatal(err)
	}
	third := run("heads")
	if st := detailsByUnique(third)["exit-handler-1.train"].GetState(); st != aiplatformpb.PipelineTaskDetail_SUCCEEDED || countRuns("train") != 2 {
		t.Errorf("after deleting the cached execution train = %v, ran %d times", st, countRuns("train"))
	}

	// Tails: the Else branch runs and fails; the exit task still runs,
	// taking the handler's FAILED status, and the run FAILS naming the
	// handler.
	tails := run("tails")
	dt := detailsByUnique(tails)
	if tails.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED || dt["exit-handler-1"].GetState() != aiplatformpb.PipelineTaskDetail_FAILED ||
		dt["exit-handler-1.condition-branches-1.condition-2"].GetState() != aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED ||
		dt["exit-handler-1.condition-branches-1.condition-3.tails"].GetState() != aiplatformpb.PipelineTaskDetail_FAILED {
		t.Errorf("the tails run = %v\n%v", tails.GetState(), tails.GetJobDetail())
	}
	if msg := tails.GetError().GetMessage(); msg != "The DAG failed because some tasks failed. The failed tasks are: [exit-handler-1]." {
		t.Errorf("the tails run's error = %q", msg)
	}
	out = dt["notify"].GetExecution().GetMetadata().GetFields()["output:Output"].GetStringValue()
	if err := json.Unmarshal([]byte(out), &status); err != nil || status["state"] != "FAILED" || dt["notify"].GetState() != aiplatformpb.PipelineTaskDetail_SUCCEEDED {
		t.Errorf("notify after a failure = %v, %s", dt["notify"].GetState(), out)
	}
}

const resolverSpec = `{
 "pipelineInfo": {"name": "resolve-pipeline"},
 "root": {"dag": {"tasks": {
  "resolve": {"taskInfo": {"name": "resolve"}, "componentRef": {"name": "comp-resolve"}},
  "use": {"taskInfo": {"name": "use"}, "componentRef": {"name": "comp-use"},
    "inputs": {"artifacts": {"m": {"taskOutputArtifact": {"producerTask": "resolve", "outputArtifactKey": "model"}}}}}
 }}},
 "components": {
  "comp-resolve": {"executorLabel": "exec-resolve", "outputDefinitions": {"artifacts": {"model": {"artifactType": {"schemaTitle": "system.Model"}}}}},
  "comp-use": {"executorLabel": "exec-use", "inputDefinitions": {"artifacts": {"m": {"artifactType": {"schemaTitle": "system.Model"}}}}}
 },
 "deploymentSpec": {"executors": {
  "exec-resolve": {"resolver": {"outputArtifactQueries": {"model": {"filter": "schema_title=\"SCHEMA\"", "limit": 1}}}},
  "exec-use": {"container": {"image": "local/tool:1", "args": ["{{$.inputs.artifacts['m'].uri}}"]}}
 }}
}`

func TestPipelineResolverAndLimits(t *testing.T) {
	ctx := context.Background()
	s, r := newPipelineServer(t)
	p := &pipelineService{s: s}
	md := &metadataService{s: s}
	parent := "projects/p/locations/us-central1"
	store := s.defaultMetadataStore(parent)
	if _, err := md.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, Artifact: &aiplatformpb.Artifact{
		SchemaTitle: "system.Model", Uri: "gs://root/models/m1", State: aiplatformpb.Artifact_LIVE}}); err != nil {
		t.Fatal(err)
	}
	create := func(spec string) (*aiplatformpb.PipelineJob, error) {
		return p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
			PipelineSpec: testSpec(t, spec), RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://root/runs"}}})
	}
	j, err := create(strings.Replace(resolverSpec, "SCHEMA", "system.Model", 1))
	if err != nil {
		t.Fatal(err)
	}
	done := waitPipeline(t, p, j.GetName())
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the resolver run = %v %v", done.GetState(), done.GetError())
	}
	r.mu.Lock()
	last := r.runs[len(r.runs)-1]
	r.mu.Unlock()
	if strings.Join(last.Args, " ") != "gs://root/models/m1" {
		t.Errorf("use took %v, want the resolved model's URI", last.Args)
	}
	j, err = create(strings.Replace(resolverSpec, "SCHEMA", "system.Nothing", 1))
	if err != nil {
		t.Fatal(err)
	}
	if done := waitPipeline(t, p, j.GetName()); done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED ||
		!strings.Contains(pipelineTaskByName(done, "resolve").GetError().GetMessage(), "no artifact matches") {
		t.Errorf("a resolver matching nothing = %v %v", done.GetState(), done.GetJobDetail())
	}

	for what, spec := range map[string]string{
		"a bad condition":                     strings.Replace(controlFlowSpec, `== 'heads'"`, `== "`, 1),
		"a bad strategy":                      strings.Replace(controlFlowSpec, `"ALL_UPSTREAM_TASKS_COMPLETED"`, `"SOMETIMES"`, 1),
		"a final status outside an exit task": strings.Replace(controlFlowSpec, `"triggerPolicy": {"strategy": "ALL_UPSTREAM_TASKS_COMPLETED"},`, ``, 1),
		"a loop without items":                strings.Replace(controlFlowSpec, `"items": {"inputParameter": "pipelinechannel--xs"}, `, ``, 1),
		"a negative retry":                    strings.Replace(controlFlowSpec, `"maxRetryCount": 2`, `"maxRetryCount": -1`, 1),
		"a custom job":                        strings.Replace(resolverSpec, `"resolver": {"outputArtifactQueries": {"model": {"filter": "schema_title=\"SCHEMA\"", "limit": 1}}}`, `"customJob": {"customJob": {}}`, 1),
		"an unknown subtask output":           strings.Replace(controlFlowSpec, `"producerSubtask": "echo"`, `"producerSubtask": "nope"`, 1),
	} {
		if _, err := create(spec); codeOf(err) != codes.InvalidArgument {
			t.Errorf("create with %s = %v, want INVALID_ARGUMENT", what, err)
		}
	}
	// More than 10,000 tasks: a loop of 10,001 items.
	items, _ := json.Marshal(make([]int, maxPipelineTasks+1))
	big := strings.Replace(controlFlowSpec, `"items": {"inputParameter": "pipelinechannel--xs"}`, `"items": {"raw": `+strconvQuote(string(items))+`}`, 1)
	j, err = create(big)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitPipeline(t, p, j.GetName()); done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED {
		t.Errorf("a run of more than %d tasks = %v", maxPipelineTasks, done.GetState())
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func pipelineTaskByName(j *aiplatformpb.PipelineJob, name string) *aiplatformpb.PipelineTaskDetail {
	for _, d := range j.GetJobDetail().GetTaskDetails() {
		if d.GetTaskName() == name {
			return d
		}
	}
	return nil
}
