package run

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Executions (#582): google.cloud.run.v2.Executions over batch/v1 Jobs.
//
// An execution is one batch/v1 Job in Indexed completion mode: task_count
// is its completions, parallelism its parallelism, max_retries its
// backoffLimitPerIndex (retries per task, as Cloud Run counts them), and the
// task timeout each pod's activeDeadlineSeconds (per attempt, as Cloud Run
// applies it). Everything an execution reports is read from the Job's
// status, and a failed task's message from its pod.

// Annotations on an execution's batch Job.
const (
	// annTaskTemplate is the task template the execution ran, as protojson,
	// so it reads back as set rather than re-derived from the pod spec.
	annTaskTemplate    = "cloudburrow.dev/run-task-template"
	annExecLabels      = "cloudburrow.dev/run-execution-labels"
	annExecAnnotations = "cloudburrow.dev/run-execution-annotations"
	// annCancelled marks an execution CancelExecution suspended.
	annCancelled = "cloudburrow.dev/cancelled"
	// The batch controller's own pod label and annotation.
	labelJobName       = "batch.kubernetes.io/job-name"
	annCompletionIndex = "batch.kubernetes.io/job-completion-index"
)

// renderExecution renders the batch/v1 Job for one execution of a job.
func renderExecution(spec *runpb.Job, jobID, execID string, tt *runpb.TaskTemplate, taskCount int32,
	namespace, instance string, secrets SecretResolver) (string, error) {
	et := spec.GetTemplate()
	execName := spec.GetName() + "/executions/" + execID
	raw, err := protojson.Marshal(tt)
	if err != nil {
		return "", apierror.Internal(err, "encode the task template of %s", execName)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
  labels:
    %s: "true"
    %s: %q
    %s: %s
  annotations:
    %s: %q
    %s: %q
    %s: %q
`, execID, namespace, labelOwned, labelInstance, instance, labelRunJob, jobID,
		annRunName, execName, annRunJob, spec.GetName(), annTaskTemplate, string(raw))
	jsonAnnotation(&b, "    ", annExecLabels, et.GetLabels(), len(et.GetLabels()) == 0)
	jsonAnnotation(&b, "    ", annExecAnnotations, et.GetAnnotations(), len(et.GetAnnotations()) == 0)
	fmt.Fprintf(&b, `spec:
  completions: %d
  parallelism: %d
  completionMode: Indexed
  backoffLimitPerIndex: %d
  template:
    metadata:
      labels:
        %s: %s
    spec:
      restartPolicy: Never
`, taskCount, parallelismOf(et.GetParallelism(), taskCount), tt.GetMaxRetries(), labelRunJob, jobID)
	if t := tt.GetTimeout(); t != nil {
		secs := int64(t.AsDuration().Seconds())
		fmt.Fprintf(&b, "      activeDeadlineSeconds: %d\n", max(secs, 1))
	}
	b.WriteString("      containers:\n")
	// Cloud Run's task environment, where Kubernetes has a counterpart.
	// CLOUD_RUN_TASK_ATTEMPT has none: no pod field counts its retries.
	injected := []injectedEnv{
		{name: "CLOUD_RUN_JOB", value: jobID},
		{name: "CLOUD_RUN_EXECUTION", value: execID},
		{name: "CLOUD_RUN_TASK_INDEX", fieldPath: "metadata.annotations['" + annCompletionIndex + "']"},
		{name: "CLOUD_RUN_TASK_COUNT", value: strconv.Itoa(int(taskCount))},
	}
	if err := renderContainers(&b, tt.GetContainers(), containerRender{
		project: projectOf(spec.GetName()), secrets: secrets, batch: true, injected: injected}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// executionState is what an execution's batch Job says about it.
type executionState struct {
	terminal   bool
	succeeded  bool
	cancelled  bool
	cancelling bool
	running    bool
	// message is the batch controller's reason for a failure; a pod's
	// own message is preferred where there is one.
	message    string
	finished   time.Time
	completion runpb.ExecutionReference_CompletionStatus
}

// jobCondition returns a batch Job condition that is True.
func jobCondition(j kjob, typ string) (ksvcCondition, bool) {
	for _, c := range j.Status.Conditions {
		if c.Type == typ && c.Status == "True" {
			return c, true
		}
	}
	return ksvcCondition{}, false
}

// executionStateOf reads an execution's state from its batch Job.
func executionStateOf(j kjob) executionState {
	var st executionState
	if c, ok := jobCondition(j, "Complete"); ok {
		st.terminal, st.succeeded = true, true
		st.finished = c.LastTransitionTime
		if j.Status.CompletionTime != nil {
			st.finished = *j.Status.CompletionTime
		}
		st.completion = runpb.ExecutionReference_EXECUTION_SUCCEEDED
		return st
	}
	if c, ok := jobCondition(j, "Failed"); ok {
		st.terminal = true
		st.finished = c.LastTransitionTime
		st.message = c.Message
		if st.message == "" {
			st.message = c.Reason
		}
		st.completion = runpb.ExecutionReference_EXECUTION_FAILED
		return st
	}
	if j.Metadata.Annotations[annCancelled] == "true" {
		terminating := int32(0)
		if j.Status.Terminating != nil {
			terminating = *j.Status.Terminating
		}
		if j.Status.Active == 0 && terminating == 0 {
			st.terminal, st.cancelled = true, true
			st.completion = runpb.ExecutionReference_EXECUTION_CANCELLED
			st.finished = j.Metadata.CreationTimestamp
			if c, ok := jobCondition(j, "Suspended"); ok {
				st.finished = c.LastTransitionTime
			}
			return st
		}
		st.cancelling = true
	}
	st.running = j.Status.Active > 0
	st.completion = runpb.ExecutionReference_EXECUTION_PENDING
	if st.running {
		st.completion = runpb.ExecutionReference_EXECUTION_RUNNING
	}
	return st
}

// countIndexes counts the tasks in a batch index list such as "1,3-5".
func countIndexes(list string) int32 {
	var n int32
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		if !isRange {
			n++
			continue
		}
		a, errA := strconv.Atoi(lo)
		b, errB := strconv.Atoi(hi)
		if errA == nil && errB == nil && b >= a {
			n += int32(b - a + 1)
		}
	}
	return n
}

// fromBatchJob renders an execution. podMessage, when set, is the failed
// task's own message, which says more than the batch controller's.
func fromBatchJob(j kjob, podMessage string) *runpb.Execution {
	ann := j.Metadata.Annotations
	e := &runpb.Execution{
		Name:               ann[annRunName],
		Job:                ann[annRunJob],
		Uid:                j.Metadata.UID,
		Etag:               j.Metadata.ResourceVersion,
		Generation:         max(j.Metadata.Generation, 1),
		ObservedGeneration: max(j.Metadata.Generation, 1),
		CreateTime:         timestamppb.New(j.Metadata.CreationTimestamp),
		RunningCount:       j.Status.Active,
		SucceededCount:     j.Status.Succeeded,
	}
	readJSONAnnotation(ann, annExecLabels, &e.Labels)
	readJSONAnnotation(ann, annExecAnnotations, &e.Annotations)
	if raw := ann[annTaskTemplate]; raw != "" {
		var tt runpb.TaskTemplate
		if protojson.Unmarshal([]byte(raw), &tt) == nil {
			e.Template = &tt
		}
	}
	if j.Spec.Completions != nil {
		e.TaskCount = *j.Spec.Completions
	}
	if j.Spec.Parallelism != nil {
		e.Parallelism = *j.Spec.Parallelism
	}
	if j.Status.StartTime != nil {
		e.StartTime = timestamppb.New(*j.Status.StartTime)
	}
	if j.Status.FailedIndexes != nil {
		e.FailedCount = countIndexes(*j.Status.FailedIndexes)
	}
	// Every failed pod beyond the tasks that failed for good was retried.
	e.RetriedCount = max(j.Status.Failed-e.FailedCount, 0)

	st := executionStateOf(j)
	if st.cancelled {
		e.CancelledCount = max(e.TaskCount-e.SucceededCount-e.FailedCount, 0)
	}
	updated := j.Metadata.CreationTimestamp
	for _, c := range j.Status.Conditions {
		if c.LastTransitionTime.After(updated) {
			updated = c.LastTransitionTime
		}
	}
	e.UpdateTime = timestamppb.New(updated)

	started := &runpb.Condition{Type: "Started", State: runpb.Condition_CONDITION_PENDING}
	if j.Status.StartTime != nil {
		started.State = runpb.Condition_CONDITION_SUCCEEDED
		started.LastTransitionTime = timestamppb.New(*j.Status.StartTime)
	}
	completed := &runpb.Condition{Type: "Completed", State: runpb.Condition_CONDITION_PENDING}
	switch {
	case st.succeeded:
		completed.State = runpb.Condition_CONDITION_SUCCEEDED
		completed.Message = fmt.Sprintf("Execution completed successfully: %d of %d tasks succeeded.", e.SucceededCount, e.TaskCount)
	case st.terminal && !st.cancelled:
		completed.State = runpb.Condition_CONDITION_FAILED
		completed.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_NON_ZERO_EXIT_CODE}
		completed.Message = st.message
		if podMessage != "" {
			completed.Message = podMessage
		}
	case st.cancelled:
		completed.State = runpb.Condition_CONDITION_FAILED
		completed.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_CANCELLED}
		completed.Message = "The execution was cancelled."
	case st.cancelling:
		completed.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_CANCELLING}
	}
	if st.terminal {
		e.CompletionTime = timestamppb.New(st.finished)
		completed.LastTransitionTime = e.CompletionTime
	}
	e.Conditions = []*runpb.Condition{completed, started}
	e.Reconciling = !st.terminal
	return e
}

// failureMessage is the newest failed task's own account: the container
// that exited non-zero, its exit code and the tail of its log (the
// termination message FallbackToLogsOnError keeps), or the pod's reason,
// such as a task timeout's DeadlineExceeded.
func failureMessage(pods []kpod) string {
	sort.SliceStable(pods, func(a, b int) bool {
		return pods[a].Metadata.CreationTimestamp.After(pods[b].Metadata.CreationTimestamp)
	})
	for _, p := range pods {
		if p.Status.Phase != "Failed" {
			continue
		}
		task := p.Metadata.Annotations[annCompletionIndex]
		for _, c := range p.Status.ContainerStatuses {
			t := c.State.Terminated
			if t == nil || t.ExitCode == 0 {
				continue
			}
			msg := fmt.Sprintf("Task %s failed: container %s exited with code %d", task, c.Name, t.ExitCode)
			if detail := strings.TrimSpace(t.Message); detail != "" {
				msg += ": " + detail
			} else if t.Reason != "" {
				msg += " (" + t.Reason + ")"
			}
			return msg
		}
		if p.Status.Reason != "" || p.Status.Message != "" {
			return strings.TrimSpace(fmt.Sprintf("Task %s failed: %s %s", task, p.Status.Reason, p.Status.Message))
		}
	}
	return ""
}

// loadExecution reads an execution's batch Job and checks it is the one
// named: owned by the adapter and an execution of that job.
func (s *Server) loadExecution(ctx context.Context, name string) (kjob, error) {
	n, err := resource.Parse(name)
	if err != nil || n.ParentCollection != "jobs" || n.Collection != "executions" {
		return kjob{}, apierror.InvalidArgument("%q is not a Cloud Run execution name", name)
	}
	var j kjob
	if err := s.kn.getObject(ctx, &j, resBatchJob, n.ID, "execution "+name); err != nil {
		return kjob{}, err
	}
	if j.Metadata.Labels[labelOwned] != "true" || j.Metadata.Labels[labelRunJob] != n.ParentID ||
		j.Metadata.Annotations[annRunName] != name {
		return kjob{}, apierror.NotFound("execution %s not found", name)
	}
	return j, nil
}

// execution renders a batch Job, reading its pods for the failing task's
// message only when it has failed.
func (s *Server) execution(ctx context.Context, j kjob) *runpb.Execution {
	st := executionStateOf(j)
	msg := ""
	if st.terminal && !st.succeeded && !st.cancelled {
		if pods, err := listObjects[kpod](ctx, s.kn, resPod, labelJobName+"="+j.Metadata.Name); err == nil {
			msg = failureMessage(pods)
		}
	}
	return fromBatchJob(j, msg)
}

// awaitExecution polls an execution until it completes, keeping the
// operation's metadata current, then completes the operation: with the
// execution when it succeeded, and with the failing task's message when it
// did not.
//
// Bounded polling, as awaitReady is: the cluster's clock is not ours. The
// bound is generous, since a task may use every retry at its full timeout.
func (s *Server) awaitExecution(ctx context.Context, opName, execID string, bound time.Duration) {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		var j kjob
		err := s.kn.getObject(ctx, &j, resBatchJob, execID, "execution "+execID)
		if err != nil && apierror.From(err).Code == codes.NotFound {
			_ = s.ops.Fail(opName, apierror.NotFound("execution %s was deleted before it completed", execID))
			return
		}
		if err == nil {
			exec := s.execution(ctx, j)
			_ = s.ops.SetMetadata(opName, exec)
			st := executionStateOf(j)
			switch {
			case st.succeeded:
				_ = s.ops.Succeed(opName, exec)
				return
			case st.cancelled:
				_ = s.ops.Fail(opName, apierror.FailedPrecondition("execution %s was cancelled", exec.GetName()))
				return
			case st.terminal:
				msg := st.message
				for _, c := range exec.GetConditions() {
					if c.GetType() == "Completed" && c.GetMessage() != "" {
						msg = c.GetMessage()
					}
				}
				_ = s.ops.Fail(opName, apierror.FailedPrecondition("execution %s failed: %s", exec.GetName(), msg))
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	_ = s.ops.Fail(opName, apierror.FailedPrecondition("execution %s did not complete within %s", execID, bound))
}

// ExecutionsServer serves google.cloud.run.v2.Executions.
type ExecutionsServer struct {
	runpb.UnimplementedExecutionsServer
	s *Server
}

// Executions returns the Executions service over the same adapter.
func (s *Server) Executions() *ExecutionsServer { return &ExecutionsServer{s: s} }

// Register adds the Executions service to a gRPC server.
func (x *ExecutionsServer) Register(g *grpc.Server) { runpb.RegisterExecutionsServer(g, x) }

// GetExecution reads an execution from its batch Job's status.
func (x *ExecutionsServer) GetExecution(ctx context.Context, req *runpb.GetExecutionRequest) (*runpb.Execution, error) {
	j, err := x.s.loadExecution(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	return x.s.execution(ctx, j), nil
}

// ListExecutions lists a job's executions, newest first. A job that does
// not exist is NOT_FOUND, not an empty page.
func (x *ExecutionsServer) ListExecutions(ctx context.Context, req *runpb.ListExecutionsRequest) (*runpb.ListExecutionsResponse, error) {
	_, id, err := parseJobName(req.GetParent())
	if err != nil {
		return nil, err
	}
	if _, err := x.s.loadJob(ctx, req.GetParent(), id); err != nil {
		return nil, err
	}
	all, err := x.s.jobExecutions(ctx, id)
	if err != nil {
		return nil, err
	}
	var execs []kjob
	for _, j := range all {
		if j.Metadata.Annotations[annRunJob] == req.GetParent() {
			execs = append(execs, j)
		}
	}
	sortExecutionsNewestFirst(execs)
	// Paging sorts its keys ascending, so the key is the position.
	byKey := map[string]kjob{}
	keys := make([]string, 0, len(execs))
	for i, j := range execs {
		key := fmt.Sprintf("%08d/%s", i, j.Metadata.Name)
		byKey[key] = j
		keys = append(keys, key)
	}
	page, next, err := paging.Page("run-executions:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListExecutionsResponse{NextPageToken: next}
	for _, k := range page {
		resp.Executions = append(resp.Executions, x.s.execution(ctx, byKey[k]))
	}
	return resp, nil
}

// DeleteExecution removes an execution's batch Job and its pods. A running
// execution is stopped by the delete.
func (x *ExecutionsServer) DeleteExecution(ctx context.Context, req *runpb.DeleteExecutionRequest) (*longrunningpb.Operation, error) {
	s := x.s
	j, err := s.loadExecution(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(req.GetEtag(), j.Metadata.ResourceVersion, "execution"); err != nil {
		return nil, err
	}
	exec := s.execution(ctx, j)
	parent := strings.SplitN(req.GetName(), "/jobs/", 2)[0]
	if req.GetValidateOnly() {
		return s.doneOperation(parent, req.GetName(), exec)
	}
	if err := s.kn.deleteObject(ctx, resBatchJob, j.Metadata.Name); err != nil {
		return nil, err
	}
	return s.doneOperation(parent, req.GetName(), exec)
}

// CancelExecution stops a running execution by suspending its batch Job,
// which deletes the running pods; tasks not yet finished count as
// cancelled. The operation completes when no task is running any more.
func (x *ExecutionsServer) CancelExecution(ctx context.Context, req *runpb.CancelExecutionRequest) (*longrunningpb.Operation, error) {
	s := x.s
	j, err := s.loadExecution(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(req.GetEtag(), j.Metadata.ResourceVersion, "execution"); err != nil {
		return nil, err
	}
	if st := executionStateOf(j); st.terminal {
		return nil, apierror.FailedPrecondition("execution %s has already completed and cannot be cancelled", req.GetName())
	}
	parent := strings.SplitN(req.GetName(), "/jobs/", 2)[0]
	if req.GetValidateOnly() {
		return s.doneOperation(parent, req.GetName(), s.execution(ctx, j))
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:"true"}},"spec":{"suspend":true}}`, annCancelled)
	if err := s.kn.patchObject(ctx, resBatchJob, j.Metadata.Name, patch); err != nil {
		return nil, err
	}
	op := s.ops.Create(parent, req.GetName())
	go s.awaitCancel(context.WithoutCancel(ctx), op.Name, j.Metadata.Name, s.readyTimeout)
	return s.toProtoOperation(op.Name)
}

// awaitCancel completes a cancel's operation once the execution's pods are
// gone.
func (s *Server) awaitCancel(ctx context.Context, opName, execID string, bound time.Duration) {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		var j kjob
		if err := s.kn.getObject(ctx, &j, resBatchJob, execID, "execution "+execID); err == nil {
			exec := s.execution(ctx, j)
			_ = s.ops.SetMetadata(opName, exec)
			if executionStateOf(j).terminal {
				_ = s.ops.Succeed(opName, exec)
				return
			}
		} else if apierror.From(err).Code == codes.NotFound {
			_ = s.ops.Fail(opName, apierror.NotFound("execution %s was deleted while it was being cancelled", execID))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	_ = s.ops.Fail(opName, apierror.FailedPrecondition("execution %s did not stop within %s", execID, bound))
}
