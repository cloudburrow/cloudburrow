//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformRun (#591): a google_cloud_run_v2_service and a
// google_cloud_run_v2_job apply through `cloudburrow terraform` with the
// official hashicorp/google provider, plan clean, take a change to a label
// and an environment variable as a PATCH, plan clean again, and destroy,
// all with egress blocked. The provider speaks Cloud Run's v2 JSON API,
// served by the shared transcoder on the adapter's gRPC port; the evidence
// of each step is the official gRPC client's read, and the event log shows
// the provider's requests arrived as JSON.
func TestTerraformRun(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	svcs := runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	jobs, _ := runJobClients(t, h)
	ctx := h.Context()
	control := h.Endpoint(EnvControl)
	// Knative names are the IDs, shared across projects, so the IDs carry
	// the test's unique suffix.
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	svcID, jobID := "tfsvc-"+suffix, "tfjob-"+suffix
	svc, job := runParent(h)+"/services/"+svcID, runParent(h)+"/jobs/"+jobID

	module := func(label, value string) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  labels              = { env = %[4]q }
  template {
    containers {
      image = "ghcr.io/knative/helloworld-go:latest"
      env {
        name  = "TARGET"
        value = %[5]q
      }
    }
  }
}
resource "google_cloud_run_v2_job" "j" {
  project             = %[1]q
  name                = %[3]q
  location            = "us-central1"
  deletion_protection = false
  labels              = { env = %[4]q }
  template {
    template {
      containers {
        image   = %[6]q
        command = ["sh", "-c", "echo hi"]
        env {
          name  = "TARGET"
          value = %[5]q
        }
      }
      max_retries = 1
    }
  }
}
`, h.Project(), svcID, jobID, label, value, jobImage)
	}
	// check reads both resources back through gRPC.
	check := func(when, label, value string) {
		t.Helper()
		s, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: svc})
		if err != nil {
			t.Fatalf("%s, GetService: %v", when, err)
		}
		if env := s.GetTemplate().GetContainers()[0].GetEnv(); s.GetLabels()["env"] != label ||
			len(env) != 1 || env[0].GetValue() != value || s.GetUri() == "" {
			t.Errorf("%s, the service = labels %v, env %v, uri %q", when, s.GetLabels(), env, s.GetUri())
		}
		j, err := jobs.GetJob(ctx, &runpb.GetJobRequest{Name: job})
		if err != nil {
			t.Fatalf("%s, GetJob: %v", when, err)
		}
		if env := j.GetTemplate().GetTemplate().GetContainers()[0].GetEnv(); j.GetLabels()["env"] != label ||
			len(env) != 1 || env[0].GetValue() != value {
			t.Errorf("%s, the job = labels %v, env %v", when, j.GetLabels(), env)
		}
	}

	began := time.Now()
	m.write(module("local", "tf"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "local", "tf")
	m.planClean()

	patches := time.Now()
	m.write(module("changed", "tf2"))
	m.apply()
	check("after the change", "changed", "tf2")
	var patched []string
	for _, e := range events(t, h, control, "run", patches) {
		if strings.HasPrefix(e.Target, "PATCH ") {
			patched = append(patched, e.Target)
		}
	}
	if want := []string{"PATCH /v2/" + svc, "PATCH /v2/" + job}; len(patched) != 2 ||
		!(patched[0] == want[0] && patched[1] == want[1] || patched[0] == want[1] && patched[1] == want[0]) {
		t.Errorf("the change sent %q; want a PATCH to each of %q", patched, want)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: svc}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after destroy: %v; want NotFound", err)
	}
	if _, err := jobs.GetJob(ctx, &runpb.GetJobRequest{Name: job}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy: %v; want NotFound", err)
	}

	// Every request the provider made was JSON, on the adapter's port: the
	// creates, the operation polls, the reads and the deletes.
	seen := map[string]bool{}
	for _, e := range events(t, h, control, "run", began) {
		if e.Detail["transport"] != "http" {
			continue
		}
		verb, path, _ := strings.Cut(e.Target, " ")
		switch {
		case strings.Contains(path, "/operations/"):
			seen["poll"] = true
		case verb == "POST" && strings.HasSuffix(path, "/services"):
			seen["create service"] = true
		case verb == "POST" && strings.HasSuffix(path, "/jobs"):
			seen["create job"] = true
		case verb == "DELETE" && path == "/v2/"+svc:
			seen["delete service"] = true
		case verb == "DELETE" && path == "/v2/"+job:
			seen["delete job"] = true
		}
	}
	for _, want := range []string{"create service", "create job", "poll", "delete service", "delete job"} {
		if !seen[want] {
			t.Errorf("the event log has no JSON request to %s", want)
		}
	}
}
