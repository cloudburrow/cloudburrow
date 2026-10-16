package console

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// fixedResolver resolves every endpoint to the doubler's predict URL, as the
// Cloud Run resolver does for a deployed contract container.
type fixedResolver struct{ url string }

func (f fixedResolver) PredictURL(_ context.Context, _, _, _ string) (string, error) { return f.url, nil }

// explainServer is a console whose Explain reaches the real PredictionService
// of a local AI endpoint, which calls the doubler behind the fake ingress.
func explainServer(t *testing.T) (*httptest.Server, *fakeIngress) {
	t.Helper()
	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	ing := &fakeIngress{host: "doubler.default.cloudburrow.localhost", routes: routes}
	gateway := ing.start(t)
	ai := vertexai.NewServer(nil)
	ai.SetEndpoints(fixedResolver{url: "http://doubler.default.cloudburrow.localhost/v1/predict"},
		func() string { return gateway }, nil)
	api := httptest.NewServer(ai.Handler())
	t.Cleanup(api.Close)
	addr := strings.TrimPrefix(api.URL, "http://")
	srv := newPredictServer(t, &Prediction{
		Source:  &fakePredictionSource{endpoints: []prediction.Endpoint{readyPredictor(routes)}},
		Ingress: func() string { return gateway },
		AI:      func() string { return addr },
	})
	return srv, ing
}

func explain(t *testing.T, srv *httptest.Server, req map[string]any) (int, ExplainResult, string) {
	t.Helper()
	b, _ := json.Marshal(req)
	resp, got := postJSON(t, srv, "/api/ai/predict/explain?project=p1", string(b))
	var r ExplainResult
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(got), &r); err != nil {
			t.Fatalf("decode result: %v: %s", err, got)
		}
	}
	return resp.StatusCode, r, got
}

func attributionOf(t *testing.T, r ExplainResult, feature string) float64 {
	t.Helper()
	if len(r.Explanations) == 0 || len(r.Explanations[0].Attributions) == 0 {
		t.Fatalf("no attributions in %+v", r)
	}
	for _, f := range r.Explanations[0].Attributions[0].Features {
		if f.Feature == feature {
			var x float64
			if err := json.Unmarshal(f.Attribution, &x); err != nil {
				t.Fatalf("attribution %s = %s", feature, f.Attribution)
			}
			return x
		}
	}
	t.Fatalf("no attribution for %q in %+v", feature, r.Explanations[0].Attributions[0])
	return 0
}

// Explain from the Online prediction screen sends the ExplainRequest the
// official client sends — the spec as explanation_spec_override, the
// baselines as metadata.inputs[feature].input_baselines — to the local AI
// endpoint's PredictionService, and shows its attributions per feature and
// its body verbatim; the API's refusals come back in its words, and what the
// console refuses itself is never sent.
func TestExplainSendsTheSpecOverrideAndShowsAttributionsPerFeature(t *testing.T) {
	srv, ing := explainServer(t)

	_, body := getJSON(t, srv, "/api/ai/predict?project=p1")
	var st PredictStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.Explain {
		t.Fatalf("the page does not offer Explain: %s", body)
	}
	for _, want := range []string{"integrated gradients", "XRAI", "Example-based", "explanation_spec_override"} {
		if !strings.Contains(strings.Join(st.NotServed, "\n"), want) {
			t.Errorf("the page does not name %q as not served: %v", want, st.NotServed)
		}
	}

	code, r, raw := explain(t, srv, map[string]any{"endpoint": predictorName, "instances": "[3]", "pathCount": 5, "baselines": `{"x": []}`})
	if code != http.StatusOK || r.Status != http.StatusOK {
		t.Fatalf("explain = %d: %s", code, raw)
	}
	wantReq := `{"instances":[3],"explanationSpecOverride":{"metadata":{"inputs":{"x":{"inputBaselines":[]}}},` +
		`"parameters":{"sampledShapleyAttribution":{"pathCount":5}}}}`
	if r.Request != wantReq || !strings.HasSuffix(r.URL, "/v1/projects/p1/locations/us-central1/endpoints/doubler:explain") {
		t.Errorf("sent %s to %s; want %s", r.Request, r.URL, wantReq)
	}
	if r.DeployedModelID != "doubler-v1" || len(r.Explanations) != 1 || string(r.Explanations[0].Prediction) != "6" {
		t.Errorf("result = %+v", r)
	}
	a := r.Explanations[0].Attributions[0]
	if a.Baseline != 0 || a.Output != 6 || math.Abs(attributionOf(t, r, "x")-6) > 1e-9 {
		t.Errorf("attribution = %+v, want x: 6 from 0 to 6", a)
	}
	if !strings.Contains(r.Body, `"featureAttributions"`) {
		t.Errorf("the body is not the API's ExplainResponse: %s", r.Body)
	}
	// The API reached the container through the ingress with the perturbed
	// instances: the instance itself, the baseline, and one per path.
	ing.mu.Lock()
	sent := append([]string(nil), ing.requests...)
	ing.mu.Unlock()
	if len(sent) != 1 || !strings.Contains(sent[0], `/v1/predict {"instances":[3,0,3,3,3,3,3]`) {
		t.Errorf("the container saw %q", sent)
	}

	// A baseline as the API takes it: from 1 (output 2) to 3 (output 6).
	_, r, raw = explain(t, srv, map[string]any{"endpoint": "doubler", "instances": "[3]", "pathCount": 2, "baselines": `{"x": [1]}`})
	if r.Status != http.StatusOK || r.Explanations[0].Attributions[0].Baseline != 2 || math.Abs(attributionOf(t, r, "x")-4) > 1e-9 {
		t.Errorf("with a baseline of 1: %s", raw)
	}

	// The API's refusals, in its words.
	for _, tc := range []struct {
		name      string
		req       map[string]any
		status    string
		wantInMsg string
	}{
		{"path count out of range", map[string]any{"endpoint": "doubler", "instances": "[3]", "pathCount": 0, "baselines": `{"x": []}`},
			"INVALID_ARGUMENT", "path_count"},
		{"no feature named", map[string]any{"endpoint": "doubler", "instances": "[3]", "pathCount": 1, "baselines": `{}`},
			"INVALID_ARGUMENT", "metadata.inputs is empty"},
	} {
		code, r, raw := explain(t, srv, tc.req)
		if code != http.StatusOK || r.Status != http.StatusBadRequest || r.ErrorStatus != tc.status || !strings.Contains(r.Error, tc.wantInMsg) {
			t.Errorf("%s: = %d %s; want the API's 400 %s containing %q", tc.name, code, raw, tc.status, tc.wantInMsg)
		}
	}

	// What the console refuses itself.
	before := len(sent)
	for _, tc := range []struct {
		name string
		req  map[string]any
		code int
		want string
	}{
		{"no endpoint", map[string]any{"instances": "[1]", "pathCount": 1, "baselines": `{"x":[]}`}, http.StatusBadRequest, "choose an endpoint"},
		{"no baselines", map[string]any{"endpoint": "doubler", "instances": "[1]", "pathCount": 1}, http.StatusBadRequest, "feature baselines are required"},
		{"baselines not an object", map[string]any{"endpoint": "doubler", "instances": "[1]", "pathCount": 1, "baselines": `[0]`}, http.StatusBadRequest, "JSON object"},
		{"a baseline not a list", map[string]any{"endpoint": "doubler", "instances": "[1]", "pathCount": 1, "baselines": `{"x": 0}`}, http.StatusBadRequest, "must be a JSON list"},
		{"instances not an array", map[string]any{"endpoint": "doubler", "instances": "{}", "pathCount": 1, "baselines": `{"x":[]}`}, http.StatusBadRequest, "must be a JSON array"},
		{"not deployed", map[string]any{"endpoint": "nosuch", "instances": "[1]", "pathCount": 1, "baselines": `{"x":[]}`}, http.StatusNotFound, "is deployed"},
	} {
		if code, _, raw := explain(t, srv, tc.req); code != tc.code || !strings.Contains(raw, tc.want) {
			t.Errorf("%s: = %d %s; want %d containing %q", tc.name, code, raw, tc.code, tc.want)
		}
	}
	ing.mu.Lock()
	after := len(ing.requests)
	ing.mu.Unlock()
	if after != before+1 { // the one baseline-of-1 request; the API refusals send nothing
		t.Errorf("the container saw %d more requests, want 1", after-before)
	}
}

// Without the local AI endpoint the page says Explain is not available, and
// the route refuses to send.
func TestExplainWithoutTheLocalAIEndpointIsNotOffered(t *testing.T) {
	routes := prediction.DefaultRoutes()
	srv := newPredictServer(t, &Prediction{
		Source:  &fakePredictionSource{endpoints: []prediction.Endpoint{readyPredictor(routes)}},
		Ingress: func() string { return "127.0.0.1:1" },
	})
	_, body := getJSON(t, srv, "/api/ai/predict?project=p1")
	var st PredictStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Explain || !strings.Contains(st.ExplainNote, "local AI endpoint") {
		t.Errorf("the page reads %+v", st)
	}
	resp, got := postJSON(t, srv, "/api/ai/predict/explain?project=p1", `{"endpoint":"doubler","instances":"[1]","pathCount":1,"baselines":"{\"x\":[]}"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("explain with no local AI endpoint = %d: %s", resp.StatusCode, got)
	}
	if resp, got := postJSON(t, newPredictServer(t, nil), "/api/ai/predict/explain", `{}`); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("explain with no Cloud Run = %d: %s", resp.StatusCode, got)
	}
}
