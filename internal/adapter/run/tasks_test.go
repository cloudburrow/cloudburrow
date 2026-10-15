package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func podJSON(t *testing.T, raw string) kpod {
	t.Helper()
	var p kpod
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// An execution's tasks are its pods by completion index (#1223): a task
// that succeeded on its second attempt reports one retry and exit code 0;
// one that used its last retry failed with the container's exit code and
// message; one with no pod yet is pending.
func TestTasksAreReadFromTheExecutionsPods(t *testing.T) {
	t.Parallel()
	js, _, _, f := jobsServer(fakeSecrets{})
	ts := js.s.Tasks()
	ctx := context.Background()
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "tasks", Job: sampleJob()}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: jobParent + "/jobs/tasks"}); err != nil {
		t.Fatal(err)
	}
	execID, _ := f.only(t)
	pod := func(index, created, phase, term string) string {
		return `{"metadata":{"name":"p` + index + created + `","creationTimestamp":"2026-09-27T10:0` + created + `:00Z",
		  "annotations":{"batch.kubernetes.io/job-completion-index":"` + index + `"}},
		 "status":{"phase":"` + phase + `","startTime":"2026-09-27T10:0` + created + `:05Z","containerStatuses":[{"name":"migrate",
		  "state":{` + term + `}}]}}`
	}
	f.mu.Lock()
	f.pods[execID] = []kpod{
		podJSON(t, pod("0", "1", "Failed", `"terminated":{"exitCode":1,"startedAt":"2026-09-27T10:01:05Z","finishedAt":"2026-09-27T10:01:09Z"}`)),
		podJSON(t, pod("0", "2", "Succeeded", `"terminated":{"exitCode":0,"startedAt":"2026-09-27T10:02:05Z","finishedAt":"2026-09-27T10:02:09Z"}`)),
		podJSON(t, pod("1", "1", "Failed", `"terminated":{"exitCode":3,"message":"boom","startedAt":"2026-09-27T10:01:05Z","finishedAt":"2026-09-27T10:01:08Z"}`)),
		podJSON(t, pod("1", "3", "Failed", `"terminated":{"exitCode":3,"message":"boom","startedAt":"2026-09-27T10:03:05Z","finishedAt":"2026-09-27T10:03:08Z"}`)),
	}
	f.mu.Unlock()
	f.setStatus(execID, map[string]any{"startTime": time.Now().UTC().Format(time.RFC3339), "active": 1, "succeeded": 1,
		"failed": 3, "failedIndexes": "1"})

	execName := jobParent + "/jobs/tasks/executions/" + execID
	list, err := ts.ListTasks(ctx, &runpb.ListTasksRequest{Parent: execName})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetTasks()) != 3 {
		t.Fatalf("ListTasks returned %d tasks; want one per index, 3", len(list.GetTasks()))
	}
	t0, t1, t2 := list.GetTasks()[0], list.GetTasks()[1], list.GetTasks()[2]
	if t0.GetName() != execName+"/tasks/"+execID+"-task0" || t0.GetIndex() != 0 || t0.GetExecution() != execName ||
		t0.GetJob() != jobParent+"/jobs/tasks" {
		t.Errorf("task 0 is %s index %d execution %s job %s", t0.GetName(), t0.GetIndex(), t0.GetExecution(), t0.GetJob())
	}
	if t0.GetRetried() != 1 || t0.GetLastAttemptResult().GetExitCode() != 0 || t0.GetConditions()[0].GetState() != runpb.Condition_CONDITION_SUCCEEDED ||
		t0.GetCompletionTime().AsTime().Format(time.RFC3339) != "2026-09-27T10:02:09Z" || t0.GetStartTime().AsTime().Format(time.RFC3339) != "2026-09-27T10:02:05Z" ||
		t0.GetReconciling() || t0.GetMaxRetries() != 1 || len(t0.GetContainers()) != 1 {
		t.Errorf("task 0 = %v", t0)
	}
	if t1.GetRetried() != 1 || t1.GetLastAttemptResult().GetExitCode() != 3 || t1.GetConditions()[0].GetState() != runpb.Condition_CONDITION_FAILED ||
		!strings.Contains(t1.GetLastAttemptResult().GetStatus().GetMessage(), "exited with code 3: boom") ||
		codes.Code(t1.GetLastAttemptResult().GetStatus().GetCode()) != codes.Unknown {
		t.Errorf("task 1 = %v", t1)
	}
	if t2.GetConditions()[0].GetState() != runpb.Condition_CONDITION_PENDING || t2.GetStartTime() != nil || !t2.GetReconciling() {
		t.Errorf("task 2 = %v", t2)
	}

	got, err := ts.GetTask(ctx, &runpb.GetTaskRequest{Name: t1.GetName()})
	if err != nil || got.GetIndex() != 1 || got.GetUid() != t1.GetUid() || got.GetUid() == t0.GetUid() {
		t.Errorf("GetTask(%s) = %v, %v", t1.GetName(), got, err)
	}
	for _, name := range []string{execName + "/tasks/" + execID + "-task3", execName + "/tasks/other-task0",
		execName + "/tasks/" + execID + "-task01", jobParent + "/jobs/tasks/executions/missing/tasks/missing-task0"} {
		if _, err := ts.GetTask(ctx, &runpb.GetTaskRequest{Name: name}); status.Code(err) != codes.NotFound {
			t.Errorf("GetTask(%s) = %v; want NOT_FOUND", name, err)
		}
	}
	page, err := ts.ListTasks(ctx, &runpb.ListTasksRequest{Parent: execName, PageSize: 2})
	if err != nil || len(page.GetTasks()) != 2 || page.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", page, err)
	}
	rest, err := ts.ListTasks(ctx, &runpb.ListTasksRequest{Parent: execName, PageSize: 2, PageToken: page.GetNextPageToken()})
	if err != nil || len(rest.GetTasks()) != 1 || rest.GetTasks()[0].GetIndex() != 2 {
		t.Errorf("second page = %v, %v", rest, err)
	}
}
