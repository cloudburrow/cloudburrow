package run

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	runv1 "google.golang.org/api/run/v1"
)

// v1 list selectors (#1420): Kubernetes' label selector grammar, which
// gcloud's task state filters send, and fieldSelector on metadata.name and
// metadata.namespace.
func TestV1Selectors(t *testing.T) {
	sel := func(label, field string) (v1Selector, error) {
		q := url.Values{}
		if label != "" {
			q.Set("labelSelector", label)
		}
		if field != "" {
			q.Set("fieldSelector", field)
		}
		return labelSelector(httptest.NewRequest(http.MethodGet, "/x?"+q.Encode(), nil))
	}
	meta := &runv1.ObjectMeta{Name: "t-task0", Namespace: "p",
		Labels: map[string]string{"run.googleapis.com/runningState": "Succeeded", "run.googleapis.com/execution": "e1"}}
	for _, tc := range []struct {
		label, field string
		want         bool
	}{
		{"", "", true},
		{"run.googleapis.com/execution = e1", "", true},
		{"run.googleapis.com/execution==e1", "", true},
		{"run.googleapis.com/execution = e1,run.googleapis.com/runningState in (Succeeded,Failed)", "", true},
		{"run.googleapis.com/runningState in (Failed, Cancelled)", "", false},
		{"run.googleapis.com/runningState notin (Failed)", "", true},
		{"run.googleapis.com/runningState NOTIN (Succeeded)", "", false},
		{"run.googleapis.com/runningState != Succeeded", "", false},
		{"missing != x", "", true},
		{"missing notin (x)", "", true},
		{"missing in (x)", "", false},
		{"run.googleapis.com/execution", "", true},
		{"!run.googleapis.com/execution", "", false},
		{"!missing", "", true},
		{"", "metadata.name=t-task0", true},
		{"", "metadata.name==t-task0,metadata.namespace=p", true},
		{"", "metadata.name!=t-task0", false},
		{"run.googleapis.com/execution=e1", "metadata.name=other", false},
	} {
		s, err := sel(tc.label, tc.field)
		if err != nil {
			t.Errorf("%q %q: %v", tc.label, tc.field, err)
			continue
		}
		if got := selected(meta, s); got != tc.want {
			t.Errorf("selected(%q, %q) = %v; want %v", tc.label, tc.field, got, tc.want)
		}
	}
	for _, tc := range []struct{ label, field string }{
		{"a > 1", ""}, {"a < 1", ""}, {"a in (x", ""}, {"a in ((x))", ""}, {",", ""}, {"a=b c", ""},
		{"bad key!=x", ""}, {"a in (b c)", ""}, {"", "status.phase=Running"}, {"", "metadata.name"},
	} {
		if _, err := sel(tc.label, tc.field); err == nil || err.Error() == "" {
			t.Errorf("selector %q %q was accepted; want INVALID_ARGUMENT", tc.label, tc.field)
		}
	}
	s, _ := sel("run.googleapis.com/job in (j1),run.googleapis.com/execution=e1", "")
	if s.only("run.googleapis.com/job") != "j1" || s.only("run.googleapis.com/execution") != "e1" || s.only("x") != "" {
		t.Errorf("only = %q %q", s.only("run.googleapis.com/job"), s.only("run.googleapis.com/execution"))
	}
	if s, _ := sel("run.googleapis.com/job in (j1,j2)", ""); s.only("run.googleapis.com/job") != "" {
		t.Error("only of a two-valued in should be empty")
	}
}

// delayExecution (#1450): a job's own is stored and read back on the job
// and on every execution it runs; a run's overrides.delayExecution is read
// back on its execution; replaceJob without it clears it. The job's IAM
// policy is served at projects.locations.jobs (#1420).
func TestV1DelayExecution(t *testing.T) {
	js, _, _, _ := jobsServer(fakeSecrets{})
	h := js.s.V1Handler(http.NotFoundHandler())
	const ns = "/apis/run.googleapis.com/v1/namespaces/demo-project"
	job := &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "later"},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 1, DelayExecution: true,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{
				Containers: []*runv1.Container{{Image: "busybox:1.36"}}}}}}}}
	var got runv1.Job
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs", job, &got); code != 200 || !got.Spec.Template.Spec.DelayExecution {
		t.Fatalf("create with spec.template.spec.delayExecution = %d %+v", code, got.Spec)
	}
	if code := v1Do(t, h, http.MethodGet, ns+"/jobs/later", nil, &got); code != 200 || !got.Spec.Template.Spec.DelayExecution {
		t.Errorf("get = %d, delayExecution %v; want true", code, got.Spec.Template.Spec.DelayExecution)
	}
	var jobs runv1.ListJobsResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/jobs", nil, &jobs); code != 200 || len(jobs.Items) != 1 || !jobs.Items[0].Spec.Template.Spec.DelayExecution {
		t.Errorf("jobs.list = %d %+v", code, jobs.Items)
	}
	// The job's flag delays the executions it runs.
	var exec runv1.Execution
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs/later:run", &runv1.RunJobRequest{}, &exec); code != 200 || !exec.Spec.DelayExecution {
		t.Fatalf("run of a delayed job = %d %+v", code, exec.Spec)
	}
	first := exec.Metadata.Name
	if code := v1Do(t, h, http.MethodGet, ns+"/executions/"+first, nil, &exec); code != 200 || !exec.Spec.DelayExecution {
		t.Errorf("executions.get = %d, delayExecution %v; want true", code, exec.Spec.DelayExecution)
	}
	// replaceJob without the flag clears it, and a run is no longer delayed.
	got.Spec.Template.Spec.DelayExecution = false
	in := got
	got = runv1.Job{}
	if code := v1Do(t, h, http.MethodPut, ns+"/jobs/later", &in, &got); code != 200 || got.Spec.Template.Spec.DelayExecution {
		t.Fatalf("replaceJob without delayExecution = %d %+v", code, got.Spec.Template.Spec)
	}
	exec = runv1.Execution{}
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs/later:run", &runv1.RunJobRequest{}, &exec); code != 200 || exec.Spec.DelayExecution {
		t.Errorf("run of an undelayed job = %d, delayExecution %v; want false", code, exec.Spec.DelayExecution)
	}
	plain := exec.Metadata.Name
	// A run's overrides.delayExecution delays that execution alone.
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs/later:run", &runv1.RunJobRequest{Overrides: &runv1.Overrides{DelayExecution: true, TaskCount: 1}}, &exec); code != 200 ||
		exec.Metadata.Labels[v1JobLabel] != "later" || !exec.Spec.DelayExecution {
		t.Fatalf("run with overrides.delayExecution = %d %+v %+v", code, exec.Metadata, exec.Spec)
	}
	overridden := exec.Metadata.Name
	var list runv1.ListExecutionsResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/executions?labelSelector="+url.QueryEscape("run.googleapis.com/job in (later)"), nil, &list); code != 200 || len(list.Items) != 3 {
		t.Fatalf("executions.list = %d %d", code, len(list.Items))
	}
	for _, e := range list.Items {
		want := e.Metadata.Name == first || e.Metadata.Name == overridden
		if e.Metadata.Name == plain {
			want = false
		}
		if e.Spec.DelayExecution != want {
			t.Errorf("executions.list: %s delayExecution = %v; want %v", e.Metadata.Name, e.Spec.DelayExecution, want)
		}
	}
	// projects.locations.jobs: the job's v2 IAM policy (#1420).
	var pol map[string]any
	if code := v1Do(t, h, http.MethodGet, "/v1/projects/demo-project/locations/us-central1/jobs/later:getIamPolicy", nil, &pol); code != 200 || pol["etag"] == nil {
		t.Errorf("jobs getIamPolicy = %d %v", code, pol)
	}
	if code := v1Do(t, h, http.MethodGet, "/v1/projects/demo-project/locations/us-central1/jobs/nope:getIamPolicy", nil, nil); code != http.StatusNotFound {
		t.Errorf("getIamPolicy of a missing job = %d; want 404", code)
	}
}
