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
// Local differences: a task's image must already be on this machine
// (CloudBurrow pulls nothing, and never from a Google registry); the
// container has no network (--network none), so a component that calls a
// Google API cannot; template_uri, nested DAG components, importers,
// trigger conditions (dsl.If) and loops (dsl.ParallelFor), caching and
// exit handlers are INVALID_ARGUMENT naming what is not run, at create.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/uuid"
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

// kfpSpec is the part of a KFP pipeline spec (IR) run here.
type kfpSpec struct {
	PipelineInfo struct {
		Name string `json:"name"`
	} `json:"pipelineInfo"`
	DefaultPipelineRoot string                  `json:"defaultPipelineRoot"`
	SchemaVersion       string                  `json:"schemaVersion"`
	Root                kfpComponent            `json:"root"`
	Components          map[string]kfpComponent `json:"components"`
	DeploymentSpec      struct {
		Executors map[string]kfpExecutor `json:"executors"`
	} `json:"deploymentSpec"`
}

type kfpComponent struct {
	ExecutorLabel    string  `json:"executorLabel"`
	DAG              *kfpDAG `json:"dag"`
	InputDefinitions struct {
		Parameters map[string]kfpParamDef `json:"parameters"`
		Artifacts  map[string]kfpArtDef   `json:"artifacts"`
	} `json:"inputDefinitions"`
	OutputDefinitions struct {
		Parameters map[string]kfpParamDef `json:"parameters"`
		Artifacts  map[string]kfpArtDef   `json:"artifacts"`
	} `json:"outputDefinitions"`
}

type kfpParamDef struct {
	ParameterType string          `json:"parameterType"`
	DefaultValue  json.RawMessage `json:"defaultValue"`
	IsOptional    bool            `json:"isOptional"`
}

type kfpArtDef struct {
	ArtifactType struct {
		SchemaTitle   string `json:"schemaTitle"`
		SchemaVersion string `json:"schemaVersion"`
	} `json:"artifactType"`
}

type kfpDAG struct {
	Tasks   map[string]kfpTask `json:"tasks"`
	Outputs json.RawMessage    `json:"outputs"`
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
	TriggerPolicy  json.RawMessage `json:"triggerPolicy"`
	ParameterIter  json.RawMessage `json:"parameterIterator"`
	ArtifactIter   json.RawMessage `json:"artifactIterator"`
	CachingOptions struct {
		EnableCache bool `json:"enableCache"`
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
}

type kfpArtIn struct {
	ComponentInputArtifact string `json:"componentInputArtifact"`
	TaskOutputArtifact     *struct {
		ProducerTask      string `json:"producerTask"`
		OutputArtifactKey string `json:"outputArtifactKey"`
	} `json:"taskOutputArtifact"`
}

type kfpExecutor struct {
	Container *struct {
		Image   string   `json:"image"`
		Command []string `json:"command"`
		Args    []string `json:"args"`
		Env     []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	} `json:"container"`
	Importer json.RawMessage `json:"importer"`
	Resolver json.RawMessage `json:"resolver"`
}

// parseKFP reads and checks a pipeline spec: what this instance runs, or
// INVALID_ARGUMENT naming what it does not.
func parseKFP(prefix string, spec *structpb.Struct) (*kfpSpec, error) {
	if spec == nil || len(spec.GetFields()) == 0 {
		return nil, apierror.InvalidArgument("%spipeline_spec is required (a compiled Kubeflow Pipelines definition); template_uri is not run on this instance", prefix)
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
	for _, name := range sortedNames(k.Root.DAG.Tasks) {
		t := k.Root.DAG.Tasks[name]
		c, ok := k.Components[t.ComponentRef.Name]
		if !ok {
			return nil, apierror.InvalidArgument("%spipeline_spec task %q names component %q, which components does not define", prefix, name, t.ComponentRef.Name)
		}
		switch {
		case c.DAG != nil:
			return nil, apierror.InvalidArgument("%spipeline_spec task %q is a nested pipeline (a DAG component), which this instance does not run", prefix, name)
		case len(t.TriggerPolicy) > 0 && string(t.TriggerPolicy) != "{}":
			return nil, apierror.InvalidArgument("%spipeline_spec task %q has a trigger policy (dsl.If, dsl.ExitHandler), which this instance does not run", prefix, name)
		case len(t.ParameterIter) > 0 || len(t.ArtifactIter) > 0:
			return nil, apierror.InvalidArgument("%spipeline_spec task %q is a loop (dsl.ParallelFor), which this instance does not run", prefix, name)
		}
		ex, ok := k.DeploymentSpec.Executors[c.ExecutorLabel]
		if !ok || ex.Container == nil {
			what := "has no container executor"
			if ok && len(ex.Importer) > 0 {
				what = "is an importer, which this instance does not run"
			} else if ok && len(ex.Resolver) > 0 {
				what = "is a resolver, which this instance does not run"
			}
			return nil, apierror.InvalidArgument("%spipeline_spec task %q's executor %q %s", prefix, name, c.ExecutorLabel, what)
		}
		if ex.Container.Image == "" {
			return nil, apierror.InvalidArgument("%spipeline_spec executor %q has no image", prefix, c.ExecutorLabel)
		}
		for _, d := range t.DependentTasks {
			if _, ok := k.Root.DAG.Tasks[d]; !ok {
				return nil, apierror.InvalidArgument("%spipeline_spec task %q depends on %q, which is not a task", prefix, name, d)
			}
		}
		for key, in := range t.Inputs.Parameters {
			if in.TaskFinalStatus != nil {
				return nil, apierror.InvalidArgument("%spipeline_spec task %q's input %q is a task's final status (dsl.ExitHandler), which this instance does not run", prefix, name, key)
			}
			if p := in.TaskOutputParameter; p != nil {
				if _, ok := k.Root.DAG.Tasks[p.ProducerTask]; !ok {
					return nil, apierror.InvalidArgument("%spipeline_spec task %q's input %q names task %q, which is not a task", prefix, name, key, p.ProducerTask)
				}
			}
		}
	}
	if _, err := kfpOrder(k.Root.DAG); err != nil {
		return nil, apierror.InvalidArgument("%spipeline_spec: %v", prefix, err)
	}
	return k, nil
}

// kfpDeps are a task's upstream tasks: its dependentTasks and every task
// whose outputs it reads.
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
		if p := t.Inputs.Parameters[k].TaskOutputParameter; p != nil {
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
	if j.GetTemplateUri() != "" {
		return nil, apierror.InvalidArgument("%stemplate_uri: running a pipeline template from Artifact Registry or a URL is not served on this instance; send the compiled definition as pipeline_spec", prefix)
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

// taskRun is a task's run: its detail and the outputs it produced.
type taskRun struct {
	detail    *aiplatformpb.PipelineTaskDetail
	params    map[string]*structpb.Value
	artifacts map[string]string // output key -> gs:// URI
	done      chan struct{}
	ok        bool
}

// runPipeline runs e's tasks as their dependencies allow.
func (s *Server) runPipeline(ctx context.Context, e *pipelineEntry, k *kfpSpec, project string) {
	defer e.cancel()
	st := s.notebooks()
	st.mu.Lock()
	runner := st.pipelineRunner
	j := proto.Clone(e.Job).(*aiplatformpb.PipelineJob)
	st.mu.Unlock()
	fail := func(code codes.Code, msg string) {
		st.setPipeline(e, func(j *aiplatformpb.PipelineJob) {
			j.State, j.EndTime = aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, timestamppb.New(st.now())
			j.Error = &status.Status{Code: int32(code), Message: msg}
		})
	}
	tok, err := s.runAsToken(e.runAs)
	if err != nil {
		fail(codes.FailedPrecondition, fmt.Sprintf("the pipeline job's service account %s: %v", e.runAs, err))
		return
	}
	ctx = WithRunAsToken(ctx, tok)
	params, _ := rootParams("", j, k)
	order, _ := kfpOrder(k.Root.DAG)
	jobUUID := uuid.NewString()
	start := timestamppb.New(st.now())
	rootDetail := &aiplatformpb.PipelineTaskDetail{TaskId: 1, TaskName: k.PipelineInfo.Name, CreateTime: start, StartTime: start,
		State: aiplatformpb.PipelineTaskDetail_RUNNING}
	runs := map[string]*taskRun{}
	for i, name := range order {
		t := k.Root.DAG.Tasks[name]
		tn := t.TaskInfo.Name
		if tn == "" {
			tn = name
		}
		runs[name] = &taskRun{done: make(chan struct{}), detail: &aiplatformpb.PipelineTaskDetail{TaskId: int64(i + 2), ParentTaskId: 1, TaskName: tn,
			TaskUniqueName: name, CreateTime: start, State: aiplatformpb.PipelineTaskDetail_PENDING}}
	}
	var dmu sync.Mutex
	publish := func() {
		dmu.Lock()
		details := []*aiplatformpb.PipelineTaskDetail{proto.Clone(rootDetail).(*aiplatformpb.PipelineTaskDetail)}
		for _, n := range order {
			details = append(details, proto.Clone(runs[n].detail).(*aiplatformpb.PipelineTaskDetail))
		}
		dmu.Unlock()
		st.setPipeline(e, func(j *aiplatformpb.PipelineJob) {
			j.JobDetail = &aiplatformpb.PipelineJobDetail{TaskDetails: details}
		})
	}
	st.setPipeline(e, func(j *aiplatformpb.PipelineJob) {
		j.State, j.StartTime = aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, start
	})
	publish()
	failFast := j.GetRuntimeConfig().GetFailurePolicy() == aiplatformpb.PipelineFailurePolicy_PIPELINE_FAILURE_POLICY_FAIL_FAST
	stopped := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	root := strings.TrimSuffix(j.GetRuntimeConfig().GetGcsOutputDirectory(), "/")
	jobID := lastSegment(j.GetName())
	for _, name := range order {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			r := runs[name]
			defer close(r.done)
			t := k.Root.DAG.Tasks[name]
			upstreamOK := true
			for _, d := range kfpDeps(t) {
				<-runs[d].done
				if !runs[d].ok {
					upstreamOK = false
				}
			}
			select {
			case <-stopped:
				upstreamOK = false
			default:
			}
			set := func(f func(d *aiplatformpb.PipelineTaskDetail)) {
				dmu.Lock()
				f(r.detail)
				dmu.Unlock()
				publish()
			}
			if ctx.Err() != nil {
				set(func(d *aiplatformpb.PipelineTaskDetail) { d.State = aiplatformpb.PipelineTaskDetail_CANCELLED })
				return
			}
			if !upstreamOK {
				set(func(d *aiplatformpb.PipelineTaskDetail) { d.State = aiplatformpb.PipelineTaskDetail_SKIPPED })
				return
			}
			now := timestamppb.New(st.now())
			set(func(d *aiplatformpb.PipelineTaskDetail) {
				d.State, d.StartTime = aiplatformpb.PipelineTaskDetail_RUNNING, now
			})
			err := s.runTask(ctx, runner, k, name, t, r, runs, params, taskEnv{jobName: jobID, jobUUID: jobUUID, root: root, project: project,
				taskID: r.detail.GetTaskId(), labels: j.GetLabels()})
			end := timestamppb.New(st.now())
			set(func(d *aiplatformpb.PipelineTaskDetail) {
				d.EndTime = end
				switch {
				case ctx.Err() != nil:
					d.State, d.Error = aiplatformpb.PipelineTaskDetail_CANCELLED, &status.Status{Code: int32(codes.Canceled), Message: "the pipeline job was cancelled"}
				case err != nil:
					d.State, d.Error = aiplatformpb.PipelineTaskDetail_FAILED, &status.Status{Code: int32(codes.FailedPrecondition), Message: err.Error()}
				default:
					d.State = aiplatformpb.PipelineTaskDetail_SUCCEEDED
					d.Outputs = map[string]*aiplatformpb.PipelineTaskDetail_ArtifactList{}
					for _, key := range sortedNames(r.artifacts) {
						d.Outputs[key] = &aiplatformpb.PipelineTaskDetail_ArtifactList{Artifacts: []*aiplatformpb.Artifact{{Uri: r.artifacts[key], DisplayName: key}}}
					}
				}
			})
			r.ok = err == nil && ctx.Err() == nil
			if !r.ok && failFast {
				stopOnce.Do(func() { close(stopped) })
			}
		}(name)
	}
	wg.Wait()
	var failed []string
	for _, n := range order {
		if runs[n].detail.GetState() == aiplatformpb.PipelineTaskDetail_FAILED {
			failed = append(failed, n)
		}
	}
	end := timestamppb.New(st.now())
	dmu.Lock()
	rootDetail.EndTime = end
	switch {
	case ctx.Err() != nil:
		rootDetail.State = aiplatformpb.PipelineTaskDetail_CANCELLED
	case len(failed) > 0:
		rootDetail.State = aiplatformpb.PipelineTaskDetail_FAILED
	default:
		rootDetail.State = aiplatformpb.PipelineTaskDetail_SUCCEEDED
	}
	dmu.Unlock()
	publish()
	st.setPipeline(e, func(j *aiplatformpb.PipelineJob) {
		j.EndTime = end
		switch {
		case ctx.Err() != nil:
			j.State = aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED
			j.Error = &status.Status{Code: int32(codes.Canceled), Message: "The pipeline job was cancelled."}
		case len(failed) > 0:
			j.State = aiplatformpb.PipelineState_PIPELINE_STATE_FAILED
			j.Error = &status.Status{Code: int32(codes.FailedPrecondition), Message: fmt.Sprintf("The DAG failed because some tasks failed. The failed tasks are: [%s].", strings.Join(failed, ", "))}
		default:
			j.State = aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED
		}
	})
}

// taskEnv is what a task's placeholders name.
type taskEnv struct {
	jobName, jobUUID, root, project string
	taskID                          int64
	labels                          map[string]string
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

// runTask runs one task's container and collects its outputs into r.
func (s *Server) runTask(ctx context.Context, runner PipelineRunner, k *kfpSpec, name string, t kfpTask, r *taskRun,
	runs map[string]*taskRun, params map[string]*structpb.Value, env taskEnv) error {
	if runner == nil {
		return fmt.Errorf("no pipeline runner on this instance")
	}
	c := k.Components[t.ComponentRef.Name]
	ex := k.DeploymentSpec.Executors[c.ExecutorLabel].Container
	dir, err := os.MkdirTemp("", "cb-pipeline-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	gcsDir, outDir := filepath.Join(dir, "gcs"), filepath.Join(dir, "outputs")
	_ = os.MkdirAll(gcsDir, 0o777)
	_ = os.MkdirAll(outDir, 0o777)
	_ = os.Chmod(gcsDir, 0o777)
	_ = os.Chmod(outDir, 0o777)

	// Inputs.
	inParams := map[string]*structpb.Value{}
	for key, in := range t.Inputs.Parameters {
		switch {
		case in.ComponentInputParameter != "":
			inParams[key] = params[in.ComponentInputParameter]
		case in.RuntimeValue != nil:
			v := &structpb.Value{}
			if err := protojson.Unmarshal(in.RuntimeValue.Constant, v); err != nil {
				return fmt.Errorf("input %q's constant: %v", key, err)
			}
			inParams[key] = v
		case in.TaskOutputParameter != nil:
			up := runs[in.TaskOutputParameter.ProducerTask]
			v, ok := up.params[in.TaskOutputParameter.OutputParameterKey]
			if !ok {
				return fmt.Errorf("input %q: task %q has no output parameter %q", key, in.TaskOutputParameter.ProducerTask, in.TaskOutputParameter.OutputParameterKey)
			}
			inParams[key] = v
		}
	}
	for key, def := range c.InputDefinitions.Parameters {
		if _, ok := inParams[key]; ok && inParams[key] != nil {
			continue
		}
		if len(def.DefaultValue) > 0 {
			v := &structpb.Value{}
			if protojson.Unmarshal(def.DefaultValue, v) == nil {
				inParams[key] = v
				continue
			}
		}
		if !def.IsOptional {
			return fmt.Errorf("input parameter %q has no value", key)
		}
	}
	inArts := map[string]string{}
	for key, in := range t.Inputs.Artifacts {
		switch {
		case in.TaskOutputArtifact != nil:
			uri, ok := runs[in.TaskOutputArtifact.ProducerTask].artifacts[in.TaskOutputArtifact.OutputArtifactKey]
			if !ok {
				return fmt.Errorf("input artifact %q: task %q has no output artifact %q", key, in.TaskOutputArtifact.ProducerTask, in.TaskOutputArtifact.OutputArtifactKey)
			}
			inArts[key] = uri
		case in.ComponentInputArtifact != "":
			return fmt.Errorf("input artifact %q: the pipeline's own input artifacts (runtime_config.input_artifacts) are not read on this instance", key)
		}
	}
	for key, uri := range inArts {
		if err := downloadArtifact(ctx, runner, uri, filepath.Join(gcsDir, strings.TrimPrefix(uri, "gs://"))); err != nil {
			return fmt.Errorf("input artifact %q (%s): %v", key, uri, err)
		}
	}
	// Outputs.
	outArts := map[string]string{}
	for key := range c.OutputDefinitions.Artifacts {
		uri := fmt.Sprintf("%s/%s/%s_%d/%s", env.root, env.jobName, name, env.taskID, key)
		if !strings.HasPrefix(uri, "gs://") {
			uri = "gs://" + strings.TrimPrefix(uri, "/")
		}
		outArts[key] = uri
		_ = os.MkdirAll(filepath.Dir(filepath.Join(gcsDir, strings.TrimPrefix(uri, "gs://"))), 0o777)
	}
	_ = filepath.Walk(gcsDir, func(p string, _ os.FileInfo, _ error) error { _ = os.Chmod(p, 0o777); return nil })
	outFile := func(key string) string { return "/cb-outputs/" + key }
	taskUUID := uuid.NewString()
	execInput := map[string]any{}
	{
		inputs := map[string]any{}
		pv := map[string]any{}
		for key, v := range inParams {
			pv[key] = v.AsInterface()
		}
		inputs["parameterValues"] = pv
		ia := map[string]any{}
		for key, uri := range inArts {
			ia[key] = map[string]any{"artifacts": []any{map[string]any{"name": key, "uri": uri, "metadata": map[string]any{}}}}
		}
		if len(ia) > 0 {
			inputs["artifacts"] = ia
		}
		outputs := map[string]any{"outputFile": "/cb-outputs/executor_output.json"}
		op := map[string]any{}
		for key := range c.OutputDefinitions.Parameters {
			op[key] = map[string]any{"outputFile": outFile(key)}
		}
		if len(op) > 0 {
			outputs["parameters"] = op
		}
		oa := map[string]any{}
		for key, uri := range outArts {
			def := c.OutputDefinitions.Artifacts[key]
			oa[key] = map[string]any{"artifacts": []any{map[string]any{"name": key, "uri": uri, "metadata": map[string]any{},
				"type": map[string]any{"schemaTitle": def.ArtifactType.SchemaTitle}}}}
		}
		if len(oa) > 0 {
			outputs["artifacts"] = oa
		}
		execInput["inputs"], execInput["outputs"] = inputs, outputs
	}
	var substErr error
	expand := func(s string) string {
		out := kfpPlaceholders.ReplaceAllStringFunc(s, func(m string) string {
			g := kfpPlaceholders.FindStringSubmatch(m)
			switch {
			case m == "{{$}}":
				b, _ := json.Marshal(execInput)
				return string(b)
			case g[1] == "inputs" && g[2] == "parameters":
				v, ok := inParams[g[3]]
				if !ok {
					substErr = fmt.Errorf("the placeholder %s names no input parameter", m)
					return ""
				}
				return paramText(v)
			case g[1] == "outputs" && g[2] == "parameters" && g[4] == "output_file":
				return outFile(g[3])
			case g[2] == "artifacts":
				uris := inArts
				if g[1] == "outputs" {
					uris = outArts
				}
				uri, ok := uris[g[3]]
				if !ok {
					substErr = fmt.Errorf("the placeholder %s names no artifact", m)
					return ""
				}
				if g[4] == "uri" {
					return uri
				}
				return gcsPath(uri)
			case g[5] == "pipeline_job_name":
				return env.jobName
			case g[5] == "pipeline_job_uuid":
				return env.jobUUID
			case g[5] == "pipeline_task_name":
				return name
			case g[5] == "pipeline_task_uuid":
				return taskUUID
			case g[5] == "pipeline_root":
				return env.root
			}
			return m
		})
		return out
	}
	cmd := make([]string, len(ex.Command))
	for i, a := range ex.Command {
		cmd[i] = expand(a)
	}
	args := make([]string, len(ex.Args))
	for i, a := range ex.Args {
		args[i] = expand(a)
	}
	if substErr != nil {
		return substErr
	}
	envVars := map[string]string{"CLOUD_ML_PROJECT_ID": env.project, "GOOGLE_CLOUD_PROJECT": env.project, "NO_GCE_CHECK": "True"}
	for _, e := range ex.Env {
		envVars[e.Name] = expand(e.Value)
	}
	labels := map[string]string{"cloudburrow.pipeline-job": env.jobName, "cloudburrow.pipeline-task": name}
	if err := runner.RunContainer(ctx, PipelineContainer{Key: env.jobName + "-" + name + "-" + taskUUID[:8], Image: ex.Image, Command: cmd, Args: args,
		Env: envVars, Mounts: map[string]string{"/gcs": gcsDir, "/cb-outputs": outDir}, Labels: labels}); err != nil {
		return err
	}
	// Output parameters: the executor output's, else each output file.
	r.params = map[string]*structpb.Value{}
	if b, err := os.ReadFile(filepath.Join(outDir, "executor_output.json")); err == nil {
		var eo struct {
			ParameterValues map[string]json.RawMessage `json:"parameterValues"`
		}
		if err := json.Unmarshal(b, &eo); err != nil {
			return fmt.Errorf("the executor output is not JSON: %v", err)
		}
		for key, raw := range eo.ParameterValues {
			v := &structpb.Value{}
			if protojson.Unmarshal(raw, v) == nil {
				r.params[key] = v
			}
		}
	}
	for key, def := range c.OutputDefinitions.Parameters {
		if _, ok := r.params[key]; ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(outDir, key))
		if err != nil {
			return fmt.Errorf("the task wrote no output parameter %q (%s)", key, outFile(key))
		}
		v, err := parseParam(def.ParameterType, string(b))
		if err != nil {
			return fmt.Errorf("output parameter %q: %v", key, err)
		}
		r.params[key] = v
	}
	// Output artifacts: whatever the task wrote at each path.
	r.artifacts = map[string]string{}
	for key, uri := range outArts {
		local := filepath.Join(gcsDir, strings.TrimPrefix(uri, "gs://"))
		if err := uploadArtifact(ctx, runner, local, uri); err != nil {
			return fmt.Errorf("output artifact %q (%s): %v", key, uri, err)
		}
		r.artifacts[key] = uri
	}
	return nil
}

// kfpPlaceholders matches {{$}}, {{$.inputs|outputs.parameters|artifacts['K'][.attr]}}
// and {{$.pipeline_*}}.
var kfpPlaceholders = regexp.MustCompile(`\{\{\$(?:\.(inputs|outputs)\.(parameters|artifacts)\['([^']+)'\](?:\.([a-z_]+))?|\.(pipeline_[a-z_]+))?\}\}`)

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
		return fmt.Errorf("no such object")
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
