//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/googleapi"
	runv1 "google.golang.org/api/run/v1"
)

// v1Await polls get until its Ready condition is True.
func v1Await(t *testing.T, what string, get func() ([]*runv1.GoogleCloudRunV1Condition, error)) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		cs, err := get()
		if err == nil && v1Cond(cs, "Ready").Status == "True" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became Ready: %v, %+v", what, err, v1Cond(cs, "Ready"))
		}
		time.Sleep(2 * time.Second)
	}
}

func v1NotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusNotFound
}

// covers: run.namespaces.instances.create, run.namespaces.instances.get, run.namespaces.instances.list, run.namespaces.instances.replaceInstance, run.namespaces.instances.stop, run.namespaces.instances.start, run.namespaces.instances.delete, run.projects.locations.instances.setIamPolicy, run.projects.locations.instances.getIamPolicy, run.projects.locations.instances.testIamPermissions
//
// TestRunV1Instances (#1546): through the official v1 client at
// us-central1's regional host, an instance is created in v1's shape and
// becomes Ready; v2's Instances reads the same instance; list finds it by
// label; stop and start scale it; replaceInstance changes its labels and
// environment; its IAM policy is set and read back; delete removes it.
func TestRunV1Instances(t *testing.T) {
	h := New(t)
	c := runV1Regional(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	ic, err := run.NewInstancesClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.Close() })
	ns := "namespaces/" + h.Project()
	id := "v1-instance"
	name := ns + "/instances/" + id
	v2name := runParent(h) + "/instances/" + id
	t.Cleanup(func() { _, _ = c.Namespaces.Instances.Delete(name).Do() })
	in := &runv1.Instance{ApiVersion: "run.googleapis.com/v1", Kind: "Instance",
		Metadata: &runv1.ObjectMeta{Name: id, Labels: map[string]string{"team": "v1"}},
		Spec: &runv1.InstanceSpec{Containers: []*runv1.Container{{Image: "ghcr.io/knative/helloworld-go:latest",
			Env: []*runv1.EnvVar{{Name: "TARGET", Value: "v1"}}}}}}
	got, err := c.Namespaces.Instances.Create(ns, in).Do()
	if err != nil {
		t.Fatalf("instances.create: %v", err)
	}
	if got.Metadata.Name != id || got.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" || got.Kind != "Instance" {
		t.Errorf("instances.create = %+v", got.Metadata)
	}
	v1Await(t, "the v1 instance", func() ([]*runv1.GoogleCloudRunV1Condition, error) {
		g, err := c.Namespaces.Instances.Get(name).Do()
		if err != nil {
			return nil, err
		}
		return g.Status.Conditions, nil
	})
	v2, err := ic.GetInstance(ctx, &runpb.GetInstanceRequest{Name: v2name})
	if err != nil || v2.GetLabels()["team"] != "v1" || v2.GetContainers()[0].GetEnv()[0].GetValue() != "v1" {
		t.Fatalf("v2 GetInstance of the v1 instance = %v, %v", v2, err)
	}
	l, err := c.Namespaces.Instances.List(ns).LabelSelector("team=v1").Do()
	if err != nil || len(l.Items) != 1 || l.Items[0].Metadata.Name != id {
		t.Errorf("instances.list = %+v, %v", l, err)
	}
	if _, err := c.Namespaces.Instances.Stop(name, &runv1.StopInstanceRequest{}).Do(); err != nil {
		t.Fatalf("instances.stop: %v", err)
	}
	if _, err := c.Namespaces.Instances.Start(name, &runv1.StartInstanceRequest{}).Do(); err != nil {
		t.Fatalf("instances.start: %v", err)
	}
	cur, err := c.Namespaces.Instances.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	cur.Metadata.Labels["team"] = "v1-replaced"
	cur.Spec.Containers[0].Env = []*runv1.EnvVar{{Name: "TARGET", Value: "replaced"}}
	rep, err := c.Namespaces.Instances.ReplaceInstance(name, cur).Do()
	if err != nil || rep.Metadata.Labels["team"] != "v1-replaced" || rep.Metadata.Generation != cur.Metadata.Generation+1 {
		t.Fatalf("instances.replaceInstance = %+v, %v", rep, err)
	}
	if v2, err := ic.GetInstance(ctx, &runpb.GetInstanceRequest{Name: v2name}); err != nil || v2.GetContainers()[0].GetEnv()[0].GetValue() != "replaced" {
		t.Errorf("v2 GetInstance after replaceInstance = %v, %v", v2, err)
	}

	res := runParent(h) + "/instances/" + id
	pol, err := c.Projects.Locations.Instances.SetIamPolicy(res, &runv1.SetIamPolicyRequest{Policy: &runv1.Policy{
		Bindings: []*runv1.Binding{{Role: "roles/run.invoker", Members: []string{"user:dev@example.com"}}}}}).Do()
	if err != nil || pol.Etag == "" {
		t.Fatalf("instances.setIamPolicy = %+v, %v", pol, err)
	}
	if got, err := c.Projects.Locations.Instances.GetIamPolicy(res).Do(); err != nil || len(got.Bindings) != 1 || got.Bindings[0].Role != "roles/run.invoker" {
		t.Errorf("instances.getIamPolicy = %+v, %v", got, err)
	}
	if tp, err := c.Projects.Locations.Instances.TestIamPermissions(res, &runv1.TestIamPermissionsRequest{Permissions: []string{"run.instances.get"}}).Do(); err != nil || len(tp.Permissions) != 1 {
		t.Errorf("instances.testIamPermissions = %+v, %v", tp, err)
	}
	if _, err := c.Projects.Locations.Instances.GetIamPolicy(runParent(h) + "/instances/nope").Do(); !v1NotFound(err) {
		t.Errorf("getIamPolicy of a missing instance = %v; want 404", err)
	}

	if st, err := c.Namespaces.Instances.Delete(name).Do(); err != nil || st.Status != "Success" {
		t.Fatalf("instances.delete = %+v, %v", st, err)
	}
	if _, err := c.Namespaces.Instances.Get(name).Do(); !v1NotFound(err) {
		t.Errorf("instances.get after delete = %v; want 404", err)
	}
}

// covers: run.namespaces.workerpools.create, run.namespaces.workerpools.get, run.namespaces.workerpools.list, run.namespaces.workerpools.replaceWorkerPool, run.namespaces.workerpools.delete
//
// TestRunV1WorkerPools (#1546): through the official v1 client, a worker
// pool is created in v1's shape with one manual instance and becomes
// Ready; v2's WorkerPools reads it with that scaling and its revision;
// list finds it; replaceWorkerPool changes its labels and its instance
// count, and splits instances to the latest revision; delete removes it.
func TestRunV1WorkerPools(t *testing.T) {
	h := New(t)
	c := runV1Regional(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	pc, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ns := "namespaces/" + h.Project()
	id := "v1-pool"
	name := ns + "/workerpools/" + id
	t.Cleanup(func() { _, _ = c.Namespaces.Workerpools.Delete(name).Do() })
	wp := &runv1.WorkerPool{ApiVersion: "run.googleapis.com/v1", Kind: "WorkerPool",
		Metadata: &runv1.ObjectMeta{Name: id, Labels: map[string]string{"team": "v1"},
			Annotations: map[string]string{"run.googleapis.com/manualInstanceCount": "1"}},
		Spec: &runv1.WorkerPoolSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{Containers: []*runv1.Container{{
			Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{"exec sleep 3600"}}}}}}}
	got, err := c.Namespaces.Workerpools.Create(ns, wp).Do()
	if err != nil {
		t.Fatalf("workerpools.create: %v", err)
	}
	if got.Metadata.Annotations["run.googleapis.com/manualInstanceCount"] != "1" || got.Kind != "WorkerPool" {
		t.Errorf("workerpools.create = %+v", got.Metadata)
	}
	v1Await(t, "the v1 worker pool", func() ([]*runv1.GoogleCloudRunV1Condition, error) {
		g, err := c.Namespaces.Workerpools.Get(name).Do()
		if err != nil {
			return nil, err
		}
		return g.Status.Conditions, nil
	})
	v2, err := pc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: runParent(h) + "/workerPools/" + id})
	if err != nil || v2.GetScaling().GetManualInstanceCount() != 1 || v2.GetLabels()["team"] != "v1" || v2.GetLatestCreatedRevision() == "" {
		t.Fatalf("v2 GetWorkerPool of the v1 pool = %v, %v", v2, err)
	}
	if l, err := c.Namespaces.Workerpools.List(ns).Do(); err != nil || len(l.Items) == 0 {
		t.Errorf("workerpools.list = %+v, %v", l, err)
	}
	cur, err := c.Namespaces.Workerpools.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status.LatestCreatedRevisionName == "" {
		t.Errorf("workerpools.get status = %+v", cur.Status)
	}
	cur.Metadata.Labels["team"] = "v1-replaced"
	cur.Metadata.Annotations["run.googleapis.com/manualInstanceCount"] = "2"
	cur.Spec.InstanceSplits = []*runv1.InstanceSplit{{LatestRevision: true, Percent: 100}}
	rep, err := c.Namespaces.Workerpools.ReplaceWorkerPool(name, cur).Do()
	if err != nil || rep.Metadata.Labels["team"] != "v1-replaced" || rep.Metadata.Annotations["run.googleapis.com/manualInstanceCount"] != "2" {
		t.Fatalf("workerpools.replaceWorkerPool = %+v, %v", rep, err)
	}
	if len(rep.Spec.InstanceSplits) != 1 || !rep.Spec.InstanceSplits[0].LatestRevision || rep.Spec.InstanceSplits[0].Percent != 100 {
		t.Errorf("replaceWorkerPool instanceSplits = %+v", rep.Spec.InstanceSplits)
	}
	if st, err := c.Namespaces.Workerpools.Delete(name).Do(); err != nil || st.Status != "Success" {
		t.Fatalf("workerpools.delete = %+v, %v", st, err)
	}
	if _, err := c.Namespaces.Workerpools.Get(name).Do(); !v1NotFound(err) {
		t.Errorf("workerpools.get after delete = %v; want 404", err)
	}
}
