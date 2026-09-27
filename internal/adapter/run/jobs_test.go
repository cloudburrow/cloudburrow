package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// fakeKube is a kubectl that keeps ConfigMaps and batch Jobs in memory, so
// the Jobs and Executions servers run end to end without a cluster. A batch
// Job's status is what the test sets, as the Job controller would.
type fakeKube struct {
	mu    sync.Mutex
	rv    int
	cms   map[string]map[string]any
	jobs  map[string]*fakeBatchJob
	pods  map[string][]kpod // by batch Job name
	calls []string
}

type fakeBatchJob struct {
	manifest    string
	uid, rv     string
	created     time.Time
	labels      map[string]string
	annotations map[string]string
	completions int32
	parallelism int32
	suspend     bool
	status      map[string]any
}

func newFakeKube() *fakeKube {
	return &fakeKube{cms: map[string]map[string]any{}, jobs: map[string]*fakeBatchJob{}, pods: map[string][]kpod{}}
}

func (f *fakeKube) nextRV() string { f.rv++; return strconv.Itoa(f.rv) }

func matches(labels map[string]string, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		k, v, hasValue := strings.Cut(term, "=")
		have, ok := labels[k]
		if !ok || (hasValue && have != v) {
			return false
		}
	}
	return true
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	if m, ok := v.(map[string]any); ok {
		for k, x := range m {
			out[k], _ = x.(string)
		}
	}
	return out
}

func (f *fakeKube) jobJSON(name string, j *fakeBatchJob) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "uid": j.uid, "resourceVersion": j.rv, "generation": 1,
			"creationTimestamp": j.created.Format(time.RFC3339), "labels": j.labels, "annotations": j.annotations},
		"spec":   map[string]any{"completions": j.completions, "parallelism": j.parallelism, "suspend": j.suspend},
		"status": j.status,
	}
}

func (f *fakeKube) Run(_ context.Context, stdin, _ string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	args = args[4:] // --kubeconfig k -n ns
	f.calls = append(f.calls, strings.Join(args, " "))
	enc := func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }
	switch {
	case args[0] == "create" && strings.HasPrefix(stdin, "{"):
		var cm map[string]any
		if err := json.Unmarshal([]byte(stdin), &cm); err != nil {
			return "", err
		}
		md := cm["metadata"].(map[string]any)
		name := md["name"].(string)
		if _, ok := f.cms[name]; ok {
			return "", errors.New(`Error from server (AlreadyExists): configmaps "` + name + `" already exists`)
		}
		md["uid"], md["resourceVersion"], md["creationTimestamp"] = "uid-"+name, f.nextRV(), time.Now().UTC().Format(time.RFC3339)
		f.cms[name] = cm
		return "", nil
	case args[0] == "create":
		j := parseBatchJob(stdin)
		name := j.labels["__name"]
		delete(j.labels, "__name")
		if _, ok := f.jobs[name]; ok {
			return "", errors.New("AlreadyExists")
		}
		j.uid, j.rv, j.created = "uid-"+name, f.nextRV(), time.Now().UTC()
		j.status = map[string]any{}
		f.jobs[name] = j
		return "", nil
	case args[0] == "replace":
		var cm map[string]any
		if err := json.Unmarshal([]byte(stdin), &cm); err != nil {
			return "", err
		}
		md := cm["metadata"].(map[string]any)
		name := md["name"].(string)
		old, ok := f.cms[name]
		if !ok {
			return "", errors.New("NotFound")
		}
		oldMD := old["metadata"].(map[string]any)
		if md["resourceVersion"] != oldMD["resourceVersion"] {
			return "", errors.New("Operation cannot be fulfilled on configmaps: the object has been modified")
		}
		md["uid"], md["resourceVersion"], md["creationTimestamp"] = oldMD["uid"], f.nextRV(), oldMD["creationTimestamp"]
		f.cms[name] = cm
		return "", nil
	case args[0] == "get" && args[2] == "-l":
		var items []any
		switch args[1] {
		case resConfigMap:
			for _, cm := range f.cms {
				if matches(stringMap(cm["metadata"].(map[string]any)["labels"]), args[3]) {
					items = append(items, cm)
				}
			}
		case resBatchJob:
			for name, j := range f.jobs {
				if matches(j.labels, args[3]) {
					items = append(items, f.jobJSON(name, j))
				}
			}
		case resPod:
			_, job, _ := strings.Cut(args[3], "=")
			for _, p := range f.pods[job] {
				items = append(items, p)
			}
		}
		return enc(map[string]any{"items": items})
	case args[0] == "get":
		switch args[1] {
		case resConfigMap:
			if cm, ok := f.cms[args[2]]; ok {
				return enc(cm)
			}
		case resBatchJob:
			if j, ok := f.jobs[args[2]]; ok {
				return enc(f.jobJSON(args[2], j))
			}
		}
		return "", errors.New(`Error from server (NotFound): "` + args[2] + `" not found`)
	case args[0] == "delete" && args[2] == "-l":
		for name, j := range f.jobs {
			if matches(j.labels, args[3]) {
				delete(f.jobs, name)
			}
		}
		return "", nil
	case args[0] == "delete":
		delete(f.cms, args[2])
		delete(f.jobs, args[2])
		return "", nil
	case args[0] == "patch":
		j, ok := f.jobs[args[2]]
		if !ok {
			return "", errors.New("NotFound")
		}
		var p struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Suspend bool `json:"suspend"`
			} `json:"spec"`
		}
		if err := json.Unmarshal([]byte(args[len(args)-1]), &p); err != nil {
			return "", err
		}
		for k, v := range p.Metadata.Annotations {
			j.annotations[k] = v
		}
		j.suspend = p.Spec.Suspend
		j.rv = f.nextRV()
		return "", nil
	}
	return "", fmt.Errorf("fakeKube: unhandled %v", args)
}

// parseBatchJob reads back what renderExecution wrote: the name, labels,
// annotations, completions and parallelism.
func parseBatchJob(manifest string) *fakeBatchJob {
	j := &fakeBatchJob{manifest: manifest, labels: map[string]string{}, annotations: map[string]string{}}
	var section string
	for _, line := range strings.Split(manifest, "\n") {
		switch {
		case strings.HasPrefix(line, "  name: ") && section == "":
			j.labels["__name"] = strings.TrimPrefix(line, "  name: ")
		case line == "  labels:" || line == "  annotations:":
			section = strings.TrimSpace(strings.TrimSuffix(line, ":"))
		case strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") && (section == "labels" || section == "annotations"):
			k, v, _ := strings.Cut(strings.TrimSpace(line), ": ")
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			}
			if section == "labels" {
				j.labels[k] = v
			} else {
				j.annotations[k] = v
			}
		case line == "spec:":
			section = "spec"
		case strings.HasPrefix(line, "  completions: "):
			n, _ := strconv.Atoi(strings.TrimPrefix(line, "  completions: "))
			j.completions = int32(n)
		case strings.HasPrefix(line, "  parallelism: "):
			n, _ := strconv.Atoi(strings.TrimPrefix(line, "  parallelism: "))
			j.parallelism = int32(n)
		}
	}
	return j
}

// setStatus replaces a batch Job's status, as the Job controller would.
func (f *fakeKube) setStatus(name string, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[name].status = status
	f.jobs[name].rv = f.nextRV()
}

func (f *fakeKube) only(t *testing.T) (string, *fakeBatchJob) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) != 1 {
		t.Fatalf("%d batch Jobs exist, want 1", len(f.jobs))
	}
	for n, j := range f.jobs {
		return n, j
	}
	return "", nil
}

func (f *fakeKube) ran(verb string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, verb) {
			n++
		}
	}
	return n
}

const jobParent = "projects/demo-project/locations/us-central1"

func jobsServer(secrets SecretResolver) (*JobsServer, *ExecutionsServer, *OperationsServer, *fakeKube) {
	f := newFakeKube()
	s := NewServer(&Knative{Kubeconfig: "k", Namespace: "default", Runner: f}, "inst", 5*time.Second)
	if secrets != nil {
		s = s.WithSecrets(secrets)
	}
	return s.Jobs(), s.Executions(), NewOperationsServer(s), f
}

func sampleJob() *runpb.Job {
	return &runpb.Job{
		Labels: map[string]string{"team": "data"},
		Template: &runpb.ExecutionTemplate{
			TaskCount: 3, Parallelism: 2, Labels: map[string]string{"run": "nightly"},
			Template: &runpb.TaskTemplate{
				Retries: &runpb.TaskTemplate_MaxRetries{MaxRetries: 1},
				Timeout: durationpb.New(30 * time.Second),
				Containers: []*runpb.Container{{
					Name: "migrate", Image: "docker.io/library/busybox:1.36", Command: []string{"sh", "-c"}, Args: []string{"echo hi"},
					Env: []*runpb.EnvVar{
						{Name: "MODE", Values: &runpb.EnvVar_Value{Value: "full"}},
						{Name: "TOKEN", Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
							SecretKeyRef: &runpb.SecretKeySelector{Secret: "api-token", Version: "2"}}}},
					},
					Resources: &runpb.ResourceRequirements{Limits: map[string]string{"memory": "256Mi", "cpu": "1"}},
				}},
			},
		},
	}
}

// waitDone polls an operation until it is done.
func waitDone(t *testing.T, ops *OperationsServer, name string) *longrunningpb.Operation {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		op, err := ops.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if op.GetDone() {
			return op
		}
	}
	t.Fatalf("operation %s never completed", name)
	return nil
}

func unpackExecution(t *testing.T, op *longrunningpb.Operation) *runpb.Execution {
	t.Helper()
	var e runpb.Execution
	if err := op.GetResponse().UnmarshalTo(&e); err != nil {
		t.Fatalf("operation response is not an Execution: %v (%v)", err, op)
	}
	return &e
}

// A job is stored, read back with Cloud Run's defaults, run as one batch Job
// with the services' container mapping, reported from the Job's status, and
// deleted with its executions.
func TestJobRunsAsABatchJob(t *testing.T) {
	t.Parallel()
	js, xs, ops, f := jobsServer(fakeSecrets{})
	ctx := context.Background()
	name := jobParent + "/jobs/nightly"

	op, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "nightly", Job: sampleJob()})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if !op.GetDone() {
		t.Error("CreateJob's operation is not done; a job is configuration and is ready when written")
	}
	var created runpb.Job
	if err := op.GetResponse().UnmarshalTo(&created); err != nil {
		t.Fatal(err)
	}
	if created.GetName() != name || created.GetUid() == "" || created.GetEtag() == "" || created.GetGeneration() != 1 ||
		created.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("created job = %v", &created)
	}
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "nightly", Job: sampleJob()}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateJob = %v, want AlreadyExists", err)
	}

	// Defaults fill what was not set, as Google returns them.
	bare := sampleJob()
	bare.Template.TaskCount, bare.Template.Template.Retries, bare.Template.Template.Timeout = 0, nil, nil
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "bare", Job: bare}); err != nil {
		t.Fatal(err)
	}
	got, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: jobParent + "/jobs/bare"})
	if err != nil {
		t.Fatal(err)
	}
	if tt := got.GetTemplate(); tt.GetTaskCount() != 1 || tt.GetTemplate().GetMaxRetries() != 3 || tt.GetTemplate().GetTimeout().AsDuration() != 10*time.Minute {
		t.Errorf("defaults = taskCount %d, maxRetries %d, timeout %v; want 1, 3, 10m", tt.GetTaskCount(),
			tt.GetTemplate().GetMaxRetries(), tt.GetTemplate().GetTimeout().AsDuration())
	}

	list, err := js.ListJobs(ctx, &runpb.ListJobsRequest{Parent: jobParent})
	if err != nil || len(list.GetJobs()) != 2 {
		t.Fatalf("ListJobs = %v, %v; want both jobs", list, err)
	}
	other, err := js.ListJobs(ctx, &runpb.ListJobsRequest{Parent: "projects/other-project/locations/us-central1"})
	if err != nil || len(other.GetJobs()) != 0 {
		t.Errorf("ListJobs in another project = %v, %v; want none", other, err)
	}
	if _, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: "projects/other-project/locations/us-central1/jobs/nightly"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob under another project = %v, want NotFound", err)
	}

	// Update: a new generation, applied to the next execution.
	upd := sampleJob()
	upd.Name = name
	upd.Template.Template.Containers[0].Env[0] = &runpb.EnvVar{Name: "MODE", Values: &runpb.EnvVar_Value{Value: "incremental"}}
	uop, err := js.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: upd})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	var updated runpb.Job
	_ = uop.GetResponse().UnmarshalTo(&updated)
	if updated.GetGeneration() != 2 || updated.GetEtag() == created.GetEtag() {
		t.Errorf("updated generation %d etag %q; want 2 and a new etag", updated.GetGeneration(), updated.GetEtag())
	}

	rop, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if rop.GetDone() {
		t.Error("RunJob's operation is done before the execution ran")
	}
	var md runpb.Execution
	if err := rop.GetMetadata().UnmarshalTo(&md); err != nil {
		t.Fatalf("RunJob's operation carries no Execution metadata: %v", err)
	}
	execID, j := f.only(t)
	if md.GetName() != name+"/executions/"+execID || !strings.HasPrefix(execID, "nightly-") || md.GetJob() != name {
		t.Errorf("execution %q of %q, batch Job %q", md.GetName(), md.GetJob(), execID)
	}
	for _, want := range []string{
		"completions: 3", "parallelism: 2", "completionMode: Indexed", "backoffLimitPerIndex: 1",
		"restartPolicy: Never", "activeDeadlineSeconds: 30",
		"- image: docker.io/library/busybox:1.36", "name: migrate", "terminationMessagePolicy: FallbackToLogsOnError",
		"imagePullPolicy: IfNotPresent", `command: ["sh", "-c"]`, `args: ["echo hi"]`,
		"- name: MODE\n              value: \"incremental\"",
		"secretKeyRef:\n                  name: k8s-secret\n                  key: 2",
		`cpu: "1"`, `memory: "256Mi"`,
		"- name: CLOUD_RUN_TASK_INDEX\n              valueFrom:\n                fieldRef:\n                  fieldPath: \"metadata.annotations['batch.kubernetes.io/job-completion-index']\"",
		"- name: CLOUD_RUN_TASK_COUNT\n              value: \"3\"",
		"cloudburrow.dev/owned: \"true\"", "cloudburrow.dev/run-job: nightly",
	} {
		if !strings.Contains(j.manifest, want) {
			t.Errorf("the batch Job lacks %q:\n%s", want, j.manifest)
		}
	}

	start := time.Now().UTC().Format(time.RFC3339)
	f.setStatus(execID, map[string]any{"startTime": start, "active": 2, "succeeded": 1})
	running, err := xs.GetExecution(ctx, &runpb.GetExecutionRequest{Name: md.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if running.GetRunningCount() != 2 || running.GetSucceededCount() != 1 || !running.GetReconciling() || running.GetStartTime() == nil {
		t.Errorf("running execution = %v", running)
	}
	f.setStatus(execID, map[string]any{"startTime": start, "completionTime": start, "succeeded": 3,
		"conditions": []any{map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": start}}})
	done := waitDone(t, ops, rop.GetName())
	exec := unpackExecution(t, done)
	if exec.GetSucceededCount() != 3 || exec.GetTaskCount() != 3 || exec.GetParallelism() != 2 || exec.GetReconciling() ||
		exec.GetCompletionTime() == nil || exec.GetConditions()[0].GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("completed execution = %v", exec)
	}
	if env := exec.GetTemplate().GetContainers()[0].GetEnv(); len(env) != 2 {
		t.Errorf("the execution's template reads back with %d env vars, want the 2 set (not the injected ones): %v", len(env), env)
	}

	after, _ := js.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if after.GetExecutionCount() != 1 || after.GetLatestCreatedExecution().GetName() != exec.GetName() ||
		after.GetLatestCreatedExecution().GetCompletionStatus() != runpb.ExecutionReference_EXECUTION_SUCCEEDED {
		t.Errorf("job after a run: count %d, latest %v", after.GetExecutionCount(), after.GetLatestCreatedExecution())
	}
	le, err := xs.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: name})
	if err != nil || len(le.GetExecutions()) != 1 || le.GetExecutions()[0].GetName() != exec.GetName() {
		t.Errorf("ListExecutions = %v, %v", le, err)
	}
	if _, err := xs.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: jobParent + "/jobs/absent"}); status.Code(err) != codes.NotFound {
		t.Errorf("ListExecutions of a missing job = %v, want NotFound", err)
	}

	dop, err := xs.DeleteExecution(ctx, &runpb.DeleteExecutionRequest{Name: exec.GetName()})
	if err != nil || !dop.GetDone() {
		t.Fatalf("DeleteExecution = %v, %v", dop, err)
	}
	if _, err := xs.GetExecution(ctx, &runpb.GetExecutionRequest{Name: exec.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetExecution after delete = %v, want NotFound", err)
	}

	// DeleteJob takes the job's executions with it.
	if _, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: name}); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after delete = %v, want NotFound", err)
	}
	f.mu.Lock()
	left := len(f.jobs)
	f.mu.Unlock()
	if left != 0 {
		t.Errorf("%d batch Jobs survive DeleteJob", left)
	}
}

// A task whose container exits non-zero fails the execution with a failed
// Completed condition carrying the pod's own message, and fails RunJob's
// operation with it.
func TestFailedExecutionReportsThePodsMessage(t *testing.T) {
	t.Parallel()
	js, xs, ops, f := jobsServer(fakeSecrets{})
	ctx := context.Background()
	name := jobParent + "/jobs/fails"
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "fails", Job: sampleJob()}); err != nil {
		t.Fatal(err)
	}
	rop, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	execID, _ := f.only(t)
	var pod kpod
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"p","creationTimestamp":"2026-09-27T10:00:00Z",
	  "annotations":{"batch.kubernetes.io/job-completion-index":"0"}},
	 "status":{"phase":"Failed","containerStatuses":[{"name":"migrate",
	  "state":{"terminated":{"exitCode":3,"reason":"Error","message":"boom: table missing\n"}}}]}}`), &pod); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.pods[execID] = []kpod{pod}
	f.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	f.setStatus(execID, map[string]any{"startTime": now, "succeeded": 2, "failed": 2, "failedIndexes": "0",
		"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "FailedIndexes",
			"message": "Job has failed indexes", "lastTransitionTime": now}}})

	done := waitDone(t, ops, rop.GetName())
	if done.GetError() == nil || codes.Code(done.GetError().GetCode()) != codes.FailedPrecondition ||
		!strings.Contains(done.GetError().GetMessage(), "exited with code 3: boom: table missing") {
		t.Errorf("RunJob's operation = %v; want FAILED_PRECONDITION with the pod's message", done.GetResult())
	}
	var md runpb.Execution
	if err := done.GetMetadata().UnmarshalTo(&md); err != nil || md.GetName() == "" {
		t.Errorf("a failed RunJob's operation names no execution: %v", err)
	}
	exec, err := xs.GetExecution(ctx, &runpb.GetExecutionRequest{Name: md.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	c := exec.GetConditions()[0]
	if c.GetType() != "Completed" || c.GetState() != runpb.Condition_CONDITION_FAILED ||
		c.GetExecutionReason() != runpb.Condition_NON_ZERO_EXIT_CODE || !strings.Contains(c.GetMessage(), "Task 0 failed: container migrate exited with code 3") {
		t.Errorf("Completed condition = %v", c)
	}
	if exec.GetFailedCount() != 1 || exec.GetRetriedCount() != 1 || exec.GetSucceededCount() != 2 {
		t.Errorf("counts: failed %d retried %d succeeded %d; want 1, 1, 2", exec.GetFailedCount(), exec.GetRetriedCount(), exec.GetSucceededCount())
	}
}

// CancelExecution suspends the batch Job; the execution then reports its
// unfinished tasks cancelled. A completed one cannot be cancelled.
func TestCancelExecutionSuspendsTheBatchJob(t *testing.T) {
	t.Parallel()
	js, xs, ops, f := jobsServer(fakeSecrets{})
	ctx := context.Background()
	name := jobParent + "/jobs/long"
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "long", Job: sampleJob()}); err != nil {
		t.Fatal(err)
	}
	rop, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	execID, j := f.only(t)
	execName := name + "/executions/" + execID
	f.setStatus(execID, map[string]any{"active": 2, "succeeded": 1})
	cop, err := xs.CancelExecution(ctx, &runpb.CancelExecutionRequest{Name: execName})
	if err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	f.mu.Lock()
	suspended, marked := j.suspend, j.annotations[annCancelled]
	f.mu.Unlock()
	if !suspended || marked != "true" {
		t.Fatalf("the batch Job was not suspended and marked: suspend %v, annotation %q", suspended, marked)
	}
	mid, _ := xs.GetExecution(ctx, &runpb.GetExecutionRequest{Name: execName})
	if mid.GetConditions()[0].GetExecutionReason() != runpb.Condition_CANCELLING {
		t.Errorf("while pods stop, reason = %v; want CANCELLING", mid.GetConditions()[0].GetExecutionReason())
	}
	f.setStatus(execID, map[string]any{"active": 0, "succeeded": 1})
	exec := unpackExecution(t, waitDone(t, ops, cop.GetName()))
	if exec.GetCancelledCount() != 2 || exec.GetConditions()[0].GetExecutionReason() != runpb.Condition_CANCELLED ||
		exec.GetConditions()[0].GetState() != runpb.Condition_CONDITION_FAILED {
		t.Errorf("cancelled execution = %v", exec)
	}
	if run := waitDone(t, ops, rop.GetName()); run.GetError() == nil || !strings.Contains(run.GetError().GetMessage(), "cancelled") {
		t.Errorf("RunJob's operation after a cancel = %v", run.GetResult())
	}
	if _, err := xs.CancelExecution(ctx, &runpb.CancelExecutionRequest{Name: execName}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("cancelling a finished execution = %v, want FailedPrecondition", err)
	}
}

// Fields a batch Job has no counterpart for are refused as the services
// path refuses them, in the same words, and nothing reaches the cluster.
func TestJobRefusesWhatItDoesNotMap(t *testing.T) {
	t.Parallel()
	js, _, _, f := jobsServer(fakeSecrets{})
	ctx := context.Background()
	cases := map[string]func(*runpb.Job){
		"template.template.vpcAccess: no VPC exists locally": func(j *runpb.Job) {
			j.Template.Template.VpcAccess = &runpb.VpcAccess{Connector: "c"}
		},
		"template.template.serviceAccount: CloudBurrow performs no authentication": func(j *runpb.Job) {
			j.Template.Template.ServiceAccount = "sa@p.iam.gserviceaccount.com"
		},
		"template.template.encryptionKey": func(j *runpb.Job) { j.Template.Template.EncryptionKey = "k" },
		"template.template.volumes":       func(j *runpb.Job) { j.Template.Template.Volumes = []*runpb.Volume{{Name: "v"}} },
		"template.template.executionEnvironment": func(j *runpb.Job) {
			j.Template.Template.ExecutionEnvironment = runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2
		},
		"template.template.nodeSelector": func(j *runpb.Job) { j.Template.Template.NodeSelector = &runpb.NodeSelector{Accelerator: "l4"} },
		"container.volumeMounts": func(j *runpb.Job) {
			j.Template.Template.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "v", MountPath: "/v"}}
		},
		"container.dependsOn": func(j *runpb.Job) { j.Template.Template.Containers[0].DependsOn = []string{"x"} },
		"container.ports": func(j *runpb.Job) {
			j.Template.Template.Containers[0].Ports = []*runpb.ContainerPort{{ContainerPort: 8080}}
		},
		"container.startupProbe": func(j *runpb.Job) { j.Template.Template.Containers[0].StartupProbe = &runpb.Probe{} },
		"binaryAuthorization":    func(j *runpb.Job) { j.BinaryAuthorization = &runpb.BinaryAuthorization{} },
		"startExecutionToken":    func(j *runpb.Job) { j.CreateExecution = &runpb.Job_StartExecutionToken{StartExecutionToken: "t"} },
	}
	for want, mutate := range cases {
		job := sampleJob()
		mutate(job)
		_, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "refused", Job: job})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want Unimplemented naming it", want, err)
		}
	}
	// The services path refuses the same field in the same words.
	svc := &runpb.Service{Name: jobParent + "/services/s", Template: &runpb.RevisionTemplate{
		VpcAccess: &runpb.VpcAccess{Connector: "c"}, Containers: []*runpb.Container{{Image: "a:1"}}}}
	if err := Unsupported(svc); err == nil || !strings.Contains(err.Error(), "template.vpcAccess: no VPC exists locally") {
		t.Errorf("services vpcAccess refusal = %v", err)
	}

	invalid := map[string]func(*runpb.Job){
		"untagged image": func(j *runpb.Job) { j.Template.Template.Containers[0].Image = "busybox" },
		"no containers":  func(j *runpb.Job) { j.Template.Template.Containers = nil },
		"reserved label": func(j *runpb.Job) { j.Labels["run.googleapis.com/x"] = "1" },
		"negative tasks": func(j *runpb.Job) { j.Template.TaskCount = -1 },
	}
	for what, mutate := range invalid {
		job := sampleJob()
		mutate(job)
		if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "refused", Job: job}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v; want InvalidArgument", what, err)
		}
	}
	for _, id := range []string{"", "Upper", "ends-", strings.Repeat("a", 64)} {
		if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: id, Job: sampleJob()}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("job ID %q: %v; want InvalidArgument", id, err)
		}
	}
	// A secretKeyRef without Secret Manager is refused, not dropped.
	noSecrets, _, _, _ := jobsServer(nil)
	if _, err := noSecrets.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "s", Job: sampleJob()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("secretKeyRef without Secret Manager = %v; want FailedPrecondition", err)
	}
	// validate_only writes nothing.
	if op, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "dry", Job: sampleJob(), ValidateOnly: true}); err != nil || !op.GetDone() {
		t.Errorf("validate-only CreateJob = %v, %v", op, err)
	}
	if n := f.ran("create") + f.ran("replace"); n != 0 {
		t.Errorf("refused and validate-only creates wrote %d objects", n)
	}
}

// RunJob's overrides change that execution only: args, env, task count and
// timeout. A stale etag is ABORTED; an override naming no container is
// INVALID_ARGUMENT.
func TestRunJobOverridesAndEtags(t *testing.T) {
	t.Parallel()
	js, _, _, f := jobsServer(fakeSecrets{})
	ctx := context.Background()
	name := jobParent + "/jobs/ov"
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "ov", Job: sampleJob()}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name, Etag: `"stale"`}); status.Code(err) != codes.Aborted {
		t.Errorf("RunJob with a stale etag = %v, want Aborted", err)
	}
	if _, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name, Overrides: &runpb.RunJobRequest_Overrides{
		ContainerOverrides: []*runpb.RunJobRequest_Overrides_ContainerOverride{{Name: "nope"}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("override naming no container = %v, want InvalidArgument", err)
	}
	if _, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: name, Overrides: &runpb.RunJobRequest_Overrides{
		TaskCount: 5, Timeout: durationpb.New(7 * time.Second),
		ContainerOverrides: []*runpb.RunJobRequest_Overrides_ContainerOverride{{Name: "migrate", Args: []string{"echo override"},
			Env: []*runpb.EnvVar{{Name: "EXTRA", Values: &runpb.EnvVar_Value{Value: "1"}}}}}}}); err != nil {
		t.Fatalf("RunJob with overrides: %v", err)
	}
	_, j := f.only(t)
	for _, want := range []string{"completions: 5", "activeDeadlineSeconds: 7", `args: ["echo override"]`, "- name: EXTRA", "- name: MODE"} {
		if !strings.Contains(j.manifest, want) {
			t.Errorf("overridden execution lacks %q:\n%s", want, j.manifest)
		}
	}
	got, _ := js.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if got.GetTemplate().GetTaskCount() != 3 || got.GetTemplate().GetTemplate().GetContainers()[0].GetArgs()[0] != "echo hi" {
		t.Errorf("an override changed the job itself: %v", got.GetTemplate())
	}
	if _, err := js.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: name, Etag: `"stale"`}); status.Code(err) != codes.Aborted {
		t.Errorf("DeleteJob with a stale etag = %v, want Aborted", err)
	}
	upd := sampleJob()
	upd.Name, upd.Etag = name, `"stale"`
	if _, err := js.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: upd}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateJob with a stale etag = %v, want Aborted", err)
	}
	missing := sampleJob()
	missing.Name = jobParent + "/jobs/new"
	if _, err := js.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateJob of a missing job = %v, want NotFound", err)
	}
	if _, err := js.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: missing, AllowMissing: true}); err != nil {
		t.Errorf("UpdateJob with allow_missing = %v, want a create", err)
	}
}

func TestExecutionHelpers(t *testing.T) {
	t.Parallel()
	for list, want := range map[string]int32{"": 0, "3": 1, "0,2": 2, "1,3-5": 4, "0-9": 10} {
		if got := countIndexes(list); got != want {
			t.Errorf("countIndexes(%q) = %d, want %d", list, got, want)
		}
	}
	long := strings.Repeat("a", 56) + "-bcdef"
	if id := executionID(long); len(id) > 63 || strings.Contains(id, "--") {
		t.Errorf("executionID(%q) = %q; want at most 63 characters and no double hyphen", long, id)
	}
	// A task timeout: the kubelet fails the pod, and no container exited
	// non-zero on its own.
	var timedOut kpod
	if err := json.Unmarshal([]byte(`{"metadata":{"annotations":{"batch.kubernetes.io/job-completion-index":"2"}},
	 "status":{"phase":"Failed","reason":"DeadlineExceeded","message":"Pod was active on the node longer than the specified deadline"}}`), &timedOut); err != nil {
		t.Fatal(err)
	}
	if got := failureMessage([]kpod{timedOut}); got != "Task 2 failed: DeadlineExceeded Pod was active on the node longer than the specified deadline" {
		t.Errorf("a timed-out task's message = %q", got)
	}
	for p, want := range map[int32]int32{0: 4, 2: 2, 9: 4} {
		if got := parallelismOf(p, 4); got != want {
			t.Errorf("parallelismOf(%d, 4) = %d, want %d", p, got, want)
		}
	}
}
