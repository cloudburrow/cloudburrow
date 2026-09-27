package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Cloud Run v2 is wired for Terraform (#591): its JSON API is served on the
// adapter's port, and TestTerraformRun applies, updates and destroys a
// google_cloud_run_v2_service and a google_cloud_run_v2_job through it.

func TestTerraformSetsRun(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceRun}
	body, skipped := terraformProvider(cfg, false)
	if slices.Contains(skipped, config.ServiceRun) {
		t.Errorf("run is still named as left out: %v", skipped)
	}
	if !strings.Contains(body, `cloud_run_v2_custom_endpoint = "http://127.0.0.1:9004/v2/"`) {
		t.Errorf("cloud_run_v2_custom_endpoint is not set to the local endpoint:\n%s", body)
	}
}

func TestEnvTerraformExportsRun(t *testing.T) {
	var out bytes.Buffer
	writeTerraformEnv(&out, formatsConfig(t, "run"))
	if !strings.Contains(out.String(), `GOOGLE_CLOUD_RUN_V2_CUSTOM_ENDPOINT="http://127.0.0.1:9004/v2/"`) {
		t.Errorf("GOOGLE_CLOUD_RUN_V2_CUSTOM_ENDPOINT is not the local endpoint:\n%s", out.String())
	}
}

// A Cloud Run JSON request, under /v2/, is recorded with its project, as a
// KMS or Secret Manager one under /v1/ is: the compat tests find the
// provider's requests by it.
func TestRequestEventsNameTheProjectUnderAnyVersion(t *testing.T) {
	rec := admin.NewRecorder(10, nil)
	for _, p := range []string{"/v2/projects/p2/locations/us-central1/services/s", "/v1/projects/p1/locations/global/keyRings"} {
		requestEvents(rec, nil, "run")(rest.Request{Method: "GET", Path: p, Status: 200})
	}
	var got []string
	for _, e := range rec.EventsWhere(admin.Filter{Service: "run", Kind: requestKind}, 0) {
		got = append(got, e.Detail["project"])
	}
	slices.Sort(got)
	if strings.Join(got, ",") != "p1,p2" {
		t.Errorf("projects = %q; want p1 and p2", got)
	}
}
