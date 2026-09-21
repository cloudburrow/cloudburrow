package prediction

import (
	"strings"
	"testing"
	"time"
)

// A revision that is still starting must not be reported as failed, or every
// healthy deployment looks broken for its first few seconds.
func TestUnknownReadyIsPendingNotFailed(t *testing.T) {
	t.Parallel()
	got := StateFrom(ConditionSource{Ready: "Unknown", Reason: "Deploying"})
	if got != EndpointPending {
		t.Errorf("state = %s, want %s", got, EndpointPending)
	}
	if got.Terminal() {
		t.Error("a pending endpoint must not be terminal")
	}
}

func TestReadyWithNoReplicasIsScaledToZero(t *testing.T) {
	t.Parallel()
	// Scale-to-zero is healthy, not broken: requests still succeed, they just
	// pay cold start. Reporting it as failed would be wrong.
	got := StateFrom(ConditionSource{Ready: "True", Replicas: 0})
	if got != EndpointScaledToZero {
		t.Errorf("state = %s, want %s", got, EndpointScaledToZero)
	}
	if got.Terminal() {
		t.Error("scaled to zero is not terminal")
	}
}

func TestReadyWithReplicasIsReady(t *testing.T) {
	t.Parallel()
	if got := StateFrom(ConditionSource{Ready: "True", Replicas: 2}); got != EndpointReady {
		t.Errorf("state = %s, want %s", got, EndpointReady)
	}
}

func TestFailedIsTerminal(t *testing.T) {
	t.Parallel()
	got := StateFrom(ConditionSource{Ready: "False", Reason: "RevisionFailed"})
	if got != EndpointFailed {
		t.Fatalf("state = %s, want %s", got, EndpointFailed)
	}
	if !got.Terminal() {
		t.Error("a failed endpoint must be terminal; a caller that keeps waiting would hang forever")
	}
}

// The runtime's own reason must survive unchanged: a reworded Kubernetes
// reason is harder to search for than the original.
func TestNewEndpointPassesTheRuntimeReasonThrough(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	e := NewEndpoint("predictor", DefaultRoutes(), ConditionSource{
		Ready:     "False",
		Reason:    "RevisionMissing",
		Message:   "Configuration does not have any ready Revision.",
		UpdatedAt: now,
	})
	if e.Reason != "RevisionMissing" {
		t.Errorf("reason = %q, want the runtime's own reason", e.Reason)
	}
	if !strings.Contains(e.Message, "ready Revision") {
		t.Errorf("message = %q, want the runtime's own message", e.Message)
	}
	if !e.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want %v", e.UpdatedAt, now)
	}
}

func TestPredictURLUsesTheConfiguredRoute(t *testing.T) {
	t.Parallel()
	e := NewEndpoint("p", Routes{Port: 9090, Health: "/healthz", Predict: "/v1/predict"},
		ConditionSource{Ready: "True", Replicas: 1, URI: "http://p.default.example"})
	got, err := e.PredictURL()
	if err != nil {
		t.Fatalf("PredictURL: %v", err)
	}
	if got != "http://p.default.example/v1/predict" {
		t.Errorf("PredictURL() = %q", got)
	}
}

func TestPredictURLDefaultsWhenNoRouteWasSet(t *testing.T) {
	t.Parallel()
	e := Endpoint{Name: "p", URI: "http://p.default.example", State: EndpointReady}
	got, err := e.PredictURL()
	if err != nil {
		t.Fatalf("PredictURL: %v", err)
	}
	if got != "http://p.default.example"+DefaultPredictRoute {
		t.Errorf("PredictURL() = %q, want the contract default route", got)
	}
}

// A console showing an unusable URL is worse than one showing none.
func TestPredictURLFailsWithoutAURI(t *testing.T) {
	t.Parallel()
	e := Endpoint{Name: "p", State: EndpointPending}
	if _, err := e.PredictURL(); err == nil {
		t.Fatal("PredictURL returned a URL for an endpoint that is not serving")
	} else if !strings.Contains(err.Error(), "PENDING") {
		t.Errorf("error should name the state: %v", err)
	}
}
