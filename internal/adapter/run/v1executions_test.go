package run

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	runv1 "google.golang.org/api/run/v1"
)

// v1Do sends a v1 request at us-central1's regional host and decodes the
// answer into out.
func v1Do(t *testing.T, h http.Handler, method, path string, in, out any) int {
	t.Helper()
	var body string
	if in != nil {
		raw, _ := json.Marshal(in)
		body = string(raw)
	}
	r := httptest.NewRequest(method, "http://us-central1-run.localhost"+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if out != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, w.Body.String())
		}
	}
	if w.Code != http.StatusOK {
		t.Logf("%s %s = %d %s", method, path, w.Code, w.Body.String())
	}
	return w.Code
}

// A v1 job is replaced, run with overrides, and its execution and tasks
// are read, listed by gcloud's selectors, cancelled and deleted in v1's
// shape, over the v2 servers.
func TestV1JobExecutionsAndTasks(t *testing.T) {
	js, _, _, f := jobsServer(fakeSecrets{})
	h := js.s.V1Handler(http.NotFoundHandler())
	const ns = "/apis/run.googleapis.com/v1/namespaces/demo-project"
	job := &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "nightly"},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 2,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{MaxRetries: 1, TimeoutSeconds: 60,
				Containers: []*runv1.Container{{Image: "busybox:1.36", Env: []*runv1.EnvVar{{Name: "A", Value: "b"}}}}}}}}}}
	var created runv1.Job
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs", job, &created); code != 200 {
		t.Fatalf("create = %d", code)
	}

	// replaceJob: gcloud run jobs update sends the job it read, changed.
	created.Spec.Template.Spec.TaskCount = 3
	var replaced runv1.Job
	if code := v1Do(t, h, http.MethodPut, ns+"/jobs/nightly", &created, &replaced); code != 200 ||
		replaced.Spec.Template.Spec.TaskCount != 3 || replaced.Metadata.Generation != 2 {
		t.Fatalf("replaceJob = %d %+v", code, replaced.Metadata)
	}
	stale := created
	if code := v1Do(t, h, http.MethodPut, ns+"/jobs/nightly", &stale, nil); code != http.StatusConflict {
		t.Errorf("replaceJob with a stale resourceVersion = %d; want 409", code)
	}

	// run, with gcloud's --args, --update-env-vars and --tasks overrides.
	var exec runv1.Execution
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs/nightly:run", &runv1.RunJobRequest{Overrides: &runv1.Overrides{TaskCount: 2,
		ContainerOverrides: []*runv1.ContainerOverride{{Args: []string{"x"}, Env: []*runv1.EnvVar{{Name: "A", Value: "c"}}}}}}, &exec); code != 200 {
		t.Fatalf("run = %d", code)
	}
	id := exec.Metadata.Name
	if !strings.HasPrefix(id, "nightly-") || exec.Metadata.Labels[v1JobLabel] != "nightly" || exec.Metadata.Labels[v1LocationLabel] != "us-central1" ||
		exec.Spec.TaskCount != 2 || exec.Spec.Template.Spec.Containers[0].Args[0] != "x" || exec.Spec.Template.Spec.Containers[0].Env[0].Value != "c" {
		t.Fatalf("run = %+v %+v", exec.Metadata, exec.Spec)
	}
	u, err := url.Parse(exec.Status.LogUri)
	if err != nil || u.Host != "console.cloud.google.com" || u.Query().Get("project") != "demo-project" ||
		!strings.Contains(u.Query().Get("advancedFilter"), `labels."run.googleapis.com/execution_name"="`+id+`"`) {
		t.Errorf("logUri = %q", exec.Status.LogUri)
	}
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs/nightly:run", &runv1.RunJobRequest{Overrides: &runv1.Overrides{DelayExecution: true}}, nil); code != http.StatusNotImplemented {
		t.Errorf("run with delayExecution = %d; want 501", code)
	}
	f.setStatus(id, map[string]any{"active": 1, "startTime": "2026-10-16T10:00:00Z"})

	var got runv1.Execution
	if code := v1Do(t, h, http.MethodGet, ns+"/executions/"+id, nil, &got); code != 200 || got.Status.RunningCount != 1 || got.Status.StartTime == "" {
		t.Fatalf("executions.get = %d %+v", code, got.Status)
	}
	conds := map[string]string{}
	for _, c := range got.Status.Conditions {
		conds[c.Type] = c.Status
	}
	if conds["Started"] != "True" || conds["ResourcesAvailable"] != "True" || conds["Completed"] != "Unknown" {
		t.Errorf("conditions = %v", conds)
	}
	var list runv1.ListExecutionsResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/executions?labelSelector="+url.QueryEscape("run.googleapis.com/job = nightly"), nil, &list); code != 200 ||
		len(list.Items) != 1 || list.Items[0].Metadata.Name != id {
		t.Errorf("executions.list = %d %+v", code, list.Items)
	}
	var none, all runv1.ListExecutionsResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/executions?labelSelector="+url.QueryEscape("run.googleapis.com/job = other"), nil, &none); code != 200 || len(none.Items) != 0 {
		t.Errorf("executions.list of a missing job = %d %+v", code, none.Items)
	}
	if code := v1Do(t, h, http.MethodGet, ns+"/executions", nil, &all); code != 200 || len(all.Items) != 1 {
		t.Errorf("executions.list of the region = %d %+v", code, all.Items)
	}

	var tasks runv1.ListTasksResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/tasks?labelSelector="+url.QueryEscape(v1ExecutionLabel+" = "+id), nil, &tasks); code != 200 || len(tasks.Items) != 2 ||
		tasks.Items[1].Metadata.Name != id+"-task1" || tasks.Items[1].Status.Index != 1 || tasks.Items[0].Metadata.Labels[v1RunningStateLabel] != "Pending" {
		t.Fatalf("tasks.list = %d %+v", code, tasks.Items)
	}
	var task runv1.Task
	if code := v1Do(t, h, http.MethodGet, ns+"/tasks/"+id+"-task1", nil, &task); code != 200 || task.Metadata.Labels[v1ExecutionLabel] != id ||
		!strings.Contains(task.Status.LogUri, "task_index") {
		t.Errorf("tasks.get = %d %+v", code, task.Metadata)
	}
	if code := v1Do(t, h, http.MethodGet, ns+"/tasks/"+id+"-task5", nil, nil); code != http.StatusNotFound {
		t.Errorf("tasks.get beyond the count = %d; want 404", code)
	}

	var cancelled runv1.Execution
	if code := v1Do(t, h, http.MethodPost, ns+"/executions/"+id+":cancel", struct{}{}, &cancelled); code != 200 || cancelled.Metadata.Name != id {
		t.Errorf("executions.cancel = %d %+v", code, cancelled.Metadata)
	}
	var st runv1.Status
	if code := v1Do(t, h, http.MethodDelete, ns+"/executions/"+id, nil, &st); code != 200 || st.Status != "Success" {
		t.Errorf("executions.delete = %d %+v", code, st)
	}
	if code := v1Do(t, h, http.MethodGet, ns+"/executions/"+id, nil, nil); code != http.StatusNotFound {
		t.Errorf("executions.get after delete = %d; want 404", code)
	}
}

func TestV1TaskExecution(t *testing.T) {
	for in, want := range map[string]string{"a-b1c2d-task0": "a-b1c2d", "job-task-x2-task12": "job-task-x2"} {
		if got, _, ok := v1TaskExecution(in); !ok || got != want {
			t.Errorf("v1TaskExecution(%q) = %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"task0", "a-taskx", "a-task01", "a-task-1"} {
		if _, _, ok := v1TaskExecution(in); ok {
			t.Errorf("v1TaskExecution(%q) accepted", in)
		}
	}
}
