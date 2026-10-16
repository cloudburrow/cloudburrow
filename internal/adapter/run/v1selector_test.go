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

// A run's overrides.delayExecution starts the execution at once (#1420),
// a job's own delayExecution is refused naming it, and the job's IAM
// policy is served at projects.locations.jobs.
func TestV1DelayExecution(t *testing.T) {
	js, _, _, _ := jobsServer(fakeSecrets{})
	h := js.s.V1Handler(http.NotFoundHandler())
	const ns = "/apis/run.googleapis.com/v1/namespaces/demo-project"
	job := &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "later"},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 1, DelayExecution: true,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{
				Containers: []*runv1.Container{{Image: "busybox:1.36"}}}}}}}}
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs", job, nil); code != http.StatusNotImplemented {
		t.Errorf("create with spec.template.spec.delayExecution = %d; want 501", code)
	}
	job.Spec.Template.Spec.DelayExecution = false
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs", job, nil); code != 200 {
		t.Fatalf("create = %d", code)
	}
	var exec runv1.Execution
	if code := v1Do(t, h, http.MethodPost, ns+"/jobs/later:run", &runv1.RunJobRequest{Overrides: &runv1.Overrides{DelayExecution: true, TaskCount: 1}}, &exec); code != 200 ||
		exec.Metadata.Labels[v1JobLabel] != "later" {
		t.Fatalf("run with overrides.delayExecution = %d %+v", code, exec.Metadata)
	}
	var list runv1.ListExecutionsResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/executions?labelSelector="+url.QueryEscape("run.googleapis.com/job in (later)"), nil, &list); code != 200 || len(list.Items) != 1 {
		t.Errorf("executions.list = %d %d", code, len(list.Items))
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
