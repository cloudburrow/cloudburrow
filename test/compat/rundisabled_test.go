//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// serviceAnswers polls the service at host through the gateway until want
// says the status is the expected one, or two minutes pass; it returns the
// last status (0 when nothing answered).
func serviceAnswers(t *testing.T, base, host string, want func(int) bool) int {
	t.Helper()
	code := 0
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		code = 0
		if err == nil {
			code = resp.StatusCode
			_ = resp.Body.Close()
		}
		cancel()
		if want(code) {
			return code
		}
	}
	return code
}

// covers: google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Services/GetService
//
// TestRunServiceDisabledByManualZero (#1800): manual scaling with 0
// instances disables a service, as Google documents
// (https://docs.cloud.google.com/run/docs/configuring/services/manual-scaling):
// through the official run/v2 client, a masked update to MANUAL 0 reads
// back as set, keeps the revision and the URL, stops every instance, and
// its URL stops serving; MANUAL 1 serves again on the same revision.
func TestRunServiceDisabledByManualZero(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	base := ingress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	id := "svc-disabled"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Scaling:  &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(1)},
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uri, rev, host := svc.GetUri(), svc.GetLatestReadyRevision(), hostOf(t, svc.GetUri())
	if code := serviceAnswers(t, base, host, func(c int) bool { return c == http.StatusOK }); code != http.StatusOK {
		t.Fatalf("the service answers %d before it is disabled", code)
	}
	waitRevisionPods(ctx, t, kubeconfig, id, 1)

	scale := func(n int32) *runpb.Service {
		t.Helper()
		uop, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: &runpb.Service{Name: name,
			Scaling: &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(n)}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"scaling"}}})
		if err != nil {
			t.Fatalf("UpdateService MANUAL %d: %v", n, err)
		}
		if _, err := waitLRO(ctx, uop); err != nil {
			t.Fatalf("update MANUAL %d: %v", n, err)
		}
		got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		if sc := got.GetScaling(); sc.GetScalingMode() != runpb.ServiceScaling_MANUAL || sc.ManualInstanceCount == nil || sc.GetManualInstanceCount() != n {
			t.Errorf("scaling = %v; want MANUAL %d", sc, n)
		}
		if got.GetLatestReadyRevision() != rev || got.GetUri() != uri {
			t.Errorf("after MANUAL %d: revision %s, uri %s; want %s and %s, unchanged", n, got.GetLatestReadyRevision(), got.GetUri(), rev, uri)
		}
		return got
	}

	scale(0)
	if code := serviceAnswers(t, base, host, func(c int) bool { return c != 0 && c != http.StatusOK }); code == http.StatusOK || code == 0 {
		t.Errorf("the disabled service's URL answers %d; want a refusal", code)
	} else {
		t.Logf("the disabled service's URL answers %d", code)
	}
	waitRevisionPods(ctx, t, kubeconfig, id, 0)

	scale(1)
	if code := serviceAnswers(t, base, host, func(c int) bool { return c == http.StatusOK }); code != http.StatusOK {
		t.Errorf("the service answers %d after MANUAL 1; want 200", code)
	}
	waitRevisionPods(ctx, t, kubeconfig, id, 1)
}

// TestTerraformRunServiceDisabled (#1800): hashicorp/google's
// google_cloud_run_v2_service with scaling_mode MANUAL and
// manual_instance_count 0, Google's Terraform example of a disabled
// service, applies, reads back through the official client, and plans
// clean; a count of 1 changes it in place.
func TestTerraformRunServiceDisabled(t *testing.T) {
	h := New(t)
	c := runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	id := "tfsvcoff-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/services/" + id
	module := func(n int) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  scaling {
    scaling_mode          = "MANUAL"
    manual_instance_count = %[3]d
  }
  template {
    containers {
      image = %[4]q
    }
  }
}
`, h.Project(), id, n, helloDigest)
	}
	check := func(when string, n int32) {
		t.Helper()
		got, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, get: %v", when, err)
		}
		if sc := got.GetScaling(); sc.GetScalingMode() != runpb.ServiceScaling_MANUAL || sc.GetManualInstanceCount() != n {
			t.Errorf("%s, scaling = %v; want MANUAL %d", when, sc, n)
		}
	}
	m.write(module(0))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 0)
	m.planClean()
	m.write(module(1))
	m.apply()
	check("after the change", 1)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
