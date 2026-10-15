//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
)

// runV1Client is the official generated Cloud Run v1 client, at the
// adapter's port, which serves v1 beside v2 (#1293).
func runV1Client(t *testing.T, h *Harness) *runv1.APIService {
	t.Helper()
	c, err := runv1.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvRun)+"/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("run/v1 NewService: %v", err)
	}
	return c
}

// runV1Ready waits for a v1 service's Ready condition to be True.
func runV1Ready(t *testing.T, c *runv1.APIService, name string) *runv1.Service {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		svc, err := c.Namespaces.Services.Get(name).Do()
		if err == nil {
			for _, cond := range svc.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					return svc
				}
				if cond.Type == "Ready" && cond.Status == "False" {
					t.Fatalf("service %s failed: %s", name, cond.Message)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("service %s not ready: %v", name, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// covers: run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.list, run.namespaces.services.replaceService, run.namespaces.services.delete, run.namespaces.revisions.list, run.namespaces.revisions.get, run.namespaces.routes.list, run.namespaces.routes.get, run.namespaces.configurations.list, run.namespaces.configurations.get, run.projects.locations.services.list, run.projects.locations.services.get
//
// TestRunV1KnativeAPI (#1293): the official v1 client creates a service in
// Knative's shape, waits for Ready, reads it both ways (namespaces and
// projects.locations), replaces its template, which cuts a second revision,
// lists and reads the revisions, route and configuration, refuses a field
// v1 has and the adapter does not map, and deletes the service.
func TestRunV1KnativeAPI(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h)
	ns := "namespaces/" + h.Project()
	id := "v1-hello"
	name := ns + "/services/" + id
	t.Cleanup(func() { _, _ = c.Namespaces.Services.Delete(name).Do() })

	svc := &runv1.Service{ApiVersion: "serving.knative.dev/v1", Kind: "Service",
		Metadata: &runv1.ObjectMeta{Name: id, Namespace: h.Project(),
			Labels: map[string]string{"cloud.googleapis.com/location": "us-central1", "team": "a"}},
		Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
			Containers: []*runv1.Container{{Image: helloDigest, Env: []*runv1.EnvVar{{Name: "TARGET", Value: "first"}}}}}}}}
	if _, err := c.Namespaces.Services.Create(ns, svc).Do(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := runV1Ready(t, c, name)
	if got.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" || got.Metadata.Labels["team"] != "a" ||
		got.Status.Url == "" || got.Status.LatestReadyRevisionName == "" {
		t.Errorf("service = %+v / %+v", got.Metadata, got.Status)
	}
	first := got.Status.LatestReadyRevisionName

	list, err := c.Namespaces.Services.List(ns).Do()
	if err != nil || len(list.Items) == 0 {
		t.Fatalf("List: %v %v", list, err)
	}
	pl, err := c.Projects.Locations.Services.List("projects/" + h.Project() + "/locations/us-central1").Do()
	if err != nil || len(pl.Items) == 0 {
		t.Fatalf("projects.locations.services.list: %v %v", pl, err)
	}
	if _, err := c.Projects.Locations.Services.Get("projects/" + h.Project() + "/locations/us-central1/services/" + id).Do(); err != nil {
		t.Fatalf("projects.locations.services.get: %v", err)
	}

	// Replace: what the read returned, with a new env value.
	got.Spec.Template.Spec.Containers[0].Env = []*runv1.EnvVar{{Name: "TARGET", Value: "second"}}
	got.Status = nil
	if _, err := c.Namespaces.Services.ReplaceService(name, got).Do(); err != nil {
		t.Fatalf("ReplaceService: %v", err)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		s := runV1Ready(t, c, name)
		if s.Status.LatestReadyRevisionName != first && s.Status.ObservedGeneration >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no second revision ready: %+v", s.Status)
		}
		time.Sleep(2 * time.Second)
	}

	revs, err := c.Namespaces.Revisions.List(ns).LabelSelector("serving.knative.dev/service=" + id).Do()
	if err != nil || len(revs.Items) != 2 {
		t.Fatalf("Revisions.List: %v %v", revs, err)
	}
	if r, err := c.Namespaces.Revisions.Get(ns + "/revisions/" + first).Do(); err != nil || r.Metadata.Labels["serving.knative.dev/service"] != id {
		t.Errorf("Revisions.Get: %v %v", r, err)
	}
	if r, err := c.Namespaces.Routes.Get(ns + "/routes/" + id).Do(); err != nil || r.Status.Url == "" {
		t.Errorf("Routes.Get: %v %v", r, err)
	}
	if l, err := c.Namespaces.Routes.List(ns).Do(); err != nil || len(l.Items) == 0 {
		t.Errorf("Routes.List: %v %v", l, err)
	}
	if cfg, err := c.Namespaces.Configurations.Get(ns + "/configurations/" + id).Do(); err != nil || cfg.Status.LatestReadyRevisionName == "" {
		t.Errorf("Configurations.Get: %v %v", cfg, err)
	}
	if l, err := c.Namespaces.Configurations.List(ns).Do(); err != nil || len(l.Items) == 0 {
		t.Errorf("Configurations.List: %v %v", l, err)
	}

	// A field the adapter does not map is refused, not dropped.
	bad := *svc
	bad.Metadata = &runv1.ObjectMeta{Name: "v1-bad", Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}}
	bad.Spec = &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
		Volumes:    []*runv1.Volume{{Name: "v"}},
		Containers: []*runv1.Container{{Image: helloDigest}}}}}
	_, err = c.Namespaces.Services.Create(ns, &bad).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented || !strings.Contains(ge.Message, "volumes") {
		t.Errorf("Create with volumes = %v; want 501 naming volumes", err)
	}

	if _, err := c.Namespaces.Services.Delete(name).Do(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Namespaces.Services.Get(name).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("Get after delete = %v; want 404", err)
	}
}

// TestGcloudRunServicesList (#1293): the pinned gcloud lists and describes
// Cloud Run services through the v1 API. `gcloud run services list` with no
// --region is the global projects.locations call; describe is the regional
// namespaced one, at the {region}- host gcloud derives from the override.
func TestGcloudRunServicesList(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	c := runV1Client(t, h)
	// gcloud's project is the one gcloud-setup configures.
	g := newGcloudSession(t, h)
	ns := "namespaces/" + g.project
	id := "gcloud-v1"
	t.Cleanup(func() { _, _ = c.Namespaces.Services.Delete(ns + "/services/" + id).Do() })
	if _, err := c.Namespaces.Services.Create(ns, &runv1.Service{
		Metadata: &runv1.ObjectMeta{Name: id, Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
		Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
			Containers: []*runv1.Container{{Image: helloDigest}}}}}}).Do(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	runV1Ready(t, c, ns+"/services/"+id)
	out := g.must("run", "services", "list", "--format=value(metadata.name,region,status.url)")
	if !strings.Contains(out, id) || !strings.Contains(out, "us-central1") {
		t.Errorf("gcloud run services list = %q", out)
	}
	out = g.must("run", "services", "describe", id, "--region=us-central1", "--format=value(status.latestReadyRevisionName)")
	if !strings.HasPrefix(strings.TrimSpace(out), id+"-") {
		t.Errorf("gcloud run services describe = %q", out)
	}
	out = g.must("run", "revisions", "list", "--service="+id, "--region=us-central1", "--format=value(metadata.name)")
	if !strings.Contains(out, id+"-") {
		t.Errorf("gcloud run revisions list = %q", out)
	}
}
