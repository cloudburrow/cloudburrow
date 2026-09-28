package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// Online prediction (#869): send a Vertex AI custom prediction request to an
// endpoint this instance actually runs, and show what came back.
//
// What CloudBurrow serves for prediction is the custom container contract
// (docs/prediction.md): a container that reads AIP_HTTP_PORT,
// AIP_HEALTH_ROUTE and AIP_PREDICT_ROUTE, deployed as a Cloud Run service onto
// Knative. There is no Vertex Endpoint resource and no PredictionService, so
// the screen offers neither — it lists the deployed contract containers and
// POSTs {"instances": [...], "parameters": {...}} to one's predict route,
// through the cluster ingress, exactly as `curl` in docs/prediction.md does.
//
// Like the playground it is a relay: it does not parse or reshape the answer.
// The container's status and body reach the screen unchanged, so an error the
// container returned is shown in the container's own words.

// PredictionSource lists the deployed prediction endpoints.
//
// Implemented over the Cloud Run API, so the screen shows the services that
// API reports and nothing it does not.
type PredictionSource interface {
	// PredictionEndpoints returns every service in project whose container is
	// configured with the Vertex prediction contract (any AIP_* variable).
	PredictionEndpoints(ctx context.Context, project string) ([]prediction.Endpoint, error)
}

// Prediction is the online prediction screen's configuration.
type Prediction struct {
	// Source lists endpoints. Nil means Cloud Run is not enabled on this
	// instance, and the screen is not offered.
	Source PredictionSource
	// Ingress resolves the cluster ingress gateway's host address, "" when it
	// is not published. Resolved per request for the same reason the
	// playground's address is: the console is built before it is known.
	Ingress func() string
	// Client is the HTTP client for the relay; nil uses http.DefaultClient.
	Client *http.Client
}

// Configured reports whether the screen is offered.
func (p *Prediction) Configured() bool { return p != nil && p.Source != nil }

func (p *Prediction) ingress() string {
	if p == nil || p.Ingress == nil {
		return ""
	}
	return p.Ingress()
}

func (p *Prediction) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// predictDeadline bounds one prediction. The first request to an endpoint
// scaled to zero pays a cold start, so this is generous; the browser's Cancel
// ends it sooner.
const predictDeadline = 2 * time.Minute

// predictBodyLimit bounds the request the console builds and the response it
// relays. It is the console's own limit, not a Vertex quota.
const predictBodyLimit = 8 << 20

// PredictEndpoint is one endpoint as the screen shows it.
type PredictEndpoint struct {
	// Name is the Cloud Run resource name; it is what a request names.
	Name string `json:"name"`
	// ID is the service ID, the last segment of Name.
	ID    string `json:"id"`
	State string `json:"state"`
	// Message is the runtime's own explanation of a state that is not ready.
	Message      string `json:"message,omitempty"`
	URI          string `json:"uri,omitempty"`
	PredictRoute string `json:"predictRoute"`
	HealthRoute  string `json:"healthRoute"`
	// PredictURL is the address the request is sent to, empty until the
	// endpoint has a URI.
	PredictURL string `json:"predictUrl,omitempty"`
}

// PredictStatus is what the screen needs to render.
type PredictStatus struct {
	Configured bool `json:"configured"`
	// Note explains an unconfigured screen.
	Note      string            `json:"note,omitempty"`
	Endpoints []PredictEndpoint `json:"endpoints"`
	// Unavailable means the endpoints could not be listed, or cannot be
	// reached: distinct from an empty list.
	Unavailable string `json:"unavailable,omitempty"`
	// Ingress is the gateway requests go through.
	Ingress string `json:"ingress,omitempty"`
	// Deploy says how to deploy an endpoint, for the empty state.
	Deploy string `json:"deploy,omitempty"`
	// NotServed lists the Vertex prediction surfaces CloudBurrow does not
	// serve, so their absence from the screen reads as a decision.
	NotServed []string `json:"notServed"`
}

// predictDeploy is the empty state's instruction.
const predictDeploy = "Deploy a container that honours the Vertex AI custom prediction contract " +
	"as a Cloud Run service, with AIP_HTTP_PORT, AIP_HEALTH_ROUTE and AIP_PREDICT_ROUTE set in its " +
	"environment: from Cloud Run's Deploy container here, or CreateService through the Cloud Run " +
	"API. A service with no AIP_* variable is not a prediction endpoint and is not listed. " +
	"See docs/prediction.md."

// predictNotServed are the Vertex surfaces that are not implemented, named
// on the screen rather than silently absent.
var predictNotServed = []string{
	"Vertex AI Endpoint and DeployedModel resources (the container is called directly)",
	"PredictionService.Predict, RawPredict, StreamingPredict and Explain",
	"Model Registry upload and versions",
	"Batch prediction",
}

func (p *Prediction) endpoints(ctx context.Context, project string) ([]PredictEndpoint, error) {
	eps, err := p.Source.PredictionEndpoints(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]PredictEndpoint, 0, len(eps))
	for _, e := range eps {
		pe := PredictEndpoint{
			Name: e.Name, ID: e.Name[strings.LastIndex(e.Name, "/")+1:],
			State: string(e.State), Message: e.Message, URI: e.URI,
			PredictRoute: orDefault(e.Routes.Predict, prediction.DefaultPredictRoute),
			HealthRoute:  orDefault(e.Routes.Health, prediction.DefaultHealthRoute),
		}
		if u, err := e.PredictURL(); err == nil {
			pe.PredictURL = u
		}
		out = append(out, pe)
	}
	return out, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Status lists the endpoints in project.
func (p *Prediction) Status(ctx context.Context, project string) PredictStatus {
	if !p.Configured() {
		return PredictStatus{
			Configured: false, Endpoints: []PredictEndpoint{}, NotServed: predictNotServed,
			Note: "Online prediction runs custom prediction containers as Cloud Run services, " +
				"and Cloud Run is not enabled on this instance. Start it with --services including run.",
		}
	}
	st := PredictStatus{
		Configured: true, Endpoints: []PredictEndpoint{}, Deploy: predictDeploy,
		NotServed: predictNotServed, Ingress: p.ingress(),
	}
	eps, err := p.endpoints(ctx, project)
	if err != nil {
		st.Unavailable = "cannot list prediction endpoints: " + userMessage(err)
		return st
	}
	st.Endpoints = eps
	if st.Ingress == "" && len(eps) > 0 {
		st.Unavailable = ingressUnpublished
	}
	return st
}

const ingressUnpublished = "the cluster ingress is not published on a host port, so the console " +
	"cannot reach an endpoint; `cloudburrow delete` then `cloudburrow up` publishes it"

func (s *Server) handlePredictStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	writeJSON(w, http.StatusOK, s.prediction.Status(ctx, r.URL.Query().Get("project")))
}

// predictRequest is what the screen sends: the two editors' text, unparsed,
// so the server is the one place that decides whether it is a request.
type predictRequest struct {
	Endpoint   string `json:"endpoint"`
	Instances  string `json:"instances"`
	Parameters string `json:"parameters"`
}

// PredictResult is the container's answer, unchanged.
type PredictResult struct {
	Endpoint string `json:"endpoint"`
	// URL is the predict URL the request went to, as the endpoint advertises
	// it; the console reaches it through the ingress with that host.
	URL string `json:"url"`
	// Request is the exact body sent.
	Request     string `json:"request"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	// Body is the response body, verbatim.
	Body      string `json:"body"`
	Truncated bool   `json:"truncated,omitempty"`
	// DurationMs is the round trip as the console measured it, cold start
	// included.
	DurationMs int64 `json:"durationMs"`
	// ContractError is set when a 200 answer breaks the contract — not one
	// prediction per instance — so the screen can say so beside the body
	// rather than let predictions be matched to the wrong inputs.
	ContractError string `json:"contractError,omitempty"`
}

// buildPredictBody validates the editors' text and returns the request body.
//
// The instances and parameters are kept as the user wrote them, compacted,
// rather than decoded and re-encoded: a round trip through float64 would
// change a large integer before the container ever saw it.
func buildPredictBody(instances, parameters string) ([]byte, error) {
	inst := strings.TrimSpace(instances)
	if inst == "" {
		return nil, errors.New("instances are required: a JSON array, one element per instance")
	}
	var probe any
	if err := json.Unmarshal([]byte(inst), &probe); err != nil {
		return nil, fmt.Errorf("instances are not valid JSON: %v", err)
	}
	if _, ok := probe.([]any); !ok {
		return nil, errors.New("instances must be a JSON array, one element per instance")
	}
	var b bytes.Buffer
	b.WriteString(`{"instances":`)
	if err := json.Compact(&b, []byte(inst)); err != nil {
		return nil, fmt.Errorf("instances are not valid JSON: %v", err)
	}
	if params := strings.TrimSpace(parameters); params != "" {
		var v any
		if err := json.Unmarshal([]byte(params), &v); err != nil {
			return nil, fmt.Errorf("parameters are not valid JSON: %v", err)
		}
		b.WriteString(`,"parameters":`)
		if err := json.Compact(&b, []byte(params)); err != nil {
			return nil, fmt.Errorf("parameters are not valid JSON: %v", err)
		}
	}
	b.WriteString("}")
	if b.Len() > predictBodyLimit {
		return nil, fmt.Errorf("the request is %d bytes; the console sends at most %d", b.Len(), predictBodyLimit)
	}
	return b.Bytes(), nil
}

// handlePredict relays one prediction request.
//
// The console's own refusals — no such endpoint, malformed JSON, nothing to
// reach it through — are HTTP errors with {"error"}. Once the container has
// answered, whatever it answered is a 200 carrying its status and body, so
// the screen can tell "the console could not send this" from "the model said
// no".
func (s *Server) handlePredict(w http.ResponseWriter, r *http.Request) {
	p := s.prediction
	if !p.Configured() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "online prediction needs Cloud Run, which is not enabled on this instance",
		})
		return
	}
	var req predictRequest
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
	body, err := buildPredictBody(req.Instances, req.Parameters)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// The endpoint is looked up afresh, by name, among the ones the Cloud
	// Run API reports: the browser names an endpoint, never a URL, so this
	// route can reach a deployed predict route and nothing else.
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
	if ep.PredictURL == "" {
		msg := fmt.Sprintf("endpoint %s is %s and has no URL yet", ep.ID, ep.State)
		if ep.Message != "" {
			msg += ": " + ep.Message
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}
	gateway := p.ingress()
	if gateway == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": ingressUnpublished})
		return
	}
	target, err := url.Parse(ep.PredictURL)
	if err != nil || target.Host == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("endpoint %s advertises an unusable URL %q", ep.ID, ep.PredictURL),
		})
		return
	}

	ctx, cancelSend := context.WithTimeout(r.Context(), predictDeadline)
	defer cancelSend()
	// Knative routes on the Host header, so the request goes to the gateway
	// with the endpoint's own host — the path docs/prediction.md gives for a
	// caller that cannot resolve *.cloudburrow.localhost.
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+gateway+target.RequestURI(), bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstream.Host = target.Host
	upstream.Header.Set("Content-Type", "application/json")

	started := time.Now()
	resp, err := p.client().Do(upstream)
	if err != nil {
		code, msg := http.StatusBadGateway, "the endpoint did not answer: "+err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
			msg = fmt.Sprintf("the endpoint did not answer within %s", predictDeadline)
		}
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, predictBodyLimit+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "reading the endpoint's answer: " + err.Error()})
		return
	}
	result := PredictResult{
		Endpoint: ep.Name, URL: ep.PredictURL, Request: string(body),
		Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
		DurationMs: time.Since(started).Milliseconds(),
	}
	if len(got) > predictBodyLimit {
		got, result.Truncated = got[:predictBodyLimit], true
	}
	result.Body = string(got)
	if resp.StatusCode == http.StatusOK && !result.Truncated {
		result.ContractError = contractError(body, got)
	}
	writeJSON(w, http.StatusOK, result)
}

// contractError checks a 200 answer against the request with the contract's
// own rules, and returns what is wrong with it, or "".
func contractError(reqBody, respBody []byte) string {
	req, err := prediction.DecodeRequest(reqBody)
	if err != nil {
		// The container accepted a request the contract would refuse (no
		// instances); its answer is shown as it is.
		return ""
	}
	var resp prediction.Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "the response is not a prediction response: " + err.Error()
	}
	if err := prediction.ValidateResponse(req, resp); err != nil {
		return err.Error()
	}
	return ""
}

// SetPrediction configures the online prediction screen. Nil, or one with no
// source, offers no screen.
func (s *Server) SetPrediction(p *Prediction) {
	if p == nil {
		p = &Prediction{}
	}
	s.prediction = p
}
