//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Cloud Run services scaling gaps of #1802.

// unavailableBody is what Google's manual-scaling page says a disabled
// service's URL answers: "Service unavailable" (or "Service disabled").
const unavailableBody = "Service unavailable"

// helloService is a helloworld-go template answering "Hello <target>!".
func helloService(target string) *runpb.RevisionTemplate {
	return &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: helloDigest,
		Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: target}}}}}}
}

// twoRevisions creates id with a first revision, then a second one, and
// returns their names and the service as it stands.
func twoRevisions(ctx context.Context, t *testing.T, h *Harness, c *run.ServicesClient, id string) (string, string) {
	t.Helper()
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: helloService("first")}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	first, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uop, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: &runpb.Service{Name: name, Template: helloService("second")}})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	second, err := waitLRO(ctx, uop)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return first.GetLatestReadyRevision(), second.GetLatestReadyRevision()
}

// updateScalingAndTraffic sets the service's scaling and traffic, masked.
func updateScalingAndTraffic(ctx context.Context, t *testing.T, c *run.ServicesClient, name string, sc *runpb.ServiceScaling, tr []*runpb.TrafficTarget) *runpb.Service {
	t.Helper()
	paths := []string{"scaling"}
	if tr != nil {
		paths = append(paths, "traffic")
	}
	op, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: &runpb.Service{Name: name, Scaling: sc, Traffic: tr},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	if err != nil {
		t.Fatalf("UpdateService %v: %v", sc, err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("update %v: %v", sc, err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		if len(got.GetTrafficStatuses()) == len(got.GetTraffic()) || time.Now().After(deadline) {
			return got
		}
	}
}

// answerUntil polls host until ok accepts its status and body, or two
// minutes pass, and returns the last of them.
func answerUntil(t *testing.T, base, host string, ok func(int, string) bool) (int, string) {
	t.Helper()
	var code int
	var body string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		var err error
		code, body, err = doRequest(http.MethodGet, base+"/", host, "", 15*time.Second)
		if err != nil {
			code, body = 0, err.Error()
		}
		if ok(code, body) {
			break
		}
	}
	return code, body
}

func unavailable(code int, body string) bool {
	return code == http.StatusServiceUnavailable && strings.TrimSpace(body) == unavailableBody
}

// covers: google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Services/GetService
//
// TestRunServiceDisabledAnswersUnavailable (#1802): a service with a
// tag-only revision, disabled with MANUAL 0 through the official run/v2
// client, answers its URL 503 "Service unavailable", as Google's
// manual-scaling page says ("any further requests to the service URL will
// fail with a Service unavailable or Service disabled error"; the status
// is UNVERIFIED, re-read 2026-11-18: no status code is named on
// https://docs.cloud.google.com/run/docs/configuring/services/manual-scaling), while the tag-only revision keeps serving through its tag
// URL ("revisions that are only active due to traffic tags are not
// impacted"). MANUAL 1 serves the URL again.
func TestRunServiceDisabledAnswersUnavailable(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	base := ingress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	id := "svc-unavailable"
	name := runParent(h) + "/services/" + id
	firstRev, _ := twoRevisions(ctx, t, h, c, id)
	latest := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
	revision := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
	tr := []*runpb.TrafficTarget{{Type: latest, Percent: 100}, {Type: revision, Revision: firstRev, Tag: "old"}}
	one := &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(1)}
	svc := updateScalingAndTraffic(ctx, t, c, name, one, tr)
	uri, host := svc.GetUri(), hostOf(t, svc.GetUri())
	tagHost := ""
	for _, st := range svc.GetTrafficStatuses() {
		if st.GetTag() == "old" {
			tagHost = hostOf(t, st.GetUri())
		}
	}
	if tagHost == "" {
		t.Fatalf("traffic statuses %v; want the tag old with a URL", svc.GetTrafficStatuses())
	}
	if code, body := answerUntil(t, base, host, func(c int, b string) bool { return c == 200 && strings.Contains(b, "second") }); code != 200 {
		t.Fatalf("before it is disabled the service answers %d %q", code, body)
	}

	off := updateScalingAndTraffic(ctx, t, c, name, &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(0)}, nil)
	if off.GetUri() != uri {
		t.Errorf("disabled, the service's uri is %s; want %s, unchanged", off.GetUri(), uri)
	}
	if code, body := answerUntil(t, base, host, unavailable); !unavailable(code, body) {
		t.Errorf("the disabled service's URL answers %d %q; want 503 %q", code, body, unavailableBody)
	}
	if code, body := answerUntil(t, base, tagHost, func(c int, b string) bool { return c == 200 }); code != 200 || !strings.Contains(body, "first") {
		t.Errorf("the tag-only revision answers %d %q while the service is disabled; want 200 from the first revision", code, body)
	}
	for _, st := range off.GetTrafficStatuses() {
		if st.GetTag() == "old" && hostOf(t, st.GetUri()) != tagHost {
			t.Errorf("disabled, the tag URL is %s; want it unchanged", st.GetUri())
		}
	}

	updateScalingAndTraffic(ctx, t, c, name, one, nil)
	if code, body := answerUntil(t, base, host, func(c int, b string) bool { return c == 200 }); code != 200 || !strings.Contains(body, "second") {
		t.Errorf("after MANUAL 1 the service answers %d %q; want 200 from the second revision", code, body)
	}
}

// covers: google.cloud.run.v2.Services/UpdateService
//
// TestRunServiceManualSplitLeavesOneAtZero (#1802): MANUAL 1 over a 50/50
// split gives the first-listed revision the instance and the other none.
// Google: "If the number of revisions receiving traffic exceeds the manual
// instance count, some of the revisions will have no instances. Traffic
// sent to those revisions will get the same error as if the revisions were
// disabled." So the URL answers about half its requests from the first
// revision and the rest 503 "Service unavailable", never from the second.
// MANUAL 2 serves both again.
func TestRunServiceManualSplitLeavesOneAtZero(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	base := ingress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	id := "svc-split-zero"
	name := runParent(h) + "/services/" + id
	firstRev, secondRev := twoRevisions(ctx, t, h, c, id)
	revision := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
	tr := []*runpb.TrafficTarget{{Type: revision, Revision: firstRev, Percent: 50}, {Type: revision, Revision: secondRev, Percent: 50}}
	svc := updateScalingAndTraffic(ctx, t, c, name, &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(1)}, tr)
	if sc := svc.GetScaling(); sc.GetManualInstanceCount() != 1 || len(svc.GetTraffic()) != 2 {
		t.Fatalf("service = %v, %v; want MANUAL 1 over two revisions", sc, svc.GetTraffic())
	}
	host := hostOf(t, svc.GetUri())
	answerUntil(t, base, host, unavailable)
	seen := map[string]int{}
	for i := 0; i < 40; i++ {
		code, body, err := doRequest(http.MethodGet, base+"/", host, "", 15*time.Second)
		switch {
		case err != nil:
			seen["error"]++
		case code == 200 && strings.Contains(body, "first"):
			seen["first"]++
		case unavailable(code, body):
			seen["unavailable"]++
		default:
			seen[fmt.Sprintf("%d %s", code, strings.TrimSpace(body))]++
		}
	}
	if seen["first"] < 8 || seen["unavailable"] < 8 || seen["first"]+seen["unavailable"] != 40 {
		t.Errorf("40 requests over MANUAL 1 at 50/50 answered %v; want about half from the first revision and half %q, nothing else", seen, unavailableBody)
	}
	// One instance in all: the first revision's, which every 200 above
	// came from.
	pods := func() int { return runRevisionPods(t, kubeconfig, id) }
	if n := podsOver(t, 3*time.Minute, true, 1, pods); n != 1 {
		t.Errorf("MANUAL 1 over two revisions runs %d pods; want 1", n)
	}

	updateScalingAndTraffic(ctx, t, c, name, &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(2)}, nil)
	answerUntil(t, base, host, func(c int, b string) bool { return c == 200 && strings.Contains(b, "second") })
	both := map[string]int{}
	for i := 0; i < 40; i++ {
		code, body, err := doRequest(http.MethodGet, base+"/", host, "", 15*time.Second)
		if err == nil && code == 200 {
			both[strings.TrimSpace(body)]++
		} else {
			both[fmt.Sprintf("%d %v", code, err)]++
		}
	}
	if len(both) != 2 {
		t.Errorf("40 requests over MANUAL 2 at 50/50 answered %v; want both revisions, nothing else", both)
	}
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestRunServiceCPUOnlyScaling (#1802): concurrencyUtilization 0.0
// disables the concurrency driver, "to scale only by CPU"
// (https://docs.cloud.google.com/run/docs/configuring/scaling-controls).
// A service with a concurrency limit of 10, 1 to 3 instances and
// cpuUtilization 0.5 holds eight requests that only wait (at Cloud Run's
// default 60% concurrency target they would need two instances) on one
// instance, then grows to three when its requests burn CPU, and returns to
// one. Through the official run/v2 client.
func TestRunServiceCPUOnlyScaling(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	t.Cleanup(cancel)
	needMetricsServer(ctx, t, kubeconfig)
	base := ingress(t)
	loadCPUBurn(ctx, t)

	id := "svc-cpu-only"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 3,
				CpuUtilization: proto.Float32(0.5), ConcurrencyUtilization: proto.Float32(0)},
			MaxInstanceRequestConcurrency: 10,
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
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if sc := got.GetTemplate().GetScaling(); err != nil || sc.ConcurrencyUtilization == nil || sc.GetConcurrencyUtilization() != 0 || sc.GetCpuUtilization() != 0.5 {
		t.Fatalf("GetService = %v, %v; want concurrencyUtilization 0 and cpuUtilization 0.5", sc, err)
	}
	pods := func() int { return runRevisionPods(t, kubeconfig, id) }
	if n := podsOver(t, 2*time.Minute, true, 1, pods); n != 1 {
		t.Fatalf("idle, the service runs %d pods; want 1", n)
	}
	host := hostOf(t, svc.GetUri())
	load := func(path string, n int, d time.Duration, during func()) {
		lctx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for lctx.Err() == nil {
					req, _ := http.NewRequestWithContext(lctx, http.MethodGet, base+path, nil)
					req.Host = host
					if resp, err := http.DefaultClient.Do(req); err == nil {
						_ = resp.Body.Close()
					} else {
						time.Sleep(200 * time.Millisecond)
					}
				}
			}()
		}
		during()
		stop()
		wg.Wait()
	}
	load("/sleep?ms=3000", 8, 0, func() {
		if n := podsOver(t, 90*time.Second, false, 2, pods); n > 1 {
			t.Errorf("eight waiting requests grew the CPU-only service to %d pods; want 1, concurrency does not scale it", n)
		}
	})
	load("/burn?ms=2000", 4, 0, func() {
		if n := podsOver(t, 4*time.Minute, false, 3, pods); n < 3 {
			t.Errorf("under CPU load the CPU-only service grew to %d pods; want its maximum, 3", n)
		}
	})
	if n := podsOver(t, 5*time.Minute, true, 1, pods); n != 1 {
		t.Errorf("after the load the service still runs %d pods; want 1", n)
	}
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Services/GetService
//
// TestTerraformRunServiceScalingControls (#1802): Google's Terraform for
// custom scaling controls (scaling-controls page): template.scaling cpu_utilization 0.7 and concurrency_utilization
// 0.8 apply, read back through the official client and plan clean;
// concurrency_utilization 0, "scale only by CPU", changes them in place
// and plans clean too. hashicorp/google 8.5.0 is the first release with
// cpu_utilization and concurrency_utilization (8.4.0's schema has only
// min_instance_count and max_instance_count, read 2026-11-18); the pin is
// past it since #1578.
func TestTerraformRunServiceScalingControls(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	m := newTFModule(t)
	id := "tfsvcsc-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/services/" + id
	module := func(cpu, concurrency string) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  template {
    scaling {
      max_instance_count      = 3
      cpu_utilization         = %[3]s
      concurrency_utilization = %[4]s
    }
    containers {
      image = %[5]q
    }
  }
}
`, h.Project(), id, cpu, concurrency, helloDigest)
	}
	write := m.write
	check := func(when string, cpu, concurrency float32) {
		t.Helper()
		got, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, get: %v", when, err)
		}
		sc := got.GetTemplate().GetScaling()
		if sc.GetCpuUtilization() != cpu || sc.GetConcurrencyUtilization() != concurrency || sc.GetMaxInstanceCount() != 3 {
			t.Errorf("%s, template.scaling = %v; want cpuUtilization %v, concurrencyUtilization %v", when, sc, cpu, concurrency)
		}
	}
	write(module("0.7", "0.8"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 0.7, 0.8)
	m.planClean()
	write(module("0.7", "0"))
	m.apply()
	check("after concurrency_utilization 0", 0.7, 0)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

// TestGcloudRunServiceScalingControls (#1802): the scaling-controls page's
// gcloud commands, through Cloud Run v1's run.googleapis.com/scaling-cpu-target
// and scaling-concurrency-target template annotations: --scaling-cpu-target
// and --scaling-concurrency-target set both targets, read back through the
// official run/v2 client; --scaling-concurrency-target=disabled, "to scale
// only by CPU", sets concurrencyUtilization 0.0; both disabled is refused,
// as gcloud's own help says ("CPU and concurrency scaling cannot both be
// disabled"). Through gcloud beta, as Google's scaling-controls page gives
// them: CI's gcloud 586.0.0 has the flags in the alpha and beta tracks
// only (main run 44326182935: "unrecognized arguments"), and 593.0.0 has
// them in beta as well as GA.
func TestGcloudRunServiceScalingControls(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // the scale shard only (#1821)
	c := runClient(t, h)
	g := newGcloudSession(t, h)
	id := "gc-svcctl"
	name := runParent(h) + "/services/" + id
	region, project := "--region=us-central1", "--project="+h.Project()
	g.must("run", "deploy", id, "--image="+helloDigest, region, project, "--no-allow-unauthenticated", "--quiet")
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	targets := func(when string, cpu, concurrency float32) {
		t.Helper()
		got, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, get: %v", when, err)
		}
		sc := got.GetTemplate().GetScaling()
		if sc.CpuUtilization == nil || sc.ConcurrencyUtilization == nil || sc.GetCpuUtilization() != cpu || sc.GetConcurrencyUtilization() != concurrency {
			t.Errorf("%s, template.scaling = %v; want cpuUtilization %v, concurrencyUtilization %v", when, sc, cpu, concurrency)
		}
	}
	g.must("beta", "run", "services", "update", id, region, project, "--scaling-cpu-target=0.7", "--scaling-concurrency-target=0.8", "--quiet")
	targets("after both targets", 0.7, 0.8)
	g.must("beta", "run", "services", "update", id, region, project, "--scaling-concurrency-target=disabled", "--quiet")
	targets("after --scaling-concurrency-target=disabled", 0.7, 0)
	if out, err := g.run(nil, "beta", "run", "services", "update", id, region, project, "--scaling-cpu-target=disabled", "--quiet"); err == nil {
		t.Errorf("disabling both drivers succeeded:\n%s", out)
	}
}
