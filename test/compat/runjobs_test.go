//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// jobImage is a registry image, so the batch Job may pull it; a bare
// "busybox:1.36" would be localised to dev.local and never pulled.
const jobImage = "docker.io/library/busybox:1.36"

// runJobClients returns the official Jobs and Executions clients pointed at
// the local adapter.
func runJobClients(t *testing.T, h *Harness) (*run.JobsClient, *run.ExecutionsClient) {
	t.Helper()
	jc, err := run.NewJobsClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewJobsClient: %v", err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	xc, err := run.NewExecutionsClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewExecutionsClient: %v", err)
	}
	t.Cleanup(func() { _ = xc.Close() })
	return jc, xc
}

// shellJob is a job of one busybox container running script.
func shellJob(script string, tasks int32, env map[string]string) *runpb.Job {
	c := &runpb.Container{Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{script}}
	for k, v := range env {
		c.Env = append(c.Env, &runpb.EnvVar{Name: k, Values: &runpb.EnvVar_Value{Value: v}})
	}
	return &runpb.Job{Template: &runpb.ExecutionTemplate{TaskCount: tasks, Template: &runpb.TaskTemplate{
		Retries:    &runpb.TaskTemplate_MaxRetries{MaxRetries: 0},
		Timeout:    durationpb.New(3 * time.Minute),
		Containers: []*runpb.Container{c},
	}}}
}

func completedCondition(e *runpb.Execution) *runpb.Condition {
	for _, c := range e.GetConditions() {
		if c.GetType() == "Completed" {
			return c
		}
	}
	return nil
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/GetJob, google.cloud.run.v2.Jobs/ListJobs, google.cloud.run.v2.Jobs/UpdateJob, google.cloud.run.v2.Jobs/RunJob, google.cloud.run.v2.Jobs/DeleteJob, google.cloud.run.v2.Executions/GetExecution, google.cloud.run.v2.Executions/ListExecutions, google.cloud.run.v2.Executions/DeleteExecution
//
// TestRunJobLifecycle (#582): through the official JobsClient and
// ExecutionsClient, a job is created, read, listed and updated; RunJob
// creates a batch/v1 Job with one completion per task, and its operation
// completes when both tasks have succeeded; the execution is read and
// listed; deleting it removes the batch Job; deleting the job removes the
// job.
func TestRunJobLifecycle(t *testing.T) {
	h := New(t)
	jc, xc := runJobClients(t, h)
	ctx := h.Context()
	id := "compat-job"
	name := runParent(h) + "/jobs/" + id
	script := `echo "task $CLOUD_RUN_TASK_INDEX of $CLOUD_RUN_TASK_COUNT says $GREETING"`

	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id,
		Job: shellJob(script, 2, map[string]string{"GREETING": "first"})})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	job, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for CreateJob: %v", err)
	}
	if job.GetName() != name || job.GetEtag() == "" || job.GetTemplate().GetTaskCount() != 2 {
		t.Errorf("created job = %v", job)
	}

	got, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.GetUid() == "" || got.GetTemplate().GetTemplate().GetContainers()[0].GetImage() != jobImage {
		t.Errorf("GetJob = %v", got)
	}
	found := false
	for it := jc.ListJobs(ctx, &runpb.ListJobsRequest{Parent: runParent(h)}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		found = found || j.GetName() == name
	}
	if !found {
		t.Error("ListJobs omitted the job just created")
	}

	update := shellJob(script, 2, map[string]string{"GREETING": "second"})
	update.Name = name
	uop, err := jc.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: update})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	updated, err := uop.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for UpdateJob: %v", err)
	}
	if updated.GetGeneration() != 2 {
		t.Errorf("updated generation = %d, want 2", updated.GetGeneration())
	}

	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	md, err := rop.Metadata()
	if err != nil || !strings.HasPrefix(md.GetName(), name+"/executions/") {
		t.Fatalf("RunJob's operation metadata = %v, %v; want the execution", md, err)
	}
	execution, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the execution: %v", err)
	}
	if execution.GetName() != md.GetName() || execution.GetJob() != name || execution.GetTaskCount() != 2 || execution.GetSucceededCount() != 2 ||
		execution.GetCompletionTime() == nil || completedCondition(execution).GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("completed execution = %v", execution)
	}
	if env := execution.GetTemplate().GetContainers()[0].GetEnv(); len(env) != 1 || env[0].GetValue() != "second" {
		t.Errorf("the execution ran with env %v; want the updated GREETING=second", env)
	}
	execID := execution.GetName()[strings.LastIndex(execution.GetName(), "/")+1:]
	// On the cluster itself: one batch Job with a completion per task.
	if os.Getenv(envKubeconfig) != "" {
		if y, err := kubectlGet(t, "jobs.batch", execID); err != nil {
			t.Error(err)
		} else if !strings.Contains(y, "completions: 2") || !strings.Contains(y, "completionMode: Indexed") {
			t.Errorf("the batch Job is not two indexed completions:\n%s", y)
		}
		// Each task saw its own CLOUD_RUN_TASK_INDEX and the updated env.
		logs, err := exec.Command("kubectl", "--kubeconfig", os.Getenv(envKubeconfig), "-n", "default", "logs",
			"-l", "batch.kubernetes.io/job-name="+execID, "--tail", "5").CombinedOutput()
		if err != nil {
			t.Errorf("kubectl logs: %v\n%s", err, logs)
		}
		for _, want := range []string{"task 0 of 2 says second", "task 1 of 2 says second"} {
			if !strings.Contains(string(logs), want) {
				t.Errorf("the tasks' output lacks %q:\n%s", want, logs)
			}
		}
	}

	read, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: execution.GetName()})
	if err != nil || read.GetSucceededCount() != 2 {
		t.Errorf("GetExecution = %v, %v", read, err)
	}
	var listed []string
	for it := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: name}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListExecutions: %v", err)
		}
		listed = append(listed, e.GetName())
	}
	if len(listed) != 1 || listed[0] != execution.GetName() {
		t.Errorf("ListExecutions = %v, want [%s]", listed, execution.GetName())
	}
	if after, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name}); err != nil ||
		after.GetLatestCreatedExecution().GetName() != execution.GetName() ||
		after.GetLatestCreatedExecution().GetCompletionStatus() != runpb.ExecutionReference_EXECUTION_SUCCEEDED {
		t.Errorf("the job's latest execution = %v, %v", after.GetLatestCreatedExecution(), err)
	}

	dop, err := xc.DeleteExecution(ctx, &runpb.DeleteExecutionRequest{Name: execution.GetName()})
	if err != nil {
		t.Fatalf("DeleteExecution: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("waiting for DeleteExecution: %v", err)
	}
	if _, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: execution.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetExecution after DeleteExecution = %v, want NotFound", err)
	}

	djop, err := jc.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := djop.Wait(ctx); err != nil {
		t.Fatalf("waiting for DeleteJob: %v", err)
	}
	if _, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after DeleteJob = %v, want NotFound", err)
	}
}

// covers: google.cloud.run.v2.Executions/CancelExecution
//
// TestRunJobFailureAndCancel (#582): a task whose container exits non-zero
// fails its execution with a failed Completed condition carrying the
// container's exit code and its own output, and fails RunJob's operation; a
// running execution (the same job with its args overridden to sleep) is
// cancelled, its task counted cancelled; and a field a batch Job cannot
// honour is refused by name.
func TestRunJobFailureAndCancel(t *testing.T) {
	h := New(t)
	jc, xc := runJobClients(t, h)
	// Longer than the harness's 60s: this test runs a job to failure, waits
	// up to three minutes for a second one to start, then cancels it. On a CI
	// runner the three together overran 60s.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	id := "compat-job-fail"
	name := runParent(h) + "/jobs/" + id

	refused := shellJob("true", 1, nil)
	refused.Template.Template.VpcAccess = &runpb.VpcAccess{Connector: "projects/p/locations/l/connectors/c"}
	if _, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: refused}); status.Code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "template.template.vpcAccess") {
		t.Fatalf("CreateJob with vpcAccess = %v; want Unimplemented naming template.template.vpcAccess", err)
	}

	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id,
		Job: shellJob(`echo "migration failed: table users is missing"; exit 3`, 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err == nil || !strings.Contains(err.Error(), "exited with code 3") {
		t.Errorf("waiting for a failing execution = %v; want an error naming the exit code", err)
	}
	md, err := rop.Metadata()
	if err != nil || md.GetName() == "" {
		t.Fatalf("a failed RunJob's operation names no execution: %v, %v", md, err)
	}
	failed, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: md.GetName()})
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	c := completedCondition(failed)
	t.Logf("failed execution's Completed condition: %v", c)
	if c.GetState() != runpb.Condition_CONDITION_FAILED || c.GetExecutionReason() != runpb.Condition_NON_ZERO_EXIT_CODE ||
		!strings.Contains(c.GetMessage(), "exited with code 3") || !strings.Contains(c.GetMessage(), "table users is missing") {
		t.Errorf("Completed condition = %v; want FAILED, NON_ZERO_EXIT_CODE, with the exit code and the container's output", c)
	}
	if failed.GetFailedCount() != 1 || failed.GetSucceededCount() != 0 {
		t.Errorf("failed %d, succeeded %d; want 1, 0", failed.GetFailedCount(), failed.GetSucceededCount())
	}

	// The same job, overridden to sleep, is cancelled while it runs. The task
	// exits on SIGTERM, as a Cloud Run task is expected to: `sh -c` as PID 1
	// ignores it, so a bare sleep held every cancel for the pod's full 30s
	// grace period.
	sop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name, Overrides: &runpb.RunJobRequest_Overrides{
		ContainerOverrides: []*runpb.RunJobRequest_Overrides_ContainerOverride{{Args: []string{`trap 'exit 143' TERM; sleep 300 & wait`}}}}})
	if err != nil {
		t.Fatalf("RunJob with overrides: %v", err)
	}
	smd, err := sop.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	var running *runpb.Execution
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if running, err = xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: smd.GetName()}); err == nil && running.GetRunningCount() > 0 {
			break
		}
	}
	if running.GetRunningCount() == 0 {
		t.Fatalf("the overridden execution never ran: %v, %v", running, err)
	}
	cop, err := xc.CancelExecution(ctx, &runpb.CancelExecutionRequest{Name: smd.GetName()})
	if err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	cancelled, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for CancelExecution: %v", err)
	}
	t.Logf("cancelled execution: cancelled %d, failed %d, retried %d, %v", cancelled.GetCancelledCount(),
		cancelled.GetFailedCount(), cancelled.GetRetriedCount(), completedCondition(cancelled))
	if cancelled.GetCancelledCount() != 1 || cancelled.GetRunningCount() != 0 ||
		completedCondition(cancelled).GetExecutionReason() != runpb.Condition_CANCELLED {
		t.Errorf("cancelled execution = %v", cancelled)
	}
	if _, err := sop.Wait(ctx); err == nil {
		t.Error("the cancelled execution's RunJob operation succeeded")
	}
}

// TestRunJobReadsASecretManagerSecret (#582): a job's secretKeyRef env var
// is mapped as a service's is, onto the Kubernetes Secret the Secret Manager
// store wrote. The task exits 0 only when it was given the payload, so the
// execution succeeding is the proof; the batch Job carries the reference,
// not the payload.
func TestRunJobReadsASecretManagerSecret(t *testing.T) {
	h := New(t)
	sc := secretsClient(t, h)
	jc, _ := runJobClients(t, h)
	ctx := h.Context()
	const secretID, payload = "compat-job-secret", "job-secret-value"
	secretName := secretsParent(h) + "/secrets/" + secretID
	if _, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: secretName}) })
	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secretName,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(payload)}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}

	id := "compat-job-secret"
	name := runParent(h) + "/jobs/" + id
	// The check is split so the payload is not a literal in the manifest.
	job := shellJob(`[ "${TOKEN#job-secret-}" = "value" ] || { echo "TOKEN is not the secret's payload"; exit 1; }`, 1, nil)
	job.Template.Template.Containers[0].Env = []*runpb.EnvVar{{Name: "TOKEN", Values: &runpb.EnvVar_ValueSource{
		ValueSource: &runpb.EnvVarSource{SecretKeyRef: &runpb.SecretKeySelector{Secret: secretID, Version: "latest"}}}}}
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	execution, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("the task was not given the secret: %v", err)
	}
	ref := execution.GetTemplate().GetContainers()[0].GetEnv()[0].GetValueSource().GetSecretKeyRef()
	if ref.GetSecret() != secretID || ref.GetVersion() != "latest" {
		t.Errorf("the execution's env reads back as %v; want the secretKeyRef that was set", ref)
	}
	if os.Getenv(envKubeconfig) != "" {
		id := execution.GetName()[strings.LastIndex(execution.GetName(), "/")+1:]
		if y, err := kubectlGet(t, "jobs.batch", id); err != nil {
			t.Error(err)
		} else if strings.Contains(y, payload) || !strings.Contains(y, "secretKeyRef:") {
			t.Errorf("the batch Job should reference the secret, not carry its payload:\n%s", y)
		}
	}
}
