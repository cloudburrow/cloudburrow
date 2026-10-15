//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
)

// runV1Service creates a v1 service in us-central1 and waits for it.
func runV1Service(t *testing.T, c *runv1.APIService, project, id string) string {
	t.Helper()
	ns := "namespaces/" + project
	t.Cleanup(func() { _, _ = c.Namespaces.Services.Delete(ns + "/services/" + id).Do() })
	if _, err := c.Namespaces.Services.Create(ns, &runv1.Service{
		Metadata: &runv1.ObjectMeta{Name: id, Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
		Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
			Containers: []*runv1.Container{{Image: helloDigest}}}}}}).Do(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	runV1Ready(t, c, ns+"/services/"+id)
	return "projects/" + project + "/locations/us-central1/services/" + id
}

// covers: run.projects.locations.list, run.projects.locations.services.getIamPolicy, run.projects.locations.services.setIamPolicy, run.projects.locations.services.testIamPermissions
//
// TestRunV1LocationsAndIAM (#1349): the official v1 client lists the
// project's locations, which include us-central1, and stores, reads and
// tests a service's IAM policy, which is the service's v2 policy.
func TestRunV1LocationsAndIAM(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h)
	locs, err := c.Projects.Locations.List("projects/" + h.Project()).PageSize(500).Do()
	if err != nil {
		t.Fatalf("projects.locations.list: %v", err)
	}
	found := false
	for _, l := range locs.Locations {
		found = found || (l.LocationId == "us-central1" && l.Name == "projects/"+h.Project()+"/locations/us-central1")
	}
	if !found {
		t.Errorf("projects.locations.list = %+v; want us-central1", locs.Locations)
	}

	name := runV1Service(t, c, h.Project(), "v1-iam")
	p, err := c.Projects.Locations.Services.GetIamPolicy(name).Do()
	if err != nil || p.Etag == "" || len(p.Bindings) != 0 {
		t.Fatalf("getIamPolicy = %+v, %v; want an empty policy with an etag", p, err)
	}
	p.Bindings = []*runv1.Binding{{Role: "roles/run.invoker", Members: []string{"allUsers"}}}
	set, err := c.Projects.Locations.Services.SetIamPolicy(name, &runv1.SetIamPolicyRequest{Policy: p}).Do()
	if err != nil || set.Etag == p.Etag || len(set.Bindings) != 1 {
		t.Fatalf("setIamPolicy = %+v, %v", set, err)
	}
	if _, err := c.Projects.Locations.Services.SetIamPolicy(name, &runv1.SetIamPolicyRequest{Policy: p}).Do(); err == nil {
		t.Error("setIamPolicy with a stale etag succeeded")
	}
	got, err := c.Projects.Locations.Services.GetIamPolicy(name).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Role != "roles/run.invoker" {
		t.Errorf("getIamPolicy after set = %+v, %v", got, err)
	}
	tp, err := c.Projects.Locations.Services.TestIamPermissions(name, &runv1.TestIamPermissionsRequest{
		Permissions: []string{"run.services.get"}}).Do()
	if err != nil || len(tp.Permissions) != 1 {
		t.Errorf("testIamPermissions = %+v, %v", tp, err)
	}
}

// TestGcloudRunRegionsAndIAM (#1349): the pinned gcloud, through
// gcloud-setup alone, lists Cloud Run's regions (projects.locations.list)
// and adds, reads and removes an IAM binding on a service in a region
// (v1 getIamPolicy and setIamPolicy at the {region}- host).
func TestGcloudRunRegionsAndIAM(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	c := runV1Client(t, h)
	g := newGcloudSession(t, h)
	if out := g.must("run", "regions", "list", "--format=value(locationId)"); !strings.Contains(out, "us-central1") {
		t.Errorf("gcloud run regions list = %q", out)
	}
	runV1Service(t, c, g.project, "gcloud-iam")
	g.must("run", "services", "add-iam-policy-binding", "gcloud-iam", "--region=us-central1",
		"--member=allUsers", "--role=roles/run.invoker")
	out := g.must("run", "services", "get-iam-policy", "gcloud-iam", "--region=us-central1", "--format=value(bindings)")
	if !strings.Contains(out, "allUsers") || !strings.Contains(out, "roles/run.invoker") {
		t.Errorf("gcloud run services get-iam-policy = %q", out)
	}
	g.must("run", "services", "remove-iam-policy-binding", "gcloud-iam", "--region=us-central1",
		"--member=allUsers", "--role=roles/run.invoker")
	if out := g.must("run", "services", "get-iam-policy", "gcloud-iam", "--region=us-central1", "--format=value(bindings)"); strings.Contains(out, "allUsers") {
		t.Errorf("get-iam-policy after remove = %q", out)
	}
}

// regionalHost sends every request with Host {region}-run.localhost, as
// gcloud's regional calls arrive, while dialing the harness's address.
type regionalHost struct{ host string }

func (rh regionalHost) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = rh.host
	return http.DefaultTransport.RoundTrip(r)
}

// covers: run.namespaces.jobs.create, run.namespaces.jobs.get, run.namespaces.jobs.list, run.namespaces.jobs.delete
//
// TestRunV1Jobs (#1349): the official v1 client, at the us-central1
// regional host, creates a job in v1's shape, reads it with v1 and v2
// (one job), lists it, and deletes it.
func TestRunV1Jobs(t *testing.T) {
	h := New(t)
	c, err := runv1.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvRun)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{Transport: regionalHost{"us-central1-run.localhost"}}))
	if err != nil {
		t.Fatal(err)
	}
	ns := "namespaces/" + h.Project()
	id := "v1-job"
	t.Cleanup(func() { _, _ = c.Namespaces.Jobs.Delete(ns + "/jobs/" + id).Do() })
	in := &runv1.Job{ApiVersion: "run.googleapis.com/v1", Kind: "Job",
		Metadata: &runv1.ObjectMeta{Name: id, Labels: map[string]string{"team": "a"}},
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{Spec: &runv1.ExecutionSpec{TaskCount: 2,
			Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{MaxRetries: 1, TimeoutSeconds: 120,
				Containers: []*runv1.Container{{Image: helloDigest, Env: []*runv1.EnvVar{{Name: "A", Value: "b"}}}}}}}}}}
	created, err := c.Namespaces.Jobs.Create(ns, in).Do()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" || created.Metadata.Labels["team"] != "a" ||
		created.Spec.Template.Spec.TaskCount != 2 {
		t.Errorf("created = %+v %+v", created.Metadata, created.Spec.Template.Spec)
	}
	got, err := c.Namespaces.Jobs.Get(ns + "/jobs/" + id).Do()
	if err != nil || got.Spec.Template.Spec.Template.Spec.Containers[0].Env[0].Value != "b" ||
		got.Spec.Template.Spec.Template.Spec.MaxRetries != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	jc, _ := runJobClients(t, h)
	v2, err := jc.GetJob(h.Context(), &runpb.GetJobRequest{Name: "projects/" + h.Project() + "/locations/us-central1/jobs/" + id})
	if err != nil || v2.GetTemplate().GetTaskCount() != 2 {
		t.Errorf("v2 GetJob = %v, %v", v2, err)
	}
	list, err := c.Namespaces.Jobs.List(ns).Do()
	if err != nil || len(list.Items) == 0 {
		t.Fatalf("List: %v %v", list, err)
	}
	if _, err := c.Namespaces.Jobs.Delete(ns + "/jobs/" + id).Do(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var ge *googleapi.Error
	if _, err := c.Namespaces.Jobs.Get(ns + "/jobs/" + id).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("Get after delete = %v; want 404", err)
	}
}

// TestGcloudRunJobs (#1349): the pinned gcloud, through gcloud-setup alone,
// creates, lists, describes and deletes a job through the v1 API.
func TestGcloudRunJobs(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	id := "gcloud-v1-job"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "jobs", "delete", id, "--region=us-central1", "--quiet") })
	g.must("run", "jobs", "create", id, "--image="+helloDigest, "--region=us-central1", "--tasks=2", "--set-env-vars=A=b")
	if out := g.must("run", "jobs", "list", "--region=us-central1", "--format=value(metadata.name)"); !strings.Contains(out, id) {
		t.Errorf("gcloud run jobs list = %q", out)
	}
	if out := g.must("run", "jobs", "describe", id, "--region=us-central1", "--format=value(spec.template.spec.taskCount)"); strings.TrimSpace(out) != "2" {
		t.Errorf("gcloud run jobs describe = %q; want 2 tasks", out)
	}
	g.must("run", "jobs", "delete", id, "--region=us-central1", "--quiet")
}

// covers: run.namespaces.domainmappings.create, run.namespaces.domainmappings.get, run.namespaces.domainmappings.list, run.namespaces.domainmappings.delete, run.projects.locations.domainmappings.list
//
// TestRunV1DomainMappings (#1349): the official v1 client maps a domain to
// a service. The mapping is stored and reports the CNAME record Google
// documents for a subdomain; it never becomes Ready, as no DNS is served.
// It is listed both ways and deleted.
func TestRunV1DomainMappings(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h)
	runV1Service(t, c, h.Project(), "v1-domain")
	ns := "namespaces/" + h.Project()
	domain := "www.cloudburrow-test.example.com"
	t.Cleanup(func() { _, _ = c.Namespaces.Domainmappings.Delete(ns + "/domainmappings/" + domain).Do() })
	dm, err := c.Namespaces.Domainmappings.Create(ns, &runv1.DomainMapping{ApiVersion: "domains.cloudrun.com/v1", Kind: "DomainMapping",
		Metadata: &runv1.ObjectMeta{Name: domain, Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
		Spec:     &runv1.DomainMappingSpec{RouteName: "v1-domain"}}).Do()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rr := dm.Status.ResourceRecords; len(rr) != 1 || rr[0].Type != "CNAME" || rr[0].Rrdata != "ghs.googlehosted.com." || rr[0].Name != "www.cloudburrow-test" {
		t.Errorf("resourceRecords = %+v", dm.Status.ResourceRecords)
	}
	for _, cond := range dm.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == "True" {
			t.Errorf("mapping is Ready: %+v", cond)
		}
	}
	if got, err := c.Namespaces.Domainmappings.Get(ns + "/domainmappings/" + domain).Do(); err != nil || got.Spec.RouteName != "v1-domain" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if l, err := c.Namespaces.Domainmappings.List(ns).Do(); err != nil || len(l.Items) != 1 {
		t.Errorf("List = %+v, %v", l, err)
	}
	if l, err := c.Projects.Locations.Domainmappings.List("projects/" + h.Project() + "/locations/us-central1").Do(); err != nil || len(l.Items) != 1 {
		t.Errorf("projects.locations.domainmappings.list = %+v, %v", l, err)
	}
	if _, err := c.Namespaces.Domainmappings.Create(ns, &runv1.DomainMapping{
		Metadata: &runv1.ObjectMeta{Name: "api.cloudburrow-test.example.com", Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
		Spec:     &runv1.DomainMappingSpec{RouteName: "no-such-service"}}).Do(); err == nil {
		t.Error("a mapping to a missing service was accepted")
	}
	if _, err := c.Namespaces.Domainmappings.Delete(ns + "/domainmappings/" + domain).Do(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var ge *googleapi.Error
	if _, err := c.Namespaces.Domainmappings.Get(ns + "/domainmappings/" + domain).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("Get after delete = %v; want 404", err)
	}
}

// covers: run.projects.authorizeddomains.list
//
// TestGcloudRunDomainMappings (#1349): the pinned gcloud, through
// gcloud-setup alone, lists, describes and deletes a domain mapping in a
// region. Its create checks projects.authorizeddomains.list first, which is
// empty, as no domain is verified without Search Console, so gcloud refuses
// the domain with its own message; the mapping is made with the API.
func TestGcloudRunDomainMappings(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	c := runV1Client(t, h)
	g := newGcloudSession(t, h)
	runV1Service(t, c, g.project, "gcloud-domain")
	domain := "app.cloudburrow-gcloud.example.com"
	ns := "namespaces/" + g.project
	t.Cleanup(func() { _, _ = c.Namespaces.Domainmappings.Delete(ns + "/domainmappings/" + domain).Do() })
	if out, err := g.run(nil, "beta", "run", "domain-mappings", "create", "--service=gcloud-domain", "--domain="+domain, "--region=us-central1"); err == nil ||
		!strings.Contains(out, "does not appear to be verified") {
		t.Errorf("domain-mappings create = %v; want gcloud's not-verified refusal:\n%s", err, out)
	}
	if l, err := c.Projects.Authorizeddomains.List("projects/" + g.project).Do(); err != nil || len(l.Domains) != 0 {
		t.Errorf("authorizeddomains.list = %+v, %v; want none", l, err)
	}
	if _, err := c.Namespaces.Domainmappings.Create(ns, &runv1.DomainMapping{
		Metadata: &runv1.ObjectMeta{Name: domain, Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
		Spec:     &runv1.DomainMappingSpec{RouteName: "gcloud-domain"}}).Do(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out := g.must("beta", "run", "domain-mappings", "list", "--region=us-central1", "--format=value(metadata.name)"); !strings.Contains(out, domain) {
		t.Errorf("domain-mappings list = %q", out)
	}
	if out := g.must("beta", "run", "domain-mappings", "describe", "--domain="+domain, "--region=us-central1", "--format=value(spec.routeName,status.resourceRecords[0].rrdata)"); !strings.Contains(out, "gcloud-domain") || !strings.Contains(out, "ghs.googlehosted.com.") {
		t.Errorf("domain-mappings describe = %q", out)
	}
	g.must("beta", "run", "domain-mappings", "delete", "--domain="+domain, "--region=us-central1", "--quiet")
	if out := g.must("beta", "run", "domain-mappings", "list", "--region=us-central1", "--format=value(metadata.name)"); strings.Contains(out, domain) {
		t.Errorf("domain-mappings list after delete = %q", out)
	}
}
