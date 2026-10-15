package vertexai

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

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// Custom prediction through PredictionService (#1193).
//
// Vertex names an online prediction target
// projects/{p}/locations/{l}/endpoints/{endpoint}. CloudBurrow has no Endpoint
// resource: what it runs is a custom prediction container deployed as a Cloud
// Run service (docs/prediction.md). So {endpoint} is that service's ID, and a
// Predict or RawPredict on it is relayed to the container's predict route
// through the cluster ingress — the same request the console's Online
// prediction screen and `curl` in docs/prediction.md send. Nothing else is
// resolved: an ID that is not a deployed contract container is NOT_FOUND.

// EndpointResolver finds the deployed custom prediction container an endpoint
// name refers to.
type EndpointResolver interface {
	// PredictURL returns the URL the container's predict route is served at,
	// as the runtime advertises it, or an *apierror.Error saying why there is
	// none.
	PredictURL(ctx context.Context, project, location, endpoint string) (string, error)
}

// SetEndpoints installs the custom prediction resolver and the cluster
// ingress address requests go through. Nil leaves every endpoint request
// UNIMPLEMENTED, which is the answer on an instance without Cloud Run.
func (s *Server) SetEndpoints(r EndpointResolver, ingress func() string, client *http.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints, s.ingress, s.client = r, ingress, client
}

// ServesEndpoints reports whether custom prediction endpoints are served.
func (s *Server) ServesEndpoints() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.endpoints != nil
}

// endpointPredictDeadline bounds one relayed prediction when the caller set
// no deadline. The first request to an endpoint scaled to zero pays a cold
// start, so it is generous.
const endpointPredictDeadline = 2 * time.Minute

// endpointBodyLimit bounds a relayed response.
const endpointBodyLimit = 8 << 20

// endpointAnswer is a container's answer, unchanged.
type endpointAnswer struct {
	status      int
	contentType string
	body        []byte
}

// relay sends body to endpoint's predict route and returns what came back.
// It errs only when the request could not be delivered; a container's own
// refusal is an answer, which the caller maps.
func (s *Server) relay(ctx context.Context, project, location, endpoint string, body []byte, contentType string) (*endpointAnswer, error) {
	s.mu.RLock()
	res, ingress, client := s.endpoints, s.ingress, s.client
	s.mu.RUnlock()
	if res == nil {
		return nil, apierror.Unimplemented(
			"custom prediction endpoints are not served on this instance: they are Cloud Run services, " +
				"and Cloud Run is not enabled (start with --services including run); see docs/prediction.md")
	}
	target, err := res.PredictURL(ctx, project, location, endpoint)
	if err != nil {
		return nil, err
	}
	gateway := ""
	if ingress != nil {
		gateway = ingress()
	}
	if gateway == "" {
		return nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: "the cluster ingress is not published " +
			"on a host port, so the endpoint cannot be reached; `cloudburrow delete` then `cloudburrow up` publishes it"}
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return nil, apierror.Internal(err, "endpoint %s advertises an unusable URL %q", endpoint, target)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, endpointPredictDeadline)
		defer cancel()
	}
	// Knative routes on the Host header, so the request goes to the gateway
	// with the endpoint's own host.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+gateway+u.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return nil, apierror.Internal(err, "building the request: %v", err)
	}
	req.Host = u.Host
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &apierror.Error{Code: codes.DeadlineExceeded, Reason: "deadlineExceeded",
				Message: fmt.Sprintf("endpoint %s did not answer before the deadline", endpoint)}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError",
			Message: fmt.Sprintf("endpoint %s did not answer: %v", endpoint, err)}
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, endpointBodyLimit+1))
	if err != nil {
		return nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError",
			Message: fmt.Sprintf("reading endpoint %s's answer: %v", endpoint, err)}
	}
	if len(got) > endpointBodyLimit {
		return nil, &apierror.Error{Code: codes.ResourceExhausted, Reason: "rateLimitExceeded",
			Message: fmt.Sprintf("endpoint %s answered more than %d bytes", endpoint, endpointBodyLimit)}
	}
	return &endpointAnswer{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: got}, nil
}

// containerError turns a container's non-2xx answer into a status, keeping
// its own words: a paraphrase of a model's refusal is harder to act on.
func containerError(endpoint string, a *endpointAnswer) error {
	msg := strings.TrimSpace(string(a.body))
	var env struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(a.body, &env) == nil {
		if m, ok := env.Error.(string); ok && m != "" {
			msg = m
		} else if o, ok := env.Error.(map[string]any); ok {
			if m, ok := o["message"].(string); ok && m != "" {
				msg = m
			}
		}
	}
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	return &apierror.Error{Code: codeForHTTP(a.status), Reason: "endpointError",
		Message: fmt.Sprintf("endpoint %s answered %d: %s", endpoint, a.status, msg)}
}

// codeForHTTP is the standard HTTP-to-google.rpc.Code mapping.
func codeForHTTP(status int) codes.Code {
	switch status {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.Aborted
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case 499:
		return codes.Canceled
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	}
	if status >= 400 && status < 500 {
		return codes.FailedPrecondition
	}
	return codes.Internal
}

// endpointPredict serves Predict on a custom endpoint, for the JSON route and
// gRPC alike. body is the JSON request. The answer is checked against the
// contract: one prediction per instance, or the predictions could be matched
// to the wrong inputs.
func (s *Server) endpointPredict(ctx context.Context, project, location, endpoint string, body []byte) (json.RawMessage, error) {
	req, err := prediction.DecodeRequest(body)
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	a, err := s.relay(ctx, project, location, endpoint, body, "application/json")
	if err != nil {
		return nil, err
	}
	if a.status < 200 || a.status > 299 {
		return nil, containerError(endpoint, a)
	}
	var resp prediction.Response
	if err := json.Unmarshal(a.body, &resp); err != nil {
		return nil, apierror.Internal(err, "endpoint %s answered something that is not a prediction response: %v", endpoint, err)
	}
	if err := prediction.ValidateResponse(req, resp); err != nil {
		return nil, apierror.Internal(err, "endpoint %s broke the prediction contract: %v", endpoint, err)
	}
	return a.body, nil
}

// handleEndpoint serves the JSON routes for custom endpoints:
// POST /v1/projects/{p}/locations/{l}/endpoints/{endpoint}:predict and
// :rawPredict, and :streamRawPredict (#1285).
func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	endpoint, verb, ok := strings.Cut(r.PathValue("endpoint"), ":")
	if !ok {
		apierror.WriteJSON(w, apierror.NotFound("missing method: expected {endpoint}:predict or {endpoint}:rawPredict"))
		return
	}
	project, location := r.PathValue("project"), r.PathValue("location")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("reading the request: %v", err))
		return
	}
	if len(body) > maxRequestBytes {
		apierror.WriteJSON(w, apierror.InvalidArgument("the request is larger than %d bytes", maxRequestBytes))
		return
	}
	switch verb {
	case "predict":
		out, err := s.endpointPredict(r.Context(), project, location, endpoint, body)
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	case "rawPredict":
		a, err := s.relay(r.Context(), project, location, endpoint, body, r.Header.Get("Content-Type"))
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		if a.contentType != "" {
			w.Header().Set("Content-Type", a.contentType)
		}
		w.WriteHeader(a.status)
		_, _ = w.Write(a.body)
	case "streamRawPredict":
		// The container's contract has no streaming route: its one answer (#1285).
		s.streamRawPredictREST(w, r, target{project: project, location: location, endpoint: endpoint}, body)
	default:
		apierror.WriteJSON(w, apierror.Unimplemented(
			"method %q is not implemented for endpoints; only :predict, :rawPredict and :streamRawPredict are served", verb))
	}
}
