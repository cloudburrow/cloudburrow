package run

import (
	"context"
	"strconv"
	"strings"
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestResetProjectDropsOnlyThatProjectsWorkloads: a project's reset (#1540)
// deletes its jobs, functions and Cloud Build state, under its ID or its
// number, and leaves another project's.
func TestResetProjectDropsOnlyThatProjectsWorkloads(t *testing.T) {
	js, _, _, _ := jobsServer(nil)
	ctx := context.Background()
	job := func() *runpb.Job {
		return &runpb.Job{Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{
			Containers: []*runpb.Container{{Image: "docker.io/library/busybox:1.36"}}}}}
	}
	for _, p := range []string{"gone-1540", "kept-1540"} {
		if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: "projects/" + p + "/locations/us-central1", JobId: "nightly", Job: job()}); err != nil {
			t.Fatalf("CreateJob in %s: %v", p, err)
		}
	}
	number := strconv.FormatInt(resource.ProjectNumber("gone-1540"), 10)
	names := map[string]bool{ // name -> survives
		"projects/gone-1540/locations/us-central1/builds/b1":       false,
		"projects/" + number + "/locations/us-central1/builds/b2":  false,
		"projects/kept-1540/locations/us-central1/builds/b3":       true,
		"projects/gone-1540/locations/global/triggers/t1":          false,
		"projects/kept-1540/locations/global/triggers/t2":          true,
		"projects/gone-1540/locations/us-central1/functions/fn":    false,
		"projects/kept-1540/locations/us-central1/functions/fn":    true,
		"projects/gone-1540-other/locations/us-central1/builds/b4": true,
	}
	cloudBuilds.mu.Lock()
	triggerStore.mu.Lock()
	functions.mu.Lock()
	for n := range names {
		switch {
		case strings.Contains(n, "/builds/"):
			cloudBuilds.builds[n] = &cbpb.Build{Name: n}
		case strings.Contains(n, "/triggers/"):
			triggerStore.triggers[n] = &cbpb.BuildTrigger{ResourceName: n}
			triggerApprovals[n] = true
		default:
			functions.fns[n] = &storedFunction{fn: &functionspb.Function{Name: n}}
		}
	}
	functions.mu.Unlock()
	triggerStore.mu.Unlock()
	cloudBuilds.mu.Unlock()
	t.Cleanup(func() {
		cloudBuilds.mu.Lock()
		triggerStore.mu.Lock()
		functions.mu.Lock()
		for n := range names {
			delete(cloudBuilds.builds, n)
			delete(triggerStore.triggers, n)
			delete(triggerApprovals, n)
			delete(functions.fns, n)
		}
		functions.mu.Unlock()
		triggerStore.mu.Unlock()
		cloudBuilds.mu.Unlock()
	})

	if err := js.s.ResetProject(ctx, "gone-1540", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: "projects/gone-1540/locations/us-central1/jobs/nightly"}); status.Code(err) != codes.NotFound {
		t.Errorf("the reset project's job: %v, want NOT_FOUND", err)
	}
	if _, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: "projects/kept-1540/locations/us-central1/jobs/nightly"}); err != nil {
		t.Errorf("another project's job: %v", err)
	}
	cloudBuilds.mu.Lock()
	triggerStore.mu.Lock()
	functions.mu.Lock()
	defer cloudBuilds.mu.Unlock()
	defer triggerStore.mu.Unlock()
	defer functions.mu.Unlock()
	for n, survives := range names {
		_, b := cloudBuilds.builds[n]
		_, tr := triggerStore.triggers[n]
		_, fn := functions.fns[n]
		if got := b || tr || fn; got != survives {
			t.Errorf("%s kept = %v, want %v", n, got, survives)
		}
	}
}
