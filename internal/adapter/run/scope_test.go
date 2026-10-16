package run

import (
	"context"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestJobsOfOneIDInTwoProjects (#1399): two projects' jobs of one ID are
// two jobs, each with its own ConfigMap and executions, and deleting one
// leaves the other and its executions.
func TestJobsOfOneIDInTwoProjects(t *testing.T) {
	ctx := context.Background()
	js, xs, _, f := jobsServer(fakeSecrets{})
	const other = "projects/other-project/locations/us-central1"
	for _, parent := range []string{jobParent, other} {
		if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: "same", Job: sampleJob()}); err != nil {
			t.Fatalf("CreateJob in %s: %v", parent, err)
		}
	}
	if len(f.cms) != 2 {
		t.Fatalf("ConfigMaps = %d, want one per project", len(f.cms))
	}
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: other, JobId: "same", Job: sampleJob()}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second create in one project = %v, want AlreadyExists", err)
	}
	if _, err := js.RunJob(ctx, &runpb.RunJobRequest{Name: jobParent + "/jobs/same"}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	for parent, want := range map[string]int32{jobParent: 1, other: 0} {
		job, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: parent + "/jobs/same"})
		if err != nil || job.GetName() != parent+"/jobs/same" || job.GetExecutionCount() != want {
			t.Errorf("GetJob in %s = %v (%d executions), %v; want its own name and %d", parent, job.GetName(), job.GetExecutionCount(), err, want)
		}
		execs, err := xs.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: parent + "/jobs/same"})
		if err != nil || int32(len(execs.GetExecutions())) != want {
			t.Errorf("ListExecutions in %s = %d, %v; want %d", parent, len(execs.GetExecutions()), err, want)
		}
		list, err := js.ListJobs(ctx, &runpb.ListJobsRequest{Parent: parent})
		if err != nil || len(list.GetJobs()) != 1 || list.GetJobs()[0].GetExecutionCount() != want {
			t.Errorf("ListJobs in %s = %v, %v; want its one job", parent, list, err)
		}
	}
	// Deleting the other project's job leaves this one's and its execution.
	if _, err := js.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: other + "/jobs/same"}); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if job, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: jobParent + "/jobs/same"}); err != nil || job.GetExecutionCount() != 1 {
		t.Errorf("the remaining job = %v, %v; want it and its execution", job, err)
	}
	if len(f.jobs) != 1 {
		t.Errorf("batch Jobs = %d after deleting the other project's job, want 1", len(f.jobs))
	}
	if _, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: other + "/jobs/same"}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted job = %v, want NotFound", err)
	}
}

// TestJobUnderTheUnscopedName: a job an older version created, at
// run-job-<id> with no scope label, still reads back, updates in place and
// deletes; another project's job of its ID is not it.
func TestJobUnderTheUnscopedName(t *testing.T) {
	ctx := context.Background()
	js, _, _, f := jobsServer(fakeSecrets{})
	name := jobParent + "/jobs/old"
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "old", Job: sampleJob()}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// Move it to the name an older version gave it.
	f.mu.Lock()
	cm := f.cms[jobCMName(name, "old")]
	delete(f.cms, jobCMName(name, "old"))
	md := cm["metadata"].(map[string]any)
	md["name"] = jobCMPrefix + "old"
	delete(md["labels"].(map[string]any), labelScope)
	f.cms[jobCMPrefix+"old"] = cm
	f.mu.Unlock()

	job, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil || job.GetName() != name {
		t.Fatalf("GetJob of the unscoped job = %v, %v", job, err)
	}
	if _, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: "projects/other-project/locations/us-central1/jobs/old"}); status.Code(err) != codes.NotFound {
		t.Errorf("another project's job of its ID = %v, want NotFound", err)
	}
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "old", Job: sampleJob()}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("create over the unscoped job = %v, want AlreadyExists", err)
	}
	job.Labels = map[string]string{"team": "ops"}
	if _, err := js.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: job}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if _, ok := f.cms[jobCMPrefix+"old"]; !ok || len(f.cms) != 1 {
		t.Errorf("the update moved the job: %d ConfigMaps", len(f.cms))
	}
	if _, err := js.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: name}); err != nil || len(f.cms) != 0 {
		t.Errorf("DeleteJob = %v, %d ConfigMaps left", err, len(f.cms))
	}
}

// TestWorkloadNamesAreScoped: a worker pool's and an instance's objects,
// selectors and host carry their project and location; an unscoped
// workload keeps the names an older version gave it and selects none of a
// scoped one's.
func TestWorkloadNamesAreScoped(t *testing.T) {
	a := newWorkload(kindWorkerPool, "p", "projects/a/locations/us-central1/workerPools/p")
	b := newWorkload(kindWorkerPool, "p", "projects/b/locations/us-central1/workerPools/p")
	r := newWorkload(kindWorkerPool, "p", "projects/a/locations/europe-west1/workerPools/p")
	if a.objectName() == b.objectName() || a.objectName() == r.objectName() || a.selector() == b.selector() {
		t.Errorf("two scopes share names: %s %s %s", a.objectName(), b.objectName(), r.objectName())
	}
	if !strings.HasPrefix(a.objectName(), "run-worker-pool-") || !strings.HasSuffix(a.objectName(), "-p") {
		t.Errorf("objectName = %s", a.objectName())
	}
	legacy := workload{kind: kindWorkerPool, id: "p", name: a.name}
	if legacy.objectName() != "run-worker-pool-p" || !strings.Contains(legacy.selector(), "!"+labelScope) {
		t.Errorf("unscoped names = %s, %s", legacy.objectName(), legacy.selector())
	}
	s := NewServer(nil, "test", 0)
	spec := workerPoolSpec(&runpb.WorkerPool{Name: a.name, Template: &runpb.WorkerPoolRevisionTemplate{
		Containers: []*runpb.Container{{Image: "docker.io/library/busybox:1.36"}}}}, 1, a.name+"/revisions/p-00001-abc")
	cm, dep, err := s.renderWorkerPool(a, spec, 1, "")
	if err != nil {
		t.Fatalf("renderWorkerPool: %v", err)
	}
	if strings.Count(dep, labelScope+`: "`+a.scope+`"`) != 3 || !strings.Contains(dep, "name: "+a.objectName()) {
		t.Errorf("Deployment lacks the scope on its labels, selector and pods:\n%s", dep)
	}
	if !strings.Contains(cm, `"`+labelScope+`":"`+a.scope+`"`) || !strings.Contains(cm, a.name) {
		t.Errorf("ConfigMap lacks the scope label or the full name: %s", cm)
	}
	// An instance's route selects only its own pods, at a host of its own.
	ia := newWorkload(kindInstance, "web", "projects/a/locations/l/instances/web")
	ib := newWorkload(kindInstance, "web", "projects/b/locations/l/instances/web")
	if s.instanceHost(ia) == s.instanceHost(ib) {
		t.Errorf("two projects' instances share host %s", s.instanceHost(ia))
	}
	if route := s.instanceRoute(ia, 8080); !strings.Contains(route, "selector:\n    "+labelWorkload+": instance-web\n    "+labelScope) {
		t.Errorf("the route does not select by scope:\n%s", route)
	}
	long := newWorkload(kindInstance, strings.Repeat("x", 63), "projects/a/locations/l/instances/"+strings.Repeat("x", 63))
	if n := long.objectName(); len(n) > 63 {
		t.Errorf("objectName %s is %d characters", n, len(n))
	}
	if h := s.instanceHost(long); len(strings.SplitN(h, ".", 2)[0]) > 63 {
		t.Errorf("host label of %s is over 63 characters", h)
	}
}
