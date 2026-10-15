package run

import (
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
