//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	runv1 "google.golang.org/api/run/v1"
)

// v1FailingTaskScript fails task 1 of a job and passes every other task;
// it has no commas, so gcloud's --args can carry it too.
const v1FailingTaskScript = `test "$CLOUD_RUN_TASK_INDEX" != 1`

// covers: run.projects.locations.jobs.getIamPolicy, run.projects.locations.jobs.setIamPolicy, run.projects.locations.jobs.testIamPermissions, run.namespaces.executions.list, run.namespaces.services.list
//
// TestRunV1SelectorsDelayAndJobIAM (#1420, #1450): the official v1 client
// creates a job of two tasks, one failing, with the job's own
// delayExecution, which reads back on the job, on the global endpoint's
// jobs list (every region's jobs, with their location label) and on the
// executions it runs; replaces it without the flag, which clears it; runs
// it with overrides.delayExecution, which starts it at once and reads back
// on that execution; lists its tasks with set-based label selectors (in,
// notin, !=, existence and !key) and its executions with in; ignores a
// fieldSelector on executions.list and services.list, as Cloud Run
// documents (#1595); is refused a gt label selector as INVALID_ARGUMENT;
// and stores, reads and tests the
// job's IAM policy through projects.locations.jobs, the policy v2's Jobs
// IAM serves.
func TestRunV1SelectorsDelayAndJobIAM(t *testing.T) {
	h := New(t)
	c := runV1Regional(t, h)
	ns := "namespaces/" + h.Project()
	id := "v1-select-job"
	t.Cleanup(func() { _, _ = c.Namespaces.Jobs.Delete(ns + "/jobs/" + id).Do() })
	spec := func(delay bool) *runv1.Job {
		return &runv1.Job{ApiVersion: "run.googleapis.com/v1", Kind: "Job", Metadata: &runv1.ObjectMeta{Name: id},
			Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 2, DelayExecution: delay,
				Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{MaxRetries: 1, TimeoutSeconds: 120,
					Containers: []*runv1.Container{{Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{v1FailingTaskScript}}}}}}}}}
	}
	var ge *googleapi.Error
	if j, err := c.Namespaces.Jobs.Create(ns, spec(true)).Do(); err != nil || !j.Spec.Template.Spec.DelayExecution {
		t.Fatalf("Create with the job's own delayExecution = %v; want it stored", err)
	}
	j, err := c.Namespaces.Jobs.Get(ns + "/jobs/" + id).Do()
	if err != nil || !j.Spec.Template.Spec.DelayExecution {
		t.Fatalf("Get = %v; want delayExecution read back", err)
	}
	// The global endpoint (no region) lists every region's jobs, which is
	// how gcloud finds a job's region.
	all, err := runV1Client(t, h).Namespaces.Jobs.List(ns).Do()
	if err != nil {
		t.Fatalf("global jobs.list: %v", err)
	}
	var listed *runv1.Job
	for _, item := range all.Items {
		if item.Metadata.Name == id {
			listed = item
		}
	}
	if listed == nil || listed.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" || !listed.Spec.Template.Spec.DelayExecution {
		t.Errorf("global jobs.list = %+v; want %s in us-central1 with delayExecution", listed, id)
	}
	ex, err := c.Namespaces.Jobs.Run(ns+"/jobs/"+id, &runv1.RunJobRequest{}).Do()
	if err != nil || !ex.Spec.DelayExecution {
		t.Fatalf("Run of a delayed job = %v; want its execution delayed", err)
	}
	first := ns + "/executions/" + ex.Metadata.Name
	t.Cleanup(func() { _, _ = c.Namespaces.Executions.Delete(first).Do() })
	if e, err := c.Namespaces.Executions.Get(first).Do(); err != nil || !e.Spec.DelayExecution {
		t.Errorf("executions.get = %v; want spec.delayExecution", err)
	}
	v1AwaitExecution(t, c, first, "completed", func(e *runv1.Execution) bool { return e.Status.CompletionTime != "" })
	if _, err := c.Namespaces.Executions.Delete(first).Do(); err != nil {
		t.Fatalf("delete the first execution: %v", err)
	}
	j.Spec.Template.Spec.DelayExecution = false
	if j, err := c.Namespaces.Jobs.ReplaceJob(ns+"/jobs/"+id, j).Do(); err != nil || j.Spec.Template.Spec.DelayExecution {
		t.Fatalf("replaceJob without delayExecution = %v; want it cleared", err)
	}

	ex, err = c.Namespaces.Jobs.Run(ns+"/jobs/"+id, &runv1.RunJobRequest{Overrides: &runv1.Overrides{DelayExecution: true}}).Do()
	if err != nil || !ex.Spec.DelayExecution {
		t.Fatalf("Run with overrides.delayExecution: %v; want its execution delayed", err)
	}
	exec := ex.Metadata.Name
	execName := ns + "/executions/" + exec
	t.Cleanup(func() { _, _ = c.Namespaces.Executions.Delete(execName).Do() })
	done := v1AwaitExecution(t, c, execName, "completed", func(e *runv1.Execution) bool { return e.Status.CompletionTime != "" })
	if done.Status.SucceededCount != 1 || done.Status.FailedCount != 1 || v1Cond(done.Status.Conditions, "Completed").Status != "False" {
		t.Fatalf("execution = %+v", done.Status)
	}

	byExec := "run.googleapis.com/execution = " + exec + ","
	for _, tc := range []struct {
		selector string
		want     []int64
	}{
		{"run.googleapis.com/runningState in (Succeeded)", []int64{0}},
		{"run.googleapis.com/runningState in (Failed,Cancelled)", []int64{1}},
		{"run.googleapis.com/runningState notin (Succeeded)", []int64{1}},
		{"run.googleapis.com/runningState!=Failed", []int64{0}},
		{"run.googleapis.com/runningState", []int64{0, 1}},
		{"!run.googleapis.com/runningState", nil},
		{"run.googleapis.com/runningState in (Running,Pending)", nil},
	} {
		list, err := c.Namespaces.Tasks.List(ns).LabelSelector(byExec + tc.selector).Do()
		if err != nil {
			t.Errorf("tasks.list %q: %v", tc.selector, err)
			continue
		}
		var got []int64
		for _, tk := range list.Items {
			got = append(got, tk.Status.Index)
		}
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("tasks.list %q = indexes %v; want %v", tc.selector, got, tc.want)
		}
	}
	if _, err := c.Namespaces.Tasks.List(ns).LabelSelector(byExec + "run.googleapis.com/runningState > 1").Do(); !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Errorf("tasks.list with a gt selector = %v; want 400", err)
	}

	list, err := c.Namespaces.Executions.List(ns).LabelSelector("run.googleapis.com/job in (" + id + ",other)").Do()
	if err != nil || len(list.Items) != 1 || list.Items[0].Metadata.Name != exec || !list.Items[0].Spec.DelayExecution {
		t.Errorf("executions.list with in = %+v, %v; want the delayed execution", list, err)
	}
	// fieldSelector: "Not supported, and ignored by Cloud Run" (services.list)
	// and "Not supported by Cloud Run" (every other v1 list), read
	// 2026-10-17 (#1595): whatever it names, a list is the one without it.
	allExec, err := c.Namespaces.Executions.List(ns).Do()
	if err != nil || len(allExec.Items) == 0 {
		t.Fatalf("executions.list = %+v, %v", allExec, err)
	}
	for _, fs := range []string{"metadata.name=" + exec, "metadata.name=nope", "status.succeededCount=1", "metadata.name"} {
		got, err := c.Namespaces.Executions.List(ns).FieldSelector(fs).Do()
		if err != nil || len(got.Items) != len(allExec.Items) {
			t.Errorf("executions.list with fieldSelector %q = %+v, %v; want the %d executions of a list without it", fs, got, err, len(allExec.Items))
		}
	}
	allSvc, err := c.Namespaces.Services.List(ns).Do()
	if err != nil {
		t.Fatalf("services.list = %v", err)
	}
	for _, fs := range []string{"metadata.name=nope", "spec.template=x"} {
		got, err := c.Namespaces.Services.List(ns).FieldSelector(fs).Do()
		if err != nil || len(got.Items) != len(allSvc.Items) {
			t.Errorf("services.list with fieldSelector %q = %+v, %v; want the %d services of a list without it", fs, got, err, len(allSvc.Items))
		}
	}
	jobs, err := c.Namespaces.Jobs.List(ns).LabelSelector("cloud.googleapis.com/location in (us-central1),!nope").Do()
	if err != nil || len(jobs.Items) == 0 {
		t.Errorf("jobs.list with in and !key = %+v, %v", jobs, err)
	}

	// projects.locations.jobs: the job's IAM policy.
	name := "projects/" + h.Project() + "/locations/us-central1/jobs/" + id
	p, err := c.Projects.Locations.Jobs.GetIamPolicy(name).Do()
	if err != nil || p.Etag == "" || len(p.Bindings) != 0 {
		t.Fatalf("jobs getIamPolicy = %+v, %v; want an empty policy with an etag", p, err)
	}
	p.Bindings = []*runv1.Binding{{Role: "roles/run.invoker", Members: []string{"user:dev@example.com"}}}
	set, err := c.Projects.Locations.Jobs.SetIamPolicy(name, &runv1.SetIamPolicyRequest{Policy: p}).Do()
	if err != nil || set.Etag == p.Etag || len(set.Bindings) != 1 {
		t.Fatalf("jobs setIamPolicy = %+v, %v", set, err)
	}
	if _, err := c.Projects.Locations.Jobs.SetIamPolicy(name, &runv1.SetIamPolicyRequest{Policy: p}).Do(); !errors.As(err, &ge) || ge.Code != http.StatusConflict {
		t.Errorf("jobs setIamPolicy with a stale etag = %v; want 409 ABORTED", err)
	}
	got, err := c.Projects.Locations.Jobs.GetIamPolicy(name).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:dev@example.com" {
		t.Errorf("jobs getIamPolicy after set = %+v, %v", got, err)
	}
	tp, err := c.Projects.Locations.Jobs.TestIamPermissions(name, &runv1.TestIamPermissionsRequest{
		Permissions: []string{"run.jobs.run", "run.jobs.get"}}).Do()
	if err != nil || len(tp.Permissions) != 2 {
		t.Errorf("jobs testIamPermissions = %+v, %v", tp, err)
	}
	if _, err := c.Projects.Locations.Jobs.GetIamPolicy(name + "-missing").Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("getIamPolicy of a missing job = %v; want 404", err)
	}
}

// TestGcloudRunJobsTaskFiltersAndIAM (#1420): the pinned gcloud, through
// gcloud-setup alone, executes a job of two tasks, one failing, with
// `gcloud beta run jobs execute --delay-execution --wait`; lists its tasks
// with --succeeded, --failed, --completed and --running (set-based
// run.googleapis.com/runningState selectors); adds, reads and removes an IAM
// binding on the job (projects.locations.jobs); and, for a service,
// `gcloud beta run services describe` with no region finds the service's
// region through projects.locations.services.list, whose fieldSelector
// Cloud Run ignores (gcloud matches the name itself).
func TestGcloudRunJobsTaskFiltersAndIAM(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	id := "gcloud-v1-filters"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "jobs", "delete", id, region, "--quiet") })
	g.must("run", "jobs", "create", id, "--image="+jobImage, region, "--tasks=2", "--max-retries=1",
		"--command=sh", "--args=-c,"+v1FailingTaskScript)

	out, err := g.run(nil, "beta", "run", "jobs", "execute", id, region, "--wait", "--delay-execution")
	if err == nil {
		t.Errorf("gcloud beta run jobs execute --wait of a failing task succeeded:\n%s", out)
	}
	t.Logf("gcloud beta run jobs execute --delay-execution --wait:\n%s", out)
	execs := strings.Fields(g.must("run", "jobs", "executions", "list", "--job="+id, region, "--format=value(metadata.name)"))
	if len(execs) != 1 {
		t.Fatalf("gcloud run jobs executions list = %q", execs)
	}
	exec := execs[0]
	tasks := func(filter ...string) []string {
		// By name, {execution}-task{index}: v1 omits an index of 0, so
		// value(status.index) prints nothing for task 0.
		args := append([]string{"run", "jobs", "executions", "tasks", "list", "--execution=" + exec, region, "--format=value(metadata.name)"}, filter...)
		var out []string
		for _, n := range strings.Fields(g.must(args...)) {
			out = append(out, strings.TrimPrefix(n, exec+"-task"))
		}
		return out
	}
	for _, tc := range []struct {
		flag string
		want string
	}{
		{"--succeeded", "0"},
		{"--failed", "1"},
		{"--completed", "0 1"},
		{"--running", ""},
	} {
		if got := strings.Join(tasks(tc.flag), " "); got != tc.want {
			t.Errorf("gcloud run jobs executions tasks list %s = %q; want %q", tc.flag, got, tc.want)
		}
	}

	g.must("run", "jobs", "add-iam-policy-binding", id, region, "--member=user:dev@example.com", "--role=roles/run.invoker")
	if out := g.must("run", "jobs", "get-iam-policy", id, region, "--format=value(bindings)"); !strings.Contains(out, "user:dev@example.com") {
		t.Errorf("gcloud run jobs get-iam-policy = %q", out)
	}
	g.must("run", "jobs", "remove-iam-policy-binding", id, region, "--member=user:dev@example.com", "--role=roles/run.invoker")
	if out := g.must("run", "jobs", "get-iam-policy", id, region, "--format=value(bindings)"); strings.Contains(out, "dev@example.com") {
		t.Errorf("get-iam-policy after remove = %q", out)
	}

	runV1Service(t, runV1Client(t, h), g.project, "gcloud-field-sel")
	out, err = g.run(nil, "beta", "run", "services", "describe", "gcloud-field-sel", "--quiet")
	if err == nil || !strings.Contains(out, "found in region us-central1") {
		t.Errorf("gcloud beta run services describe with no region = %v:\n%s\nwant the region found through services.list", err, out)
	}
}

// TestGcloudRunJobsDelayExecution (#1450): the pinned gcloud, through
// gcloud-setup alone, creates a job with `gcloud beta run jobs create
// --delay-execution`, which describe reads back; `gcloud beta run jobs
// describe` with no region finds the job's region through the global
// endpoint's namespaces.jobs.list; `gcloud beta run jobs update
// --no-delay-execution` clears the flag; and the delayed job runs.
func TestGcloudRunJobsDelayExecution(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	id := "gcloud-v1-delay"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "jobs", "delete", id, region, "--quiet") })
	g.must("beta", "run", "jobs", "create", id, "--image="+jobImage, region, "--delay-execution", "--command=true")
	delay := func() string {
		return strings.TrimSpace(g.must("run", "jobs", "describe", id, region, "--format=value(spec.template.spec.delayExecution)"))
	}
	if got := delay(); got != "True" {
		t.Errorf("describe after create --delay-execution: delayExecution = %q; want True", got)
	}
	out, err := g.run(nil, "beta", "run", "jobs", "describe", id, "--quiet")
	if err == nil || !strings.Contains(out, "found in region us-central1") {
		t.Errorf("gcloud beta run jobs describe with no region = %v:\n%s\nwant the region found through the global jobs list", err, out)
	}
	g.must("beta", "run", "jobs", "execute", id, region, "--wait")
	g.must("beta", "run", "jobs", "update", id, region, "--no-delay-execution")
	if got := delay(); got != "" && got != "False" {
		t.Errorf("describe after update --no-delay-execution: delayExecution = %q; want unset", got)
	}
}
