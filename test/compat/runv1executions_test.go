//go:build compat

package compat

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
)

// runV1Regional is the official v1 client at us-central1's regional host,
// where gcloud sends a job's calls.
func runV1Regional(t *testing.T, h *Harness) *runv1.APIService {
	t.Helper()
	c, err := runv1.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvRun)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{Transport: regionalHost{"us-central1-run.localhost"}}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// v1Cond is a v1 condition's status, "" when it is absent.
func v1Cond(cs []*runv1.GoogleCloudRunV1Condition, typ string) *runv1.GoogleCloudRunV1Condition {
	for _, c := range cs {
		if c.Type == typ {
			return c
		}
	}
	return &runv1.GoogleCloudRunV1Condition{}
}

// v1AwaitExecution polls executions.get until done says so.
func v1AwaitExecution(t *testing.T, c *runv1.APIService, name, what string, done func(*runv1.Execution) bool) *runv1.Execution {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		e, err := c.Namespaces.Executions.Get(name).Do()
		if err == nil && done(e) {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution %s never %s: %+v %v", name, what, e, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// covers: run.namespaces.jobs.replaceJob, run.namespaces.jobs.run, run.namespaces.executions.get, run.namespaces.executions.list, run.namespaces.executions.cancel, run.namespaces.executions.delete, run.namespaces.tasks.get, run.namespaces.tasks.list
//
// TestRunV1JobExecutionsAndTasks (#1379): the official v1 client, at the
// us-central1 regional host, replaces a job's template and runs it with
// overrides. The execution completes with both tasks succeeded and a
// logUri; it is listed by gcloud's run.googleapis.com/job selector, by the
// v2 ExecutionsClient and on the console's job page, and its tasks are
// listed by the run.googleapis.com/execution selector and read. A second
// run is cancelled, and both executions are deleted.
func TestRunV1JobExecutionsAndTasks(t *testing.T) {
	h := New(t)
	c := runV1Regional(t, h)
	ns := "namespaces/" + h.Project()
	id := "v1-exec-job"
	t.Cleanup(func() { _, _ = c.Namespaces.Jobs.Delete(ns + "/jobs/" + id).Do() })
	created, err := c.Namespaces.Jobs.Create(ns, &runv1.Job{ApiVersion: "run.googleapis.com/v1", Kind: "Job",
		Metadata: &runv1.ObjectMeta{Name: id},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 1,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{MaxRetries: 0, TimeoutSeconds: 120,
				Containers: []*runv1.Container{{Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{"echo v1 $A"},
					Env: []*runv1.EnvVar{{Name: "A", Value: "b"}}}}}}}}}}).Do()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// replaceJob: two tasks and a new value, from the job as read.
	created.Spec.Template.Spec.TaskCount = 2
	created.Spec.Template.Spec.Template.Spec.Containers[0].Env[0].Value = "c"
	replaced, err := c.Namespaces.Jobs.ReplaceJob(ns+"/jobs/"+id, created).Do()
	if err != nil {
		t.Fatalf("ReplaceJob: %v", err)
	}
	if replaced.Spec.Template.Spec.TaskCount != 2 || replaced.Metadata.Generation != 2 ||
		replaced.Spec.Template.Spec.Template.Spec.Containers[0].Env[0].Value != "c" {
		t.Errorf("replaced = %+v %+v", replaced.Metadata, replaced.Spec.Template.Spec)
	}
	var ge *googleapi.Error
	if _, err := c.Namespaces.Jobs.ReplaceJob(ns+"/jobs/"+id, created).Do(); !errors.As(err, &ge) || ge.Code != http.StatusConflict {
		t.Errorf("ReplaceJob with a stale resourceVersion = %v; want 409", err)
	}

	// run, with an argument and an environment override.
	ex, err := c.Namespaces.Jobs.Run(ns+"/jobs/"+id, &runv1.RunJobRequest{Overrides: &runv1.Overrides{
		ContainerOverrides: []*runv1.ContainerOverride{{Args: []string{"echo v1 $A $B task $CLOUD_RUN_TASK_INDEX"},
			Env: []*runv1.EnvVar{{Name: "B", Value: "d"}}}}}}).Do()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	execName := ns + "/executions/" + ex.Metadata.Name
	t.Cleanup(func() { _, _ = c.Namespaces.Executions.Delete(execName).Do() })
	if ex.Metadata.Labels["run.googleapis.com/job"] != id || ex.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" ||
		ex.Spec.TaskCount != 2 || len(ex.Spec.Template.Spec.Containers[0].Env) != 2 {
		t.Errorf("Run = %+v %+v", ex.Metadata, ex.Spec)
	}
	done := v1AwaitExecution(t, c, execName, "completed", func(e *runv1.Execution) bool { return e.Status.CompletionTime != "" })
	if v1Cond(done.Status.Conditions, "Completed").Status != "True" || done.Status.SucceededCount != 2 ||
		v1Cond(done.Status.Conditions, "Started").Status != "True" {
		t.Fatalf("execution = %+v", done.Status)
	}
	u, err := url.Parse(done.Status.LogUri)
	if err != nil || u.Host != "console.cloud.google.com" || u.Query().Get("project") != h.Project() ||
		!strings.Contains(u.Query().Get("advancedFilter"), `resource.type="cloud_run_job"`) ||
		!strings.Contains(u.Query().Get("advancedFilter"), `"run.googleapis.com/execution_name"="`+done.Metadata.Name+`"`) {
		t.Errorf("logUri = %q", done.Status.LogUri)
	}

	list, err := c.Namespaces.Executions.List(ns).LabelSelector("run.googleapis.com/job = " + id).Do()
	if err != nil || len(list.Items) != 1 || list.Items[0].Metadata.Name != done.Metadata.Name {
		t.Errorf("executions.list = %+v, %v", list, err)
	}
	_, xc := runJobClients(t, h)
	v2, err := xc.GetExecution(h.Context(), &runpb.GetExecutionRequest{Name: runParent(h) + "/jobs/" + id + "/executions/" + done.Metadata.Name})
	if err != nil || v2.GetSucceededCount() != 2 || v2.GetLogUri() != done.Status.LogUri {
		t.Errorf("v2 GetExecution = %v, %v", v2, err)
	}
	if page := consoleDetailOf(t, consoleAddr(t, h), "run-jobs", h.Project(), id, done.Metadata.Name); page.summary("Status") != "Succeeded" {
		t.Errorf("the console's page for the v1 execution = %v", page.Summary)
	}

	tasks, err := c.Namespaces.Tasks.List(ns).LabelSelector("run.googleapis.com/execution = " + done.Metadata.Name).Do()
	if err != nil || len(tasks.Items) != 2 {
		t.Fatalf("tasks.list = %+v, %v", tasks, err)
	}
	for i, tk := range tasks.Items {
		if tk.Status.Index != int64(i) || tk.Metadata.Labels["run.googleapis.com/runningState"] != "Succeeded" ||
			tk.Status.CompletionTime == "" || v1Cond(tk.Status.Conditions, "Completed").Status != "True" {
			t.Errorf("task %d = %+v %+v", i, tk.Metadata, tk.Status)
		}
	}
	tk, err := c.Namespaces.Tasks.Get(ns + "/tasks/" + tasks.Items[1].Metadata.Name).Do()
	if err != nil || tk.Status.Index != 1 || tk.Metadata.Labels["run.googleapis.com/execution"] != done.Metadata.Name {
		t.Errorf("tasks.get = %+v, %v", tk, err)
	}

	// A run that would not end, cancelled.
	long, err := c.Namespaces.Jobs.Run(ns+"/jobs/"+id, &runv1.RunJobRequest{Overrides: &runv1.Overrides{TaskCount: 1,
		ContainerOverrides: []*runv1.ContainerOverride{{Args: []string{`trap 'exit 143' TERM; sleep 300 & wait`}}}}}).Do()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	longName := ns + "/executions/" + long.Metadata.Name
	t.Cleanup(func() { _, _ = c.Namespaces.Executions.Delete(longName).Do() })
	v1AwaitExecution(t, c, longName, "started", func(e *runv1.Execution) bool { return e.Status.RunningCount > 0 })
	if _, err := c.Namespaces.Executions.Cancel(longName, &runv1.CancelExecutionRequest{}).Do(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	cancelled := v1AwaitExecution(t, c, longName, "cancelled", func(e *runv1.Execution) bool { return e.Status.CompletionTime != "" })
	if cond := v1Cond(cancelled.Status.Conditions, "Completed"); cond.Status != "False" || cond.Reason != "Cancelled" || cancelled.Status.CancelledCount != 1 {
		t.Errorf("cancelled execution = %+v", cancelled.Status)
	}

	for _, n := range []string{execName, longName} {
		if _, err := c.Namespaces.Executions.Delete(n).Do(); err != nil {
			t.Fatalf("Delete %s: %v", n, err)
		}
		if _, err := c.Namespaces.Executions.Get(n).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
			t.Errorf("Get %s after delete = %v; want 404", n, err)
		}
	}
}

// TestGcloudRunJobsExecute (#1379): the pinned gcloud, through
// gcloud-setup alone, updates a job (replaceJob), executes it with --wait,
// --args and --update-env-vars, lists, describes, cancels and deletes its
// executions, and lists an execution's tasks.
func TestGcloudRunJobsExecute(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	id := "gcloud-v1-exec"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "jobs", "delete", id, region, "--quiet") })
	g.must("run", "jobs", "create", id, "--image="+jobImage, region, "--tasks=1", "--max-retries=0",
		"--command=sh", "--args=-c,echo gcloud $A", "--set-env-vars=A=b")
	g.must("run", "jobs", "update", id, region, "--tasks=2", "--update-env-vars=A=c")
	if out := g.must("run", "jobs", "describe", id, region, "--format=value(spec.template.spec.taskCount)"); strings.TrimSpace(out) != "2" {
		t.Errorf("describe after update = %q; want 2 tasks", out)
	}

	out := g.must("run", "jobs", "execute", id, region, "--wait", "--args=-c,echo gcloud $A $B", "--update-env-vars=B=d")
	t.Logf("gcloud run jobs execute --wait:\n%s", out)
	list := g.must("run", "jobs", "executions", "list", "--job="+id, region, "--format=value(metadata.name)")
	execs := strings.Fields(list)
	if len(execs) != 1 || !strings.HasPrefix(execs[0], id+"-") {
		t.Fatalf("gcloud run jobs executions list = %q", list)
	}
	exec := execs[0]
	if out := g.must("run", "jobs", "executions", "describe", exec, region,
		"--format=value(status.succeededCount,spec.template.spec.containers[0].args)"); !strings.Contains(out, "2") || !strings.Contains(out, "echo gcloud $A $B") {
		t.Errorf("gcloud run jobs executions describe = %q", out)
	}
	tasks := g.must("run", "jobs", "executions", "tasks", "list", "--execution="+exec, region, "--format=value(status.index,metadata.labels.'run.googleapis.com/runningState')")
	if lines := strings.Split(strings.TrimSpace(tasks), "\n"); len(lines) != 2 || !strings.Contains(tasks, "Succeeded") {
		t.Errorf("gcloud run jobs executions tasks list = %q", tasks)
	}

	// Executed without --wait, overridden to run until cancelled.
	g.must("run", "jobs", "execute", id, region, "--tasks=1", "--args=-c,trap 'exit 143' TERM; sleep 300 & wait")
	list = g.must("run", "jobs", "executions", "list", "--job="+id, region, "--format=value(metadata.name)")
	var running string
	for _, e := range strings.Fields(list) {
		if e != exec {
			running = e
		}
	}
	if running == "" {
		t.Fatalf("the second execution is not listed: %q", list)
	}
	g.must("run", "jobs", "executions", "cancel", running, region, "--quiet")
	g.must("run", "jobs", "executions", "delete", exec, region, "--quiet")
	g.must("run", "jobs", "executions", "delete", running, region, "--quiet")
	if out := g.must("run", "jobs", "executions", "list", "--job="+id, region, "--format=value(metadata.name)"); strings.TrimSpace(out) != "" {
		t.Errorf("executions after delete = %q", out)
	}
	g.must("run", "jobs", "delete", id, region, "--quiet")
}
