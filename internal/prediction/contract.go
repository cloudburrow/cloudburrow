// Package prediction implements the Vertex AI custom prediction container
// contract.
//
// The decision #42 asks for, made explicitly: Google's tooling is used for the
// *contract* and for container construction and testing; **execution goes
// through CloudBurrow's owned runtime**, which is Knative in the cluster.
//
// The SDK's deploy_to_local_endpoint starts a Docker container directly,
// outside the cluster and outside anything that tracks it. That is precisely
// the "another unmanaged runtime" the issue warns against: it would not be
// covered by cluster ownership, readiness, reset or cleanup, and a crashed
// container would simply be orphaned. A container that honours the contract
// runs perfectly well as a Knative service, so nothing is lost by declining it.
package prediction

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Contract environment variables, as Vertex defines them. A container reads
// these rather than hard-coding, which is what lets the same image run here
// and on Vertex unchanged.
const (
	EnvPort         = "AIP_HTTP_PORT"
	EnvHealthRoute  = "AIP_HEALTH_ROUTE"
	EnvPredictRoute = "AIP_PREDICT_ROUTE"
	EnvStorageURI   = "AIP_STORAGE_URI"
)

// Contract defaults.
const (
	DefaultPort         = 8080
	DefaultHealthRoute  = "/health"
	DefaultPredictRoute = "/predict"
)

// Errors callers are expected to distinguish.
var (
	// ErrMalformedRequest means the body is not a valid prediction request.
	ErrMalformedRequest = errors.New("malformed prediction request")
	// ErrUnhealthy means the container is not ready to serve.
	ErrUnhealthy = errors.New("prediction container is not healthy")
)

// Request is the Vertex prediction request body.
type Request struct {
	// Instances are the inputs. At least one is required: an empty request is
	// a caller mistake, not an empty result.
	Instances []json.RawMessage `json:"instances"`
	// Parameters are optional model parameters.
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// Response is the Vertex prediction response body.
type Response struct {
	Predictions []json.RawMessage `json:"predictions"`
	// DeployedModelID identifies the model that served the request.
	DeployedModelID string `json:"deployedModelId,omitempty"`
}

// DecodeRequest parses and validates a prediction request.
//
// Validation is strict because a silently accepted malformed request produces
// predictions that do not correspond to the caller's input, which is worse
// than an error.
func DecodeRequest(body []byte) (Request, error) {
	var req Request
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	if len(req.Instances) == 0 {
		return Request{}, fmt.Errorf("%w: at least one instance is required", ErrMalformedRequest)
	}
	return req, nil
}

// ValidateResponse checks that a response answers the request it was given.
//
// Vertex requires one prediction per instance. A container returning a
// different count has produced results that cannot be matched to inputs, and
// accepting that would let a caller silently mis-attribute predictions.
func ValidateResponse(req Request, resp Response) error {
	if len(resp.Predictions) != len(req.Instances) {
		return fmt.Errorf("container returned %d predictions for %d instances; "+
			"the contract requires one per instance",
			len(resp.Predictions), len(req.Instances))
	}
	return nil
}

// Routes describes where a container serves.
type Routes struct {
	Port    int
	Health  string
	Predict string
}

// DefaultRoutes returns the contract defaults.
func DefaultRoutes() Routes {
	return Routes{Port: DefaultPort, Health: DefaultHealthRoute, Predict: DefaultPredictRoute}
}

// Env renders the contract environment for a container.
func (r Routes) Env() map[string]string {
	return map[string]string{
		EnvPort:         fmt.Sprint(r.Port),
		EnvHealthRoute:  r.Health,
		EnvPredictRoute: r.Predict,
	}
}

// HealthyStatus reports whether a health response means ready.
//
// Vertex treats any 2xx as healthy. Anything else is not ready, including
// redirects: a container answering 302 on its health route is misconfigured,
// and following the redirect would hide that.
func HealthyStatus(code int) bool { return code >= 200 && code < 300 }

// Serve wires a predictor into an http.ServeMux following the contract.
//
// Provided so a fixture, and any future owned predictor, cannot get the routes
// or the error shapes subtly wrong.
func Serve(mux *http.ServeMux, routes Routes, predict func(Request) (Response, error)) {
	mux.HandleFunc(routes.Health, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc(routes.Predict, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "predict requires POST")
			return
		}
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
			if len(body) > 8<<20 {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
		}

		req, err := DecodeRequest(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := predict(req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := ValidateResponse(req, resp); err != nil {
			// Catch a broken predictor here rather than letting a caller
			// mis-attribute predictions to inputs.
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
