//go:build compat

package compat

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func sleeper(greeting string) []*runpb.Container {
	return []*runpb.Container{{Image: jobImage, Command: []string{"sh", "-c"},
		Args: []string{"echo $GREETING; exec sleep 3600"},
		Env:  []*runpb.EnvVar{{Name: "GREETING", Values: &runpb.EnvVar_Value{Value: greeting}}}}}
}

// waitCondition polls get until its terminal condition succeeds.
func waitCondition(t *testing.T, what string, get func() (*runpb.Condition, error)) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		c, err := get()
		if err != nil {
			t.Fatalf("reading %s: %v", what, err)
		}
		if c.GetState() == runpb.Condition_CONDITION_SUCCEEDED {
			return
		}
		if c.GetState() == runpb.Condition_CONDITION_FAILED || time.Now().After(deadline) {
			t.Fatalf("%s did not become ready: %v", what, c)
		}
		time.Sleep(2 * time.Second)
	}
}

// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool, google.cloud.run.v2.WorkerPools/ListWorkerPools, google.cloud.run.v2.WorkerPools/UpdateWorkerPool, google.cloud.run.v2.WorkerPools/DeleteWorkerPool, google.cloud.run.v2.WorkerPools/GetIamPolicy, google.cloud.run.v2.WorkerPools/SetIamPolicy, google.cloud.run.v2.WorkerPools/TestIamPermissions
//
// TestRunWorkerPoolLifecycle (#1300): through the official WorkerPoolsClient
// and RevisionsClient, a worker pool of two instances is created and becomes
// ready; it is read and listed; scaling it alone keeps its revision, and a
// changed template is a new revision, the old one retired; its revisions are
// listed and read; its IAM policy is stored; a port is refused; deleting it
// removes it.
func TestRunWorkerPoolLifecycle(t *testing.T) {
	h := New(t)
	ctx := runWait(t)
	c, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	rc, err := run.NewRevisionsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewRevisionsClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	id := "compat-pool"
	name := runParent(h) + "/workerPools/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: name}); err == nil {
			_, _ = waitLRO(ctx, op)
		}
	})

	two := int32(2)
	op, err := c.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: runParent(h), WorkerPoolId: id,
		WorkerPool: &runpb.WorkerPool{Labels: map[string]string{"team": "a"},
			Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper("one")},
			Scaling:  &runpb.WorkerPoolScaling{ManualInstanceCount: &two}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	created, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("waiting for CreateWorkerPool: %v", err)
	}
	if created.GetName() != name || created.GetGeneration() != 1 || created.GetLatestCreatedRevision() == "" {
		t.Fatalf("created = %v", created)
	}
	// The operation completes once the instances are ready (#1348).
	if st := created.GetTerminalCondition().GetState(); st != runpb.Condition_CONDITION_SUCCEEDED || created.GetReconciling() {
		t.Errorf("CreateWorkerPool's operation completed with the pool %v (reconciling %v); want ready", st, created.GetReconciling())
	}
	firstRev := created.GetLatestCreatedRevision()
	ready := func() {
		waitCondition(t, "worker pool", func() (*runpb.Condition, error) {
			wp, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
			return wp.GetTerminalCondition(), err
		})
	}
	ready()
	got, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil {
		t.Fatalf("GetWorkerPool: %v", err)
	}
	if got.GetScaling().GetManualInstanceCount() != 2 || got.GetLatestReadyRevision() != firstRev ||
		got.GetLabels()["team"] != "a" || got.GetEtag() == "" {
		t.Errorf("GetWorkerPool = %v", got)
	}
	it := c.ListWorkerPools(ctx, &runpb.ListWorkerPoolsRequest{Parent: runParent(h)})
	found := false
	for {
		wp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListWorkerPools: %v", err)
		}
		found = found || wp.GetName() == name
	}
	if !found {
		t.Errorf("ListWorkerPools did not list %s", name)
	}

	// Scaling alone keeps the revision.
	one := int32(1)
	scaled := proto.Clone(got).(*runpb.WorkerPool)
	scaled.Scaling = &runpb.WorkerPoolScaling{ManualInstanceCount: &one}
	uop, err := c.UpdateWorkerPool(ctx, &runpb.UpdateWorkerPoolRequest{WorkerPool: scaled})
	if err != nil {
		t.Fatalf("UpdateWorkerPool (scale): %v", err)
	}
	after, err := waitLRO(ctx, uop)
	if err != nil {
		t.Fatalf("waiting for UpdateWorkerPool: %v", err)
	}
	if after.GetLatestCreatedRevision() != firstRev || after.GetGeneration() != 2 || after.GetScaling().GetManualInstanceCount() != 1 {
		t.Errorf("after scaling = %v; want revision %s, generation 2, one instance", after, firstRev)
	}
	// A stale etag is ABORTED.
	if _, err := c.UpdateWorkerPool(ctx, &runpb.UpdateWorkerPoolRequest{WorkerPool: scaled}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateWorkerPool with a stale etag = %v, want ABORTED", err)
	}
	// A changed template is a new revision.
	changed := proto.Clone(after).(*runpb.WorkerPool)
	changed.Template.Containers = sleeper("two")
	uop, err = c.UpdateWorkerPool(ctx, &runpb.UpdateWorkerPoolRequest{WorkerPool: changed})
	if err != nil {
		t.Fatalf("UpdateWorkerPool (template): %v", err)
	}
	after, err = waitLRO(ctx, uop)
	if err != nil {
		t.Fatalf("waiting for UpdateWorkerPool: %v", err)
	}
	if after.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_SUCCEEDED || after.GetLatestReadyRevision() != after.GetLatestCreatedRevision() {
		t.Errorf("UpdateWorkerPool's operation completed before the new revision was ready: %v", after.GetTerminalCondition())
	}
	secondRev := after.GetLatestCreatedRevision()
	if secondRev == firstRev || secondRev == "" {
		t.Fatalf("a changed template kept revision %s", secondRev)
	}
	ready()

	var names []string
	rit := rc.ListRevisions(ctx, &runpb.ListRevisionsRequest{Parent: name})
	for {
		r, err := rit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListRevisions: %v", err)
		}
		names = append(names, r.GetName())
	}
	if len(names) != 2 || names[0] != secondRev || names[1] != firstRev {
		t.Errorf("ListRevisions = %v; want [%s %s], newest first", names, secondRev, firstRev)
	}
	rev, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: secondRev})
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	if rev.GetContainers()[0].GetEnv()[0].GetValue() != "two" {
		t.Errorf("GetRevision = %v; want the changed template", rev)
	}
	if _, err := rc.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{Name: secondRev}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteRevision of the running revision = %v, want FAILED_PRECONDITION", err)
	}

	checkRunIamPolicy(t, ctx, name, runParent(h)+"/workerPools/compat-pool-missing",
		func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return c.GetIamPolicy(ctx, r) },
		func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return c.SetIamPolicy(ctx, r) },
		func(r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
			return c.TestIamPermissions(ctx, r)
		})

	withPort := &runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{{
		Image: jobImage, Ports: []*runpb.ContainerPort{{ContainerPort: 8080}}}}}}
	if _, err := c.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: runParent(h), WorkerPoolId: "compat-pool-port",
		WorkerPool: withPort}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateWorkerPool with a port = %v, want UNIMPLEMENTED", err)
	}

	dop, err := c.DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteWorkerPool: %v", err)
	}
	if _, err := waitLRO(ctx, dop); err != nil {
		t.Fatalf("waiting for DeleteWorkerPool: %v", err)
	}
	if _, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWorkerPool after delete = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.run.v2.Instances/CreateInstance, google.cloud.run.v2.Instances/GetInstance, google.cloud.run.v2.Instances/ListInstances, google.cloud.run.v2.Instances/StopInstance, google.cloud.run.v2.Instances/StartInstance, google.cloud.run.v2.Instances/DeleteInstance
//
// TestRunInstanceLifecycle (#1299, #1348): through the official
// InstancesClient, a standalone instance is created and its operation
// completes once it is ready; it has a URL that serves its container; it is
// read and listed, stopped and started again; ingress other than ALL is
// refused; and it is deleted.
func TestRunInstanceLifecycle(t *testing.T) {
	h := New(t)
	ctx := runWait(t)
	c, err := run.NewInstancesClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewInstancesClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	id := "compat-instance"
	name := runParent(h) + "/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: name}); err == nil {
			_, _ = waitLRO(ctx, op)
		}
	})
	if os.Getenv(envKubeconfig) == "" {
		if cli := os.Getenv(EnvCLI); cli != "" {
			t.Setenv(envKubeconfig, filepath.Join(instanceDirFrom(t, cliArgs()), "kubeconfig"))
		}
	}
	op, err := c.CreateInstance(ctx, &runpb.CreateInstanceRequest{Parent: runParent(h), InstanceId: id,
		Instance: &runpb.Instance{Description: "one box", Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_ALL,
			Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest",
				Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "instance"}}}}}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	created, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("waiting for CreateInstance: %v", err)
	}
	if created.GetName() != name || created.GetDescription() != "one box" {
		t.Fatalf("created = %v", created)
	}
	if st := created.GetTerminalCondition().GetState(); st != runpb.Condition_CONDITION_SUCCEEDED || created.GetReconciling() {
		t.Errorf("CreateInstance's operation completed with the instance %v (reconciling %v); want ready", st, created.GetReconciling())
	}
	if len(created.GetUrls()) != 1 || created.GetIngress() != runpb.IngressTraffic_INGRESS_TRAFFIC_ALL {
		t.Fatalf("created urls = %v, ingress = %v; want one URL and INGRESS_TRAFFIC_ALL", created.GetUrls(), created.GetIngress())
	}
	if body, code := secretEnvGet(t, ingress(t), hostOf(t, created.GetUrls()[0])); code != http.StatusOK || !strings.Contains(body, "Hello instance") {
		t.Errorf("GET %s = %d %q; want Hello instance", created.GetUrls()[0], code, body)
	}
	ready := func() {
		waitCondition(t, "instance", func() (*runpb.Condition, error) {
			in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name})
			return in.GetTerminalCondition(), err
		})
	}
	ready()
	it := c.ListInstances(ctx, &runpb.ListInstancesRequest{Parent: runParent(h)})
	found := false
	for {
		in, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		found = found || in.GetName() == name
	}
	if !found {
		t.Errorf("ListInstances did not list %s", name)
	}
	sop, err := c.StopInstance(ctx, &runpb.StopInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if _, err := waitLRO(ctx, sop); err != nil {
		t.Fatalf("waiting for StopInstance: %v", err)
	}
	ready()
	stopped, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name})
	if err != nil || stopped.GetTerminalCondition().GetMessage() != "the instance is stopped" {
		t.Errorf("GetInstance after stop = %v, %v", stopped, err)
	}
	stop, err := c.StartInstance(ctx, &runpb.StartInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if _, err := waitLRO(ctx, stop); err != nil {
		t.Fatalf("waiting for StartInstance: %v", err)
	}
	ready()
	if _, err := c.CreateInstance(ctx, &runpb.CreateInstanceRequest{Parent: runParent(h), InstanceId: "compat-instance-ingress",
		Instance: &runpb.Instance{Containers: sleeper("x"), Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateInstance with internal-only ingress = %v, want UNIMPLEMENTED", err)
	}
	dop, err := c.DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := waitLRO(ctx, dop); err != nil {
		t.Fatalf("waiting for DeleteInstance: %v", err)
	}
	if _, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after delete = %v, want NOT_FOUND", err)
	}
}
