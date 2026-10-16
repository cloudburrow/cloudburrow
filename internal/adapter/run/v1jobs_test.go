package run

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A v1 job converts to v2's shape and back, and a field the service
// mapping refuses is refused here too.
func TestV1JobRoundTrip(t *testing.T) {
	in := &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "j", Labels: map[string]string{v1LocationLabel: "us-central1", "team": "a"},
		Annotations: map[string]string{v1ClientName: "gcloud"}},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 2, Parallelism: 1,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{MaxRetries: 1, TimeoutSeconds: 60,
				Containers: []*runv1.Container{{Image: "a:1", Env: []*runv1.EnvVar{{Name: "A", Value: "b"}}}}}}}}}}
	job, err := fromV1Job(in)
	if err != nil {
		t.Fatal(err)
	}
	if job.GetLabels()["team"] != "a" || job.GetLabels()[v1LocationLabel] != "" || job.GetClient() != "gcloud" ||
		job.GetTemplate().GetTaskCount() != 2 || job.GetTemplate().GetTemplate().GetMaxRetries() != 1 ||
		job.GetTemplate().GetTemplate().GetTimeout().GetSeconds() != 60 || job.GetTemplate().GetTemplate().GetContainers()[0].GetImage() != "a:1" {
		t.Fatalf("fromV1Job = %v", job)
	}
	job.Name = "projects/demo-project/locations/us-central1/jobs/j"
	out := toV1Job(job)
	if out.Metadata.Name != "j" || out.Metadata.Namespace != "demo-project" || out.Metadata.Labels[v1LocationLabel] != "us-central1" ||
		out.Spec.Template.Spec.TaskCount != 2 || out.Spec.Template.Spec.Template.Spec.MaxRetries != 1 ||
		out.Spec.Template.Spec.Template.Spec.Containers[0].Env[0].Value != "b" {
		t.Errorf("toV1Job = %+v", out)
	}
	in.Spec.Template.Spec.Template.Spec.Containers[0].ReadinessProbe = &runv1.Probe{}
	if _, err := fromV1Job(in); status.Code(err) != codes.Unimplemented {
		t.Errorf("readinessProbe = %v; want Unimplemented", err)
	}
}

// A subdomain maps by CNAME to ghs.googlehosted.com., an apex domain by
// Google's four A and four AAAA records.
func TestDomainRecords(t *testing.T) {
	if rr := domainRecords("www.example.com"); len(rr) != 1 || rr[0].Type != "CNAME" || rr[0].Name != "www" || rr[0].Rrdata != ghsCNAME {
		t.Errorf("subdomain records = %+v", rr)
	}
	if rr := domainRecords("example.com"); len(rr) != 8 || rr[0].Type != "A" || rr[7].Type != "AAAA" {
		t.Errorf("apex records = %+v", rr)
	}
}

// At the global endpoint (no region), namespaces.jobs.list is every
// region's jobs, each with its location label, which is how gcloud run jobs
// describe with no --region finds a job's region (#1450).
func TestV1ListJobsWithoutARegion(t *testing.T) {
	js, _, _, _ := jobsServer(fakeSecrets{})
	h := js.s.V1Handler(http.NotFoundHandler())
	const ns = "/apis/run.googleapis.com/v1/namespaces/demo-project"
	for _, loc := range []string{"us-central1", "europe-west1"} {
		job := &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "j-" + loc, Labels: map[string]string{v1LocationLabel: loc}},
			Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 1,
				Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{Containers: []*runv1.Container{{Image: "busybox:1.36"}}}}}}}}
		raw, _ := json.Marshal(job)
		r := httptest.NewRequest(http.MethodPost, "http://run.localhost"+ns+"/jobs", bytes.NewReader(raw))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("create in %s = %d %s", loc, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://run.localhost"+ns+"/jobs", nil))
	var list runv1.ListJobsResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("global jobs.list = %d %s", w.Code, w.Body)
	}
	got := map[string]string{}
	for _, j := range list.Items {
		got[j.Metadata.Name] = j.Metadata.Labels[v1LocationLabel]
	}
	if len(got) != 2 || got["j-us-central1"] != "us-central1" || got["j-europe-west1"] != "europe-west1" {
		t.Errorf("global jobs.list = %v; want both regions' jobs with their location", got)
	}
	// At a regional host, the region's alone.
	var regional runv1.ListJobsResponse
	if code := v1Do(t, h, http.MethodGet, ns+"/jobs", nil, &regional); code != 200 || len(regional.Items) != 1 {
		t.Errorf("us-central1 jobs.list = %d %d items; want 1", code, len(regional.Items))
	}
}

// gcloud beta and alpha send run.googleapis.com/launch-stage on a job; it is
// v2's launchStage, and reads back as the annotation (#1450).
func TestV1JobLaunchStage(t *testing.T) {
	job := func(stage string) *runv1.Job {
		return &runv1.Job{Metadata: &runv1.ObjectMeta{Name: "j", Annotations: map[string]string{v1LaunchStage: stage}},
			Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{
				Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{Containers: []*runv1.Container{{Image: "a:1"}}}}}}}}
	}
	js, _, _, _ := jobsServer(fakeSecrets{})
	h := js.s.V1Handler(http.NotFoundHandler())
	var got runv1.Job
	if code := v1Do(t, h, http.MethodPost, "/apis/run.googleapis.com/v1/namespaces/demo-project/jobs", job("BETA"), &got); code != 200 ||
		got.Metadata.Annotations[v1LaunchStage] != "BETA" {
		t.Errorf("create with launch-stage BETA = %d %v", code, got.Metadata.Annotations)
	}
	if _, err := fromV1Job(job("SOON")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("launch-stage SOON = %v; want INVALID_ARGUMENT", err)
	}
}
