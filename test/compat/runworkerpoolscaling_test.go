//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
	runv2 "google.golang.org/api/run/v2"
)

// runV2REST is the official generated Cloud Run v2 JSON client.
func runV2REST(t *testing.T, h *Harness) *runv2.Service {
	t.Helper()
	ep := h.Endpoint(EnvRun)
	if ep == "" {
		t.Fatal("no Cloud Run endpoint")
	}
	s, err := runv2.NewService(h.Context(), option.WithEndpoint("http://"+ep+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatalf("runv2.NewService: %v", err)
	}
	return s
}

// waitRunV2Op polls a Cloud Run v2 operation until it is done.
func waitRunV2Op(t *testing.T, s *runv2.Service, op *runv2.GoogleLongrunningOperation) *runv2.GoogleLongrunningOperation {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for !op.Done {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s not done", op.Name)
		}
		time.Sleep(2 * time.Second)
		var err error
		if op, err = s.Projects.Locations.Operations.Get(op.Name).Do(); err != nil {
			t.Fatalf("operations.get %s: %v", op.Name, err)
		}
	}
	if op.Error != nil {
		t.Fatalf("operation %s failed: %+v", op.Name, op.Error)
	}
	return op
}

// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool, google.cloud.run.v2.WorkerPools/UpdateWorkerPool
//
// TestRunWorkerPoolAutomaticScaling (#1788): through the official
// google.golang.org/api/run/v2 client (discovery revision 20261108), a pool
// created with scaling.scalingMode AUTOMATIC, minInstanceCount 2,
// maxInstanceCount 4 and cpuScaling.cpuUtilization 0.6 reads back as set,
// over REST and in the create operation's response, and runs two instances
// (its minimum); patching only scaling.minInstanceCount to 1 keeps the
// revision and stops one; the gRPC client reads the pool as the proto
// defines it. A cpuUtilization outside 0.1-0.90 (other than 0), and a
// minimum above the maximum, are 400.
//
// unverified: google.cloud.run.v2.WorkerPools/CreateWorkerPool INVALID_ARGUMENT: minInstanceCount above maxInstanceCount
// Docs re-read 2026-11-18, still unanswered: https://docs.cloud.google.com/run/docs/reference/rest/v2/projects.locations.workerPools
func TestRunWorkerPoolAutomaticScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	s := runV2REST(t, h)
	ctx := runWait(t)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	parent := runParent(h)
	id := "compat-autoscale"
	name := parent + "/workerPools/" + id
	t.Cleanup(func() {
		if op, err := s.Projects.Locations.WorkerPools.Delete(name).Do(); err == nil {
			waitRunV2Op(t, s, op)
		}
	})
	container := &runv2.GoogleCloudRunV2Container{Image: jobImage, Command: []string{"sh", "-c"}, Args: []string{"exec sleep 3600"}}
	pool := func(sc *runv2.GoogleCloudRunV2WorkerPoolScaling) *runv2.GoogleCloudRunV2WorkerPool {
		return &runv2.GoogleCloudRunV2WorkerPool{Scaling: sc,
			Template: &runv2.GoogleCloudRunV2WorkerPoolRevisionTemplate{Containers: []*runv2.GoogleCloudRunV2Container{container}}}
	}

	// The documented limits.
	for _, bad := range []*runv2.GoogleCloudRunV2WorkerPoolScaling{
		{ScalingMode: "AUTOMATIC", MinInstanceCount: 1, MaxInstanceCount: 2, CpuScaling: &runv2.GoogleCloudRunV2CpuScaling{CpuUtilization: 0.95}},
		{ScalingMode: "AUTOMATIC", MinInstanceCount: 1, MaxInstanceCount: 2, CpuScaling: &runv2.GoogleCloudRunV2CpuScaling{CpuUtilization: 0.05}},
		{ScalingMode: "AUTOMATIC", MinInstanceCount: 5, MaxInstanceCount: 4},
	} {
		if _, err := s.Projects.Locations.WorkerPools.Create(parent, pool(bad)).WorkerPoolId(id).Do(); httpCode(err) != http.StatusBadRequest {
			b, _ := json.Marshal(bad)
			t.Errorf("create with scaling %s = %v, want 400", b, err)
		}
	}

	sc := &runv2.GoogleCloudRunV2WorkerPoolScaling{ScalingMode: "AUTOMATIC", MinInstanceCount: 2, MaxInstanceCount: 4,
		CpuScaling: &runv2.GoogleCloudRunV2CpuScaling{CpuUtilization: 0.6}}
	op, err := s.Projects.Locations.WorkerPools.Create(parent, pool(sc)).WorkerPoolId(id).Do()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	op = waitRunV2Op(t, s, op)
	var fromOp runv2.GoogleCloudRunV2WorkerPool
	if err := json.Unmarshal(op.Response, &fromOp); err != nil || fromOp.Scaling == nil || fromOp.Scaling.ScalingMode != "AUTOMATIC" {
		t.Errorf("the create operation's pool = %s, %v", op.Response, err)
	}
	got, err := s.Projects.Locations.WorkerPools.Get(name).Do()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	g := got.Scaling
	if g == nil || g.ScalingMode != "AUTOMATIC" || g.MinInstanceCount != 2 || g.MaxInstanceCount != 4 ||
		g.CpuScaling == nil || g.CpuScaling.CpuUtilization < 0.599 || g.CpuScaling.CpuUtilization > 0.601 || g.ManualInstanceCount != 0 {
		b, _ := json.Marshal(g)
		t.Errorf("scaling = %s; want AUTOMATIC 2-4 at 0.6", b)
	}
	rev := got.LatestCreatedRevision
	pods := func() int {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "default", "get", "pods",
			"-l", "cloudburrow.dev/run-revision="+rev[strings.LastIndex(rev, "/")+1:], "--field-selector=status.phase=Running", "-o", "name").CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl get pods: %v\n%s", err, out)
		}
		return len(strings.Fields(string(out)))
	}
	waitPods := func(want int) int {
		var n int
		for range 60 {
			if n = pods(); n == want {
				return n
			}
			time.Sleep(2 * time.Second)
		}
		return n
	}
	if n := waitPods(2); n != 2 {
		t.Errorf("running pods = %d, want the minimum, 2", n)
	}

	// The gRPC client reads the pool as the proto defines it.
	c, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if wp, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name}); err != nil || wp.GetScaling().ManualInstanceCount != nil {
		t.Errorf("gRPC GetWorkerPool = %v, %v", wp, err)
	}

	// Lowering the minimum alone keeps the revision.
	op, err = s.Projects.Locations.WorkerPools.Patch(name, pool(&runv2.GoogleCloudRunV2WorkerPoolScaling{MinInstanceCount: 1})).
		UpdateMask("scaling.minInstanceCount").Do()
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	waitRunV2Op(t, s, op)
	got, err = s.Projects.Locations.WorkerPools.Get(name).Do()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g := got.Scaling; g == nil || g.ScalingMode != "AUTOMATIC" || g.MinInstanceCount != 1 || g.MaxInstanceCount != 4 || got.LatestCreatedRevision != rev {
		b, _ := json.Marshal(got.Scaling)
		t.Errorf("after the patch: scaling %s, revision %s (was %s)", b, got.LatestCreatedRevision, rev)
	}
	if n := waitPods(1); n != 1 {
		t.Errorf("running pods = %d, want the new minimum, 1", n)
	}
}

// TestTerraformRunWorkerPoolAutomaticScaling (#1788): a
// google_cloud_run_v2_worker_pool with scaling_mode AUTOMATIC and
// min_instance_count / max_instance_count (hashicorp/google 8.4.0, which
// sends scaling.scalingMode, minInstanceCount and maxInstanceCount) applies
// through `cloudburrow terraform` with egress blocked; the official
// google.golang.org/api/run/v2 client reads the settings back; the plan is
// clean; raising the minimum changes it in place, the plan is clean again;
// and destroy removes it.
// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool, google.cloud.run.v2.WorkerPools/UpdateWorkerPool, google.cloud.run.v2.WorkerPools/DeleteWorkerPool
func TestTerraformRunWorkerPoolAutomaticScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	_ = runClient(t, h)
	s := runV2REST(t, h)
	m := newTFModule(t)
	id := "tfauto-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/workerPools/" + id
	module := func(minN, maxN int) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_worker_pool" "p" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  scaling {
    scaling_mode       = "AUTOMATIC"
    min_instance_count = %[3]d
    max_instance_count = %[4]d
  }
  template {
    containers {
      image   = %[5]q
      command = ["sh", "-c"]
      args    = ["exec sleep 3600"]
    }
  }
}
`, h.Project(), id, minN, maxN, jobImage)
	}
	check := func(when string, minN, maxN int64) {
		t.Helper()
		got, err := s.Projects.Locations.WorkerPools.Get(name).Do()
		if err != nil {
			t.Fatalf("%s, get: %v", when, err)
		}
		if g := got.Scaling; g == nil || g.ScalingMode != "AUTOMATIC" || g.MinInstanceCount != minN || g.MaxInstanceCount != maxN {
			b, _ := json.Marshal(got.Scaling)
			t.Errorf("%s, scaling = %s; want AUTOMATIC %d-%d", when, b, minN, maxN)
		}
	}
	m.write(module(1, 3))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 1, 3)
	m.planClean()

	m.write(module(2, 3))
	m.apply()
	check("after the change", 2, 3)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := s.Projects.Locations.WorkerPools.Get(name).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("get after destroy: %v; want 404", err)
	}
}
