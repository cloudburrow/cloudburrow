//go:build compat

package compat

import (
	"strings"
	"testing"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.run.v2.Tasks/ListTasks, google.cloud.run.v2.Tasks/GetTask
//
// TestRunTasksAreAnExecutionsPods (#1223): through the official TasksClient,
// an execution of two tasks lists one task per index. Task 0 succeeded on
// its first attempt with exit code 0; task 1 exited 4 on both of its
// attempts (max_retries 1), so it reports one retry, a failed Completed
// condition and the container's exit code and message. GetTask reads each
// by name, and a task beyond the count is NOT_FOUND.
func TestRunTasksAreAnExecutionsPods(t *testing.T) {
	h := New(t)
	jc, _ := runJobClients(t, h)
	ctx := h.Context()
	tc, err := run.NewTasksClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewTasksClient: %v", err)
	}
	t.Cleanup(func() { _ = tc.Close() })

	id := "compat-tasks"
	name := runParent(h) + "/jobs/" + id
	job := shellJob(`if [ "$CLOUD_RUN_TASK_INDEX" = 1 ]; then echo "task one breaks"; exit 4; fi; echo fine`, 2, nil)
	job.Template.Template.Retries = &runpb.TaskTemplate_MaxRetries{MaxRetries: 1}
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for CreateJob: %v", err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	md, err := rop.Metadata()
	if err != nil {
		t.Fatalf("RunJob metadata: %v", err)
	}
	if _, err := rop.Wait(ctx); err == nil {
		t.Fatal("RunJob succeeded; task 1 exits 4, so the execution should fail")
	}
	exec := md.GetName()

	var tasks []*runpb.Task
	for it := tc.ListTasks(ctx, &runpb.ListTasksRequest{Parent: exec}); ; {
		task, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		tasks = append(tasks, task)
	}
	if len(tasks) != 2 {
		t.Fatalf("ListTasks returned %d tasks, want 2", len(tasks))
	}
	ok, bad := tasks[0], tasks[1]
	completed := func(task *runpb.Task) *runpb.Condition {
		for _, c := range task.GetConditions() {
			if c.GetType() == "Completed" {
				return c
			}
		}
		return nil
	}
	if ok.GetIndex() != 0 || ok.GetExecution() != exec || ok.GetJob() != name || ok.GetRetried() != 0 ||
		completed(ok).GetState() != runpb.Condition_CONDITION_SUCCEEDED || ok.GetLastAttemptResult().GetExitCode() != 0 ||
		ok.GetStartTime() == nil || ok.GetCompletionTime() == nil || ok.GetMaxRetries() != 1 || ok.GetReconciling() {
		t.Errorf("task 0 = %v", ok)
	}
	if bad.GetIndex() != 1 || bad.GetRetried() != 1 || completed(bad).GetState() != runpb.Condition_CONDITION_FAILED ||
		bad.GetLastAttemptResult().GetExitCode() != 4 ||
		!strings.Contains(bad.GetLastAttemptResult().GetStatus().GetMessage(), "exited with code 4") {
		t.Errorf("task 1 = %v", bad)
	}
	if !ok.GetCompletionTime().AsTime().After(ok.GetStartTime().AsTime().Add(-1)) {
		t.Errorf("task 0 finished %v before it started %v", ok.GetCompletionTime().AsTime(), ok.GetStartTime().AsTime())
	}

	got, err := tc.GetTask(ctx, &runpb.GetTaskRequest{Name: bad.GetName()})
	if err != nil || got.GetUid() != bad.GetUid() || got.GetIndex() != 1 {
		t.Errorf("GetTask(%s) = %v, %v", bad.GetName(), got, err)
	}
	missing := strings.TrimSuffix(bad.GetName(), "1") + "2"
	if _, err := tc.GetTask(ctx, &runpb.GetTaskRequest{Name: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTask(%s) = %v; want NOT_FOUND", missing, err)
	}
}
