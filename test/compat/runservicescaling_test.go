//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// runRevisionPods counts the running pods of a Knative revision of the
// service id, in any namespace.
func runRevisionPods(t *testing.T, kubeconfig, id string) int {
	t.Helper()
	kname := ksvcName(kubeconfig, id)
	out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A",
		"-l", "serving.knative.dev/service="+kname, "--field-selector=status.phase=Running", "-o", "name").Output()
	if err != nil {
		t.Fatalf("kubectl get pods: %v", err)
	}
	return len(strings.Fields(string(out)))
}

// waitRevisionPods waits until the service runs want pods.
func waitRevisionPods(ctx context.Context, t *testing.T, kubeconfig, id string, want int) {
	t.Helper()
	got := -1
	for ctx.Err() == nil {
		if got = runRevisionPods(t, kubeconfig, id); got == want {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("service %s runs %d pods; want %d", id, got, want)
}

// covers: google.cloud.run.v2.Services/UpdateService
//
// TestRunServiceLevelScaling (#1790): service-level scaling over the
// official client. A service-level minimum keeps that many instances
// running with no request; MANUAL runs exactly manualInstanceCount; both
// change in place, without a new revision; the revision-level concurrency
// target reads back; the documented limits are refused.
func TestRunServiceLevelScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	ctx := runWait(t)
	id := "svc-scaling"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Scaling: &runpb.ServiceScaling{MinInstanceCount: 2, MaxInstanceCount: 5},
		Template: &runpb.RevisionTemplate{
			Scaling:    &runpb.RevisionScaling{ConcurrencyUtilization: proto.Float32(0.8), CpuUtilization: proto.Float32(0)},
			Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sc := svc.GetScaling(); sc.GetMinInstanceCount() != 2 || sc.GetMaxInstanceCount() != 5 {
		t.Errorf("scaling = %v; want min 2 max 5", sc)
	}
	if ts := svc.GetTemplate().GetScaling(); ts.GetConcurrencyUtilization() != 0.8 || ts.CpuUtilization == nil || ts.GetCpuUtilization() != 0 {
		t.Errorf("template.scaling = %v; want concurrencyUtilization 0.8, cpuUtilization 0", ts)
	}
	waitRevisionPods(ctx, t, kubeconfig, id, 2)
	rev := svc.GetLatestReadyRevision()

	// MANUAL: exactly that many, in place.
	uop, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: &runpb.Service{Name: name,
		Scaling: &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(3)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"scaling"}}})
	if err != nil {
		t.Fatalf("UpdateService MANUAL: %v", err)
	}
	if svc, err = waitLRO(ctx, uop); err != nil {
		t.Fatalf("update: %v", err)
	}
	if svc.GetScaling().GetScalingMode() != runpb.ServiceScaling_MANUAL || svc.GetScaling().GetManualInstanceCount() != 3 {
		t.Errorf("scaling = %v; want MANUAL 3", svc.GetScaling())
	}
	if svc.GetLatestReadyRevision() != rev {
		t.Errorf("latest revision %s after a scaling change; want %s (no new revision)", svc.GetLatestReadyRevision(), rev)
	}
	waitRevisionPods(ctx, t, kubeconfig, id, 3)

	// The documented limits.
	for _, bad := range []struct {
		sc   *runpb.ServiceScaling
		ts   *runpb.RevisionScaling
		code codes.Code
	}{
		{sc: &runpb.ServiceScaling{MinInstanceCount: -1}, code: codes.InvalidArgument},
		{ts: &runpb.RevisionScaling{CpuUtilization: proto.Float32(0.95)}, code: codes.InvalidArgument},
		{ts: &runpb.RevisionScaling{ConcurrencyUtilization: proto.Float32(0.05)}, code: codes.InvalidArgument},
		{ts: &runpb.RevisionScaling{CpuUtilization: proto.Float32(0), ConcurrencyUtilization: proto.Float32(0)}, code: codes.InvalidArgument},
	} {
		_, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: &runpb.Service{Name: name, Scaling: bad.sc,
			Template: &runpb.RevisionTemplate{Scaling: bad.ts, Containers: []*runpb.Container{{Image: helloDigest}}}}})
		if status.Code(err) != bad.code {
			t.Errorf("UpdateService(scaling %v, template.scaling %v) = %v; want %v", bad.sc, bad.ts, err, bad.code)
		}
	}
}

// TestTerraformRunServiceLevelScaling (#1790): hashicorp/google's
// google_cloud_run_v2_service scaling block applies, changes in place and
// plans clean.
func TestTerraformRunServiceLevelScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	m := newTFModule(t)
	id := "tfsvcscale-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/services/" + id
	module := func(minN, maxN int) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  scaling {
    min_instance_count = %[3]d
    max_instance_count = %[4]d
  }
  template {
    containers {
      image = %[5]q
    }
  }
}
`, h.Project(), id, minN, maxN, helloDigest)
	}
	check := func(when string, minN, maxN int32) {
		t.Helper()
		got, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, get: %v", when, err)
		}
		if sc := got.GetScaling(); sc.GetMinInstanceCount() != minN || sc.GetMaxInstanceCount() != maxN {
			t.Errorf("%s, scaling = %v; want %d-%d", when, sc, minN, maxN)
		}
	}
	m.write(module(1, 4))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 1, 4)
	m.planClean()
	m.write(module(2, 4))
	m.apply()
	check("after the change", 2, 4)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

// TestGcloudRunServiceLevelScaling (#1790): gcloud run deploy --min --max
// and services update --scaling, through Cloud Run v1's service
// annotations.
func TestGcloudRunServiceLevelScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	g := newGcloudSession(t, h)
	id := "gc-svcscale"
	name := runParent(h) + "/services/" + id
	region, project := "--region=us-central1", "--project="+h.Project()
	g.must("run", "deploy", id, "--image="+helloDigest, region, project, "--no-allow-unauthenticated", "--min=1", "--max=3", "--quiet")
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	got, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sc := got.GetScaling(); sc.GetMinInstanceCount() != 1 || sc.GetMaxInstanceCount() != 3 {
		t.Errorf("after deploy --min=1 --max=3, scaling = %v", sc)
	}
	g.must("run", "services", "update", id, region, project, "--scaling=2", "--quiet")
	if got, err = c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if sc := got.GetScaling(); sc.GetScalingMode() != runpb.ServiceScaling_MANUAL || sc.GetManualInstanceCount() != 2 {
		t.Errorf("after update --scaling=2, scaling = %v", sc)
	}
}
