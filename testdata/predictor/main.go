// Command predictor is a Vertex custom prediction container fixture.
//
// It honours the Vertex serving contract and nothing else, so it runs
// unchanged under CloudBurrow's Knative path or on Vertex itself. That is the
// point of the contract: the runtime is CloudBurrow's choice, the interface is
// Google's.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

type request struct {
	Instances  []json.RawMessage `json:"instances"`
	Parameters json.RawMessage   `json:"parameters,omitempty"`
}

type response struct {
	Predictions     []json.RawMessage `json:"predictions"`
	DeployedModelID string            `json:"deployedModelId,omitempty"`
}

func main() {
	port := envOr("AIP_HTTP_PORT", "8080")
	health := envOr("AIP_HEALTH_ROUTE", "/health")
	predict := envOr("AIP_PREDICT_ROUTE", "/predict")

	// A deliberate startup failure path, so the test can prove a broken
	// container is reported rather than silently never becoming ready.
	if os.Getenv("FAIL_STARTUP") == "true" {
		log.Fatal("FAIL_STARTUP is set: refusing to start")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(health, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc(predict, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, http.StatusMethodNotAllowed, "predict requires POST")
			return
		}
		var req request
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, "malformed request: "+err.Error())
			return
		}
		if len(req.Instances) == 0 {
			httpError(w, http.StatusBadRequest, "at least one instance is required")
			return
		}

		// A deliberate slow path, driven by the contract's own parameters
		// field, so a test can prove that a caller's deadline bounds a
		// prediction rather than hanging on it.
		if delay := requestedDelay(req.Parameters); delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				// The caller gave up; stop working for it.
				return
			}
		}

		// A trivial model: double each numeric instance. One prediction per
		// instance, as the contract requires.
		out := make([]json.RawMessage, 0, len(req.Instances))
		for _, inst := range req.Instances {
			var n float64
			if err := json.Unmarshal(inst, &n); err != nil {
				httpError(w, http.StatusBadRequest, "instances must be numbers")
				return
			}
			out = append(out, json.RawMessage(strconv.FormatFloat(n*2, 'f', -1, 64)))
		}
		log.Printf("predicted %d instance(s)", len(out))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{Predictions: out, DeployedModelID: "doubler-v1"})
	})

	log.Printf("predictor listening on :%s (health=%s predict=%s)", port, health, predict)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// requestedDelay reads the fixture's only parameter. An unparseable
// parameters document means no delay rather than an error: parameters are
// model-defined, and a model that does not recognise one ignores it.
func requestedDelay(params json.RawMessage) time.Duration {
	if len(params) == 0 {
		return 0
	}
	var p struct {
		DelayMS int `json:"delayMs"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.DelayMS <= 0 {
		return 0
	}
	return time.Duration(p.DelayMS) * time.Millisecond
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}
