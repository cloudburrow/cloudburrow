//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv2 "google.golang.org/api/run/v2"
	"google.golang.org/protobuf/proto"
)

// cpuBurnImage is testdata/cpuburn, built and loaded by loadCPUBurn.
const cpuBurnImage = "dev.local/cloudburrow-cpuburn:compat"

// loadCPUBurn compiles testdata/cpuburn, builds its image and loads it into
// the instance's kind cluster, skipping without one. The image ID is
// logged, so the run that built it can remove it.
func loadCPUBurn(ctx context.Context, t *testing.T) {
	t.Helper()
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the fixture must be loaded into the owned cluster")
	}
	for _, bin := range []string{"docker", "kind", "go", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the fixture", bin)
		}
	}
	root := moduleRoot(t)
	dir := root + "/testdata/cpuburn"
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dir+"/cpuburn", "./testdata/cpuburn")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile cpuburn: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/cpuburn") })
	if b, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", cpuBurnImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the cpuburn image: %v\n%s", err, b)
	} else {
		t.Logf("built %s: %s", cpuBurnImage, strings.TrimSpace(string(b)))
	}
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", cpuBurnImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load cpuburn: %v\n%s", err, b)
	}
}

// needMetricsServer skips unless the cluster serves metrics.k8s.io, which
// up installs with Cloud Run (#1789).
func needMetricsServer(ctx context.Context, t *testing.T, kubeconfig string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(5 * time.Second) {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "--raw",
			"/apis/metrics.k8s.io/v1beta1/nodes").CombinedOutput()
		if err == nil && strings.Contains(string(out), `"usage"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics.k8s.io does not answer: %v\n%s", err, out)
		}
	}
}

// podsOver polls count until it returns at least want, or at most want when
// down, within d; it returns the extreme count it saw.
func podsOver(t *testing.T, d time.Duration, down bool, want int, count func() int) int {
	t.Helper()
	seen := -1
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		n := count()
		if seen < 0 || (!down && n > seen) || (down && n < seen) {
			seen = n
		}
		if (!down && n >= want) || (down && n <= want) {
			return n
		}
	}
	return seen
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestRunServiceCPUUtilizationScaling (#1800): a service whose template
// sets scaling.cpuUtilization 0.5, 1 CPU, minimum 1 and maximum 3
// instances, and a concurrency limit of 80 that four requests in flight
// never approach, grows past one instance when those requests burn its CPU
// (Cloud Run scales on "whichever scaling driver requires more
// instances"), reaches its maximum and no further, and returns to its
// minimum once the load stops. Created and read back through the official
// run/v2 gRPC client.
func TestRunServiceCPUUtilizationScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	needMetricsServer(ctx, t, kubeconfig)
	base := ingress(t)
	loadCPUBurn(ctx, t)

	id := "svc-cpu-scaling"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			Scaling:                       &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 3, CpuUtilization: proto.Float32(0.5)},
			MaxInstanceRequestConcurrency: 80,
			Containers: []*runpb.Container{{Image: cpuBurnImage,
				Resources: &runpb.ResourceRequirements{Limits: map[string]string{"cpu": "1", "memory": "256Mi"}}}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name}); err != nil || got.GetTemplate().GetScaling().GetCpuUtilization() != 0.5 {
		t.Fatalf("GetService = %v, %v; want template.scaling.cpuUtilization 0.5", got.GetTemplate().GetScaling(), err)
	}
	pods := func() int { return runRevisionPods(t, kubeconfig, id) }
	if n := podsOver(t, 2*time.Minute, true, 1, pods); n != 1 {
		t.Fatalf("idle, the service runs %d pods; want its minimum, 1", n)
	}

	host := hostOf(t, svc.GetUri())
	loadCtx, stopLoad := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for loadCtx.Err() == nil {
				req, _ := http.NewRequestWithContext(loadCtx, http.MethodGet, base+"/burn?ms=2000", nil)
				req.Host = host
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
				} else {
					time.Sleep(200 * time.Millisecond)
				}
			}
		}()
	}
	peak := podsOver(t, 4*time.Minute, false, 3, pods)
	time.Sleep(30 * time.Second) // the maximum holds
	over := pods()
	stopLoad()
	wg.Wait()
	if peak < 3 {
		t.Errorf("under CPU load the service grew to %d pods; want its maximum, 3", peak)
	}
	if over > 3 {
		t.Errorf("the service ran %d pods, above its maximum of 3", over)
	}
	if n := podsOver(t, 5*time.Minute, true, 1, pods); n != 1 {
		t.Errorf("after the load stopped the service still runs %d pods; want its minimum, 1", n)
	}
}

// covers: google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool
//
// TestRunWorkerPoolCPUScaling (#1789): an AUTOMATIC worker pool with
// minInstanceCount 1, maxInstanceCount 3 and cpuScaling.cpuUtilization 0.5,
// whose instances burn their CPU for the first minutes, grows to its
// maximum while they burn and back to its minimum after. Created and read
// back through the official google.golang.org/api/run/v2 client (the
// scaling fields are REST-only, workerpoolscaling.go).
func TestRunWorkerPoolCPUScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	s := runV2REST(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	needMetricsServer(ctx, t, kubeconfig)
	loadCPUBurn(ctx, t)

	parent := runParent(h)
	id := "compat-cpu-pool"
	name := parent + "/workerPools/" + id
	t.Cleanup(func() {
		if op, err := s.Projects.Locations.WorkerPools.Delete(name).Do(); err == nil {
			waitRunV2Op(t, s, op)
		}
	})
	burnUntil := time.Now().Add(4 * time.Minute)
	op, err := s.Projects.Locations.WorkerPools.Create(parent, &runv2.GoogleCloudRunV2WorkerPool{
		Scaling: &runv2.GoogleCloudRunV2WorkerPoolScaling{ScalingMode: "AUTOMATIC", MinInstanceCount: 1, MaxInstanceCount: 3,
			CpuScaling: &runv2.GoogleCloudRunV2CpuScaling{CpuUtilization: 0.5}},
		Template: &runv2.GoogleCloudRunV2WorkerPoolRevisionTemplate{Containers: []*runv2.GoogleCloudRunV2Container{{
			Image:     cpuBurnImage,
			Env:       []*runv2.GoogleCloudRunV2EnvVar{{Name: "BURN_UNTIL", Value: fmt.Sprint(burnUntil.Unix())}},
			Resources: &runv2.GoogleCloudRunV2ResourceRequirements{Limits: map[string]string{"cpu": "1", "memory": "256Mi"}},
		}}},
	}).WorkerPoolId(id).Do()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitRunV2Op(t, s, op)
	got, err := s.Projects.Locations.WorkerPools.Get(name).Do()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g := got.Scaling; g == nil || g.ScalingMode != "AUTOMATIC" || g.CpuScaling == nil || g.CpuScaling.CpuUtilization < 0.499 || g.CpuScaling.CpuUtilization > 0.501 {
		t.Fatalf("scaling = %+v; want AUTOMATIC at 0.5", g)
	}
	rev := got.LatestCreatedRevision[strings.LastIndex(got.LatestCreatedRevision, "/")+1:]
	pods := func() int {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A",
			"-l", "cloudburrow.dev/run-revision="+rev, "--field-selector=status.phase=Running", "-o", "name").CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl get pods: %v\n%s", err, out)
		}
		return len(strings.Fields(string(out)))
	}
	peak := podsOver(t, time.Until(burnUntil), false, 3, pods)
	if peak < 3 {
		t.Errorf("while its instances burned CPU the pool grew to %d; want its maximum, 3", peak)
	}
	if n := pods(); n > 3 {
		t.Errorf("the pool runs %d instances, above its maximum of 3", n)
	}
	time.Sleep(time.Until(burnUntil))
	if n := podsOver(t, 4*time.Minute, true, 1, pods); n != 1 {
		t.Errorf("after the burn the pool still runs %d instances; want its minimum, 1", n)
	}
}
