package vertexai

// Vertex AI Pipelines (#2173): google.cloud.aiplatform.v1.PipelineService's
// PipelineJob methods over gRPC and REST, so that a schedule's
// create_pipeline_job_request runs. A pipeline job runs its compiled
// Kubeflow Pipelines definition (pipeline_spec, the KFP IR, schema 2.1.0)
// on this machine: each task's container executor is a Docker container of
// its image, its inputs and outputs passed as KFP's launcher passes them.
//
// Documented behaviour (the PipelineService and PipelineJob protos,
// cloud.google.com/go/aiplatform v1.127.0; https://docs.cloud.google.com/vertex-ai/docs/pipelines/run-pipeline;
// .../pipelines/build-pipeline; https://www.kubeflow.org/docs/components/pipelines/user-guides/components/container-components/
// for the placeholders; read 2026-11-22):
//   - pipeline_job_id is optional, "less than 128 characters", of
//     [a-z][0-9]-; generated otherwise, as the pipeline's name and the
//     create time (PIPELINE-YYYYMMDDHHMMSS);
//   - runtime_config.gcs_output_directory is required ("the root output
//     directory of the pipeline"; the spec's defaultPipelineRoot when it is
//     unset), artifacts written under {job_id}/{task_id}/{output_key};
//     parameter_values give the root's input parameters, their defaults
//     the spec's; a required parameter without a value is INVALID_ARGUMENT;
//   - failure_policy FAIL_FAST stops scheduling once a task fails, FAIL_SLOW
//     (the default) runs every task that does not depend on it;
//   - the job is PENDING, RUNNING, then SUCCEEDED, FAILED or CANCELLED;
//     CancelPipelineJob is best effort and "the PipelineJob is not deleted;
//     instead it becomes a pipeline with a PipelineJob.error value with a
//     google.rpc.Status.code of 1"; Delete answers an Operation; a batch
//     deletes or cancels at most 32 jobs atomically;
//   - the list filters pipeline_name, display_name, state, create_time,
//     update_time, end_time, labels, template_uri, orders by create_time,
//     update_time and end_time, by default create_time desc;
//   - the job's service_account, "If not specified, the Compute Engine
//     default service account in the project will be used"; its caller
//     needs iam.serviceAccounts.actAs on it (runas.go).
//
// Each task's container sees KFP's placeholders: {{$.inputs.parameters['K']}},
// {{$.outputs.parameters['K'].output_file}}, {{$.inputs.artifacts['K'].path|uri}},
// {{$.outputs.artifacts['K'].path|uri}}, {{$}} (the executor input JSON,
// whose outputs.outputFile an executor may write its executor output to),
// {{$.pipeline_job_name}}, {{$.pipeline_job_uuid}}, {{$.pipeline_task_name}}
// and {{$.pipeline_task_uuid}}. Artifacts are files under /gcs/BUCKET/PATH
// for gs://BUCKET/PATH, as Vertex AI's Cloud Storage FUSE mount names
// them: inputs are read from Cloud Storage before the task, outputs written
// there after it.
//
// Control flow (#2205, pipelinerun.go): nested DAGs (sub-pipelines),
// dsl.ParallelFor (parameter and artifact iterators, with parallelism),
// dsl.If/Elif/Else (trigger conditions, CEL), trigger strategies,
// dsl.ExitHandler (taskFinalStatus), importers, resolvers, task caching
// (cachingOptions, enable_caching) and retries (retryPolicy) run as
// Vertex AI Pipelines runs them, and every run records its pipeline and
// run contexts, executions, artifacts and events in the project's ML
// Metadata store (metadata.go).
//
// Local differences: a task's image must already be on this machine
// (CloudBurrow pulls nothing, and never from a Google registry); the
// container has no network (--network none), so a component that calls a
// Google API cannot; template_uri and customJob executors are
// INVALID_ARGUMENT naming what is not run, at create.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// PipelineContainer is one pipeline task's container.
type PipelineContainer struct {
	// Key names the run (its container), unique per task run.
	Key     string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	// Mounts are host directories by container path (/gcs, the outputs).
	Mounts map[string]string
	// Labels are the job's and task's, for the container.
	Labels map[string]string
}

// PipelineRunner runs pipeline tasks' containers, adapted in cmd/cloudburrow
// over Docker; nil fails every pipeline job's tasks.
type PipelineRunner interface {
	// RunContainer runs c to its end: nil when it exited 0, else an error
	// with its output's tail.
	RunContainer(ctx context.Context, c PipelineContainer) error
	// ReadObject and WriteObject are the instance's Cloud Storage.
	ReadObject(ctx context.Context, bucket, object string, generation int64) ([]byte, error)
	WriteObject(ctx context.Context, bucket, object, contentType string, data []byte) error
	// ListObjects are the object names under prefix.
	ListObjects(ctx context.Context, bucket, prefix string) ([]string, error)
}

// SetPipelineRunner runs pipeline jobs on r.
func (s *Server) SetPipelineRunner(r PipelineRunner) {
	st := s.notebooks()
	st.mu.Lock()
	st.pipelineRunner = r
	st.mu.Unlock()
	s.startScheduler()
}

var (
	pipelineIDRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
	pipelineNameSafe = regexp.MustCompile(`[^a-z0-9-]+`)
	pipelineFilter   = append([]aipfilter.Option{
		aipfilter.Fields("pipeline_name", "display_name", "state", "create_time", "update_time", "end_time", "start_time", "labels", "template_uri", "schedule_name"),
	}, bothCases(map[string]func(proto.Message) string{
		"pipeline_name": func(m proto.Message) string {
			return pipelineInfoName(m.(*aiplatformpb.PipelineJob).GetPipelineSpec())
		},
	}, nil)...)
	pipelineOrder = aipfilter.Fields("create_time", "update_time", "end_time", "start_time")
)

// pipelineEntry is a pipeline job and its run.
type pipelineEntry struct {
	Job    *aiplatformpb.PipelineJob
	cancel context.CancelFunc
	runAs  string
}

// pipelineService serves the PipelineJob methods; TrainingPipelines are
// not served (UNIMPLEMENTED).
type pipelineService struct {
	aiplatformpb.UnimplementedPipelineServiceServer
	s *Server
}

func pipelineInfoName(spec *structpb.Struct) string {
	return spec.GetFields()["pipelineInfo"].GetStructValue().GetFields()["name"].GetStringValue()
}

// kfpSpec is a KFP pipeline spec (IR, pipeline_spec.proto of
// kubeflow/pipelines' api/v2alpha1, as the kfp 2 compiler writes it).
type kfpSpec struct {
	PipelineInfo struct {
		Name string `json:"name"`
	} `json:"pipelineInfo"`
	DefaultPipelineRoot string                  `json:"defaultPipelineRoot"`
	SchemaVersion       string                  `json:"schemaVersion"`
	SDKVersion          string                  `json:"sdkVersion"`
	Root                kfpComponent            `json:"root"`
	Components          map[string]kfpComponent `json:"components"`
	DeploymentSpec      struct {
		Executors map[string]kfpExecutor `json:"executors"`
	} `json:"deploymentSpec"`
}

type kfpComponent struct {
	ExecutorLabel    string  `json:"executorLabel,omitempty"`
	DAG              *kfpDAG `json:"dag,omitempty"`
	InputDefinitions struct {
		Parameters map[string]kfpParamDef `json:"parameters,omitempty"`
		Artifacts  map[string]kfpArtDef   `json:"artifacts,omitempty"`
	} `json:"inputDefinitions"`
	OutputDefinitions struct {
		Parameters map[string]kfpParamDef `json:"parameters,omitempty"`
		Artifacts  map[string]kfpArtDef   `json:"artifacts,omitempty"`
	} `json:"outputDefinitions"`
}

type kfpParamDef struct {
	ParameterType string          `json:"parameterType,omitempty"`
	DefaultValue  json.RawMessage `json:"defaultValue,omitempty"`
	IsOptional    bool            `json:"isOptional,omitempty"`
}

type kfpArtDef struct {
	ArtifactType struct {
		SchemaTitle   string `json:"schemaTitle,omitempty"`
		SchemaVersion string `json:"schemaVersion,omitempty"`
	} `json:"artifactType"`
	IsArtifactList bool `json:"isArtifactList,omitempty"`
	IsOptional     bool `json:"isOptional,omitempty"`
}

type kfpDAG struct {
	Tasks   map[string]kfpTask `json:"tasks"`
	Outputs struct {
		Parameters map[string]struct {
			ValueFromParameter *kfpParamSel `json:"valueFromParameter"`
			ValueFromOneof     *struct {
				ParameterSelectors []kfpParamSel `json:"parameterSelectors"`
			} `json:"valueFromOneof"`
		} `json:"parameters"`
		Artifacts map[string]struct {
			ArtifactSelectors []kfpArtSel `json:"artifactSelectors"`
		} `json:"artifacts"`
	} `json:"outputs"`
}

type kfpParamSel struct {
	ProducerSubtask    string `json:"producerSubtask"`
	OutputParameterKey string `json:"outputParameterKey"`
}

type kfpArtSel struct {
	ProducerSubtask   string `json:"producerSubtask"`
	OutputArtifactKey string `json:"outputArtifactKey"`
}

type kfpTask struct {
	TaskInfo struct {
		Name string `json:"name"`
	} `json:"taskInfo"`
	ComponentRef struct {
		Name string `json:"name"`
	} `json:"componentRef"`
	DependentTasks []string `json:"dependentTasks"`
	Inputs         struct {
		Parameters map[string]kfpParamIn `json:"parameters"`
		Artifacts  map[string]kfpArtIn   `json:"artifacts"`
	} `json:"inputs"`
	TriggerPolicy *struct {
		Condition string `json:"condition"`
		Strategy  string `json:"strategy"`
	} `json:"triggerPolicy"`
	ParameterIter *struct {
		Items struct {
			Raw            string `json:"raw"`
			InputParameter string `json:"inputParameter"`
		} `json:"items"`
		ItemInput string `json:"itemInput"`
	} `json:"parameterIterator"`
	ArtifactIter *struct {
		Items struct {
			InputArtifact string `json:"inputArtifact"`
		} `json:"items"`
		ItemInput string `json:"itemInput"`
	} `json:"artifactIterator"`
	IteratorPolicy *struct {
		ParallelismLimit int `json:"parallelismLimit"`
	} `json:"iteratorPolicy"`
	RetryPolicy *struct {
		MaxRetryCount      int     `json:"maxRetryCount"`
		BackoffDuration    string  `json:"backoffDuration"`
		BackoffFactor      float64 `json:"backoffFactor"`
		BackoffMaxDuration string  `json:"backoffMaxDuration"`
	} `json:"retryPolicy"`
	CachingOptions struct {
		EnableCache bool   `json:"enableCache"`
		CacheKey    string `json:"cacheKey"`
	} `json:"cachingOptions"`
}

type kfpParamIn struct {
	ComponentInputParameter string `json:"componentInputParameter"`
	RuntimeValue            *struct {
		Constant json.RawMessage `json:"constant"`
	} `json:"runtimeValue"`
	TaskOutputParameter *struct {
		ProducerTask       string `json:"producerTask"`
		OutputParameterKey string `json:"outputParameterKey"`
	} `json:"taskOutputParameter"`
	TaskFinalStatus *struct {
		ProducerTask string `json:"producerTask"`
	} `json:"taskFinalStatus"`
	ParameterExpressionSelector string `json:"parameterExpressionSelector"`
}

type kfpArtIn struct {
	ComponentInputArtifact string `json:"componentInputArtifact"`
	TaskOutputArtifact     *struct {
		ProducerTask      string `json:"producerTask"`
		OutputArtifactKey string `json:"outputArtifactKey"`
	} `json:"taskOutputArtifact"`
}

type kfpContainer struct {
	Image   string   `json:"image"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
	Env     []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
}

type kfpImporter struct {
	ArtifactURI struct {
		Constant         json.RawMessage `json:"constant"`
		RuntimeParameter string          `json:"runtimeParameter"`
	} `json:"artifactUri"`
	TypeSchema struct {
		SchemaTitle   string `json:"schemaTitle"`
		SchemaVersion string `json:"schemaVersion"`
	} `json:"typeSchema"`
	Metadata json.RawMessage `json:"metadata"`
	Reimport bool            `json:"reimport"`
}

type kfpResolver struct {
	OutputArtifactQueries map[string]struct {
		Filter string `json:"filter"`
		Limit  int    `json:"limit"`
	} `json:"outputArtifactQueries"`
}

type kfpExecutor struct {
	Container *kfpContainer   `json:"container,omitempty"`
	Importer  *kfpImporter    `json:"importer,omitempty"`
	Resolver  *kfpResolver    `json:"resolver,omitempty"`
	CustomJob json.RawMessage `json:"customJob,omitempty"`
}

// Agent Platform Pipelines' documented limits ("Quotas and limits",
// https://docs.cloud.google.com/vertex-ai/docs/quotas#pipelines, read
// 2026-11-24): they cannot be increased.
const (
	maxPipelineTasks        = 10000
	maxTaskArtifacts        = 100
	maxJobArtifacts         = 10000
	maxTaskOutputPayload    = 131072
	maxTaskRunTime          = 7 * 24 * time.Hour
	maxParallelTasks        = 600
	maxConcurrentRuns       = 300
	defaultRetryBackoffMax  = time.Hour
	defaultRetryBackoffBase = 2.0
)

// parseKFP reads and checks a pipeline spec: what this instance runs, or
// INVALID_ARGUMENT naming what is wrong.
func parseKFP(prefix string, spec *structpb.Struct) (*kfpSpec, error) {
	if spec == nil || len(spec.GetFields()) == 0 {
		return nil, apierror.InvalidArgument("%spipeline_spec is required (a compiled Kubeflow Pipelines definition), or a template_uri", prefix)
	}
	b, err := protojson.Marshal(spec)
	if err != nil {
		return nil, apierror.InvalidArgument("%spipeline_spec: %v", prefix, err)
	}
	k := &kfpSpec{}
	if err := json.Unmarshal(b, k); err != nil {
		return nil, apierror.InvalidArgument("%spipeline_spec is not a Kubeflow Pipelines definition: %v", prefix, err)
	}
	if k.PipelineInfo.Name == "" {
		return nil, apierror.InvalidArgument("%spipeline_spec.pipelineInfo.name is required", prefix)
	}
	if k.Root.DAG == nil || len(k.Root.DAG.Tasks) == 0 {
		return nil, apierror.InvalidArgument("%spipeline_spec.root.dag has no tasks", prefix)
	}
	n := 0
	if err := checkDAG(prefix, k, "root", k.Root, map[string]bool{}, &n); err != nil {
		return nil, err
	}
	return k, nil
}

// checkDAG checks one DAG component's tasks and, through them, every
// component they run; seen guards against a component that runs itself.
func checkDAG(prefix string, k *kfpSpec, compName string, comp kfpComponent, seen map[string]bool, n *int) error {
	if seen[compName] {
		return apierror.InvalidArgument("%spipeline_spec component %q runs itself", prefix, compName)
	}
	seen[compName] = true
	defer delete(seen, compName)
	d := comp.DAG
	for _, name := range sortedNames(d.Tasks) {
		t := d.Tasks[name]
		if *n++; *n > maxPipelineTasks {
			return apierror.InvalidArgument("%spipeline_spec has more than %d pipeline tasks, the most a pipeline job can have", prefix, maxPipelineTasks)
		}
		c, ok := k.Components[t.ComponentRef.Name]
		if !ok {
			return apierror.InvalidArgument("%spipeline_spec task %q names component %q, which components does not define", prefix, name, t.ComponentRef.Name)
		}
		if len(c.InputDefinitions.Artifacts)+len(c.OutputDefinitions.Artifacts) > maxTaskArtifacts {
			return apierror.InvalidArgument("%spipeline_spec task %q has more than %d input and output artifacts, the most a pipeline task can have", prefix, name, maxTaskArtifacts)
		}
		for _, dep := range t.DependentTasks {
			if _, ok := d.Tasks[dep]; !ok {
				return apierror.InvalidArgument("%spipeline_spec task %q depends on %q, which is not a task of its DAG", prefix, name, dep)
			}
		}
		for key, in := range t.Inputs.Parameters {
			var producer string
			switch {
			case in.TaskOutputParameter != nil:
				producer = in.TaskOutputParameter.ProducerTask
			case in.TaskFinalStatus != nil:
				producer = in.TaskFinalStatus.ProducerTask
				if t.TriggerPolicy == nil || t.TriggerPolicy.Strategy != "ALL_UPSTREAM_TASKS_COMPLETED" {
					return apierror.InvalidArgument("%spipeline_spec task %q's input %q is a task's final status, which only an exit task (trigger strategy ALL_UPSTREAM_TASKS_COMPLETED) can take", prefix, name, key)
				}
			case in.ComponentInputParameter != "":
				if _, ok := comp.InputDefinitions.Parameters[in.ComponentInputParameter]; !ok {
					return apierror.InvalidArgument("%spipeline_spec task %q's input %q names %q, which is not an input parameter of %s", prefix, name, key, in.ComponentInputParameter, compName)
				}
			}
			if producer != "" {
				if _, ok := d.Tasks[producer]; !ok {
					return apierror.InvalidArgument("%spipeline_spec task %q's input %q names task %q, which is not a task of its DAG", prefix, name, key, producer)
				}
			}
			if sel := in.ParameterExpressionSelector; sel != "" {
				if _, err := compileCEL(sel); err != nil {
					return apierror.InvalidArgument("%spipeline_spec task %q's input %q: parameterExpressionSelector %q: %v", prefix, name, key, sel, err)
				}
			}
		}
		for key, in := range t.Inputs.Artifacts {
			if a := in.TaskOutputArtifact; a != nil {
				if _, ok := d.Tasks[a.ProducerTask]; !ok {
					return apierror.InvalidArgument("%spipeline_spec task %q's input artifact %q names task %q, which is not a task of its DAG", prefix, name, key, a.ProducerTask)
				}
			}
			if a := in.ComponentInputArtifact; a != "" {
				if _, ok := comp.InputDefinitions.Artifacts[a]; !ok {
					return apierror.InvalidArgument("%spipeline_spec task %q's input artifact %q names %q, which is not an input artifact of %s", prefix, name, key, a, compName)
				}
			}
		}
		if tp := t.TriggerPolicy; tp != nil {
			switch tp.Strategy {
			case "", "TRIGGER_STRATEGY_UNSPECIFIED", "ALL_UPSTREAM_TASKS_SUCCEEDED", "ALL_UPSTREAM_TASKS_COMPLETED":
			default:
				return apierror.InvalidArgument("%spipeline_spec task %q's trigger strategy %q is not ALL_UPSTREAM_TASKS_SUCCEEDED or ALL_UPSTREAM_TASKS_COMPLETED", prefix, name, tp.Strategy)
			}
			if tp.Condition != "" {
				if _, err := compileCEL(tp.Condition); err != nil {
					return apierror.InvalidArgument("%spipeline_spec task %q's trigger condition %q: %v", prefix, name, tp.Condition, err)
				}
			}
		}
		if rp := t.RetryPolicy; rp != nil {
			if rp.MaxRetryCount < 0 {
				return apierror.InvalidArgument("%spipeline_spec task %q's retryPolicy.maxRetryCount must not be negative", prefix, name)
			}
			for _, d := range []string{rp.BackoffDuration, rp.BackoffMaxDuration} {
				if _, err := kfpDuration(d); err != nil {
					return apierror.InvalidArgument("%spipeline_spec task %q's retryPolicy: %v", prefix, name, err)
				}
			}
		}
		if t.ParameterIter != nil || t.ArtifactIter != nil {
			if c.DAG == nil {
				return apierror.InvalidArgument("%spipeline_spec task %q is a loop whose component %q is not a DAG", prefix, name, t.ComponentRef.Name)
			}
			if p := t.ParameterIter; p != nil {
				if p.ItemInput == "" {
					return apierror.InvalidArgument("%spipeline_spec task %q's parameterIterator has no itemInput", prefix, name)
				}
				switch {
				case p.Items.Raw != "":
					var items []any
					if err := json.Unmarshal([]byte(p.Items.Raw), &items); err != nil {
						return apierror.InvalidArgument("%spipeline_spec task %q's parameterIterator.items.raw %q is not a JSON list", prefix, name, p.Items.Raw)
					}
				case p.Items.InputParameter != "":
					if _, ok := t.Inputs.Parameters[p.Items.InputParameter]; !ok {
						return apierror.InvalidArgument("%spipeline_spec task %q's parameterIterator names input %q, which the task does not take", prefix, name, p.Items.InputParameter)
					}
				default:
					return apierror.InvalidArgument("%spipeline_spec task %q's parameterIterator has no items", prefix, name)
				}
			}
			if a := t.ArtifactIter; a != nil {
				if _, ok := t.Inputs.Artifacts[a.Items.InputArtifact]; !ok || a.ItemInput == "" {
					return apierror.InvalidArgument("%spipeline_spec task %q's artifactIterator names input artifact %q, which the task does not take", prefix, name, a.Items.InputArtifact)
				}
			}
			if ip := t.IteratorPolicy; ip != nil && ip.ParallelismLimit < 0 {
				return apierror.InvalidArgument("%spipeline_spec task %q's iteratorPolicy.parallelismLimit must not be negative", prefix, name)
			}
		}
		if c.DAG != nil {
			if len(c.DAG.Tasks) == 0 {
				return apierror.InvalidArgument("%spipeline_spec component %q's dag has no tasks", prefix, t.ComponentRef.Name)
			}
			if err := checkDAG(prefix, k, t.ComponentRef.Name, c, seen, n); err != nil {
				return err
			}
			continue
		}
		ex, ok := k.DeploymentSpec.Executors[c.ExecutorLabel]
		if !ok {
			return apierror.InvalidArgument("%spipeline_spec task %q's component %q names executor %q, which deploymentSpec does not define", prefix, name, t.ComponentRef.Name, c.ExecutorLabel)
		}
		switch {
		case ex.Container != nil:
			if ex.Container.Image == "" {
				return apierror.InvalidArgument("%spipeline_spec executor %q has no image", prefix, c.ExecutorLabel)
			}
		case ex.Importer != nil:
			if len(ex.Importer.ArtifactURI.Constant) == 0 && ex.Importer.ArtifactURI.RuntimeParameter == "" {
				return apierror.InvalidArgument("%spipeline_spec importer %q has no artifactUri", prefix, c.ExecutorLabel)
			}
			if ex.Importer.TypeSchema.SchemaTitle == "" {
				return apierror.InvalidArgument("%spipeline_spec importer %q has no typeSchema", prefix, c.ExecutorLabel)
			}
			if len(c.OutputDefinitions.Artifacts) != 1 {
				return apierror.InvalidArgument("%spipeline_spec importer %q's component must have one output artifact", prefix, c.ExecutorLabel)
			}
		case ex.Resolver != nil:
			if len(ex.Resolver.OutputArtifactQueries) == 0 {
				return apierror.InvalidArgument("%spipeline_spec resolver %q has no outputArtifactQueries", prefix, c.ExecutorLabel)
			}
			for key := range ex.Resolver.OutputArtifactQueries {
				if _, ok := c.OutputDefinitions.Artifacts[key]; !ok {
					return apierror.InvalidArgument("%spipeline_spec resolver %q queries %q, which is not an output artifact of its component", prefix, c.ExecutorLabel, key)
				}
			}
		case len(ex.CustomJob) > 0:
			return apierror.InvalidArgument("%spipeline_spec executor %q is a Vertex AI custom job (customJob), which this instance does not run: CustomJobs are not served", prefix, c.ExecutorLabel)
		default:
			return apierror.InvalidArgument("%spipeline_spec executor %q has no container, importer or resolver", prefix, c.ExecutorLabel)
		}
	}
	for key, o := range d.Outputs.Parameters {
		sels := []kfpParamSel{}
		if o.ValueFromParameter != nil {
			sels = append(sels, *o.ValueFromParameter)
		}
		if o.ValueFromOneof != nil {
			sels = append(sels, o.ValueFromOneof.ParameterSelectors...)
		}
		for _, s := range sels {
			if _, ok := d.Tasks[s.ProducerSubtask]; !ok {
				return apierror.InvalidArgument("%spipeline_spec component %q's output %q names task %q, which is not a task of its DAG", prefix, compName, key, s.ProducerSubtask)
			}
		}
	}
	for key, o := range d.Outputs.Artifacts {
		for _, s := range o.ArtifactSelectors {
			if _, ok := d.Tasks[s.ProducerSubtask]; !ok {
				return apierror.InvalidArgument("%spipeline_spec component %q's output artifact %q names task %q, which is not a task of its DAG", prefix, compName, key, s.ProducerSubtask)
			}
		}
	}
	if _, err := kfpOrder(d); err != nil {
		return apierror.InvalidArgument("%spipeline_spec %s: %v", prefix, compName, err)
	}
	return nil
}

// kfpDuration reads a retry policy's duration ("30s", as protobuf's
// Duration JSON); "" is zero.
func kfpDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration", s)
	}
	return d, nil
}

// kfpDeps are a task's upstream tasks: its dependentTasks and every task
// whose outputs or final status it reads.
func kfpDeps(t kfpTask) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, d := range t.DependentTasks {
		add(d)
	}
	for _, k := range sortedNames(t.Inputs.Parameters) {
		in := t.Inputs.Parameters[k]
		if p := in.TaskOutputParameter; p != nil {
			add(p.ProducerTask)
		}
		if p := in.TaskFinalStatus; p != nil {
			add(p.ProducerTask)
		}
	}
	for _, k := range sortedNames(t.Inputs.Artifacts) {
		if a := t.Inputs.Artifacts[k].TaskOutputArtifact; a != nil {
			add(a.ProducerTask)
		}
	}
	sort.Strings(out)
	return out
}

// kfpOrder is the DAG's tasks in dependency order, or the cycle's error.
func kfpOrder(d *kfpDAG) ([]string, error) {
	state := map[string]int{}
	var out []string
	var visit func(string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("the tasks' dependencies have a cycle through %q", n)
		case 2:
			return nil
		}
		state[n] = 1
		for _, dep := range kfpDeps(d.Tasks[n]) {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[n] = 2
		out = append(out, n)
		return nil
	}
	for _, n := range sortedNames(d.Tasks) {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pipelineRoot is the job's output directory: runtime_config's, else the
// spec's defaultPipelineRoot.
func pipelineRoot(j *aiplatformpb.PipelineJob, k *kfpSpec) string {
	if r := j.GetRuntimeConfig().GetGcsOutputDirectory(); r != "" {
		return r
	}
	return k.DefaultPipelineRoot
}

// rootParams are the root's parameter values: runtime_config's, then the
// spec's defaults; a required one missing is INVALID_ARGUMENT.
func rootParams(prefix string, j *aiplatformpb.PipelineJob, k *kfpSpec) (map[string]*structpb.Value, error) {
	out := map[string]*structpb.Value{}
	given := j.GetRuntimeConfig().GetParameterValues()
	for name := range given {
		if _, ok := k.Root.InputDefinitions.Parameters[name]; !ok {
			return nil, apierror.InvalidArgument("%sruntime_config.parameter_values: %q is not an input parameter of pipeline %s", prefix, name, k.PipelineInfo.Name)
		}
	}
	for _, name := range sortedNames(k.Root.InputDefinitions.Parameters) {
		def := k.Root.InputDefinitions.Parameters[name]
		if v, ok := given[name]; ok {
			out[name] = v
			continue
		}
		if len(def.DefaultValue) > 0 {
			v := &structpb.Value{}
			if err := protojson.Unmarshal(def.DefaultValue, v); err == nil {
				out[name] = v
				continue
			}
		}
		if !def.IsOptional {
			return nil, apierror.InvalidArgument("%sruntime_config.parameter_values: the pipeline's required parameter %q has no value", prefix, name)
		}
	}
	return out, nil
}

// checkPipelineJob validates a job to create; the caller holds st.mu.
func checkPipelineJob(prefix string, j *aiplatformpb.PipelineJob) (*kfpSpec, error) {
	if j == nil {
		return nil, apierror.InvalidArgument("%spipeline_job is required", strings.TrimSuffix(prefix, "pipeline_job."))
	}
	k, err := parseKFP(prefix, j.GetPipelineSpec())
	if err != nil {
		return nil, err
	}
	root := pipelineRoot(j, k)
	if root == "" {
		return nil, apierror.InvalidArgument("%sruntime_config.gcs_output_directory is required: the root output directory of the pipeline", prefix)
	}
	if _, _, err := splitNotebookGCS(prefix+"runtime_config.gcs_output_directory", root, false); err != nil {
		return nil, err
	}
	if _, err := rootParams(prefix, j, k); err != nil {
		return nil, err
	}
	if utf8Len(j.GetDisplayName()) > 128 {
		return nil, apierror.InvalidArgument("%sdisplay_name can be up to 128 characters long", prefix)
	}
	if sa := j.GetServiceAccount(); sa != "" && !strings.Contains(sa, "@") {
		return nil, apierror.InvalidArgument("%sservice_account %q is not a service account email", prefix, sa)
	}
	return k, nil
}

func utf8Len(s string) int { return len([]rune(s)) }

func (p *pipelineService) CreatePipelineJob(ctx context.Context, req *aiplatformpb.CreatePipelineJobRequest) (*aiplatformpb.PipelineJob, error) {
	in := req.GetPipelineJob()
	j, err := p.s.createPipelineJob(req.GetParent(), req.GetPipelineJobId(), in, "", in.GetServiceAccount())
	if err != nil {
		return nil, grpcError(err)
	}
	return j, nil
}

// createPipelineJob records a pipeline job and starts it; schedule names
// the schedule that made it, runAs the account it runs as.
func (s *Server) createPipelineJob(parent, id string, in *aiplatformpb.PipelineJob, schedule, runAs string) (*aiplatformpb.PipelineJob, error) {
	project, _, err := parseParent("parent", parent)
	if err != nil {
		return nil, err
	}
	if id != "" && (len(id) >= 128 || !pipelineIDRE.MatchString(id)) {
		return nil, apierror.InvalidArgument("pipeline_job_id %q must be less than 128 characters of lowercase letters, digits and hyphens", id)
	}
	if in == nil {
		return nil, apierror.InvalidArgument("pipeline_job is required")
	}
	j := proto.Clone(in).(*aiplatformpb.PipelineJob)
	if err := s.loadTemplate(j); err != nil {
		return nil, err
	}
	k, err := checkPipelineJob("pipeline_job.", j)
	if err != nil {
		return nil, err
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pipelineRunner == nil {
		return nil, apierror.FailedPrecondition("Vertex AI Pipelines needs Docker and this instance's Cloud Storage to run a pipeline's containers (start with --services including storage)")
	}
	now := st.now()
	if id == "" {
		base := strings.Trim(pipelineNameSafe.ReplaceAllString(strings.ToLower(k.PipelineInfo.Name), "-"), "-")
		if len(base) > 110 {
			base = base[:110]
		}
		id = base + "-" + now.UTC().Format("20060102150405")
		for n := 2; st.pipelines[parent+"/pipelineJobs/"+id] != nil; n++ {
			id = base + "-" + now.UTC().Format("20060102150405") + "-" + strconv.Itoa(n)
		}
	}
	name := parent + "/pipelineJobs/" + id
	if _, ok := st.pipelines[name]; ok {
		return nil, apierror.AlreadyExists("PipelineJob %s already exists", name)
	}
	ts := timestamppb.New(now)
	j.Name, j.CreateTime, j.UpdateTime, j.StartTime, j.EndTime = name, ts, ts, nil, nil
	j.State, j.Error, j.JobDetail = aiplatformpb.PipelineState_PIPELINE_STATE_PENDING, nil, nil
	if schedule != "" {
		j.ScheduleName = schedule
	}
	if j.RuntimeConfig == nil {
		j.RuntimeConfig = &aiplatformpb.PipelineJob_RuntimeConfig{}
	}
	j.RuntimeConfig.GcsOutputDirectory = pipelineRoot(j, k)
	ctx, cancel := context.WithCancel(context.Background())
	e := &pipelineEntry{Job: j, cancel: cancel, runAs: strings.TrimPrefix(runAs, "serviceAccount:")}
	st.pipelines[name] = e
	st.save()
	go s.runPipeline(ctx, e, k, project)
	return proto.Clone(j).(*aiplatformpb.PipelineJob), nil
}

// setPipeline updates e under st.mu unless it was deleted.
func (st *notebookStore) setPipeline(e *pipelineEntry, f func(j *aiplatformpb.PipelineJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pipelines[e.Job.GetName()] != e {
		return
	}
	f(e.Job)
	e.Job.UpdateTime = timestamppb.New(st.now())
	st.save()
}

// paramText is a parameter's value as a placeholder writes it: a string
// as is, anything else as JSON.
func paramText(v *structpb.Value) string {
	if v == nil {
		return ""
	}
	if s, ok := v.GetKind().(*structpb.Value_StringValue); ok {
		return s.StringValue
	}
	if n, ok := v.GetKind().(*structpb.Value_NumberValue); ok && n.NumberValue == float64(int64(n.NumberValue)) {
		return strconv.FormatInt(int64(n.NumberValue), 10)
	}
	b, _ := protojson.Marshal(v)
	return string(b)
}

// parseParam reads an output parameter file as its type.
func parseParam(kind, text string) (*structpb.Value, error) {
	switch kind {
	case "STRING", "":
		return structpb.NewStringValue(text), nil
	case "NUMBER_INTEGER":
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", text)
		}
		return structpb.NewNumberValue(float64(n)), nil
	case "NUMBER_DOUBLE":
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", text)
		}
		return structpb.NewNumberValue(f), nil
	case "BOOLEAN":
		b, err := strconv.ParseBool(strings.TrimSpace(strings.ToLower(text)))
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean", text)
		}
		return structpb.NewBoolValue(b), nil
	}
	v := &structpb.Value{}
	if err := protojson.Unmarshal([]byte(text), v); err != nil {
		return nil, fmt.Errorf("%q is not JSON for a %s", text, kind)
	}
	return v, nil
}

// gcsPath is the /gcs path of gs://bucket/object, as Cloud Storage FUSE
// mounts it.
func gcsPath(uri string) string { return "/gcs/" + strings.TrimPrefix(uri, "gs://") }

// kfpPlaceholders matches {{$}}, {{$.inputs|outputs.parameters|artifacts['K'][.attr][['KEY']]}}
// and {{$.pipeline_*}}.
var kfpPlaceholders = regexp.MustCompile(`\{\{\$(?:\.(inputs|outputs)\.(parameters|artifacts)\['([^']+)'\](?:\.([a-z_]+)(?:\['([^']+)'\])?)?|\.(pipeline_[a-z_]+))?\}\}`)

var errNoSuchObject = errors.New("no such object")

// downloadArtifact copies gs://bucket/object (or every object under it as
// a folder) to local.
func downloadArtifact(ctx context.Context, runner PipelineRunner, uri, local string) error {
	bucket, object, err := splitNotebookGCS("artifact", uri, true)
	if err != nil {
		return err
	}
	if b, err := runner.ReadObject(ctx, bucket, object, 0); err == nil {
		_ = os.MkdirAll(filepath.Dir(local), 0o777)
		return os.WriteFile(local, b, 0o666)
	}
	names, err := runner.ListObjects(ctx, bucket, object+"/")
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errNoSuchObject
	}
	for _, n := range names {
		b, err := runner.ReadObject(ctx, bucket, n, 0)
		if err != nil {
			return err
		}
		p := filepath.Join(local, filepath.FromSlash(strings.TrimPrefix(n, object+"/")))
		_ = os.MkdirAll(filepath.Dir(p), 0o777)
		if err := os.WriteFile(p, b, 0o666); err != nil {
			return err
		}
	}
	return nil
}

// uploadArtifact writes local (a file, or a folder's files) to uri; a task
// that wrote nothing there leaves no object, as on Vertex AI.
func uploadArtifact(ctx context.Context, runner PipelineRunner, local, uri string) error {
	bucket, object, err := splitNotebookGCS("artifact", uri, true)
	if err != nil {
		return err
	}
	fi, err := os.Stat(local)
	if err != nil {
		return nil
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(local)
		if err != nil {
			return err
		}
		return runner.WriteObject(ctx, bucket, object, "application/octet-stream", b)
	}
	return filepath.Walk(local, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(local, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return runner.WriteObject(ctx, bucket, path.Join(object, filepath.ToSlash(rel)), "application/octet-stream", b)
	})
}

func (st *notebookStore) pipeline(name string) (*pipelineEntry, error) {
	if _, _, _, err := parseChild("name", "pipelineJobs", name); err != nil {
		return nil, err
	}
	e, ok := st.pipelines[name]
	if !ok {
		return nil, apierror.NotFound("PipelineJob %s not found", name)
	}
	return e, nil
}

func (p *pipelineService) GetPipelineJob(_ context.Context, req *aiplatformpb.GetPipelineJobRequest) (*aiplatformpb.PipelineJob, error) {
	st := p.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.pipeline(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(e.Job).(*aiplatformpb.PipelineJob), nil
}

func (p *pipelineService) ListPipelineJobs(_ context.Context, req *aiplatformpb.ListPipelineJobsRequest) (*aiplatformpb.ListPipelineJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := p.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.PipelineJob
	for _, k := range sortedNames(st.pipelines) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, proto.Clone(st.pipelines[k].Job).(*aiplatformpb.PipelineJob))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, pipelineFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	order := req.GetOrderBy()
	if order == "" {
		order = "create_time desc"
	}
	if err := aipfilter.SortBy(order, all, pipelineOrder); err != nil {
		return nil, grpcError(err)
	}
	if req.GetReadMask() != nil {
		for _, j := range all {
			keep := map[string]bool{}
			for _, p := range req.GetReadMask().GetPaths() {
				keep[camelToSnake(p)] = true
			}
			m := j.ProtoReflect()
			fields := m.Descriptor().Fields()
			for i := 0; i < fields.Len(); i++ {
				if fd := fields.Get(i); !keep[string(fd.Name())] && fd.Name() != "name" {
					m.Clear(fd)
				}
			}
		}
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = 100
	}
	items, next, err := aipfilter.Page("pipelineJobs:"+req.GetParent()+"|"+req.GetFilter()+"|"+order, all, req.GetPageToken(), size)
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListPipelineJobsResponse{PipelineJobs: items, NextPageToken: next}, nil
}

func pipelineRunning(j *aiplatformpb.PipelineJob) bool {
	switch j.GetState() {
	case aiplatformpb.PipelineState_PIPELINE_STATE_QUEUED, aiplatformpb.PipelineState_PIPELINE_STATE_PENDING,
		aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLING:
		return true
	}
	return false
}

func (p *pipelineService) DeletePipelineJob(_ context.Context, req *aiplatformpb.DeletePipelineJobRequest) (*longrunningpb.Operation, error) {
	st := p.s.notebooks()
	st.mu.Lock()
	e, err := st.pipeline(req.GetName())
	if err == nil && pipelineRunning(e.Job) {
		err = apierror.FailedPrecondition("PipelineJob %s is %s: cancel it before deleting it", req.GetName(), e.Job.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.pipelines, req.GetName())
	st.save()
	st.mu.Unlock()
	return p.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func batchNames(parent string, names []string) error {
	if _, _, err := parseParent("parent", parent); err != nil {
		return err
	}
	if len(names) == 0 {
		return apierror.InvalidArgument("names is required")
	}
	if len(names) > 32 {
		return apierror.InvalidArgument("names: a maximum of 32 PipelineJobs can be in a batch, got %d", len(names))
	}
	for _, n := range names {
		if !strings.HasPrefix(n, parent+"/pipelineJobs/") {
			return apierror.InvalidArgument("names: %q is not a PipelineJob of %s", n, parent)
		}
	}
	return nil
}

func (p *pipelineService) BatchDeletePipelineJobs(_ context.Context, req *aiplatformpb.BatchDeletePipelineJobsRequest) (*longrunningpb.Operation, error) {
	if err := batchNames(req.GetParent(), req.GetNames()); err != nil {
		return nil, grpcError(err)
	}
	st := p.s.notebooks()
	st.mu.Lock()
	var out []*aiplatformpb.PipelineJob
	for _, n := range req.GetNames() {
		e, err := st.pipeline(n)
		if err == nil && pipelineRunning(e.Job) {
			err = apierror.FailedPrecondition("PipelineJob %s is %s: cancel it before deleting it", n, e.Job.GetState())
		}
		if err != nil {
			st.mu.Unlock()
			return nil, grpcError(err)
		}
		out = append(out, proto.Clone(e.Job).(*aiplatformpb.PipelineJob))
	}
	for _, n := range req.GetNames() {
		delete(st.pipelines, n)
	}
	st.save()
	st.mu.Unlock()
	return p.s.finished(req.GetParent(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()},
		&aiplatformpb.BatchDeletePipelineJobsResponse{PipelineJobs: out}, nil)
}

// cancelPipeline starts e's cancellation; the caller holds st.mu.
func (st *notebookStore) cancelPipeline(e *pipelineEntry) error {
	if !pipelineRunning(e.Job) {
		return apierror.FailedPrecondition("PipelineJob %s is %s and cannot be cancelled", e.Job.GetName(), e.Job.GetState())
	}
	e.Job.State, e.Job.UpdateTime = aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return nil
}

func (p *pipelineService) CancelPipelineJob(_ context.Context, req *aiplatformpb.CancelPipelineJobRequest) (*emptypb.Empty, error) {
	st := p.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.pipeline(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if err := st.cancelPipeline(e); err != nil {
		return nil, grpcError(err)
	}
	return &emptypb.Empty{}, nil
}

func (p *pipelineService) BatchCancelPipelineJobs(_ context.Context, req *aiplatformpb.BatchCancelPipelineJobsRequest) (*longrunningpb.Operation, error) {
	if err := batchNames(req.GetParent(), req.GetNames()); err != nil {
		return nil, grpcError(err)
	}
	st := p.s.notebooks()
	st.mu.Lock()
	var entries []*pipelineEntry
	for _, n := range req.GetNames() {
		e, err := st.pipeline(n)
		if err == nil && !pipelineRunning(e.Job) {
			err = apierror.FailedPrecondition("PipelineJob %s is %s and cannot be cancelled", n, e.Job.GetState())
		}
		if err != nil {
			st.mu.Unlock()
			return nil, grpcError(err)
		}
		entries = append(entries, e)
	}
	var out []*aiplatformpb.PipelineJob
	for _, e := range entries {
		_ = st.cancelPipeline(e)
		out = append(out, proto.Clone(e.Job).(*aiplatformpb.PipelineJob))
	}
	st.mu.Unlock()
	return p.s.finished(req.GetParent(), &aiplatformpb.BatchCancelPipelineJobsOperationMetadata{GenericMetadata: generic()},
		&aiplatformpb.BatchCancelPipelineJobsResponse{PipelineJobs: out}, nil)
}

// savedPipelines are the jobs as the store's file keeps them; the caller
// holds st.mu.
func (st *notebookStore) savedPipelines() []json.RawMessage {
	var out []json.RawMessage
	for _, n := range sortedNames(st.pipelines) {
		if b, err := protojson.Marshal(st.pipelines[n].Job); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// loadPipelines restores saved jobs: one running when the instance
// stopped failed with it.
func (st *notebookStore) loadPipelines(raw []json.RawMessage) {
	for _, b := range raw {
		j := &aiplatformpb.PipelineJob{}
		if lenient.Unmarshal(b, j) != nil || j.GetName() == "" {
			continue
		}
		if pipelineRunning(j) {
			j.State = aiplatformpb.PipelineState_PIPELINE_STATE_FAILED
			j.Error = &status.Status{Code: int32(codes.Aborted), Message: "the instance stopped while the pipeline ran"}
			if j.EndTime == nil {
				j.EndTime = timestamppb.New(time.Now())
			}
		}
		st.pipelines[j.GetName()] = &pipelineEntry{Job: j}
	}
}
