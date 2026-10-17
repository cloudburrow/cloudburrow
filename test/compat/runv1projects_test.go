//go:build compat

package compat

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/googleapi"
	runv1 "google.golang.org/api/run/v1"
)

// covers: run.projects.locations.services.create, run.projects.locations.services.replaceService, run.projects.locations.services.delete, run.projects.locations.revisions.list, run.projects.locations.revisions.get, run.projects.locations.revisions.delete, run.namespaces.revisions.delete, run.projects.locations.routes.list, run.projects.locations.routes.get, run.projects.locations.configurations.list, run.projects.locations.configurations.get, run.projects.locations.domainmappings.create, run.projects.locations.domainmappings.get, run.projects.locations.domainmappings.delete, run.projects.locations.authorizeddomains.list, run.namespaces.authorizeddomains.list
//
// TestRunV1ProjectsLocationsAPI (#1533): the official v1 client's
// projects.locations form of the Knative API, the one the regional
// endpoint serves. A service is created at its location, replaced twice,
// which cuts three revisions; they are listed and read there, and the two
// that serve no traffic are deleted, one each way (namespaces and
// projects.locations), while deleting the serving one is refused. Its route
// and configuration are read and listed; a domain is mapped to it, read and
// removed; authorized domains are none locally, both ways; and the service
// is deleted.
func TestRunV1ProjectsLocationsAPI(t *testing.T) {
	h := New(t)
	c := runV1Client(t, h)
	parent := runParent(h)
	ns := "namespaces/" + h.Project()
	id := "v1-located"
	name := parent + "/services/" + id
	t.Cleanup(func() { _, _ = c.Projects.Locations.Services.Delete(name).Do() })

	if _, err := c.Projects.Locations.Services.Create(parent, &runv1.Service{ApiVersion: "serving.knative.dev/v1", Kind: "Service",
		Metadata: &runv1.ObjectMeta{Name: id, Namespace: h.Project()},
		Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
			Containers: []*runv1.Container{{Image: helloDigest, Env: []*runv1.EnvVar{{Name: "TARGET", Value: "1"}}}}}}}}).Do(); err != nil {
		t.Fatalf("projects.locations.services.create: %v", err)
	}
	got := runV1Ready(t, c, ns+"/services/"+id)
	if got.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" {
		t.Errorf("a service created at us-central1 has labels %v", got.Metadata.Labels)
	}
	revs := []string{got.Status.LatestReadyRevisionName}
	for _, target := range []string{"2", "3"} {
		got.Spec.Template.Spec.Containers[0].Env = []*runv1.EnvVar{{Name: "TARGET", Value: target}}
		got.Status = nil
		if _, err := c.Projects.Locations.Services.ReplaceService(name, got).Do(); err != nil {
			t.Fatalf("projects.locations.services.replaceService: %v", err)
		}
		deadline := time.Now().Add(4 * time.Minute)
		for {
			got = runV1Ready(t, c, ns+"/services/"+id)
			if !slices.Contains(revs, got.Status.LatestReadyRevisionName) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no new revision after replacing with TARGET=%s: %+v", target, got.Status)
			}
			time.Sleep(2 * time.Second)
		}
		revs = append(revs, got.Status.LatestReadyRevisionName)
	}

	l, err := c.Projects.Locations.Revisions.List(parent).LabelSelector("serving.knative.dev/service=" + id).Do()
	if err != nil || len(l.Items) != 3 {
		t.Fatalf("projects.locations.revisions.list = %v, %v; want 3", l, err)
	}
	if r, err := c.Projects.Locations.Revisions.Get(parent + "/revisions/" + revs[2]).Do(); err != nil ||
		r.Metadata.Labels["serving.knative.dev/service"] != id || r.Metadata.Name != revs[2] {
		t.Errorf("projects.locations.revisions.get = %+v, %v", r, err)
	}
	var ge *googleapi.Error
	if _, err := c.Projects.Locations.Revisions.Delete(parent + "/revisions/" + revs[2]).Do(); !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Errorf("deleting the serving revision = %v; want 400 (FAILED_PRECONDITION)", err)
	}
	if _, err := c.Namespaces.Revisions.Delete(ns + "/revisions/" + revs[0]).Do(); err != nil {
		t.Errorf("namespaces.revisions.delete: %v", err)
	}
	if _, err := c.Projects.Locations.Revisions.Delete(parent + "/revisions/" + revs[1]).Do(); err != nil {
		t.Errorf("projects.locations.revisions.delete: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		l, err = c.Projects.Locations.Revisions.List(parent).LabelSelector("serving.knative.dev/service=" + id).Do()
		if err == nil && len(l.Items) == 1 && l.Items[0].Metadata.Name == revs[2] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("revisions after deleting two = %v, %v; want only %s", l, err, revs[2])
		}
		time.Sleep(2 * time.Second)
	}

	if r, err := c.Projects.Locations.Routes.Get(parent + "/routes/" + id).Do(); err != nil || r.Status.Url == "" || r.Kind != "Route" {
		t.Errorf("projects.locations.routes.get = %+v, %v", r, err)
	}
	if rl, err := c.Projects.Locations.Routes.List(parent).Do(); err != nil || !slices.ContainsFunc(rl.Items, func(r *runv1.Route) bool { return r.Metadata.Name == id }) {
		t.Errorf("projects.locations.routes.list = %+v, %v", rl, err)
	}
	if cfg, err := c.Projects.Locations.Configurations.Get(parent + "/configurations/" + id).Do(); err != nil || cfg.Status.LatestReadyRevisionName != revs[2] {
		t.Errorf("projects.locations.configurations.get = %+v, %v", cfg, err)
	}
	if cl, err := c.Projects.Locations.Configurations.List(parent).Do(); err != nil || !slices.ContainsFunc(cl.Items, func(c *runv1.Configuration) bool { return c.Metadata.Name == id }) {
		t.Errorf("projects.locations.configurations.list = %+v, %v", cl, err)
	}

	domain := "located.cloudburrow-test.example.com"
	t.Cleanup(func() { _, _ = c.Projects.Locations.Domainmappings.Delete(parent + "/domainmappings/" + domain).Do() })
	dm, err := c.Projects.Locations.Domainmappings.Create(parent, &runv1.DomainMapping{ApiVersion: "domains.cloudrun.com/v1", Kind: "DomainMapping",
		Metadata: &runv1.ObjectMeta{Name: domain, Namespace: h.Project()},
		Spec:     &runv1.DomainMappingSpec{RouteName: id, CertificateMode: "NONE"}}).Do()
	if err != nil {
		t.Fatalf("projects.locations.domainmappings.create: %v", err)
	}
	if dm.Metadata.Name != domain || dm.Spec.RouteName != id {
		t.Errorf("created mapping = %+v %+v", dm.Metadata, dm.Spec)
	}
	if got, err := c.Projects.Locations.Domainmappings.Get(parent + "/domainmappings/" + domain).Do(); err != nil || got.Spec.RouteName != id ||
		got.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" {
		t.Errorf("projects.locations.domainmappings.get = %+v, %v", got, err)
	}
	if _, err := c.Projects.Locations.Domainmappings.Delete(parent + "/domainmappings/" + domain).Do(); err != nil {
		t.Errorf("projects.locations.domainmappings.delete: %v", err)
	}
	if _, err := c.Projects.Locations.Domainmappings.Get(parent + "/domainmappings/" + domain).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("get after delete = %v; want 404", err)
	}

	// No domain is verified locally (no Search Console), so none is
	// authorized, at a location or in the namespace.
	if d, err := c.Projects.Locations.Authorizeddomains.List(parent).Do(); err != nil || len(d.Domains) != 0 {
		t.Errorf("projects.locations.authorizeddomains.list = %+v, %v", d, err)
	}
	if d, err := c.Namespaces.Authorizeddomains.List(ns).Do(); err != nil || len(d.Domains) != 0 {
		t.Errorf("namespaces.authorizeddomains.list = %+v, %v", d, err)
	}

	if _, err := c.Projects.Locations.Services.Delete(name).Do(); err != nil {
		t.Fatalf("projects.locations.services.delete: %v", err)
	}
	if _, err := c.Projects.Locations.Services.Get(name).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("get after delete = %v; want 404", err)
	}
}

// covers: run.projects.locations.operations.list, run.projects.locations.operations.get, run.projects.locations.operations.wait, run.projects.locations.operations.delete, run.projects.locations.workerpools.getIamPolicy, run.projects.locations.workerpools.setIamPolicy, run.projects.locations.workerpools.testIamPermissions
//
// TestRunV1OperationsAndWorkerPoolIAM (#1533): the operation v2's
// CreateWorkerPool returns is v1's too. The official v1 client waits on it,
// reads it, finds it listed at its location and deletes it; a missing
// operation is NOT_FOUND. The pool's v1 IAM methods are its v2 policy: a
// binding v1 sets, v2's GetIamPolicy reads.
func TestRunV1OperationsAndWorkerPoolIAM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := runV1Client(t, h)
	wp, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = wp.Close() })
	parent := runParent(h)
	id := "v1-ops-pool"
	name := parent + "/workerPools/" + id
	t.Cleanup(func() {
		if op, err := wp.DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: name}); err == nil {
			_, _ = op.Wait(ctx)
		}
	})
	one := int32(1)
	op, err := wp.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: id,
		WorkerPool: &runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper("v1")},
			Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &one}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	opName := op.Name()
	if !strings.HasPrefix(opName, parent+"/operations/") {
		t.Fatalf("operation %q is not under %s/operations", opName, parent)
	}

	deadline := time.Now().Add(5 * time.Minute)
	var got *runv1.GoogleLongrunningOperation
	for {
		got, err = c.Projects.Locations.Operations.Wait(opName, &runv1.GoogleLongrunningWaitOperationRequest{Timeout: "30s"}).Do()
		if err != nil {
			t.Fatalf("operations.wait: %v", err)
		}
		if got.Done || time.Now().After(deadline) {
			break
		}
	}
	if !got.Done || got.Error != nil || got.Name != opName || len(got.Response) == 0 {
		t.Fatalf("operations.wait = %+v; want the operation done with the pool", got)
	}
	if g, err := c.Projects.Locations.Operations.Get(opName).Do(); err != nil || !g.Done || g.Name != opName {
		t.Errorf("operations.get = %+v, %v", g, err)
	}
	l, err := c.Projects.Locations.Operations.List(parent).Do()
	if err != nil || !slices.ContainsFunc(l.Operations, func(o *runv1.GoogleLongrunningOperation) bool { return o.Name == opName }) {
		t.Errorf("operations.list = %+v, %v; want %s listed", l, err, opName)
	}
	if _, err := c.Projects.Locations.Operations.Delete(opName).Do(); err != nil {
		t.Errorf("operations.delete: %v", err)
	}
	var ge *googleapi.Error
	if _, err := c.Projects.Locations.Operations.Get(parent + "/operations/none").Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("operations.get of a missing operation = %v; want 404", err)
	}

	resource := parent + "/workerpools/" + id
	pol, err := c.Projects.Locations.Workerpools.GetIamPolicy(resource).Do()
	if err != nil || pol.Etag == "" {
		t.Fatalf("workerpools.getIamPolicy = %+v, %v", pol, err)
	}
	member := "user:dev@example.com"
	if _, err := c.Projects.Locations.Workerpools.SetIamPolicy(resource, &runv1.SetIamPolicyRequest{Policy: &runv1.Policy{Etag: pol.Etag,
		Bindings: []*runv1.Binding{{Role: "roles/run.developer", Members: []string{member}}}}}).Do(); err != nil {
		t.Fatalf("workerpools.setIamPolicy: %v", err)
	}
	v2, err := wp.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(v2.GetBindings()) != 1 || v2.GetBindings()[0].GetRole() != "roles/run.developer" || !slices.Contains(v2.GetBindings()[0].GetMembers(), member) {
		t.Errorf("v2 GetIamPolicy after v1's setIamPolicy = %v, %v", v2, err)
	}
	tp, err := c.Projects.Locations.Workerpools.TestIamPermissions(resource, &runv1.TestIamPermissionsRequest{Permissions: []string{"run.workerpools.get"}}).Do()
	if err != nil || !slices.Equal(tp.Permissions, []string{"run.workerpools.get"}) {
		t.Errorf("workerpools.testIamPermissions = %+v, %v", tp, err)
	}
	if _, err := c.Projects.Locations.Workerpools.GetIamPolicy(parent + "/workerpools/none").Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("workerpools.getIamPolicy of a missing pool = %v; want 404", err)
	}
}
