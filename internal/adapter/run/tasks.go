package run

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/uuid"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Tasks (#1223): google.cloud.run.v2.Tasks over an execution's pods.
//
// An execution is a batch/v1 Job in Indexed completion mode, so its task
// with index i is the pods the batch controller created for completion
// index i: one pod per attempt, the newest being the current attempt. A
// task's ID is "{execution}-task{index}". Everything a task reports is read
// from those pods and the Job; nothing is stored.
//
// log_uri is Google's Logs Explorer link for the task's entries
// (executionLogURI); the console's task page shows the pod's log.

// taskID is the ID of an execution's task with an index.
func taskID(execID string, index int) string { return fmt.Sprintf("%s-task%d", execID, index) }

// parseTaskName splits .../executions/{exec}/tasks/{task} into the
// execution's name and the task's index.
func parseTaskName(name string) (execName string, index int, err error) {
	i := strings.LastIndex(name, "/tasks/")
	if i < 0 {
		return "", 0, apierror.InvalidArgument("%q is not a Cloud Run task name", name)
	}
	execName, id := name[:i], name[i+len("/tasks/"):]
	execID := execName[strings.LastIndex(execName, "/")+1:]
	n, convErr := strconv.Atoi(strings.TrimPrefix(id, execID+"-task"))
	if !strings.HasPrefix(id, execID+"-task") || convErr != nil || n < 0 || strconv.Itoa(n) != strings.TrimPrefix(id, execID+"-task") {
		return "", 0, apierror.NotFound("task %s not found", name)
	}
	return execName, n, nil
}

// taskAttempts groups an execution's pods by completion index, oldest
// attempt first.
func taskAttempts(pods []kpod) map[int][]kpod {
	out := map[int][]kpod{}
	for _, p := range pods {
		i, err := strconv.Atoi(p.Metadata.Annotations[annCompletionIndex])
		if err != nil {
			continue
		}
		out[i] = append(out[i], p)
	}
	for i := range out {
		sort.SliceStable(out[i], func(a, b int) bool {
			return out[i][a].Metadata.CreationTimestamp.Before(out[i][b].Metadata.CreationTimestamp)
		})
	}
	return out
}

// indexFailed reports whether a batch index list such as "1,3-5" holds i.
func indexFailed(list *string, i int) bool {
	if list == nil {
		return false
	}
	for _, part := range strings.Split(*list, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, errA := strconv.Atoi(lo)
		if errA != nil {
			continue
		}
		b := a
		if isRange {
			var errB error
			if b, errB = strconv.Atoi(hi); errB != nil {
				continue
			}
		}
		if i >= a && i <= b {
			return true
		}
	}
	return false
}

// podTermination is the first container of a pod that has terminated
// unsuccessfully, or else its first terminated container.
func podTermination(p kpod) *kterminated {
	var first *kterminated
	for _, c := range p.Status.ContainerStatuses {
		t := c.State.Terminated
		if t == nil {
			continue
		}
		if t.ExitCode != 0 {
			return t
		}
		if first == nil {
			first = t
		}
	}
	return first
}

// podStarted is when a pod's containers started: the pod's start time, or
// the earliest container start.
func podStarted(p kpod) *time.Time {
	var at *time.Time
	for _, c := range p.Status.ContainerStatuses {
		var s *time.Time
		if c.State.Running != nil {
			s = &c.State.Running.StartedAt
		} else if c.State.Terminated != nil {
			s = &c.State.Terminated.StartedAt
		}
		if s != nil && !s.IsZero() && (at == nil || s.Before(*at)) {
			at = s
		}
	}
	if at == nil && p.Status.StartTime != nil {
		at = p.Status.StartTime
	}
	return at
}

// fromPods renders the task with an index of an execution's batch Job.
func fromPods(j kjob, exec *runpb.Execution, index int, attempts []kpod) *runpb.Task {
	name := exec.GetName() + "/tasks/" + taskID(j.Metadata.Name, index)
	t := &runpb.Task{
		Name:               name,
		Uid:                uuid.NewSHA1(uuid.NameSpaceURL, []byte(j.Metadata.UID+"/"+strconv.Itoa(index))).String(),
		Generation:         1,
		ObservedGeneration: 1,
		Labels:             exec.GetLabels(),
		Annotations:        exec.GetAnnotations(),
		CreateTime:         exec.GetCreateTime(),
		Job:                exec.GetJob(),
		Execution:          exec.GetName(),
		Index:              int32(index),
		Etag:               j.Metadata.ResourceVersion,
		LogUri:             executionLogURI(exec.GetName(), index),
	}
	if tt := exec.GetTemplate(); tt != nil {
		t.Containers = tt.GetContainers()
		t.MaxRetries = tt.GetMaxRetries()
		t.Timeout = tt.GetTimeout()
	}
	updated := exec.GetCreateTime().AsTime()
	started := &runpb.Condition{Type: "Started", State: runpb.Condition_CONDITION_PENDING}
	completed := &runpb.Condition{Type: "Completed", State: runpb.Condition_CONDITION_PENDING}
	terminal := false

	if n := len(attempts); n > 0 {
		latest := attempts[n-1]
		t.Retried = int32(n - 1)
		t.ScheduledTime = timestamppb.New(latest.Metadata.CreationTimestamp)
		if latest.Metadata.CreationTimestamp.After(updated) {
			updated = latest.Metadata.CreationTimestamp
		}
		if s := podStarted(latest); s != nil {
			t.StartTime = timestamppb.New(*s)
			started.State = runpb.Condition_CONDITION_SUCCEEDED
			started.LastTransitionTime = t.StartTime
			if s.After(updated) {
				updated = *s
			}
		}
		// The result of the newest attempt that finished.
		for k := n - 1; k >= 0; k-- {
			p := attempts[k]
			if p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed" {
				continue
			}
			res := &runpb.TaskAttemptResult{Status: &rpcstatus.Status{}}
			if term := podTermination(p); term != nil {
				res.ExitCode, res.TermSignal = term.ExitCode, term.Signal
			}
			if p.Status.Phase == "Failed" {
				msg := failureMessage([]kpod{p})
				code := codes.Unknown
				if p.Status.Reason == "DeadlineExceeded" {
					code = codes.DeadlineExceeded
				}
				res.Status = &rpcstatus.Status{Code: int32(code), Message: msg}
			}
			t.LastAttemptResult = res
			break
		}
		finished := func() *timestamppb.Timestamp {
			if term := podTermination(latest); term != nil && !term.FinishedAt.IsZero() {
				return timestamppb.New(term.FinishedAt)
			}
			return timestamppb.New(updated)
		}
		switch latest.Status.Phase {
		case "Succeeded":
			terminal = true
			t.CompletionTime = finished()
			completed.State = runpb.Condition_CONDITION_SUCCEEDED
			completed.Message = "Task completed successfully."
		case "Failed":
			// A failed attempt is final when the batch controller says the
			// index failed, or no retry is left; otherwise a retry follows.
			if indexFailed(j.Status.FailedIndexes, index) || int32(n) > t.MaxRetries {
				terminal = true
				t.CompletionTime = finished()
				completed.State = runpb.Condition_CONDITION_FAILED
				completed.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_NON_ZERO_EXIT_CODE}
				completed.Message = t.GetLastAttemptResult().GetStatus().GetMessage()
			}
		}
	}
	if !terminal {
		st := executionStateOf(j)
		switch {
		case st.cancelled || (st.terminal && !st.succeeded):
			// The execution ended before this task did: cancelled, or
			// stopped when another task used its last retry.
			terminal = true
			t.CompletionTime = timestamppb.New(st.finished)
			completed.State = runpb.Condition_CONDITION_FAILED
			completed.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_CANCELLED}
			completed.Message = "The task was cancelled."
		case st.cancelling:
			completed.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_CANCELLING}
		}
	}
	if terminal {
		completed.LastTransitionTime = t.CompletionTime
		if t.CompletionTime.AsTime().After(updated) {
			updated = t.CompletionTime.AsTime()
		}
	}
	t.UpdateTime = timestamppb.New(updated)
	t.Reconciling = !terminal
	t.Conditions = []*runpb.Condition{completed, started}
	return t
}

// executionTasks renders every task of an execution, in index order.
func (s *Server) executionTasks(ctx context.Context, j kjob) ([]*runpb.Task, error) {
	pods, err := listObjects[kpod](ctx, s.kn, resPod, labelJobName+"="+j.Metadata.Name)
	if err != nil {
		return nil, err
	}
	exec := fromBatchJob(j, "")
	byIndex := taskAttempts(pods)
	out := make([]*runpb.Task, 0, exec.GetTaskCount())
	for i := 0; i < int(exec.GetTaskCount()); i++ {
		out = append(out, fromPods(j, exec, i, byIndex[i]))
	}
	return out, nil
}

// TasksServer serves google.cloud.run.v2.Tasks.
type TasksServer struct {
	runpb.UnimplementedTasksServer
	s *Server
}

// Tasks returns the Tasks service over the same adapter.
func (s *Server) Tasks() *TasksServer { return &TasksServer{s: s} }

// Register adds the Tasks service to a gRPC server or a JSON transcoder.
func (x *TasksServer) Register(g grpc.ServiceRegistrar) { runpb.RegisterTasksServer(g, x) }

// GetTask reads one task of an execution from its pods.
func (x *TasksServer) GetTask(ctx context.Context, req *runpb.GetTaskRequest) (*runpb.Task, error) {
	execName, index, err := parseTaskName(req.GetName())
	if err != nil {
		return nil, err
	}
	j, err := x.s.loadExecution(ctx, execName)
	if err != nil {
		return nil, err
	}
	tasks, err := x.s.executionTasks(ctx, j)
	if err != nil {
		return nil, err
	}
	if index >= len(tasks) {
		return nil, apierror.NotFound("task %s not found", req.GetName())
	}
	return tasks[index], nil
}

// ListTasks lists an execution's tasks in index order. An execution that
// does not exist is NOT_FOUND, not an empty page.
func (x *TasksServer) ListTasks(ctx context.Context, req *runpb.ListTasksRequest) (*runpb.ListTasksResponse, error) {
	j, err := x.s.loadExecution(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}
	tasks, err := x.s.executionTasks(ctx, j)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(tasks))
	for i := range tasks {
		keys[i] = fmt.Sprintf("%08d", i)
	}
	page, next, err := paging.Page("run-tasks:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListTasksResponse{NextPageToken: next}
	for _, k := range page {
		i, _ := strconv.Atoi(k)
		resp.Tasks = append(resp.Tasks, tasks[i])
	}
	return resp, nil
}
