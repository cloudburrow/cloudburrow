package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Explain on the Online prediction screen (#1454).
//
// Sampled Shapley attributions are computed by the local AI endpoint's
// PredictionService, which perturbs the instances and calls the container's
// predict route (internal/service/vertexai/explain.go). The screen is a relay
// to that API, as the playground is to generateContent: it builds the
// ExplainRequest the official client sends — the instances, the parameters,
// and explanation_spec_override with sampled_shapley_attribution.path_count
// and metadata.inputs[feature].input_baselines — POSTs it to
// /v1/projects/{p}/locations/{l}/endpoints/{e}:explain, and shows the API's
// answer: its status and body verbatim, and the attributions read from it per
// feature. Anything the API refuses (a path count out of range, a feature an
// instance lacks) comes back in the API's own words.

// explainRequest is what the screen sends: the editors' text, unparsed, and
// the path count.
type explainRequest struct {
	Endpoint   string `json:"endpoint"`
	Instances  string `json:"instances"`
	Parameters string `json:"parameters"`
	// PathCount is sampled_shapley_attribution.path_count.
	PathCount int `json:"pathCount"`
	// Baselines is a JSON object naming each feature to attribute, with its
	// input_baselines: {"feature": [baseline, ...]}. An empty list is no
	// baseline, which for a number is 0.
	Baselines string `json:"baselines"`
}

// ExplainResult is the API's answer, verbatim, with the attributions read
// from it.
type ExplainResult struct {
	Endpoint string `json:"endpoint"`
	// URL is the PredictionService route the request went to.
	URL string `json:"url"`
	// Request is the exact ExplainRequest body sent.
	Request     string `json:"request"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	// Body is the response body, verbatim.
	Body       string `json:"body"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMs int64  `json:"durationMs"`
	// ErrorStatus and Error are the API's google.rpc status and message for
	// a refusal.
	ErrorStatus string `json:"errorStatus,omitempty"`
	Error       string `json:"error,omitempty"`
	// DeployedModelID is the container's deployedModelId.
	DeployedModelID string `json:"deployedModelId,omitempty"`
	// Explanations has one entry per instance, in request order.
	Explanations []InstanceExplanation `json:"explanations"`
}

// InstanceExplanation is one instance's prediction and attributions.
type InstanceExplanation struct {
	Instance     int                    `json:"instance"`
	Prediction   json.RawMessage        `json:"prediction,omitempty"`
	Attributions []ExplainedAttribution `json:"attributions"`
}

// ExplainedAttribution is one explained output: the output moved from
// Baseline to Output, and each feature's share of that move.
type ExplainedAttribution struct {
	// OutputIndex is empty for a scalar prediction.
	OutputIndex []int                `json:"outputIndex,omitempty"`
	Baseline    float64              `json:"baselineOutputValue"`
	Output      float64              `json:"instanceOutputValue"`
	Features    []FeatureAttribution `json:"features"`
}

// FeatureAttribution is one feature's attribution, largest magnitude first.
type FeatureAttribution struct {
	Feature string `json:"feature"`
	// Attribution is the value as the API wrote it.
	Attribution json.RawMessage `json:"attribution"`
}

// buildExplainBody validates the screen's fields and returns the
// ExplainRequest body. Instances, parameters and baselines are kept as
// written, compacted, for the reason buildPredictBody gives.
func buildExplainBody(req explainRequest) ([]byte, error) {
	pb, err := buildPredictBody(req.Instances, req.Parameters)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(req.Baselines)
	if raw == "" {
		return nil, errors.New(`feature baselines are required: a JSON object naming each feature to attribute, ` +
			`with its input_baselines, e.g. {"x": [0]}`)
	}
	var inputs map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &inputs); err != nil || inputs == nil {
		return nil, fmt.Errorf(`feature baselines must be a JSON object of feature name to a list of baselines, e.g. {"x": [0]}`)
	}
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	md := map[string]json.RawMessage{}
	for _, name := range names {
		v := bytes.TrimSpace(inputs[name])
		var list []json.RawMessage
		if bytes.Equal(v, []byte("null")) {
			v = []byte("[]")
		} else if err := json.Unmarshal(v, &list); err != nil {
			return nil, fmt.Errorf("feature %q: its baselines must be a JSON list (input_baselines), [] for none", name)
		}
		var b bytes.Buffer
		if err := json.Compact(&b, v); err != nil {
			return nil, fmt.Errorf("feature %q: %v", name, err)
		}
		md[name] = json.RawMessage(`{"inputBaselines":` + b.String() + `}`)
	}
	override, err := json.Marshal(map[string]any{
		"parameters": map[string]any{"sampledShapleyAttribution": map[string]int{"pathCount": req.PathCount}},
		"metadata":   map[string]any{"inputs": md},
	})
	if err != nil {
		return nil, err
	}
	// pb is {"instances":...[,"parameters":...]}; the override joins it.
	body := append(pb[:len(pb)-1:len(pb)-1], `,"explanationSpecOverride":`...)
	body = append(body, override...)
	body = append(body, '}')
	if len(body) > predictBodyLimit {
		return nil, fmt.Errorf("the request is %d bytes; the console sends at most %d", len(body), predictBodyLimit)
	}
	return body, nil
}

// explainPath is the PredictionService route for an endpoint named by its
// Cloud Run resource name, projects/{p}/locations/{l}/services/{id}.
func explainPath(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "services" {
		return "", fmt.Errorf("endpoint %q is not a Cloud Run service name", name)
	}
	return fmt.Sprintf("/v1/projects/%s/locations/%s/endpoints/%s:explain", parts[1], parts[3], parts[5]), nil
}

// readExplanations reads the attributions out of an ExplainResponse.
func readExplanations(body []byte, r *ExplainResult) {
	var resp struct {
		DeployedModelID string            `json:"deployedModelId"`
		Predictions     []json.RawMessage `json:"predictions"`
		Explanations    []struct {
			Attributions []struct {
				BaselineOutputValue float64                    `json:"baselineOutputValue"`
				InstanceOutputValue float64                    `json:"instanceOutputValue"`
				FeatureAttributions map[string]json.RawMessage `json:"featureAttributions"`
				OutputIndex         []int                      `json:"outputIndex"`
			} `json:"attributions"`
		} `json:"explanations"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	r.DeployedModelID = resp.DeployedModelID
	for i, e := range resp.Explanations {
		ie := InstanceExplanation{Instance: i, Attributions: []ExplainedAttribution{}}
		if i < len(resp.Predictions) {
			ie.Prediction = resp.Predictions[i]
		}
		for _, a := range e.Attributions {
			ea := ExplainedAttribution{OutputIndex: a.OutputIndex, Baseline: a.BaselineOutputValue,
				Output: a.InstanceOutputValue, Features: []FeatureAttribution{}}
			for f, v := range a.FeatureAttributions {
				ea.Features = append(ea.Features, FeatureAttribution{Feature: f, Attribution: v})
			}
			mag := func(v json.RawMessage) float64 {
				var x float64
				if json.Unmarshal(v, &x) != nil {
					return -1
				}
				return math.Abs(x)
			}
			sort.SliceStable(ea.Features, func(x, y int) bool {
				mx, my := mag(ea.Features[x].Attribution), mag(ea.Features[y].Attribution)
				if mx != my {
					return mx > my
				}
				return ea.Features[x].Feature < ea.Features[y].Feature
			})
			ie.Attributions = append(ie.Attributions, ea)
		}
		r.Explanations = append(r.Explanations, ie)
	}
}

// handleExplain relays one Explain to the local AI endpoint's
// PredictionService. As with handlePredict, the console's own refusals are
// HTTP errors with {"error"}, and the API's answer, whatever it is, is a 200
// carrying its status and body.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	p := s.prediction
	if !p.Configured() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "online prediction needs Cloud Run, which is not enabled on this instance",
		})
		return
	}
	api := p.ai()
	if api == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": explainUnavailable})
		return
	}
	var req explainRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 2*predictBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose an endpoint"})
		return
	}
	body, err := buildExplainBody(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// The endpoint is one the Cloud Run API lists, as for Predict: the
	// browser names an endpoint, and the route is built from its name.
	listCtx, cancel := context.WithTimeout(r.Context(), readBudget)
	eps, err := p.endpoints(listCtx, r.URL.Query().Get("project"))
	cancel()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot list prediction endpoints: " + userMessage(err)})
		return
	}
	var ep *PredictEndpoint
	for i := range eps {
		if eps[i].Name == req.Endpoint || eps[i].ID == req.Endpoint {
			ep = &eps[i]
			break
		}
	}
	if ep == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no prediction endpoint named %q is deployed", req.Endpoint),
		})
		return
	}
	path, err := explainPath(ep.Name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancelSend := context.WithTimeout(r.Context(), predictDeadline)
	defer cancelSend()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+api+path, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := p.client().Do(upstream)
	if err != nil {
		code, msg := http.StatusBadGateway, "the local AI endpoint did not answer: "+err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
			msg = fmt.Sprintf("Explain did not answer within %s", predictDeadline)
		}
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, predictBodyLimit+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "reading the answer: " + err.Error()})
		return
	}
	result := ExplainResult{
		Endpoint: ep.Name, URL: "http://" + api + path, Request: string(body),
		Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
		DurationMs: time.Since(started).Milliseconds(), Explanations: []InstanceExplanation{},
	}
	if len(got) > predictBodyLimit {
		got, result.Truncated = got[:predictBodyLimit], true
	}
	result.Body = string(got)
	if resp.StatusCode == http.StatusOK {
		if !result.Truncated {
			readExplanations(got, &result)
		}
	} else {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(got, &e) == nil {
			result.Error, result.ErrorStatus = e.Error.Message, e.Error.Status
		}
	}
	writeJSON(w, http.StatusOK, result)
}

const explainUnavailable = "Explain is served by the local AI endpoint's PredictionService, which is not " +
	"running on this instance"
