//go:build compat

package compat

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool, google.cloud.run.v2.WorkerPools/ListWorkerPools, google.cloud.run.v2.WorkerPools/DeleteWorkerPool, google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/GetJob, google.cloud.run.v2.Jobs/ListJobs, google.cloud.run.v2.Jobs/RunJob, google.cloud.run.v2.Jobs/DeleteJob, google.cloud.run.v2.Executions/ListExecutions, google.cloud.run.v2.Instances/CreateInstance, google.cloud.run.v2.Instances/GetInstance, google.cloud.run.v2.Instances/ListInstances, google.cloud.run.v2.Instances/DeleteInstance, google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Services/ListServices, google.cloud.run.v2.Services/DeleteService, google.cloud.run.v2.Revisions/GetRevision
//
// TestRunNamesPerProject (#1399): through the official clients, two projects
// each create a worker pool, a job and an instance of one ID. They are two
// of each: each reads back under its own name with its own labels, lists
// only in its own project, runs its own pods (each instance answers at its
// own URL with its own greeting), and counts only its own executions.
// Deleting one project's leaves the other's ready.
//
// Each also creates a service of the ID (#1400): each serves its own
// response at its own URL, its revision reads back as <id>-00001-<suffix>,
// it lists only in its own project, and deleting one leaves the other
// serving.
func TestRunNamesPerProject(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts := runClientOptions(h)
	pc, err := run.NewWorkerPoolsClient(ctx, opts...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ic, err := run.NewInstancesClient(ctx, opts...)
	if err != nil {
		t.Fatalf("run.NewInstancesClient: %v", err)
	}
	t.Cleanup(func() { _ = ic.Close() })
	jc, xc := runJobClients(t, h)
	sc := runClient(t, h)
	rc, err := run.NewRevisionsClient(ctx, opts...)
	if err != nil {
		t.Fatalf("run.NewRevisionsClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	if os.Getenv(envKubeconfig) == "" {
		if cli := os.Getenv(EnvCLI); cli != "" {
			t.Setenv(envKubeconfig, filepath.Join(instanceDirFrom(t, cliArgs()), "kubeconfig"))
		}
	}

	const id = "compat-same"
	parents := map[string]string{"a": runParent(h), "b": "projects/compat-names-b/locations/us-central1"}
	one := int32(1)
	for _, side := range []string{"a", "b"} {
		parent := parents[side]
		pool, inst, job := parent+"/workerPools/"+id, parent+"/instances/"+id, parent+"/jobs/"+id
		t.Cleanup(func() {
			if op, err := pc.DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: pool}); err == nil {
				_, _ = op.Wait(ctx)
			}
			if op, err := ic.DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: inst}); err == nil {
				_, _ = op.Wait(ctx)
			}
			_, _ = jc.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: job})
		})
		pop, err := pc.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: id,
			WorkerPool: &runpb.WorkerPool{Labels: map[string]string{"side": side},
				Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper(side)},
				Scaling:  &runpb.WorkerPoolScaling{ManualInstanceCount: &one}}})
		if err != nil {
			t.Fatalf("CreateWorkerPool in %s: %v", parent, err)
		}
		if _, err := pop.Wait(ctx); err != nil {
			t.Fatalf("waiting for CreateWorkerPool in %s: %v", parent, err)
		}
		iop, err := ic.CreateInstance(ctx, &runpb.CreateInstanceRequest{Parent: parent, InstanceId: id,
			Instance: &runpb.Instance{Labels: map[string]string{"side": side}, Ingress: runpb.IngressTraffic_INGRESS_TRAFFIC_ALL,
				Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest",
					Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "side " + side}}}}}}})
		if err != nil {
			t.Fatalf("CreateInstance in %s: %v", parent, err)
		}
		if _, err := iop.Wait(ctx); err != nil {
			t.Fatalf("waiting for CreateInstance in %s: %v", parent, err)
		}
		jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: id,
			Job: shellJob("echo "+side, 1, nil)})
		if err != nil {
			t.Fatalf("CreateJob in %s: %v", parent, err)
		}
		if _, err := jop.Wait(ctx); err != nil {
			t.Fatalf("waiting for CreateJob in %s: %v", parent, err)
		}
	}
	// Only project a's job runs.
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: parents["a"] + "/jobs/" + id})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("waiting for RunJob: %v", err)
	}

	// check reads each project's resources back as its own.
	check := func(side string, executions int) {
		t.Helper()
		parent := parents[side]
		wp, err := pc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: parent + "/workerPools/" + id})
		if err != nil || wp.GetName() != parent+"/workerPools/"+id || wp.GetLabels()["side"] != side {
			t.Errorf("GetWorkerPool in %s = %v, %v; want its own", parent, wp, err)
		}
		waitCondition(t, "worker pool in "+parent, func() (*runpb.Condition, error) {
			wp, err := pc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: parent + "/workerPools/" + id})
			return wp.GetTerminalCondition(), err
		})
		in, err := ic.GetInstance(ctx, &runpb.GetInstanceRequest{Name: parent + "/instances/" + id})
		if err != nil || in.GetName() != parent+"/instances/"+id || in.GetLabels()["side"] != side || len(in.GetUrls()) != 1 {
			t.Fatalf("GetInstance in %s = %v, %v; want its own, with one URL", parent, in, err)
		}
		if body, code := secretEnvGet(t, ingress(t), hostOf(t, in.GetUrls()[0])); code != http.StatusOK || !strings.Contains(body, "Hello side "+side) {
			t.Errorf("GET %s = %d %q; want Hello side %s", in.GetUrls()[0], code, body, side)
		}
		job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: parent + "/jobs/" + id})
		if err != nil || job.GetName() != parent+"/jobs/"+id || int(job.GetExecutionCount()) != executions {
			t.Errorf("GetJob in %s = %v, %v; want its own, with %d executions", parent, job, err, executions)
		}
		n := 0
		eit := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: parent + "/jobs/" + id})
		for {
			e, err := eit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListExecutions in %s: %v", parent, err)
			}
			if !strings.HasPrefix(e.GetName(), parent+"/jobs/"+id+"/executions/") {
				t.Errorf("ListExecutions in %s listed %s", parent, e.GetName())
			}
			n++
		}
		if n != executions {
			t.Errorf("ListExecutions in %s = %d, want %d", parent, n, executions)
		}
		var pools, instances, jobs []string
		for wit := pc.ListWorkerPools(ctx, &runpb.ListWorkerPoolsRequest{Parent: parent}); ; {
			x, err := wit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListWorkerPools: %v", err)
			}
			pools = append(pools, x.GetName())
		}
		for iit := ic.ListInstances(ctx, &runpb.ListInstancesRequest{Parent: parent}); ; {
			x, err := iit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListInstances: %v", err)
			}
			instances = append(instances, x.GetName())
		}
		for jit := jc.ListJobs(ctx, &runpb.ListJobsRequest{Parent: parent}); ; {
			x, err := jit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			jobs = append(jobs, x.GetName())
		}
		for what, got := range map[string][]string{"workerPools": pools, "instances": instances, "jobs": jobs} {
			found := false
			for _, g := range got {
				if !strings.HasPrefix(g, parent+"/") {
					t.Errorf("List %s in %s listed %s", what, parent, g)
				}
				found = found || g == parent+"/"+what+"/"+id
			}
			if !found {
				t.Errorf("List %s in %s = %v; want its %s", what, parent, got, id)
			}
		}
	}
	check("a", 1)
	check("b", 0)

	// A service of the ID in each project (#1400): two services, each
	// serving its own response at its own URL, with revisions named as
	// Cloud Run names them, each listed only in its own project.
	svcs := map[string]*runpb.Service{}
	for _, side := range []string{"a", "b"} {
		name := parents[side] + "/services/" + id
		sop, err := sc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parents[side], ServiceId: id,
			Service: &runpb.Service{Labels: map[string]string{"side": side}, Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
				Image: "ghcr.io/knative/helloworld-go:latest",
				Env:   []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "service " + side}}}}}}}})
		if err != nil {
			t.Fatalf("CreateService in %s: %v", parents[side], err)
		}
		t.Cleanup(func() {
			if op, err := sc.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name}); err == nil {
				_, _ = op.Wait(ctx)
			}
		})
		if svcs[side], err = sop.Wait(ctx); err != nil {
			t.Fatalf("waiting for CreateService in %s: %v", parents[side], err)
		}
	}
	if svcs["a"].GetUri() == "" || svcs["a"].GetUri() == svcs["b"].GetUri() {
		t.Errorf("the two services' URLs are %q and %q; want one each", svcs["a"].GetUri(), svcs["b"].GetUri())
	}
	revRE := regexp.MustCompile("^" + id + `-00001-[a-z0-9]{3}$`)
	checkService := func(side string) {
		t.Helper()
		name := parents[side] + "/services/" + id
		got, err := sc.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil || got.GetName() != name || got.GetLabels()["side"] != side || !revRE.MatchString(got.GetLatestReadyRevision()) {
			t.Fatalf("GetService in %s = %v, %v; want its own, revision %s-00001-xxx", parents[side], got, err, id)
		}
		if body, code := secretEnvGet(t, ingress(t), hostOf(t, got.GetUri())); code != http.StatusOK || !strings.Contains(body, "Hello service "+side) {
			t.Errorf("GET %s = %d %q; want Hello service %s", got.GetUri(), code, body, side)
		}
		rev, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: name + "/revisions/" + got.GetLatestReadyRevision()})
		if err != nil || rev.GetService() != name || rev.GetGeneration() != 1 {
			t.Errorf("GetRevision %s = %v, %v", got.GetLatestReadyRevision(), rev, err)
		}
		var listed []string
		for sit := sc.ListServices(ctx, &runpb.ListServicesRequest{Parent: parents[side]}); ; {
			x, err := sit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListServices: %v", err)
			}
			listed = append(listed, x.GetName())
		}
		if !slices.Contains(listed, name) || slices.Contains(listed, parents[otherSide(side)]+"/services/"+id) {
			t.Errorf("ListServices in %s = %v; want its own service only", parents[side], listed)
		}
	}
	checkService("a")
	checkService("b")
	if _, err := sc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parents["b"], ServiceId: id,
		Service: svcs["b"]}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateService in b = %v; want AlreadyExists", err)
	}
	// Deleting a's leaves b's, serving.
	dsop, err := sc.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: parents["a"] + "/services/" + id})
	if err != nil {
		t.Fatalf("DeleteService in a: %v", err)
	}
	if _, err := dsop.Wait(ctx); err != nil {
		t.Fatalf("waiting for DeleteService in a: %v", err)
	}
	if _, err := sc.GetService(ctx, &runpb.GetServiceRequest{Name: parents["a"] + "/services/" + id}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService of a's deleted service = %v; want NotFound", err)
	}
	checkService("b")

	// Deleting project a's leaves project b's, ready and serving.
	dop, err := pc.DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: parents["a"] + "/workerPools/" + id})
	if err != nil {
		t.Fatalf("DeleteWorkerPool: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("waiting for DeleteWorkerPool: %v", err)
	}
	iop, err := ic.DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: parents["a"] + "/instances/" + id})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := iop.Wait(ctx); err != nil {
		t.Fatalf("waiting for DeleteInstance: %v", err)
	}
	djop, err := jc.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: parents["a"] + "/jobs/" + id})
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := djop.Wait(ctx); err != nil {
		t.Fatalf("waiting for DeleteJob: %v", err)
	}
	for _, name := range []string{"/workerPools/", "/instances/", "/jobs/"} {
		var err error
		switch name {
		case "/workerPools/":
			_, err = pc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: parents["a"] + name + id})
		case "/instances/":
			_, err = ic.GetInstance(ctx, &runpb.GetInstanceRequest{Name: parents["a"] + name + id})
		case "/jobs/":
			_, err = jc.GetJob(ctx, &runpb.GetJobRequest{Name: parents["a"] + name + id})
		}
		if status.Code(err) != codes.NotFound {
			t.Errorf("Get of a's deleted %s = %v; want NotFound", name, err)
		}
	}
	check("b", 0)
}

// otherSide is the other of TestRunNamesPerProject's two projects.
func otherSide(side string) string {
	if side == "a" {
		return "b"
	}
	return "a"
}
