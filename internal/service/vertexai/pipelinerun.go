package vertexai

// Running a pipeline job's KFP pipeline spec (#2205), as Agent Platform
// Pipelines (Vertex AI Pipelines) runs it: the root DAG's tasks run as
// their dependencies allow, and a task is
//   - a nested DAG (a sub-pipeline, or the DAG the kfp compiler makes for
//     dsl.If/Elif/Else, dsl.ExitHandler and dsl.ParallelFor), whose tasks
//     run in turn, its outputs its dag.outputs selectors' (valueFromOneof
//     picks the branch that ran, dsl.OneOf);
//   - a loop (parameterIterator or artifactIterator), running its DAG once
//     per item, at most iteratorPolicy.parallelismLimit at a time, its
//     outputs collected into lists (dsl.Collected);
//   - an importer, a resolver, or a container.
//
// A task's triggerPolicy.condition is a CEL expression over
// inputs.parameter_values; false leaves it NOT_TRIGGERED. Its strategy
// ALL_UPSTREAM_TASKS_COMPLETED (an exit task) runs once its upstream tasks
// end however they end, and takes their final status (taskFinalStatus:
// state, error, pipelineJobResourceName, pipelineTaskName); otherwise a
// task whose upstream task failed is SKIPPED. A task's
// parameterExpressionSelector (CEL over string_value, with parseJson)
// picks a field of a struct input.
//
// Caching ("Configure execution caching",
// https://docs.cloud.google.com/vertex-ai/docs/pipelines/configure-caching,
// read 2026-11-24): a task's cache key is its inputs (parameter values and
// input artifact IDs), its output definitions and its component and
// container spec, within one pipeline name; a cachingOptions.enableCache
// task whose key matches an earlier successful execution still in ML
// Metadata is SKIPPED, its execution CACHED, and its outputs that
// execution's. Entries have no TTL; deleting the execution from ML
// Metadata drops the entry.
//
// Retries: a failed container task runs again up to
// retryPolicy.maxRetryCount times, waiting backoffDuration, times
// backoffFactor for each retry after the first, up to backoffMaxDuration
// (an hour by default, kfp's); each attempt is in pipeline_task_status.
//
// ML Metadata: the run records a system.Pipeline context (the pipeline's
// name), its system.PipelineRun context (the job's ID), a
// system.DagExecution for each DAG, a system.ContainerExecution,
// system.ImporterExecution or system.ResolverExecution for each other
// task, with "input:K" and "output:K" metadata, and each artifact
// (its metadata the executor output's, so a system.Metrics artifact
// carries its metrics) with INPUT and OUTPUT events. A task's detail
// carries its execution and artifacts.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---- CEL, as KFP's driver evaluates conditions and selectors

var (
	celOnce sync.Once
	celEnv  *cel.Env
	celErr  error
)

func kfpCEL() (*cel.Env, error) {
	celOnce.Do(func() {
		celEnv, celErr = cel.NewEnv(
			cel.Variable("inputs", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("string_value", cel.StringType),
			cel.Function("parseJson", cel.Overload("parseJson_string", []*cel.Type{cel.StringType}, cel.DynType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					s, ok := v.Value().(string)
					if !ok {
						return types.NewErr("parseJson takes a string")
					}
					var x any
					if err := json.Unmarshal([]byte(s), &x); err != nil {
						return types.NewErr("parseJson: %v", err)
					}
					return types.DefaultTypeAdapter.NativeToValue(x)
				}))),
		)
	})
	return celEnv, celErr
}

func compileCEL(expr string) (cel.Program, error) {
	env, err := kfpCEL()
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return env.Program(ast)
}

// evalCondition is a trigger condition over the task's input parameters.
func evalCondition(expr string, params map[string]*structpb.Value) (bool, error) {
	prg, err := compileCEL(expr)
	if err != nil {
		return false, err
	}
	pv := map[string]any{}
	for k, v := range params {
		pv[k] = v.AsInterface()
	}
	out, _, err := prg.Eval(map[string]any{"inputs": map[string]any{"parameter_values": pv}, "string_value": ""})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("the condition %q is not a boolean", expr)
	}
	return b, nil
}

// selectCEL applies a parameterExpressionSelector to v.
func selectCEL(expr string, v *structpb.Value) (*structpb.Value, error) {
	prg, err := compileCEL(expr)
	if err != nil {
		return nil, err
	}
	out, _, err := prg.Eval(map[string]any{"inputs": map[string]any{}, "string_value": paramText(v)})
	if err != nil {
		return nil, err
	}
	nat, err := out.ConvertToNative(jsonValueType)
	if err != nil {
		return nil, err
	}
	return nat.(*structpb.Value), nil
}

var jsonValueType = reflect.TypeOf(&structpb.Value{})

// ---- a run's state

// artRef is an artifact a task reads or writes.
type artRef struct {
	Name          string         `json:"name,omitempty"` // its ML Metadata resource name
	URI           string         `json:"uri"`
	SchemaTitle   string         `json:"schemaTitle,omitempty"`
	SchemaVersion string         `json:"schemaVersion,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// taskOut is how a task ended and what it produced.
type taskOut struct {
	state  aiplatformpb.PipelineTaskDetail_State
	cached bool
	params map[string]*structpb.Value
	arts   map[string][]artRef
	err    *status.Status
}

// good is a task whose outputs downstream tasks may read.
func (o *taskOut) good() bool {
	return o != nil && (o.state == aiplatformpb.PipelineTaskDetail_SUCCEEDED || o.cached)
}

// passes is a task its downstream tasks may run after: it succeeded, was
// cached, or its condition was false.
func (o *taskOut) passes() bool {
	return o.good() || (o != nil && o.state == aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED)
}

// nodeIn is a task's (or DAG's) inputs.
type nodeIn struct {
	params map[string]*structpb.Value
	arts   map[string][]artRef
}

type pipelineRun struct {
	s        *Server
	st       *notebookStore
	e        *pipelineEntry
	k        *kfpSpec
	ctx      context.Context
	runner   PipelineRunner
	project  string
	parent   string // projects/P/locations/L
	jobName  string // the job's resource name
	jobID    string
	jobUUID  string
	root     string // the pipeline root, no trailing slash
	labels   map[string]string
	failFast bool
	stopped  chan struct{}
	stopOnce sync.Once
	slots    chan struct{}
	md       *metadataService
	store    string
	pipeCtx  *aiplatformpb.Context
	runCtx   *aiplatformpb.Context

	mu        sync.Mutex
	details   []*aiplatformpb.PipelineTaskDetail
	artifacts int
}

func (r *pipelineRun) now() *timestamppb.Timestamp { return timestamppb.New(r.st.now()) }

func (r *pipelineRun) isStopped() bool {
	select {
	case <-r.stopped:
		return true
	default:
		return false
	}
}

// newDetail adds a task's detail, PENDING; past the documented number of
// tasks a job may have it answers an error.
func (r *pipelineRun) newDetail(name, unique string, parentID int64) (*aiplatformpb.PipelineTaskDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	d := &aiplatformpb.PipelineTaskDetail{TaskId: int64(len(r.details) + 1), ParentTaskId: parentID, TaskName: name, TaskUniqueName: unique,
		CreateTime: now, State: aiplatformpb.PipelineTaskDetail_PENDING,
		PipelineTaskStatus: []*aiplatformpb.PipelineTaskDetail_PipelineTaskStatus{{UpdateTime: now, State: aiplatformpb.PipelineTaskDetail_PENDING}}}
	r.details = append(r.details, d)
	if len(r.details) > maxPipelineTasks+1 {
		return d, fmt.Errorf("the pipeline job has more than %d pipeline tasks, the most a pipeline job can have", maxPipelineTasks)
	}
	return d, nil
}

// update changes d and publishes the job's details.
func (r *pipelineRun) update(d *aiplatformpb.PipelineTaskDetail, f func(d *aiplatformpb.PipelineTaskDetail)) {
	r.mu.Lock()
	f(d)
	r.mu.Unlock()
	r.publish()
}

func terminalTask(s aiplatformpb.PipelineTaskDetail_State) bool {
	switch s {
	case aiplatformpb.PipelineTaskDetail_SUCCEEDED, aiplatformpb.PipelineTaskDetail_FAILED, aiplatformpb.PipelineTaskDetail_CANCELLED,
		aiplatformpb.PipelineTaskDetail_SKIPPED, aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED:
		return true
	}
	return false
}

// setState moves d to state, recording it in pipeline_task_status.
func (r *pipelineRun) setState(d *aiplatformpb.PipelineTaskDetail, state aiplatformpb.PipelineTaskDetail_State, err *status.Status) {
	r.update(d, func(d *aiplatformpb.PipelineTaskDetail) {
		now := r.now()
		d.State, d.Error = state, err
		d.PipelineTaskStatus = append(d.PipelineTaskStatus, &aiplatformpb.PipelineTaskDetail_PipelineTaskStatus{UpdateTime: now, State: state, Error: err})
		if state == aiplatformpb.PipelineTaskDetail_RUNNING && d.StartTime == nil {
			d.StartTime = now
		}
		if terminalTask(state) {
			if d.StartTime == nil {
				d.StartTime = now
			}
			d.EndTime = now
		}
	})
}

func (r *pipelineRun) publish() {
	r.mu.Lock()
	details := make([]*aiplatformpb.PipelineTaskDetail, len(r.details))
	for i, d := range r.details {
		details[i] = proto.Clone(d).(*aiplatformpb.PipelineTaskDetail)
	}
	r.mu.Unlock()
	r.st.setPipeline(r.e, func(j *aiplatformpb.PipelineJob) {
		j.JobDetail = &aiplatformpb.PipelineJobDetail{PipelineContext: r.pipeCtx, PipelineRunContext: r.runCtx, TaskDetails: details}
	})
}

func taskStatus(err error) *status.Status {
	if err == nil {
		return nil
	}
	if s, ok := grpcstatus.FromError(err); ok && s.Code() != codes.Unknown {
		return &status.Status{Code: int32(s.Code()), Message: s.Message()}
	}
	return &status.Status{Code: int32(codes.FailedPrecondition), Message: err.Error()}
}

// fail ends d FAILED with err.
func (r *pipelineRun) fail(d *aiplatformpb.PipelineTaskDetail, err error) *taskOut {
	st := taskStatus(err)
	r.setState(d, aiplatformpb.PipelineTaskDetail_FAILED, st)
	return &taskOut{state: aiplatformpb.PipelineTaskDetail_FAILED, err: st}
}

func (r *pipelineRun) end(d *aiplatformpb.PipelineTaskDetail, state aiplatformpb.PipelineTaskDetail_State) *taskOut {
	r.setState(d, state, nil)
	return &taskOut{state: state}
}

// ---- running

// runPipeline runs e's spec to its end.
func (s *Server) runPipeline(ctx context.Context, e *pipelineEntry, k *kfpSpec, project string) {
	defer e.cancel()
	st := s.notebooks()
	st.mu.Lock()
	runner := st.pipelineRunner
	j := proto.Clone(e.Job).(*aiplatformpb.PipelineJob)
	runSlots := st.slotsFor("runs/"+project, maxConcurrentRuns)
	taskSlots := st.slotsFor("tasks/"+project, maxParallelTasks)
	st.mu.Unlock()
	endJob := func(state aiplatformpb.PipelineState, code codes.Code, msg string) {
		st.setPipeline(e, func(j *aiplatformpb.PipelineJob) {
			j.State, j.EndTime = state, timestamppb.New(st.now())
			if msg != "" {
				j.Error = &status.Status{Code: int32(code), Message: msg}
			}
		})
	}
	// Runs past the concurrent-runs quota wait, PENDING, for a slot.
	select {
	case runSlots <- struct{}{}:
		defer func() { <-runSlots }()
	case <-ctx.Done():
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED, codes.Canceled, "The pipeline job was cancelled.")
		return
	}
	tok, err := s.runAsToken(e.runAs)
	if err != nil {
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, codes.FailedPrecondition, fmt.Sprintf("the pipeline job's service account %s: %v", e.runAs, err))
		return
	}
	ctx = WithRunAsToken(ctx, tok)
	params, _ := rootParams("", j, k)
	segs := strings.Split(j.GetName(), "/")
	r := &pipelineRun{s: s, st: st, e: e, k: k, ctx: ctx, runner: runner, project: project, parent: strings.Join(segs[:4], "/"),
		jobName: j.GetName(), jobID: lastSegment(j.GetName()), jobUUID: uuid.NewString(),
		root: strings.TrimSuffix(j.GetRuntimeConfig().GetGcsOutputDirectory(), "/"), labels: j.GetLabels(),
		failFast: j.GetRuntimeConfig().GetFailurePolicy() == aiplatformpb.PipelineFailurePolicy_PIPELINE_FAILURE_POLICY_FAIL_FAST,
		stopped:  make(chan struct{}), slots: taskSlots, md: &metadataService{s: s}}
	r.store = s.defaultMetadataStore(r.parent)
	if err := r.contexts(); err != nil {
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, codes.Internal, fmt.Sprintf("recording the run in ML Metadata: %v", err))
		return
	}
	rootArts, err := r.rootArtifacts(j)
	if err != nil {
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, codes.InvalidArgument, err.Error())
		return
	}
	rootDetail, _ := r.newDetail(k.PipelineInfo.Name, k.PipelineInfo.Name, 0)
	st.setPipeline(e, func(j *aiplatformpb.PipelineJob) {
		j.State, j.StartTime = aiplatformpb.PipelineState_PIPELINE_STATE_RUNNING, timestamppb.New(st.now())
	})
	r.setState(rootDetail, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
	in := nodeIn{params: params, arts: rootArts}
	exec, err := r.startExecution(rootDetail, "system.DagExecution", in)
	if err != nil {
		r.fail(rootDetail, err)
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, codes.Internal, fmt.Sprintf("recording the run in ML Metadata: %v", err))
		return
	}
	outs := r.runDAG(k.Root.DAG, in, rootDetail.GetTaskId(), "")
	o := r.endDAG(rootDetail, k.Root.DAG, outs, exec)
	switch {
	case ctx.Err() != nil:
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED, codes.Canceled, "The pipeline job was cancelled.")
	case o.state == aiplatformpb.PipelineTaskDetail_FAILED:
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, codes.FailedPrecondition, o.err.GetMessage())
	default:
		endJob(aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED, codes.OK, "")
	}
}

// slotsFor is a semaphore of n for key; the caller holds st.mu.
func (st *notebookStore) slotsFor(key string, n int) chan struct{} {
	if st.pipelineSlots == nil {
		st.pipelineSlots = map[string]chan struct{}{}
	}
	c, ok := st.pipelineSlots[key]
	if !ok {
		c = make(chan struct{}, n)
		st.pipelineSlots[key] = c
	}
	return c
}

// runDAG runs a DAG's tasks as their dependencies allow, each task's
// detail a child of parentID.
func (r *pipelineRun) runDAG(dag *kfpDAG, in nodeIn, parentID int64, prefix string) map[string]*taskOut {
	order, _ := kfpOrder(dag)
	details := map[string]*aiplatformpb.PipelineTaskDetail{}
	errs := map[string]error{}
	done := map[string]chan struct{}{}
	for _, n := range order {
		name := dag.Tasks[n].TaskInfo.Name
		if name == "" {
			name = n
		}
		details[n], errs[n] = r.newDetail(name, prefix+n, parentID)
		done[n] = make(chan struct{})
	}
	r.publish()
	var mu sync.Mutex
	outs := map[string]*taskOut{}
	var wg sync.WaitGroup
	for _, n := range order {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			defer close(done[n])
			t := dag.Tasks[n]
			up := map[string]*taskOut{}
			for _, dep := range kfpDeps(t) {
				<-done[dep]
				mu.Lock()
				up[dep] = outs[dep]
				mu.Unlock()
			}
			var o *taskOut
			if errs[n] != nil {
				o = r.fail(details[n], errs[n])
			} else {
				o = r.runNode(n, t, details[n], in, up, prefix)
			}
			if o.state == aiplatformpb.PipelineTaskDetail_FAILED && r.failFast {
				r.stopOnce.Do(func() { close(r.stopped) })
			}
			mu.Lock()
			outs[n] = o
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return outs
}

// runNode runs one task of a DAG once its upstream tasks ended.
func (r *pipelineRun) runNode(n string, t kfpTask, d *aiplatformpb.PipelineTaskDetail, in nodeIn, up map[string]*taskOut, prefix string) *taskOut {
	if r.ctx.Err() != nil {
		return r.end(d, aiplatformpb.PipelineTaskDetail_CANCELLED)
	}
	exitTask := t.TriggerPolicy != nil && t.TriggerPolicy.Strategy == "ALL_UPSTREAM_TASKS_COMPLETED"
	if !exitTask {
		for _, dep := range sortedNames(up) {
			if !up[dep].passes() {
				return r.end(d, aiplatformpb.PipelineTaskDetail_SKIPPED)
			}
		}
		if r.isStopped() {
			return r.end(d, aiplatformpb.PipelineTaskDetail_SKIPPED)
		}
	}
	ins, err := r.resolveInputs(t, in, up)
	if err != nil {
		return r.fail(d, err)
	}
	if t.TriggerPolicy != nil && t.TriggerPolicy.Condition != "" {
		ok, err := evalCondition(t.TriggerPolicy.Condition, ins.params)
		if err != nil {
			return r.fail(d, fmt.Errorf("evaluating the trigger condition %q: %v", t.TriggerPolicy.Condition, err))
		}
		if !ok {
			return r.end(d, aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED)
		}
	}
	c := r.k.Components[t.ComponentRef.Name]
	switch {
	case t.ParameterIter != nil || t.ArtifactIter != nil:
		return r.runLoop(n, t, d, c, ins, prefix)
	case c.DAG != nil:
		r.setState(d, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
		exec, err := r.startExecution(d, "system.DagExecution", ins)
		if err != nil {
			return r.fail(d, err)
		}
		outs := r.runDAG(c.DAG, ins, d.GetTaskId(), prefix+n+".")
		return r.endDAG(d, c.DAG, outs, exec)
	}
	return r.runExecutor(n, t, d, c, ins)
}

// finalStatus is a task's PipelineTaskFinalStatus, as an exit task takes it.
func (r *pipelineRun) finalStatus(producer string, o *taskOut) *structpb.Value {
	state, errv := "CANCELLED", map[string]any{}
	if o != nil {
		state = o.state.String()
		if o.cached {
			state = aiplatformpb.PipelineTaskDetail_SUCCEEDED.String()
		}
		if o.err != nil {
			errv = map[string]any{"code": float64(o.err.GetCode()), "message": o.err.GetMessage()}
		}
	}
	v, _ := structpb.NewValue(map[string]any{"state": state, "error": errv, "pipelineJobResourceName": r.jobName, "pipelineTaskName": producer})
	return v
}

// resolveInputs are a task's inputs from its DAG's inputs and its
// upstream tasks' outputs.
func (r *pipelineRun) resolveInputs(t kfpTask, in nodeIn, up map[string]*taskOut) (nodeIn, error) {
	out := nodeIn{params: map[string]*structpb.Value{}, arts: map[string][]artRef{}}
	for _, key := range sortedNames(t.Inputs.Parameters) {
		p := t.Inputs.Parameters[key]
		var v *structpb.Value
		switch {
		case p.ComponentInputParameter != "":
			v = in.params[p.ComponentInputParameter]
		case p.RuntimeValue != nil:
			v = &structpb.Value{}
			if err := protojson.Unmarshal(p.RuntimeValue.Constant, v); err != nil {
				return out, fmt.Errorf("input %q's constant: %v", key, err)
			}
		case p.TaskOutputParameter != nil:
			o := up[p.TaskOutputParameter.ProducerTask]
			got, ok := o.paramsOrNil()[p.TaskOutputParameter.OutputParameterKey]
			if !ok && o.good() {
				return out, fmt.Errorf("input %q: task %q has no output parameter %q", key, p.TaskOutputParameter.ProducerTask, p.TaskOutputParameter.OutputParameterKey)
			}
			v = got
		case p.TaskFinalStatus != nil:
			v = r.finalStatus(p.TaskFinalStatus.ProducerTask, up[p.TaskFinalStatus.ProducerTask])
		}
		if v != nil && p.ParameterExpressionSelector != "" {
			sel, err := selectCEL(p.ParameterExpressionSelector, v)
			if err != nil {
				return out, fmt.Errorf("input %q: parameterExpressionSelector %q: %v", key, p.ParameterExpressionSelector, err)
			}
			v = sel
		}
		if v != nil {
			out.params[key] = v
		}
	}
	for _, key := range sortedNames(t.Inputs.Artifacts) {
		a := t.Inputs.Artifacts[key]
		switch {
		case a.ComponentInputArtifact != "":
			if list, ok := in.arts[a.ComponentInputArtifact]; ok {
				out.arts[key] = list
			}
		case a.TaskOutputArtifact != nil:
			o := up[a.TaskOutputArtifact.ProducerTask]
			list, ok := o.artsOrNil()[a.TaskOutputArtifact.OutputArtifactKey]
			if !ok && o.good() {
				return out, fmt.Errorf("input artifact %q: task %q has no output artifact %q", key, a.TaskOutputArtifact.ProducerTask, a.TaskOutputArtifact.OutputArtifactKey)
			}
			if ok {
				out.arts[key] = list
			}
		}
	}
	return out, nil
}

func (o *taskOut) paramsOrNil() map[string]*structpb.Value {
	if o == nil {
		return nil
	}
	return o.params
}

func (o *taskOut) artsOrNil() map[string][]artRef {
	if o == nil {
		return nil
	}
	return o.arts
}

// endDAG ends a DAG's detail from its tasks: FAILED naming its failed
// tasks, as Vertex AI words it, else SUCCEEDED with its outputs.
func (r *pipelineRun) endDAG(d *aiplatformpb.PipelineTaskDetail, dag *kfpDAG, outs map[string]*taskOut, exec *aiplatformpb.Execution) *taskOut {
	var failed []string
	for _, n := range sortedNames(outs) {
		if outs[n].state == aiplatformpb.PipelineTaskDetail_FAILED {
			failed = append(failed, n)
		}
	}
	switch {
	case r.ctx.Err() != nil:
		r.endExecution(d, exec, aiplatformpb.Execution_CANCELLED, nil)
		return r.end(d, aiplatformpb.PipelineTaskDetail_CANCELLED)
	case len(failed) > 0:
		r.endExecution(d, exec, aiplatformpb.Execution_FAILED, nil)
		return r.fail(d, fmt.Errorf("The DAG failed because some tasks failed. The failed tasks are: [%s].", strings.Join(failed, ", ")))
	}
	o := dagOutputs(dag, outs)
	o.state = aiplatformpb.PipelineTaskDetail_SUCCEEDED
	r.endExecution(d, exec, aiplatformpb.Execution_COMPLETE, o)
	r.setOutputs(d, exec, o)
	r.setState(d, aiplatformpb.PipelineTaskDetail_SUCCEEDED, nil)
	return o
}

// dagOutputs are a DAG's outputs, by its selectors: of a OneOf the first
// branch that produced it.
func dagOutputs(dag *kfpDAG, outs map[string]*taskOut) *taskOut {
	o := &taskOut{params: map[string]*structpb.Value{}, arts: map[string][]artRef{}}
	for key, p := range dag.Outputs.Parameters {
		var sels []kfpParamSel
		if p.ValueFromParameter != nil {
			sels = append(sels, *p.ValueFromParameter)
		}
		if p.ValueFromOneof != nil {
			sels = append(sels, p.ValueFromOneof.ParameterSelectors...)
		}
		for _, s := range sels {
			if to := outs[s.ProducerSubtask]; to.good() {
				if v, ok := to.params[s.OutputParameterKey]; ok {
					o.params[key] = v
					break
				}
			}
		}
	}
	for key, a := range dag.Outputs.Artifacts {
		for _, s := range a.ArtifactSelectors {
			if to := outs[s.ProducerSubtask]; to.good() {
				if list, ok := to.arts[s.OutputArtifactKey]; ok {
					o.arts[key] = list
					break
				}
			}
		}
	}
	return o
}

// runLoop runs a ParallelFor's DAG once per item, at most
// parallelismLimit iterations at a time (0: all at once), each
// iteration a child DAG detail of the loop's.
func (r *pipelineRun) runLoop(n string, t kfpTask, d *aiplatformpb.PipelineTaskDetail, c kfpComponent, ins nodeIn, prefix string) *taskOut {
	type item struct {
		param *structpb.Value
		art   *artRef
	}
	var items []item
	var itemInput string
	if p := t.ParameterIter; p != nil {
		itemInput = p.ItemInput
		var list []any
		if p.Items.Raw != "" {
			if err := json.Unmarshal([]byte(p.Items.Raw), &list); err != nil {
				return r.fail(d, fmt.Errorf("parameterIterator.items.raw is not a JSON list: %v", err))
			}
		} else {
			v := ins.params[p.Items.InputParameter]
			switch {
			case v.GetListValue() != nil:
				list = v.GetListValue().AsSlice()
			case v.GetStringValue() != "":
				if err := json.Unmarshal([]byte(v.GetStringValue()), &list); err != nil {
					return r.fail(d, fmt.Errorf("the loop's items %q are not a JSON list", v.GetStringValue()))
				}
			case v == nil:
				return r.fail(d, fmt.Errorf("the loop's items input %q has no value", p.Items.InputParameter))
			default:
				return r.fail(d, fmt.Errorf("the loop's items input %q is not a list", p.Items.InputParameter))
			}
		}
		for _, x := range list {
			v, err := structpb.NewValue(x)
			if err != nil {
				return r.fail(d, err)
			}
			items = append(items, item{param: v})
		}
	} else {
		itemInput = t.ArtifactIter.ItemInput
		for _, a := range ins.arts[t.ArtifactIter.Items.InputArtifact] {
			a := a
			items = append(items, item{art: &a})
		}
	}
	r.mu.Lock()
	total := len(r.details) + len(items)
	r.mu.Unlock()
	if total > maxPipelineTasks+1 {
		return r.fail(d, fmt.Errorf("the loop's %d iterations take the pipeline job past %d pipeline tasks, the most a pipeline job can have", len(items), maxPipelineTasks))
	}
	r.setState(d, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
	exec, err := r.startExecution(d, "system.DagExecution", ins)
	if err != nil {
		return r.fail(d, err)
	}
	limit := len(items)
	if t.IteratorPolicy != nil && t.IteratorPolicy.ParallelismLimit > 0 {
		limit = t.IteratorPolicy.ParallelismLimit
	}
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	results := make([]*taskOut, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		name := fmt.Sprintf("%s-iteration-%d", d.GetTaskName(), i)
		id, derr := r.newDetail(name, fmt.Sprintf("%s%s.%d", prefix, n, i), d.GetTaskId())
		wg.Add(1)
		go func(i int, it item, id *aiplatformpb.PipelineTaskDetail) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if derr != nil {
				results[i] = r.fail(id, derr)
				return
			}
			if r.ctx.Err() != nil {
				results[i] = r.end(id, aiplatformpb.PipelineTaskDetail_CANCELLED)
				return
			}
			iin := nodeIn{params: map[string]*structpb.Value{}, arts: map[string][]artRef{}}
			for k, v := range ins.params {
				iin.params[k] = v
			}
			for k, v := range ins.arts {
				iin.arts[k] = v
			}
			if it.param != nil {
				iin.params[itemInput] = it.param
			} else {
				iin.arts[itemInput] = []artRef{*it.art}
			}
			r.setState(id, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
			iexec, err := r.startExecution(id, "system.DagExecution", iin)
			if err != nil {
				results[i] = r.fail(id, err)
				return
			}
			outs := r.runDAG(c.DAG, iin, id.GetTaskId(), fmt.Sprintf("%s%s.%d.", prefix, n, i))
			results[i] = r.endDAG(id, c.DAG, outs, iexec)
		}(i, it, id)
	}
	wg.Wait()
	var failed []string
	for i, o := range results {
		if o.state == aiplatformpb.PipelineTaskDetail_FAILED {
			failed = append(failed, fmt.Sprintf("%s-iteration-%d", d.GetTaskName(), i))
		}
	}
	switch {
	case r.ctx.Err() != nil:
		r.endExecution(d, exec, aiplatformpb.Execution_CANCELLED, nil)
		return r.end(d, aiplatformpb.PipelineTaskDetail_CANCELLED)
	case len(failed) > 0:
		r.endExecution(d, exec, aiplatformpb.Execution_FAILED, nil)
		return r.fail(d, fmt.Errorf("The DAG failed because some tasks failed. The failed tasks are: [%s].", strings.Join(failed, ", ")))
	}
	// dsl.Collected: each output a list of the iterations' values.
	o := &taskOut{state: aiplatformpb.PipelineTaskDetail_SUCCEEDED, params: map[string]*structpb.Value{}, arts: map[string][]artRef{}}
	for key := range c.DAG.Outputs.Parameters {
		var vals []*structpb.Value
		for _, res := range results {
			if v, ok := res.params[key]; ok {
				vals = append(vals, v)
			}
		}
		o.params[key] = structpb.NewListValue(&structpb.ListValue{Values: vals})
	}
	for key := range c.DAG.Outputs.Artifacts {
		var all []artRef
		for _, res := range results {
			all = append(all, res.arts[key]...)
		}
		o.arts[key] = all
	}
	r.endExecution(d, exec, aiplatformpb.Execution_COMPLETE, o)
	r.setOutputs(d, exec, o)
	r.setState(d, aiplatformpb.PipelineTaskDetail_SUCCEEDED, nil)
	return o
}

// withDefaults are a component's input parameters: the task's, then the
// component's defaults; a required one without a value is an error.
func withDefaults(c kfpComponent, given map[string]*structpb.Value) (map[string]*structpb.Value, error) {
	out := map[string]*structpb.Value{}
	for k, v := range given {
		out[k] = v
	}
	for _, key := range sortedNames(c.InputDefinitions.Parameters) {
		if out[key] != nil {
			continue
		}
		def := c.InputDefinitions.Parameters[key]
		if len(def.DefaultValue) > 0 {
			v := &structpb.Value{}
			if protojson.Unmarshal(def.DefaultValue, v) == nil {
				out[key] = v
				continue
			}
		}
		if !def.IsOptional {
			return nil, fmt.Errorf("input parameter %q has no value", key)
		}
	}
	return out, nil
}

// runExecutor runs an importer, resolver or container task.
func (r *pipelineRun) runExecutor(n string, t kfpTask, d *aiplatformpb.PipelineTaskDetail, c kfpComponent, ins nodeIn) *taskOut {
	ex := r.k.DeploymentSpec.Executors[c.ExecutorLabel]
	count := 0
	for _, l := range ins.arts {
		count += len(l)
	}
	if count+len(c.OutputDefinitions.Artifacts) > maxTaskArtifacts {
		return r.fail(d, fmt.Errorf("the task has %d input and output artifacts; a pipeline task can have at most %d", count+len(c.OutputDefinitions.Artifacts), maxTaskArtifacts))
	}
	for _, key := range sortedNames(c.InputDefinitions.Artifacts) {
		if _, ok := ins.arts[key]; !ok && !c.InputDefinitions.Artifacts[key].IsOptional {
			return r.fail(d, fmt.Errorf("input artifact %q has no value", key))
		}
	}
	switch {
	case ex.Importer != nil:
		return r.runImporter(d, c, ex.Importer, ins)
	case ex.Resolver != nil:
		return r.runResolver(d, c, ex.Resolver, ins)
	}
	params, err := withDefaults(c, ins.params)
	if err != nil {
		return r.fail(d, err)
	}
	ins.params = params
	key := r.cacheKey(t, c, ex.Container, ins)
	if t.CachingOptions.EnableCache {
		if o := r.fromCache(d, key, ins); o != nil {
			return o
		}
	}
	r.setState(d, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
	exec, err := r.startExecution(d, "system.ContainerExecution", ins)
	if err != nil {
		return r.fail(d, err)
	}
	attempts := 1
	var base, maxBackoff time.Duration
	factor := defaultRetryBackoffBase
	if rp := t.RetryPolicy; rp != nil {
		attempts += rp.MaxRetryCount
		base, _ = kfpDuration(rp.BackoffDuration)
		maxBackoff, _ = kfpDuration(rp.BackoffMaxDuration)
		if rp.BackoffFactor > 0 {
			factor = rp.BackoffFactor
		}
	}
	if maxBackoff == 0 {
		maxBackoff = defaultRetryBackoffMax
	}
	var o *taskOut
	for a := 0; a < attempts; a++ {
		if a > 0 {
			wait := time.Duration(float64(base) * math.Pow(factor, float64(a-1)))
			if wait > maxBackoff {
				wait = maxBackoff
			}
			select {
			case <-time.After(wait):
			case <-r.ctx.Done():
			}
			if r.ctx.Err() != nil {
				break
			}
			r.setState(d, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
		}
		o, err = r.runContainer(n, c, ex.Container, ins, d.GetTaskId())
		if err == nil || r.ctx.Err() != nil {
			break
		}
		if a+1 < attempts {
			// A failed attempt that will be retried.
			r.update(d, func(d *aiplatformpb.PipelineTaskDetail) {
				d.PipelineTaskStatus = append(d.PipelineTaskStatus, &aiplatformpb.PipelineTaskDetail_PipelineTaskStatus{UpdateTime: r.now(),
					State: aiplatformpb.PipelineTaskDetail_FAILED, Error: taskStatus(err)})
			})
		}
	}
	switch {
	case r.ctx.Err() != nil:
		r.endExecution(d, exec, aiplatformpb.Execution_CANCELLED, nil)
		r.setState(d, aiplatformpb.PipelineTaskDetail_CANCELLED, &status.Status{Code: int32(codes.Canceled), Message: "the pipeline job was cancelled"})
		return &taskOut{state: aiplatformpb.PipelineTaskDetail_CANCELLED}
	case err != nil:
		r.endExecution(d, exec, aiplatformpb.Execution_FAILED, nil)
		return r.fail(d, err)
	}
	if err := r.recordOutputs(exec, o, c); err != nil {
		return r.fail(d, err)
	}
	r.endExecution(d, exec, aiplatformpb.Execution_COMPLETE, o)
	r.putCache(key, exec, o)
	o.state = aiplatformpb.PipelineTaskDetail_SUCCEEDED
	r.setOutputs(d, exec, o)
	r.setState(d, aiplatformpb.PipelineTaskDetail_SUCCEEDED, nil)
	return o
}

// runImporter imports an artifact by URI: an artifact already in ML
// Metadata with that URI and type is reused unless reimport is set.
func (r *pipelineRun) runImporter(d *aiplatformpb.PipelineTaskDetail, c kfpComponent, im *kfpImporter, ins nodeIn) *taskOut {
	r.setState(d, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
	var uri string
	if im.ArtifactURI.RuntimeParameter != "" {
		v := ins.params[im.ArtifactURI.RuntimeParameter]
		if v == nil {
			return r.fail(d, fmt.Errorf("the importer's artifact URI parameter %q has no value", im.ArtifactURI.RuntimeParameter))
		}
		uri = paramText(v)
	} else {
		v := &structpb.Value{}
		if err := protojson.Unmarshal(im.ArtifactURI.Constant, v); err != nil {
			return r.fail(d, fmt.Errorf("the importer's artifactUri: %v", err))
		}
		uri = paramText(v)
	}
	if uri == "" {
		return r.fail(d, fmt.Errorf("the importer's artifact URI is empty"))
	}
	exec, err := r.startExecution(d, "system.ImporterExecution", ins)
	if err != nil {
		return r.fail(d, err)
	}
	meta := map[string]any{}
	if len(im.Metadata) > 0 {
		_ = json.Unmarshal(im.Metadata, &meta)
	}
	var key string
	for k := range c.OutputDefinitions.Artifacts {
		key = k
	}
	a := artRef{URI: uri, SchemaTitle: im.TypeSchema.SchemaTitle, SchemaVersion: im.TypeSchema.SchemaVersion, Metadata: meta}
	if !im.Reimport {
		resp, err := r.md.ListArtifacts(r.ctx, &aiplatformpb.ListArtifactsRequest{Parent: r.store,
			Filter: fmt.Sprintf("uri=%q AND schema_title=%q", uri, a.SchemaTitle), OrderBy: "create_time desc", PageSize: 1})
		if err == nil && len(resp.GetArtifacts()) > 0 {
			a = refOf(resp.GetArtifacts()[0])
		}
	}
	o := &taskOut{params: map[string]*structpb.Value{}, arts: map[string][]artRef{key: {a}}}
	if err := r.recordOutputs(exec, o, c); err != nil {
		r.endExecution(d, exec, aiplatformpb.Execution_FAILED, nil)
		return r.fail(d, err)
	}
	r.endExecution(d, exec, aiplatformpb.Execution_COMPLETE, o)
	o.state = aiplatformpb.PipelineTaskDetail_SUCCEEDED
	r.setOutputs(d, exec, o)
	r.setState(d, aiplatformpb.PipelineTaskDetail_SUCCEEDED, nil)
	return o
}

// runResolver answers each output with the newest artifacts of the
// project's ML Metadata store that its filter matches (limit, 1 by
// default); one with none FAILS.
func (r *pipelineRun) runResolver(d *aiplatformpb.PipelineTaskDetail, c kfpComponent, rs *kfpResolver, ins nodeIn) *taskOut {
	r.setState(d, aiplatformpb.PipelineTaskDetail_RUNNING, nil)
	exec, err := r.startExecution(d, "system.ResolverExecution", ins)
	if err != nil {
		return r.fail(d, err)
	}
	o := &taskOut{params: map[string]*structpb.Value{}, arts: map[string][]artRef{}}
	for _, key := range sortedNames(rs.OutputArtifactQueries) {
		q := rs.OutputArtifactQueries[key]
		limit := q.Limit
		if limit <= 0 {
			limit = 1
		}
		resp, err := r.md.ListArtifacts(r.ctx, &aiplatformpb.ListArtifactsRequest{Parent: r.store, Filter: q.Filter, OrderBy: "create_time desc", PageSize: int32(limit)})
		if err != nil {
			r.endExecution(d, exec, aiplatformpb.Execution_FAILED, nil)
			return r.fail(d, fmt.Errorf("resolving output %q with the filter %q: %v", key, q.Filter, grpcstatus.Convert(err).Message()))
		}
		if len(resp.GetArtifacts()) == 0 {
			r.endExecution(d, exec, aiplatformpb.Execution_FAILED, nil)
			return r.fail(d, fmt.Errorf("resolving output %q: no artifact matches the filter %q", key, q.Filter))
		}
		for _, a := range resp.GetArtifacts() {
			o.arts[key] = append(o.arts[key], refOf(a))
		}
	}
	if err := r.recordOutputs(exec, o, c); err != nil {
		return r.fail(d, err)
	}
	r.endExecution(d, exec, aiplatformpb.Execution_COMPLETE, o)
	o.state = aiplatformpb.PipelineTaskDetail_SUCCEEDED
	r.setOutputs(d, exec, o)
	r.setState(d, aiplatformpb.PipelineTaskDetail_SUCCEEDED, nil)
	return o
}

// ---- containers

// expandPlaceholders expands one command or arg: KFP's {{$...}}
// placeholders, and the JSON {"Concat": [...]} and {"IfPresent": {...}}
// placeholders, which may expand to no args or several.
func expandArg(a string, expand func(string) string, present func(string) bool) ([]string, error) {
	t := strings.TrimSpace(a)
	if !strings.HasPrefix(t, "{\"Concat\"") && !strings.HasPrefix(t, "{\"IfPresent\"") {
		return []string{expand(a)}, nil
	}
	var p struct {
		Concat    []json.RawMessage `json:"Concat"`
		IfPresent *struct {
			InputName string            `json:"InputName"`
			Then      []json.RawMessage `json:"Then"`
			Else      []json.RawMessage `json:"Else"`
		} `json:"IfPresent"`
	}
	if err := json.Unmarshal([]byte(t), &p); err != nil {
		return []string{expand(a)}, nil
	}
	sub := func(raws []json.RawMessage) ([]string, error) {
		var out []string
		for _, raw := range raws {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				s = string(raw)
			}
			more, err := expandArg(s, expand, present)
			if err != nil {
				return nil, err
			}
			out = append(out, more...)
		}
		return out, nil
	}
	if p.IfPresent != nil {
		if present(p.IfPresent.InputName) {
			return sub(p.IfPresent.Then)
		}
		return sub(p.IfPresent.Else)
	}
	parts, err := sub(p.Concat)
	if err != nil {
		return nil, err
	}
	return []string{strings.Join(parts, "")}, nil
}

// runContainer runs a container task once, from its inputs to its
// outputs.
func (r *pipelineRun) runContainer(name string, c kfpComponent, ex *kfpContainer, ins nodeIn, taskID int64) (*taskOut, error) {
	if r.runner == nil {
		return nil, fmt.Errorf("no pipeline runner on this instance")
	}
	dir, err := os.MkdirTemp("", "cb-pipeline-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	gcsDir, outDir := filepath.Join(dir, "gcs"), filepath.Join(dir, "outputs")
	for _, p := range []string{gcsDir, outDir} {
		_ = os.MkdirAll(p, 0o777)
		_ = os.Chmod(p, 0o777)
	}
	for _, key := range sortedNames(ins.arts) {
		for _, a := range ins.arts[key] {
			if !strings.HasPrefix(a.URI, "gs://") {
				continue
			}
			// As under Cloud Storage FUSE, an artifact with no object is
			// simply absent from /gcs.
			if err := downloadArtifact(r.ctx, r.runner, a.URI, filepath.Join(gcsDir, strings.TrimPrefix(a.URI, "gs://"))); err != nil && err != errNoSuchObject {
				return nil, fmt.Errorf("input artifact %q (%s): %v", key, a.URI, err)
			}
		}
	}
	outArts := map[string]artRef{}
	for key, def := range c.OutputDefinitions.Artifacts {
		uri := fmt.Sprintf("%s/%s/%s_%d/%s", r.root, r.jobID, name, taskID, key)
		if !strings.HasPrefix(uri, "gs://") {
			uri = "gs://" + strings.TrimPrefix(uri, "/")
		}
		outArts[key] = artRef{URI: uri, SchemaTitle: def.ArtifactType.SchemaTitle, SchemaVersion: def.ArtifactType.SchemaVersion, Metadata: map[string]any{}}
		_ = os.MkdirAll(filepath.Dir(filepath.Join(gcsDir, strings.TrimPrefix(uri, "gs://"))), 0o777)
	}
	_ = filepath.Walk(gcsDir, func(p string, _ os.FileInfo, _ error) error { _ = os.Chmod(p, 0o777); return nil })
	outFile := func(key string) string { return "/cb-outputs/" + key }
	taskUUID := uuid.NewString()
	artJSON := func(key string, a artRef) map[string]any {
		m := map[string]any{"name": a.Name, "uri": a.URI, "metadata": a.Metadata, "type": map[string]any{"schemaTitle": a.SchemaTitle, "schemaVersion": a.SchemaVersion}}
		if m["metadata"] == nil {
			m["metadata"] = map[string]any{}
		}
		if a.Name == "" {
			m["name"] = key
		}
		return m
	}
	inputs := map[string]any{}
	pv := map[string]any{}
	for key, v := range ins.params {
		pv[key] = v.AsInterface()
	}
	inputs["parameterValues"] = pv
	ia := map[string]any{}
	for key, list := range ins.arts {
		var arts []any
		for _, a := range list {
			arts = append(arts, artJSON(key, a))
		}
		ia[key] = map[string]any{"artifacts": arts}
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
	for key, a := range outArts {
		oa[key] = map[string]any{"artifacts": []any{artJSON(key, a)}}
	}
	if len(oa) > 0 {
		outputs["artifacts"] = oa
	}
	execInput := map[string]any{"inputs": inputs, "outputs": outputs}
	var substErr error
	first := func(uris map[string][]artRef, key string) (artRef, bool) {
		l := uris[key]
		if len(l) == 0 {
			return artRef{}, false
		}
		return l[0], true
	}
	outList := map[string][]artRef{}
	for k, a := range outArts {
		outList[k] = []artRef{a}
	}
	expand := func(s string) string {
		return kfpPlaceholders.ReplaceAllStringFunc(s, func(m string) string {
			g := kfpPlaceholders.FindStringSubmatch(m)
			switch {
			case m == "{{$}}":
				b, _ := json.Marshal(execInput)
				return string(b)
			case g[1] == "inputs" && g[2] == "parameters":
				v, ok := ins.params[g[3]]
				if !ok {
					substErr = fmt.Errorf("the placeholder %s names no input parameter", m)
					return ""
				}
				return paramText(v)
			case g[1] == "outputs" && g[2] == "parameters" && g[4] == "output_file":
				return outFile(g[3])
			case g[2] == "artifacts":
				uris := ins.arts
				if g[1] == "outputs" {
					uris = outList
				}
				a, ok := first(uris, g[3])
				if !ok {
					substErr = fmt.Errorf("the placeholder %s names no artifact", m)
					return ""
				}
				switch g[4] {
				case "uri":
					return a.URI
				case "metadata":
					b, _ := json.Marshal(a.Metadata)
					return string(b)
				}
				return gcsPath(a.URI)
			case g[5] == "pipeline_job_name":
				return r.jobID
			case g[5] == "pipeline_job_resource_name":
				return r.jobName
			case g[5] == "pipeline_job_uuid":
				return r.jobUUID
			case g[5] == "pipeline_task_name":
				return name
			case g[5] == "pipeline_task_uuid":
				return taskUUID
			case g[5] == "pipeline_root":
				return r.root
			}
			return m
		})
	}
	present := func(input string) bool {
		if _, ok := ins.params[input]; ok {
			return true
		}
		_, ok := ins.arts[input]
		return ok
	}
	var cmd, args []string
	for _, a := range ex.Command {
		more, err := expandArg(a, expand, present)
		if err != nil {
			return nil, err
		}
		cmd = append(cmd, more...)
	}
	for _, a := range ex.Args {
		more, err := expandArg(a, expand, present)
		if err != nil {
			return nil, err
		}
		args = append(args, more...)
	}
	if substErr != nil {
		return nil, substErr
	}
	envVars := map[string]string{"CLOUD_ML_PROJECT_ID": r.project, "GOOGLE_CLOUD_PROJECT": r.project, "NO_GCE_CHECK": "True"}
	for _, e := range ex.Env {
		envVars[e.Name] = expand(e.Value)
	}
	labels := map[string]string{"cloudburrow.pipeline-job": r.jobID, "cloudburrow.pipeline-task": name}
	// The parallel-task quota: tasks past it wait, as Vertex AI queues them.
	select {
	case r.slots <- struct{}{}:
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	}
	runCtx, cancel := context.WithTimeout(r.ctx, maxTaskRunTime)
	err = r.runner.RunContainer(runCtx, PipelineContainer{Key: r.jobID + "-" + name + "-" + taskUUID[:8], Image: ex.Image, Command: cmd, Args: args,
		Env: envVars, Mounts: map[string]string{"/gcs": gcsDir, "/cb-outputs": outDir}, Labels: labels})
	timedOut := runCtx.Err() == context.DeadlineExceeded
	cancel()
	<-r.slots
	if timedOut {
		return nil, fmt.Errorf("the task ran longer than 7 days, the maximum running time of a pipeline task, and was cancelled")
	}
	if err != nil {
		return nil, err
	}
	// Output parameters and artifact metadata: the executor output's, else
	// each output file.
	o := &taskOut{params: map[string]*structpb.Value{}, arts: map[string][]artRef{}}
	payload := 0
	if b, err := os.ReadFile(filepath.Join(outDir, "executor_output.json")); err == nil {
		var eo struct {
			ParameterValues map[string]json.RawMessage `json:"parameterValues"`
			Artifacts       map[string]struct {
				Artifacts []struct {
					Name     string         `json:"name"`
					URI      string         `json:"uri"`
					Metadata map[string]any `json:"metadata"`
				} `json:"artifacts"`
			} `json:"artifacts"`
		}
		if err := json.Unmarshal(b, &eo); err != nil {
			return nil, fmt.Errorf("the executor output is not JSON: %v", err)
		}
		payload = len(b)
		for key, raw := range eo.ParameterValues {
			v := &structpb.Value{}
			if protojson.Unmarshal(raw, v) == nil {
				o.params[key] = v
			}
		}
		for key, l := range eo.Artifacts {
			a, ok := outArts[key]
			if !ok || len(l.Artifacts) == 0 {
				continue
			}
			for mk, mv := range l.Artifacts[0].Metadata {
				a.Metadata[mk] = mv
			}
			outArts[key] = a
		}
	}
	for key, def := range c.OutputDefinitions.Parameters {
		if _, ok := o.params[key]; ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(outDir, key))
		if err != nil {
			return nil, fmt.Errorf("the task wrote no output parameter %q (%s)", key, outFile(key))
		}
		payload += len(b)
		v, err := parseParam(def.ParameterType, string(b))
		if err != nil {
			return nil, fmt.Errorf("output parameter %q: %v", key, err)
		}
		o.params[key] = v
	}
	if payload > maxTaskOutputPayload {
		return nil, fmt.Errorf("the task's output parameters and artifacts are %d bytes of JSON; a pipeline task's are at most %d bytes", payload, maxTaskOutputPayload)
	}
	for _, key := range sortedNames(outArts) {
		a := outArts[key]
		local := filepath.Join(gcsDir, strings.TrimPrefix(a.URI, "gs://"))
		if err := uploadArtifact(r.ctx, r.runner, local, a.URI); err != nil {
			return nil, fmt.Errorf("output artifact %q (%s): %v", key, a.URI, err)
		}
		o.arts[key] = []artRef{a}
	}
	return o, nil
}

// ---- caching

// cacheKey is a task's interface: its pipeline, component and container
// spec, its output definitions, its input parameter values and input
// artifact IDs, or its cachingOptions.cacheKey.
func (r *pipelineRun) cacheKey(t kfpTask, c kfpComponent, ex *kfpContainer, ins nodeIn) string {
	arts := map[string][]string{}
	for k, l := range ins.arts {
		for _, a := range l {
			arts[k] = append(arts[k], a.Name+"|"+a.URI)
		}
	}
	pv := map[string]any{}
	for k, v := range ins.params {
		pv[k] = v.AsInterface()
	}
	b, _ := json.Marshal(struct {
		Pipeline  string
		Component kfpComponent
		Container *kfpContainer
		Params    map[string]any
		Artifacts map[string][]string
		Key       string
	}{r.k.PipelineInfo.Name, c, ex, pv, arts, t.CachingOptions.CacheKey})
	if t.CachingOptions.CacheKey != "" {
		b, _ = json.Marshal([]string{r.k.PipelineInfo.Name, t.CachingOptions.CacheKey})
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type pipelineCacheEntry struct {
	Execution string                     `json:"execution"`
	Params    map[string]json.RawMessage `json:"params,omitempty"`
	Artifacts map[string][]artRef        `json:"artifacts,omitempty"`
}

// fromCache answers a task from an earlier execution with its cache key,
// if that execution is still in ML Metadata: SKIPPED, its execution
// CACHED.
func (r *pipelineRun) fromCache(d *aiplatformpb.PipelineTaskDetail, key string, ins nodeIn) *taskOut {
	r.st.mu.Lock()
	raw, ok := r.st.pipelineCache[r.parent+"/"+key]
	r.st.mu.Unlock()
	if !ok {
		return nil
	}
	var ce pipelineCacheEntry
	if json.Unmarshal(raw, &ce) != nil {
		return nil
	}
	if _, err := r.md.GetExecution(r.ctx, &aiplatformpb.GetExecutionRequest{Name: ce.Execution}); err != nil {
		r.st.mu.Lock()
		delete(r.st.pipelineCache, r.parent+"/"+key)
		r.st.save()
		r.st.mu.Unlock()
		return nil
	}
	o := &taskOut{state: aiplatformpb.PipelineTaskDetail_SKIPPED, cached: true, params: map[string]*structpb.Value{}, arts: ce.Artifacts}
	if o.arts == nil {
		o.arts = map[string][]artRef{}
	}
	for k, b := range ce.Params {
		v := &structpb.Value{}
		if protojson.Unmarshal(b, v) == nil {
			o.params[k] = v
		}
	}
	exec, err := r.startExecution(d, "system.ContainerExecution", ins)
	if err != nil {
		return nil
	}
	if err := r.addEvents(exec, o.arts, aiplatformpb.Event_OUTPUT); err != nil {
		return nil
	}
	r.endExecution(d, exec, aiplatformpb.Execution_CACHED, o)
	r.setOutputs(d, exec, o)
	r.setState(d, aiplatformpb.PipelineTaskDetail_SKIPPED, nil)
	return o
}

func (r *pipelineRun) putCache(key string, exec *aiplatformpb.Execution, o *taskOut) {
	ce := pipelineCacheEntry{Execution: exec.GetName(), Params: map[string]json.RawMessage{}, Artifacts: o.arts}
	for k, v := range o.params {
		b, _ := protojson.Marshal(v)
		ce.Params[k] = b
	}
	b, _ := json.Marshal(ce)
	r.st.mu.Lock()
	if r.st.pipelineCache == nil {
		r.st.pipelineCache = map[string]json.RawMessage{}
	}
	r.st.pipelineCache[r.parent+"/"+key] = b
	r.st.save()
	r.st.mu.Unlock()
}

// ---- ML Metadata

func mdID(s string) string {
	id := strings.Trim(pipelineNameSafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(id) > 120 {
		id = id[:120]
	}
	if id == "" {
		id = "pipeline"
	}
	return id
}

// contexts records the run's system.Pipeline and system.PipelineRun
// contexts, the run's a child of the pipeline's.
func (r *pipelineRun) contexts() error {
	pid := mdID(r.k.PipelineInfo.Name)
	pc, err := r.md.GetContext(r.ctx, &aiplatformpb.GetContextRequest{Name: r.store + "/contexts/" + pid})
	if err != nil {
		pc, err = r.md.CreateContext(r.ctx, &aiplatformpb.CreateContextRequest{Parent: r.store, ContextId: pid,
			Context: &aiplatformpb.Context{DisplayName: r.k.PipelineInfo.Name, SchemaTitle: "system.Pipeline", SchemaVersion: "0.0.1"}})
		if grpcstatus.Code(err) == codes.AlreadyExists {
			pc, err = r.md.GetContext(r.ctx, &aiplatformpb.GetContextRequest{Name: r.store + "/contexts/" + pid})
		}
		if err != nil {
			return err
		}
	}
	meta, _ := structpb.NewStruct(map[string]any{"pipeline_job_name": r.jobName, "pipeline_root": r.root})
	rc, err := r.md.CreateContext(r.ctx, &aiplatformpb.CreateContextRequest{Parent: r.store, ContextId: r.jobID,
		Context: &aiplatformpb.Context{DisplayName: r.jobID, SchemaTitle: "system.PipelineRun", SchemaVersion: "0.0.1", Metadata: meta, Labels: r.labels}})
	if grpcstatus.Code(err) == codes.AlreadyExists {
		rc, err = r.md.GetContext(r.ctx, &aiplatformpb.GetContextRequest{Name: r.store + "/contexts/" + r.jobID})
	}
	if err != nil {
		return err
	}
	if _, err := r.md.AddContextChildren(r.ctx, &aiplatformpb.AddContextChildrenRequest{Context: pc.GetName(), ChildContexts: []string{rc.GetName()}}); err != nil {
		return err
	}
	if pc2, err := r.md.GetContext(r.ctx, &aiplatformpb.GetContextRequest{Name: pc.GetName()}); err == nil {
		pc = pc2
	}
	if rc2, err := r.md.GetContext(r.ctx, &aiplatformpb.GetContextRequest{Name: rc.GetName()}); err == nil {
		rc = rc2
	}
	r.pipeCtx, r.runCtx = pc, rc
	return nil
}

// rootArtifacts are runtime_config.input_artifacts, by their ML Metadata
// artifact IDs.
func (r *pipelineRun) rootArtifacts(j *aiplatformpb.PipelineJob) (map[string][]artRef, error) {
	out := map[string][]artRef{}
	for _, key := range sortedNames(j.GetRuntimeConfig().GetInputArtifacts()) {
		id := j.GetRuntimeConfig().GetInputArtifacts()[key].GetArtifactId()
		a, err := r.md.GetArtifact(r.ctx, &aiplatformpb.GetArtifactRequest{Name: r.store + "/artifacts/" + id})
		if err != nil {
			return nil, fmt.Errorf("runtime_config.input_artifacts[%q]: artifact %s: %s", key, id, grpcstatus.Convert(err).Message())
		}
		out[key] = []artRef{refOf(a)}
	}
	return out, nil
}

func refOf(a *aiplatformpb.Artifact) artRef {
	m := map[string]any{}
	if a.GetMetadata() != nil {
		m = a.GetMetadata().AsMap()
	}
	return artRef{Name: a.GetName(), URI: a.GetUri(), SchemaTitle: a.GetSchemaTitle(), SchemaVersion: a.GetSchemaVersion(), Metadata: m}
}

// startExecution records a task's execution, RUNNING, in the run's
// context, with its inputs as "input:K" metadata and INPUT events.
func (r *pipelineRun) startExecution(d *aiplatformpb.PipelineTaskDetail, schema string, ins nodeIn) (*aiplatformpb.Execution, error) {
	meta := map[string]any{"task_name": d.GetTaskName(), "pipeline_job_name": r.jobName}
	for k, v := range ins.params {
		meta["input:"+k] = v.AsInterface()
	}
	sm, err := structpb.NewStruct(meta)
	if err != nil {
		return nil, err
	}
	exec, err := r.md.CreateExecution(r.ctx, &aiplatformpb.CreateExecutionRequest{Parent: r.store,
		Execution: &aiplatformpb.Execution{DisplayName: d.GetTaskName(), SchemaTitle: schema, SchemaVersion: "0.0.1", State: aiplatformpb.Execution_RUNNING, Metadata: sm}})
	if err != nil {
		return nil, fmt.Errorf("recording the task's execution in ML Metadata: %s", grpcstatus.Convert(err).Message())
	}
	if _, err := r.md.AddContextArtifactsAndExecutions(r.ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: r.runCtx.GetName(), Executions: []string{exec.GetName()}}); err != nil {
		return nil, fmt.Errorf("adding the task's execution to the run's context: %s", grpcstatus.Convert(err).Message())
	}
	if err := r.addEvents(exec, ins.arts, aiplatformpb.Event_INPUT); err != nil {
		return nil, err
	}
	var inputs map[string]*aiplatformpb.PipelineTaskDetail_ArtifactList
	if len(ins.arts) > 0 {
		inputs = map[string]*aiplatformpb.PipelineTaskDetail_ArtifactList{}
		for _, k := range sortedNames(ins.arts) {
			inputs[k] = r.artifactList(ins.arts[k])
		}
	}
	r.update(d, func(d *aiplatformpb.PipelineTaskDetail) {
		d.Execution = proto.Clone(exec).(*aiplatformpb.Execution)
		d.Inputs = inputs
	})
	return exec, nil
}

// addEvents records the artifacts as the execution's inputs or outputs,
// each event labelled with its key.
func (r *pipelineRun) addEvents(exec *aiplatformpb.Execution, arts map[string][]artRef, kind aiplatformpb.Event_Type) error {
	var events []*aiplatformpb.Event
	var names []string
	for _, k := range sortedNames(arts) {
		for _, a := range arts[k] {
			if a.Name == "" {
				continue
			}
			events = append(events, &aiplatformpb.Event{Artifact: a.Name, Type: kind, Labels: map[string]string{"key": mdLabel(k)}})
			names = append(names, a.Name)
		}
	}
	if len(events) == 0 {
		return nil
	}
	if _, err := r.md.AddExecutionEvents(r.ctx, &aiplatformpb.AddExecutionEventsRequest{Execution: exec.GetName(), Events: events}); err != nil {
		return fmt.Errorf("recording the task's artifacts in ML Metadata: %s", grpcstatus.Convert(err).Message())
	}
	if _, err := r.md.AddContextArtifactsAndExecutions(r.ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: r.runCtx.GetName(), Artifacts: names}); err != nil {
		return fmt.Errorf("adding the task's artifacts to the run's context: %s", grpcstatus.Convert(err).Message())
	}
	return nil
}

// mdLabel is an event label value: lowercase letters, digits, - and _.
func mdLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// recordOutputs creates the task's new output artifacts (those without
// an ML Metadata name) and their OUTPUT events.
func (r *pipelineRun) recordOutputs(exec *aiplatformpb.Execution, o *taskOut, c kfpComponent) error {
	r.mu.Lock()
	for _, l := range o.arts {
		r.artifacts += len(l)
	}
	n := r.artifacts
	r.mu.Unlock()
	if n > maxJobArtifacts {
		return fmt.Errorf("the pipeline job has more than %d input and output artifacts, the most a pipeline job can have", maxJobArtifacts)
	}
	for _, k := range sortedNames(o.arts) {
		for i, a := range o.arts[k] {
			if a.Name != "" {
				continue
			}
			meta, err := structpb.NewStruct(a.Metadata)
			if err != nil {
				return fmt.Errorf("output artifact %q's metadata: %v", k, err)
			}
			if a.SchemaTitle == "" {
				a.SchemaTitle = c.OutputDefinitions.Artifacts[k].ArtifactType.SchemaTitle
			}
			created, err := r.md.CreateArtifact(r.ctx, &aiplatformpb.CreateArtifactRequest{Parent: r.store, Artifact: &aiplatformpb.Artifact{
				DisplayName: k, Uri: a.URI, SchemaTitle: a.SchemaTitle, SchemaVersion: a.SchemaVersion, Metadata: meta, State: aiplatformpb.Artifact_LIVE}})
			if err != nil {
				return fmt.Errorf("recording output artifact %q in ML Metadata: %s", k, grpcstatus.Convert(err).Message())
			}
			o.arts[k][i] = refOf(created)
		}
	}
	return r.addEvents(exec, o.arts, aiplatformpb.Event_OUTPUT)
}

// endExecution records an execution's end and outputs ("output:K").
func (r *pipelineRun) endExecution(d *aiplatformpb.PipelineTaskDetail, exec *aiplatformpb.Execution, state aiplatformpb.Execution_State, o *taskOut) {
	if exec == nil {
		return
	}
	meta := map[string]any{}
	if exec.GetMetadata() != nil {
		meta = exec.GetMetadata().AsMap()
	}
	if o != nil {
		for k, v := range o.params {
			meta["output:"+k] = v.AsInterface()
		}
	}
	sm, _ := structpb.NewStruct(meta)
	upd := &aiplatformpb.Execution{Name: exec.GetName(), State: state, Metadata: sm}
	if got, err := r.md.UpdateExecution(context.WithoutCancel(r.ctx), &aiplatformpb.UpdateExecutionRequest{Execution: upd,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state", "metadata"}}}); err == nil {
		proto.Reset(exec)
		proto.Merge(exec, got)
		r.update(d, func(d *aiplatformpb.PipelineTaskDetail) { d.Execution = proto.Clone(got).(*aiplatformpb.Execution) })
	}
}

func (r *pipelineRun) artifactList(l []artRef) *aiplatformpb.PipelineTaskDetail_ArtifactList {
	out := &aiplatformpb.PipelineTaskDetail_ArtifactList{}
	for _, a := range l {
		if a.Name != "" {
			if got, err := r.md.GetArtifact(context.WithoutCancel(r.ctx), &aiplatformpb.GetArtifactRequest{Name: a.Name}); err == nil {
				out.Artifacts = append(out.Artifacts, got)
				continue
			}
		}
		meta, _ := structpb.NewStruct(a.Metadata)
		out.Artifacts = append(out.Artifacts, &aiplatformpb.Artifact{Name: a.Name, Uri: a.URI, SchemaTitle: a.SchemaTitle, SchemaVersion: a.SchemaVersion, Metadata: meta})
	}
	return out
}

// setOutputs puts the execution and output artifacts on the detail.
func (r *pipelineRun) setOutputs(d *aiplatformpb.PipelineTaskDetail, exec *aiplatformpb.Execution, o *taskOut) {
	outputs := map[string]*aiplatformpb.PipelineTaskDetail_ArtifactList{}
	keys := make([]string, 0, len(o.arts))
	for k := range o.arts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := r.artifactList(o.arts[k])
		for _, a := range l.Artifacts {
			if a.DisplayName == "" {
				a.DisplayName = k
			}
		}
		outputs[k] = l
	}
	r.update(d, func(d *aiplatformpb.PipelineTaskDetail) {
		if exec != nil {
			d.Execution = proto.Clone(exec).(*aiplatformpb.Execution)
		}
		if len(outputs) > 0 {
			d.Outputs = outputs
		}
	})
}
