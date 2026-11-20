//go:build compat

package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.aiplatform.v1.ModelService/UploadModel, google.cloud.aiplatform.v1.ModelService/GetModel, google.cloud.aiplatform.v1.ModelService/ListModels, google.cloud.aiplatform.v1.ModelService/DeleteModel, google.cloud.aiplatform.v1.EndpointService/CreateEndpoint, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.EndpointService/ListEndpoints, google.cloud.aiplatform.v1.EndpointService/DeployModel, google.cloud.aiplatform.v1.EndpointService/UndeployModel, google.cloud.aiplatform.v1.EndpointService/DeleteEndpoint, google.cloud.aiplatform.v1.PredictionService/Predict
//
// TestGcloudAIModelsAndEndpoints (#1940): `gcloud ai models upload`
// registers the custom serving container fixture, `gcloud ai endpoints
// create` and `deploy-model` run it in the cluster, `predict` answers
// through it, and `undeploy-model`, `endpoints delete` and `models delete`
// remove everything, through the configuration `cloudburrow gcloud-setup`
// writes (the aiplatform override) with egress to Google blocked.
func TestGcloudAIModelsAndEndpoints(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // a container start: the scale shard (#1821)
	h.Endpoint(EnvPrediction)
	g := newGcloudSession(t, h)
	buildModelServer(t, h.Context())
	n := time.Now().UnixNano() % 1e6
	model, endpoint := fmt.Sprintf("gcloud-linear-%d", n), fmt.Sprintf("%d", 100000+n)
	loc := []string{"--region", "us-central1", "--project", g.project, "--quiet"}
	run := func(args ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"ai"}, args...), loc...)...)
		if err != nil {
			t.Fatalf("gcloud ai %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	run("models", "upload", "--model-id", model, "--display-name", "gcloud-linear", "--container-image-uri", modelServerImage,
		"--container-predict-route", "/predict", "--container-health-route", "/health", "--container-ports", "7080",
		"--container-env-vars", "MODEL_FLAVOUR=linear")
	defer g.run(nil, append([]string{"ai", "models", "delete", model}, loc...)...)
	if out := run("models", "describe", model, "--format", "value(containerSpec.imageUri,versionAliases)"); !strings.Contains(out, modelServerImage) ||
		!strings.Contains(out, "default") {
		t.Errorf("models describe: %s", out)
	}
	if out := run("models", "list", "--format", "value(name)"); !strings.Contains(out, model) {
		t.Errorf("models list: %s", out)
	}
	run("endpoints", "create", "--endpoint-id", endpoint, "--display-name", "gcloud-endpoint")
	defer g.run(nil, append([]string{"ai", "endpoints", "delete", endpoint}, loc...)...)
	if out := run("endpoints", "list", "--format", "value(name)"); !strings.Contains(out, endpoint) {
		t.Errorf("endpoints list: %s", out)
	}
	run("endpoints", "deploy-model", endpoint, "--model", model, "--display-name", "gcloud-linear",
		"--machine-type", "n1-standard-2", "--traffic-split", "0=100")
	desc := run("endpoints", "describe", endpoint, "--format", "value(deployedModels[0].id)")
	// gcloud ai prints "Using endpoint [...]" first, on stderr.
	lines := strings.Fields(strings.TrimSpace(desc))
	dm := ""
	if len(lines) > 0 {
		dm = lines[len(lines)-1]
	}
	if dm == "" {
		t.Fatalf("endpoints describe: no deployed model\n%s", desc)
	}
	req := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(req, []byte(`{"instances":[2,5]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No artifacts: the fixture's model is the identity.
	out := run("endpoints", "predict", endpoint, "--json-request", req, "--format", "json")
	if !strings.Contains(out, dm) || !strings.Contains(out, "2") || !strings.Contains(out, "5") {
		t.Errorf("endpoints predict: %s", out)
	}
	run("endpoints", "undeploy-model", endpoint, "--deployed-model-id", dm)
	run("endpoints", "delete", endpoint)
	run("models", "delete", model)
	_, ec, _ := modelRegistryClients(t, h)
	if _, err := ec.GetEndpoint(h.Context(), &aiplatformpb.GetEndpointRequest{
		Name: "projects/" + g.project + "/locations/us-central1/endpoints/" + endpoint}); status.Code(err) != codes.NotFound {
		t.Errorf("the endpoint after delete: %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.aiplatform.v1.EndpointService/CreateEndpoint, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.EndpointService/UpdateEndpoint, google.cloud.aiplatform.v1.EndpointService/DeleteEndpoint
//
// TestTerraformVertexAIEndpoint (#1940): a google_vertex_ai_endpoint
// applies with the official hashicorp/google provider through `cloudburrow
// terraform` (vertex_ai_custom_endpoint), is read back through the official
// client, plans clean, updates its labels in place and destroys.
func TestTerraformVertexAIEndpoint(t *testing.T) {
	h := New(t)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	_, ec, _ := modelRegistryClients(t, h)
	m := newTFModule(t)
	id := fmt.Sprintf("%d", 200000+time.Now().UnixNano()%1e6)
	body := func(label string) string {
		return fmt.Sprintf(`
resource "google_vertex_ai_endpoint" "e" {
  project      = %q
  location     = "us-central1"
  region       = "us-central1"
  name         = %q
  display_name = "tf-endpoint"
  description  = "an endpoint from Terraform"
  labels = {
    stage = %q
  }
}
`, m.project, id, label)
	}
	m.write(body("dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/us-central1/endpoints/" + id
	e, err := ec.GetEndpoint(h.Context(), &aiplatformpb.GetEndpointRequest{Name: name})
	if err != nil || e.GetDisplayName() != "tf-endpoint" || e.GetLabels()["stage"] != "dev" {
		t.Fatalf("the endpoint: %v %v", err, e)
	}
	m.planClean()
	m.write(body("prod"))
	m.apply()
	if e, err := ec.GetEndpoint(h.Context(), &aiplatformpb.GetEndpointRequest{Name: name}); err != nil || e.GetLabels()["stage"] != "prod" {
		t.Errorf("after the label change: %v %v", err, e)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ec.GetEndpoint(h.Context(), &aiplatformpb.GetEndpointRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("the endpoint survived destroy: %v", err)
	}
}
