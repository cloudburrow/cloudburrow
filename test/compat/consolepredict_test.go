//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// consolePredictStatus is the console's GET /api/ai/predict, as much as these
// tests read.
type consolePredictStatus struct {
	Configured  bool
	Unavailable string
	Deploy      string
	Endpoints   []struct {
		Name, ID, State, PredictRoute, HealthRoute, PredictURL string
	}
}

// consolePredictResult is the console's answer to POST /api/ai/predict.
type consolePredictResult struct {
	URL, Request, Body, ContractError string
	Status                            int
}

func consolePredict(t *testing.T, addr, project string, req map[string]string) (int, consolePredictResult, string) {
	t.Helper()
	body, _ := json.Marshal(req)
	code, got := consoleDo(t, addr, http.MethodPost, "/api/ai/predict?"+url.Values{"project": {project}}.Encode(), string(body))
	var r consolePredictResult
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(got), &r); err != nil {
			t.Fatalf("decode the console's prediction result: %v: %s", err, got)
		}
	}
	return code, r, got
}

// covers: google.cloud.run.v2.Services/ListServices
//
// TestConsoleOnlinePredictionThroughTheConsoleAPI (#869): a Vertex custom
// prediction container deployed through the official Cloud Run SDK, on
// non-default AIP_* routes, is listed by the console's Online prediction page
// with those routes and READY; a prediction sent through the console reaches
// the container through the cluster ingress and returns its answer verbatim
// (one prediction per instance, its deployedModelId); the container's own
// refusals of a malformed request come back as its status and body, not the
// console's; and the console refuses an endpoint that is not deployed and
// instances that are not a JSON array without sending anything.
func TestConsoleOnlinePredictionThroughTheConsoleAPI(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	buildPredictor(t, h.Context())
	project := h.Project()

	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	svc := deployPredictor(t, c, h, "compat-console-predictor", routes, nil)

	code, body := consoleDo(t, addr, http.MethodGet, "/api/services", "")
	if code != http.StatusOK || !strings.Contains(body, `"ai-predict"`) {
		t.Fatalf("the console does not offer Online prediction with Cloud Run enabled: %d %s", code, body)
	}
	code, body = consoleDo(t, addr, http.MethodGet, "/api/ai/predict?project="+project, "")
	var st consolePredictStatus
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("GET /api/ai/predict = %d (%v): %s", code, err, body)
	}
	if !st.Configured || st.Unavailable != "" {
		t.Fatalf("the Online prediction page reads %+v", st)
	}
	found := false
	for _, e := range st.Endpoints {
		if e.Name != svc.GetName() {
			continue
		}
		found = true
		if e.ID != "compat-console-predictor" || e.State != "READY" || e.PredictRoute != "/v1/predict" ||
			e.HealthRoute != "/healthz" || e.PredictURL != svc.GetUri()+"/v1/predict" {
			t.Errorf("the console lists the endpoint as %+v; want READY on the service's AIP_* routes at %s", e, svc.GetUri())
		}
	}
	if !found {
		t.Fatalf("the console does not list %s among %+v", svc.GetName(), st.Endpoints)
	}

	code, r, raw := consolePredict(t, addr, project, map[string]string{
		"endpoint": svc.GetName(), "instances": "[1, 2, 3.5]", "parameters": `{"delayMs": 0}`,
	})
	if code != http.StatusOK || r.Status != http.StatusOK {
		t.Fatalf("a prediction through the console = %d: %s", code, raw)
	}
	var resp prediction.Response
	if err := json.Unmarshal([]byte(r.Body), &resp); err != nil {
		t.Fatalf("the container's answer is not a prediction response: %v: %q", err, r.Body)
	}
	want := []string{"2", "4", "7"}
	if len(resp.Predictions) != len(want) {
		t.Fatalf("predictions = %s, want %v", r.Body, want)
	}
	for i, p := range resp.Predictions {
		if strings.TrimSpace(string(p)) != want[i] {
			t.Errorf("prediction[%d] = %s, want %s", i, p, want[i])
		}
	}
	if resp.DeployedModelID != "doubler-v1" || r.ContractError != "" {
		t.Errorf("deployedModelId = %q, contract error %q", resp.DeployedModelID, r.ContractError)
	}
	if r.Request != `{"instances":[1,2,3.5],"parameters":{"delayMs":0}}` || r.URL != svc.GetUri()+"/v1/predict" {
		t.Errorf("the console sent %s to %s", r.Request, r.URL)
	}

	// The container's refusals, in its own words and with its own status.
	for _, tc := range []struct{ name, instances, want string }{
		{"no instances", "[]", "at least one instance is required"},
		{"wrong instance type", `["abc"]`, "instances must be numbers"},
	} {
		code, r, raw := consolePredict(t, addr, project, map[string]string{"endpoint": "compat-console-predictor", "instances": tc.instances})
		if code != http.StatusOK {
			t.Fatalf("%s: the console refused to send it: %d %s", tc.name, code, raw)
		}
		if r.Status != http.StatusBadRequest || !strings.Contains(r.Body, tc.want) {
			t.Errorf("%s: the container's answer came back as %d %q; want its 400 containing %q", tc.name, r.Status, r.Body, tc.want)
		}
	}

	// What the console refuses itself.
	for _, tc := range []struct {
		name string
		req  map[string]string
		code int
		want string
	}{
		{"not deployed", map[string]string{"endpoint": "compat-no-such-predictor", "instances": "[1]"}, http.StatusNotFound, "is deployed"},
		{"not an array", map[string]string{"endpoint": "compat-console-predictor", "instances": `{"x":1}`}, http.StatusBadRequest, "must be a JSON array"},
	} {
		if code, _, raw := consolePredict(t, addr, project, tc.req); code != tc.code || !strings.Contains(raw, tc.want) {
			t.Errorf("%s: = %d %s; want %d containing %q", tc.name, code, raw, tc.code, tc.want)
		}
	}
}
