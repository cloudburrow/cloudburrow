package prediction

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeRequestRejectsMalformed(t *testing.T) {
	t.Parallel()
	bad := map[string]string{
		"not json":        `{`,
		"no instances":    `{"parameters":{}}`,
		"empty instances": `{"instances":[]}`,
		"unknown field":   `{"instances":[1],"surprise":2}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeRequest([]byte(body)); !errors.Is(err, ErrMalformedRequest) {
				t.Errorf("DecodeRequest(%s) = %v, want ErrMalformedRequest", body, err)
			}
		})
	}
}

func TestDecodeRequestAcceptsValid(t *testing.T) {
	t.Parallel()
	req, err := DecodeRequest([]byte(`{"instances":[{"x":1},{"x":2}],"parameters":{"k":"v"}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Instances) != 2 {
		t.Errorf("instances = %d, want 2", len(req.Instances))
	}
}

// Vertex requires one prediction per instance. A mismatch means the caller
// cannot match predictions to inputs, which is worse than an error.
func TestValidateResponseRequiresOnePredictionPerInstance(t *testing.T) {
	t.Parallel()
	req := Request{Instances: []json.RawMessage{[]byte(`1`), []byte(`2`)}}

	if err := ValidateResponse(req, Response{Predictions: []json.RawMessage{[]byte(`1`), []byte(`2`)}}); err != nil {
		t.Errorf("matching counts rejected: %v", err)
	}
	err := ValidateResponse(req, Response{Predictions: []json.RawMessage{[]byte(`1`)}})
	if err == nil {
		t.Fatal("a short response was accepted; predictions could not be matched to inputs")
	}
	if !strings.Contains(err.Error(), "one per instance") {
		t.Errorf("error should explain the contract, got: %v", err)
	}
}

func TestRoutesEnvUsesContractNames(t *testing.T) {
	t.Parallel()
	env := DefaultRoutes().Env()
	for k, want := range map[string]string{
		EnvPort:         "8080",
		EnvHealthRoute:  "/health",
		EnvPredictRoute: "/predict",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}

// Vertex treats any 2xx as healthy; a redirect is a misconfiguration and must
// not be followed into looking healthy.
func TestHealthyStatus(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]bool{200: true, 204: true, 299: true, 302: false, 404: false, 500: false} {
		if got := HealthyStatus(code); got != want {
			t.Errorf("HealthyStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// serveFixture wires a predictor for testing.
func serveFixture(t *testing.T, predict func(Request) (Response, error)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	Serve(mux, DefaultRoutes(), predict)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func echoPredictor(req Request) (Response, error) {
	out := make([]json.RawMessage, len(req.Instances))
	copy(out, req.Instances)
	return Response{Predictions: out, DeployedModelID: "fixture"}, nil
}

func TestServeHealthAndPredict(t *testing.T) {
	t.Parallel()
	srv := serveFixture(t, echoPredictor)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !HealthyStatus(resp.StatusCode) {
		t.Errorf("health = %d, want 2xx", resp.StatusCode)
	}

	r, err := http.Post(srv.URL+"/predict", "application/json", strings.NewReader(`{"instances":[{"a":1},{"a":2}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("predict = %d, want 200", r.StatusCode)
	}
	var out Response
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Predictions) != 2 {
		t.Errorf("predictions = %d, want 2", len(out.Predictions))
	}
	if out.DeployedModelID != "fixture" {
		t.Errorf("deployedModelId = %q", out.DeployedModelID)
	}
}

func TestServeRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	srv := serveFixture(t, echoPredictor)
	for _, body := range []string{`{`, `{"instances":[]}`, `{"nope":1}`} {
		r, err := http.Post(srv.URL+"/predict", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", body, r.StatusCode)
		}
	}
}

func TestServeRejectsWrongMethod(t *testing.T) {
	t.Parallel()
	srv := serveFixture(t, echoPredictor)
	r, err := http.Get(srv.URL + "/predict")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /predict = %d, want 405", r.StatusCode)
	}
}

// A predictor that returns the wrong number of predictions must be caught
// here, not by the caller mis-attributing results.
func TestServeCatchesABrokenPredictor(t *testing.T) {
	t.Parallel()
	srv := serveFixture(t, func(Request) (Response, error) {
		return Response{Predictions: []json.RawMessage{[]byte(`"only one"`)}}, nil
	})
	r, err := http.Post(srv.URL+"/predict", "application/json", strings.NewReader(`{"instances":[1,2,3]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusInternalServerError {
		t.Errorf("mismatched prediction count = %d, want 500", r.StatusCode)
	}
}

func TestServePropagatesPredictorFailure(t *testing.T) {
	t.Parallel()
	srv := serveFixture(t, func(Request) (Response, error) {
		return Response{}, errors.New("model failed to load")
	})
	r, err := http.Post(srv.URL+"/predict", "application/json", strings.NewReader(`{"instances":[1]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", r.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !strings.Contains(body["error"], "model failed to load") {
		t.Errorf("the predictor's failure was lost: %v", body)
	}
}
