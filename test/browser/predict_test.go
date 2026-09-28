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

// predictorImage is the Vertex custom prediction container fixture the
// compat suite's prediction tests build (testdata/predictor): it doubles each
// numeric instance. dev.local, because Knative resolves any other tag against
// a registry.
const predictorImage = "dev.local/cloudburrow-predictor:test"

// envCluster names the instance's kind cluster, which the fixture is loaded
// into; the fixture is never pushed anywhere.
const envCluster = "CLOUDBURROW_TEST_CLUSTER"

// loadPredictor builds the prediction fixture and loads it into the
// instance's own cluster, as the compat suite does. It skips without docker,
// kind or the cluster's name.
func loadPredictor(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"docker", "kind"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the prediction fixture", bin)
		}
	}
	cluster := strings.TrimSpace(os.Getenv(envCluster))
	if cluster == "" {
		t.Skipf("%s is not set; the prediction fixture is loaded into the instance's own cluster", envCluster)
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
	build := exec.CommandContext(ctx, "docker", "build", "-t", predictorImage, "testdata/predictor")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the prediction fixture: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", predictorImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the prediction fixture into %s: %v\n%s", cluster, err, out)
	}
}

// TestCloudRunPredictorAnswersOnlinePredictionInTheBrowser (#869): a
// prediction container deployed through the console's Cloud Run form, with
// the contract's AIP_* variables, is offered on Vertex AI's Online prediction
// page with its own predict route. Instances and parameters typed into the
// page are sent with one POST, and the container's answer is shown verbatim
// with its status; the container's own 400 for a malformed instance is shown
// the same way, as its body; and instances that are not a JSON array are
// refused by the console, on the form, with nothing shown as a response.
func TestCloudRunPredictorAnswersOnlinePredictionInTheBrowser(t *testing.T) {
	needService(t, "ai-predict")
	loadPredictor(t)
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-predictor-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us-central1/services/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})
	body, _ := json.Marshal(map[string]string{"name": id, "image": predictorImage,
		"env": `{"AIP_HTTP_PORT":"8080","AIP_HEALTH_ROUTE":"/healthz","AIP_PREDICT_ROUTE":"/v1/predict"}`})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run?project="+project, string(body))
	if code != http.StatusOK && !(code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time")) {
		t.Fatalf("deploy the predictor through the console API = %d: %s", code, resp)
	}
	// Ready as the Online prediction page reports it, which is what the
	// page's select is drawn from.
	var last string
	for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(3 * time.Second) {
		_, body := consoleDo(t, http.MethodGet, "/api/ai/predict?"+url.Values{"project": {project}}.Encode(), "")
		var st struct {
			Endpoints []struct{ Name, State string }
		}
		_ = json.Unmarshal([]byte(body), &st)
		ready := false
		for _, e := range st.Endpoints {
			ready = ready || (e.Name == name && e.State == "READY")
		}
		if ready {
			break
		}
		last = body
		if time.Now().After(deadline) {
			t.Fatalf("the predictor never read READY on the Online prediction page: %s", last)
		}
	}

	p := open(t)
	p.navigate("/ai/predict?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#predict-endpoint option")].some((o) => o.value === %q)`, name))
	var pages []string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &pages)
	if !contains(pages, "Online prediction") || !contains(pages, "Model Garden") {
		t.Errorf("Vertex AI's pages are %v; want Online prediction beside Model Garden", pages)
	}
	p.setField("#predict-endpoint", name)
	var facts string
	p.eval(`document.querySelector("#predict-endpoint-facts").textContent`, &facts)
	if !strings.Contains(facts, "/v1/predict") || !strings.Contains(facts, "/healthz") || !strings.Contains(facts, "READY") {
		t.Errorf("the endpoint's facts read %q; want its AIP_* routes and READY", facts)
	}

	send := func(instances, parameters string) {
		t.Helper()
		p.setField("#predict-instances", instances)
		p.setField("#predict-parameters", parameters)
		p.eval(`(() => { const r = document.querySelector("#predict-response"); r.textContent = "";
			document.querySelector("#predict-result").hidden = true; return true; })()`, nil)
		p.clickText("#predict-form button", "Predict")
		p.waitFor(`!document.querySelector("#predict-send").disabled &&
			(!document.querySelector("#predict-result").hidden || !document.querySelector("#predict-error").hidden)`)
	}
	type shown struct {
		Status, Response, Request, Error string
		ResultShown, ErrorShown          bool
	}
	read := func() shown {
		t.Helper()
		var s shown
		p.eval(`(() => { const q = (s) => document.querySelector(s);
			return { Status: q("#predict-status").textContent, Response: q("#predict-response").textContent,
			         Request: q("#predict-request").textContent, Error: q("#predict-error").textContent,
			         ResultShown: !q("#predict-result").hidden, ErrorShown: !q("#predict-error").hidden }; })()`, &s)
		return s
	}

	send("[1, 2, 3.5]", `{"delayMs": 0}`)
	got := read()
	if !got.ResultShown || !strings.Contains(got.Status, "HTTP 200") ||
		got.Response != `{"predictions":[2,4,7],"deployedModelId":"doubler-v1"}`+"\n" {
		t.Errorf("the page shows %+v; want HTTP 200 and the container's body verbatim", got)
	}
	if !strings.Contains(got.Request, `{"instances":[1,2,3.5],"parameters":{"delayMs":0}}`) || !strings.Contains(got.Request, "/v1/predict") {
		t.Errorf("the page says it sent %q", got.Request)
	}
	if sent := p.sent(http.MethodPost, "/api/ai/predict"); len(sent) != 1 {
		t.Errorf("Predict sent %d requests, want one: %v", len(sent), sent)
	}

	send(`["abc"]`, "")
	got = read()
	if !got.ResultShown || !strings.Contains(got.Status, "HTTP 400") || got.Response != `{"error":"instances must be numbers"}` {
		t.Errorf("the container's refusal shows as %+v; want its 400 and its body verbatim", got)
	}

	send(`{"x": 1}`, "")
	got = read()
	if !got.ErrorShown || !strings.Contains(got.Error, "must be a JSON array") || got.ResultShown {
		t.Errorf("the console's own refusal shows as %+v; want it on the form and no response", got)
	}
	// The refusal's 400 is the page's own network log, provoked on purpose.
	p.forgive()
}
