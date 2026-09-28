package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// fakePredictionSource is the Cloud Run API's view of the deployed
// prediction containers.
type fakePredictionSource struct {
	endpoints []prediction.Endpoint
	err       error

	mu       sync.Mutex
	projects []string
}

func (f *fakePredictionSource) PredictionEndpoints(_ context.Context, project string) ([]prediction.Endpoint, error) {
	f.mu.Lock()
	f.projects = append(f.projects, project)
	f.mu.Unlock()
	return f.endpoints, f.err
}

// fakeIngress stands in for the Knative gateway: it routes on the Host
// header, and the routed host serves the contract through prediction.Serve,
// doubling each numeric instance as the compat fixture does. A request with
// any other host is a 404, as the gateway answers it.
type fakeIngress struct {
	host   string
	routes prediction.Routes
	// raw, when set, replaces the predictor's answer on the predict route.
	raw string

	mu       sync.Mutex
	requests []string
}

func (f *fakeIngress) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	prediction.Serve(mux, f.routes, func(req prediction.Request) (prediction.Response, error) {
		out := make([]json.RawMessage, 0, len(req.Instances))
		for _, in := range req.Instances {
			var n float64
			if err := json.Unmarshal(in, &n); err != nil {
				return prediction.Response{}, errors.New("instance is not a number: " + string(in))
			}
			b, _ := json.Marshal(n * 2)
			out = append(out, b)
		}
		return prediction.Response{Predictions: out, DeployedModelID: "doubler-v1"}, nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Host+" "+r.URL.Path+" "+string(body))
		f.mu.Unlock()
		if r.Host != f.host {
			http.NotFound(w, r)
			return
		}
		if f.raw != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.raw))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func newPredictServer(t *testing.T, p *Prediction) *httptest.Server {
	t.Helper()
	s := New("", func(context.Context) Status { return Status{} })
	s.SetPrediction(p)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

const predictorName = "projects/p1/locations/us-central1/services/doubler"

func readyPredictor(routes prediction.Routes) prediction.Endpoint {
	return prediction.NewEndpoint(predictorName, routes, prediction.ConditionSource{
		Ready: "True", Replicas: 1, URI: "http://doubler.default.cloudburrow.localhost",
	})
}

func predict(t *testing.T, srv *httptest.Server, body string) (int, PredictResult, string) {
	t.Helper()
	resp, got := postJSON(t, srv, "/api/ai/predict?project=p1", body)
	var r PredictResult
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(got), &r); err != nil {
			t.Fatalf("decode result: %v: %s", err, got)
		}
	}
	return resp.StatusCode, r, got
}

// Without Cloud Run the page is not advertised, says why, and refuses to send.
func TestConsoleWithoutCloudRunDoesNotOfferOnlinePrediction(t *testing.T) {
	srv := newPredictServer(t, nil)
	if _, body := getJSON(t, srv, "/api/services"); strings.Contains(body, "ai-predict") {
		t.Errorf("online prediction is advertised with no Cloud Run: %s", body)
	}
	_, body := getJSON(t, srv, "/api/ai/predict")
	var st PredictStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Configured || !strings.Contains(st.Note, "--services") {
		t.Errorf("an unconfigured page reads %+v", st)
	}
	if resp, got := postJSON(t, srv, "/api/ai/predict", `{"endpoint":"x","instances":"[1]"}`); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("predict with no Cloud Run = %d: %s", resp.StatusCode, got)
	}
}

// The page lists what the source reports, with each endpoint's own routes,
// and with none says how to deploy one and names what is not served.
func TestPredictStatusListsDeployedEndpoints(t *testing.T) {
	src := &fakePredictionSource{}
	srv := newPredictServer(t, &Prediction{Source: src, Ingress: func() string { return "127.0.0.1:9080" }})

	if _, body := getJSON(t, srv, "/api/services"); !strings.Contains(body, `"ai-predict"`) {
		t.Errorf("online prediction is not advertised with Cloud Run: %s", body)
	}
	_, body := getJSON(t, srv, "/api/ai/predict?project=p1")
	var st PredictStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Configured || len(st.Endpoints) != 0 || !strings.Contains(st.Deploy, "AIP_PREDICT_ROUTE") {
		t.Errorf("an empty page reads %+v", st)
	}
	for _, want := range []string{"PredictionService", "Endpoint", "Batch prediction"} {
		if !strings.Contains(strings.Join(st.NotServed, "\n"), want) {
			t.Errorf("the page does not name %q as not served: %v", want, st.NotServed)
		}
	}
	if len(src.projects) != 1 || src.projects[0] != "p1" {
		t.Errorf("the source was asked for projects %v, want [p1]", src.projects)
	}

	src.endpoints = []prediction.Endpoint{
		readyPredictor(prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}),
		prediction.NewEndpoint("projects/p1/locations/us-central1/services/broken", prediction.DefaultRoutes(),
			prediction.ConditionSource{Ready: "False", Message: "Container failed with: FAIL_STARTUP is set"}),
	}
	_, body = getJSON(t, srv, "/api/ai/predict?project=p1")
	st = PredictStatus{}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Endpoints) != 2 {
		t.Fatalf("endpoints = %+v", st.Endpoints)
	}
	ok, bad := st.Endpoints[0], st.Endpoints[1]
	if ok.ID != "doubler" || ok.State != "READY" || ok.PredictRoute != "/v1/predict" || ok.HealthRoute != "/healthz" ||
		ok.PredictURL != "http://doubler.default.cloudburrow.localhost/v1/predict" {
		t.Errorf("the ready endpoint reads %+v", ok)
	}
	if bad.State != "FAILED" || bad.PredictURL != "" || !strings.Contains(bad.Message, "FAIL_STARTUP") {
		t.Errorf("the failed endpoint reads %+v", bad)
	}

	src.err = errors.New("rpc error: code = Unavailable desc = connection refused")
	_, body = getJSON(t, srv, "/api/ai/predict?project=p1")
	st = PredictStatus{}
	_ = json.Unmarshal([]byte(body), &st)
	if !strings.Contains(st.Unavailable, "cannot list prediction endpoints") {
		t.Errorf("a failed listing reads %+v, want it unavailable rather than empty", st)
	}
}

// A prediction goes to the gateway with the endpoint's host and its own
// predict route, carries the instances and parameters as written, and the
// container's answer comes back verbatim — including its errors.
func TestPredictRelaysThroughTheIngressAndShowsTheAnswerVerbatim(t *testing.T) {
	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	ing := &fakeIngress{host: "doubler.default.cloudburrow.localhost", routes: routes}
	gateway := ing.start(t)
	src := &fakePredictionSource{endpoints: []prediction.Endpoint{readyPredictor(routes)}}
	srv := newPredictServer(t, &Prediction{Source: src, Ingress: func() string { return gateway }})

	code, r, raw := predict(t, srv, `{"endpoint":"`+predictorName+`","instances":"[1, 2, 3.5]","parameters":"{\"note\": 12345678901234567890}"}`)
	if code != http.StatusOK {
		t.Fatalf("predict = %d: %s", code, raw)
	}
	if r.Status != http.StatusOK || r.ContractError != "" {
		t.Errorf("result = %+v", r)
	}
	if !strings.Contains(r.Body, `"predictions":[2,4,7]`) || !strings.Contains(r.Body, `"deployedModelId":"doubler-v1"`) {
		t.Errorf("body = %s", r.Body)
	}
	// Kept as written: a float64 round trip would have changed the integer.
	if r.Request != `{"instances":[1,2,3.5],"parameters":{"note":12345678901234567890}}` {
		t.Errorf("request sent = %s", r.Request)
	}
	if r.URL != "http://doubler.default.cloudburrow.localhost/v1/predict" {
		t.Errorf("url = %s", r.URL)
	}
	ing.mu.Lock()
	sent := append([]string(nil), ing.requests...)
	ing.mu.Unlock()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "doubler.default.cloudburrow.localhost /v1/predict ") {
		t.Errorf("the gateway saw %q", sent)
	}

	// Addressed by service ID as well as by resource name.
	if code, r, raw := predict(t, srv, `{"endpoint":"doubler","instances":"[4]"}`); code != http.StatusOK || !strings.Contains(r.Body, "[8]") {
		t.Errorf("predict by ID = %d %+v %s", code, r, raw)
	}

	// The container's refusals are its own, verbatim, with its status.
	for _, tc := range []struct{ instances, want string }{
		{`[]`, "at least one instance is required"},
		{`["abc"]`, "instance is not a number"},
	} {
		code, r, raw := predict(t, srv, `{"endpoint":"doubler","instances":`+jsonString(tc.instances)+`}`)
		if code != http.StatusOK {
			t.Fatalf("%s: predict = %d: %s", tc.instances, code, raw)
		}
		if r.Status == http.StatusOK || !strings.Contains(r.Body, tc.want) {
			t.Errorf("%s: the container's answer came back as %d %q", tc.instances, r.Status, r.Body)
		}
		var decoded map[string]string
		if err := json.Unmarshal([]byte(r.Body), &decoded); err != nil || decoded["error"] == "" {
			t.Errorf("%s: the body is not the container's own JSON: %q", tc.instances, r.Body)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A 200 whose predictions do not match the instances one for one is shown,
// and flagged, rather than passed off as an answer.
func TestPredictFlagsAResponseThatBreaksTheContract(t *testing.T) {
	routes := prediction.DefaultRoutes()
	ing := &fakeIngress{host: "doubler.default.cloudburrow.localhost", routes: routes, raw: `{"predictions":[1]}`}
	gateway := ing.start(t)
	srv := newPredictServer(t, &Prediction{
		Source:  &fakePredictionSource{endpoints: []prediction.Endpoint{readyPredictor(routes)}},
		Ingress: func() string { return gateway },
	})
	code, r, raw := predict(t, srv, `{"endpoint":"doubler","instances":"[1, 2]"}`)
	if code != http.StatusOK || r.Status != http.StatusOK {
		t.Fatalf("predict = %d: %s", code, raw)
	}
	if r.Body != `{"predictions":[1]}` || !strings.Contains(r.ContractError, "1 predictions for 2 instances") {
		t.Errorf("result = %+v", r)
	}
}

// What the console refuses itself never reaches the gateway, and says why.
func TestPredictRefusesWhatItCannotSend(t *testing.T) {
	routes := prediction.DefaultRoutes()
	ing := &fakeIngress{host: "doubler.default.cloudburrow.localhost", routes: routes}
	gateway := ing.start(t)
	failed := prediction.NewEndpoint("projects/p1/locations/us-central1/services/broken", routes,
		prediction.ConditionSource{Ready: "False", Message: "FAIL_STARTUP is set"})
	src := &fakePredictionSource{endpoints: []prediction.Endpoint{readyPredictor(routes), failed}}
	ingress := gateway
	srv := newPredictServer(t, &Prediction{Source: src, Ingress: func() string { return ingress }})

	for _, tc := range []struct {
		name, body string
		code       int
		want       string
	}{
		{"no endpoint", `{"instances":"[1]"}`, http.StatusBadRequest, "choose an endpoint"},
		{"no instances", `{"endpoint":"doubler","instances":"  "}`, http.StatusBadRequest, "instances are required"},
		{"instances not JSON", `{"endpoint":"doubler","instances":"[1,"}`, http.StatusBadRequest, "instances are not valid JSON"},
		{"instances not an array", `{"endpoint":"doubler","instances":"{\"a\":1}"}`, http.StatusBadRequest, "must be a JSON array"},
		{"parameters not JSON", `{"endpoint":"doubler","instances":"[1]","parameters":"{x"}`, http.StatusBadRequest, "parameters are not valid JSON"},
		{"unknown field", `{"endpoint":"doubler","instances":"[1]","url":"http://example.com"}`, http.StatusBadRequest, "malformed request"},
		{"not deployed", `{"endpoint":"nosuch","instances":"[1]"}`, http.StatusNotFound, `nosuch\" is deployed`},
		{"failed endpoint", `{"endpoint":"broken","instances":"[1]"}`, http.StatusConflict, "FAIL_STARTUP is set"},
	} {
		resp, got := postJSON(t, srv, "/api/ai/predict?project=p1", tc.body)
		if resp.StatusCode != tc.code || !strings.Contains(got, tc.want) {
			t.Errorf("%s: = %d %s, want %d containing %q", tc.name, resp.StatusCode, got, tc.code, tc.want)
		}
	}
	ing.mu.Lock()
	n := len(ing.requests)
	ing.mu.Unlock()
	if n != 0 {
		t.Errorf("refused requests reached the gateway %d times", n)
	}

	ingress = ""
	if resp, got := postJSON(t, srv, "/api/ai/predict?project=p1", `{"endpoint":"doubler","instances":"[1]"}`); resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(got, "ingress is not published") {
		t.Errorf("with no published ingress = %d %s", resp.StatusCode, got)
	}
}
