//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// modelServerImage is the Model Registry's serving container fixture the
// compat suite builds (testdata/modelserver): with no artifacts its model
// is the identity, so a prediction is its instance.
const modelServerImage = "dev.local/cloudburrow-modelserver:test"

// loadModelServer builds the fixture and loads it into the instance's own
// cluster, as the compat suite does.
func loadModelServer(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"docker", "kind"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the model server fixture", bin)
		}
	}
	cluster := strings.TrimSpace(os.Getenv(envCluster))
	if cluster == "" {
		t.Skipf("%s is not set; the fixture is loaded into the instance's own cluster", envCluster)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		if filepath.Dir(dir) == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = filepath.Dir(dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "docker", "build", "-t", modelServerImage, "testdata/modelserver")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the model server fixture: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", modelServerImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the model server fixture into %s: %v\n%s", cluster, err, out)
	}
}

// TestModelRegistryAndEndpointsThroughThePages (#1940), in the run shard,
// whose instance serves Cloud Run and so the screens (a container start).
// From the drawer's Agent Platform row: Import on
// the Model Registry page uploads the serving container fixture, Create
// endpoint on the Endpoints page makes an endpoint, Deploy to endpoint on
// the model's page runs the container in the cluster, and on the
// endpoint's page the deployed model is listed with all the traffic, Test
// model answers the container's predictions, Edit compute nodes changes its
// replicas (Edit and Import evaluation on the model's page before), and
// Undeploy model, then
// Delete, remove everything; the console API reads each change back from
// the same server the SDK calls.
func TestModelRegistryAndEndpointsThroughThePages(t *testing.T) {
	needService(t, "vertex-models")
	needService(t, "vertex-endpoints")
	loadModelServer(t)
	project := uniqueProject(t)
	n := time.Now().UnixNano() % 1e6
	modelID := fmt.Sprintf("browser-linear-%d", n)
	epID := fmt.Sprintf("%d", 300000+n)
	model := "projects/" + project + "/locations/us-central1/models/" + modelID
	endpoint := "projects/" + project + "/locations/us-central1/endpoints/" + epID
	detail := func(service, name string) string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/"+service+"?project="+project+"&name="+url.QueryEscape(name), "")
		if code != http.StatusOK {
			t.Fatalf("read %s through the console API = %d: %s", name, code, body)
		}
		return body
	}
	t.Cleanup(func() {
		consoleDeploy(t, http.MethodDelete, "/api/resources/vertex-endpoints?project="+project+"&name="+url.QueryEscape(endpoint), "")
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-models?project="+project+"&name="+url.QueryEscape(model), "")
	})

	p := open(t)
	p.navigateByDrawer("AI and machine learning", "/agent-platform/overview", "Agent Platform")
	p.navigate("/agent-platform/models?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Model Registry")`)
	p.clickText("#view button", "Import")
	p.submitForm(map[string]string{"displayName": "browser-linear", "modelId": modelID, "imageUri": modelServerImage,
		"predictRoute": "/predict", "healthRoute": "/health", "port": "7080"}, "displayName")
	p.waitFor(fmt.Sprintf(`location.pathname === "/agent-platform/models" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, model))
	if !strings.Contains(detail("vertex-models", model), modelServerImage) {
		t.Fatalf("the imported model does not name its image")
	}

	p.navigate("/agent-platform/endpoints?project=" + project)
	p.clickText("#view button", "Create")
	p.submitForm(map[string]string{"displayName": "browser-endpoint", "endpointId": epID}, "displayName")
	p.waitFor(fmt.Sprintf(`location.pathname === "/agent-platform/endpoints" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, endpoint))

	p.navigate("/agent-platform/models?project=" + project)
	p.follow(model)
	p.waitFor(`document.querySelector("#view").textContent.includes("Versions")`)
	p.pageAction("Edit", map[string]string{"description": "edited on the page"}, "")
	if !strings.Contains(detail("vertex-models", model), "edited on the page") {
		t.Errorf("the model's description after Edit is not the one typed")
	}
	p.pageAction("Import evaluation", map[string]string{"displayName": "browser-eval", "metrics": `{"meanAbsoluteError": 0.25}`}, "")
	var md struct {
		Sections []struct {
			ID      string
			Listing struct{ Items []struct{ Name string } }
		}
	}
	_ = json.Unmarshal([]byte(detail("vertex-models", model)), &md)
	evalID := ""
	for _, sec := range md.Sections {
		if sec.ID == "evaluate" && len(sec.Listing.Items) == 1 {
			evalID = sec.Listing.Items[0].Name
		}
	}
	if evalID == "" {
		t.Fatalf("the Evaluate tab lists no evaluation after Import evaluation: %+v", md)
	}
	if rows := p.pageAction("View evaluation", map[string]string{"evaluation": evalID}, ""); !strings.Contains(rows, "0.25") {
		t.Errorf("View evaluation answered %q; want the metric imported", rows)
	}
	// The deploy answers once the model's container is ready, which on a
	// loaded machine takes longer than a dialog's usual 30 seconds.
	p.pageActionWithin("Deploy to endpoint", map[string]string{"endpoint": epID, "displayName": "browser-linear-v1"}, "", 4*time.Minute)
	var ep struct {
		Sections []struct {
			Listing struct {
				Items []struct {
					Name   string
					Fields map[string]string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(detail("vertex-endpoints", endpoint)), &ep); err != nil || len(ep.Sections) == 0 ||
		len(ep.Sections[0].Listing.Items) != 1 || ep.Sections[0].Listing.Items[0].Fields["Traffic split"] != "100%" {
		t.Fatalf("the endpoint after Deploy: %v %+v", err, ep)
	}
	dm := ep.Sections[0].Listing.Items[0].Name

	p.navigate("/agent-platform/endpoints?project=" + project)
	p.follow(endpoint)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, dm))
	rows := p.pageAction("Test model", map[string]string{"request": `{"instances": [4, 9]}`}, "")
	if !strings.Contains(rows, "4") || !strings.Contains(rows, "9") {
		t.Errorf("Test model answered %q; want the identity model's 4 and 9", rows)
	}
	p.pageAction("Edit compute nodes", map[string]string{"deployedModelId": dm, "minReplicas": "1", "maxReplicas": "2"}, "")
	if body := detail("vertex-endpoints", endpoint); !strings.Contains(body, "1–2") {
		t.Errorf("the deployed model's compute nodes after Edit compute nodes are not 1–2")
	}
	p.pageAction("Undeploy model from endpoint", map[string]string{"deployedModelId": dm}, "")
	if body := detail("vertex-endpoints", endpoint); strings.Contains(body, dm) {
		t.Errorf("the deployed model is still listed after Undeploy")
	}
	if code, body := consoleDo(t, http.MethodDelete, "/api/resources/vertex-endpoints?project="+project+"&name="+url.QueryEscape(endpoint), ""); code != http.StatusOK {
		t.Errorf("delete the endpoint = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodDelete, "/api/resources/vertex-models?project="+project+"&name="+url.QueryEscape(model), ""); code != http.StatusOK {
		t.Errorf("delete the model = %d: %s", code, body)
	}
}
