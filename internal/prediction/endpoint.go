package prediction

import (
	"fmt"
	"time"
)

// EndpointState is the lifecycle of a prediction endpoint on the owned runtime.
//
// Vertex has no "local endpoint" resource to mirror — the SDK's LocalEndpoint
// is a Python object wrapping a Docker container, not an API resource. So this
// describes what CloudBurrow actually runs, and the console pages (#43-#49)
// render this rather than inventing a Vertex-shaped status that no API returns.
type EndpointState string

const (
	// EndpointPending means the revision exists but is not serving yet.
	EndpointPending EndpointState = "PENDING"
	// EndpointReady means the container passed its health route and is serving.
	EndpointReady EndpointState = "READY"
	// EndpointFailed means the revision will not become ready. This is
	// terminal: a container that failed to start is not retried into health.
	EndpointFailed EndpointState = "FAILED"
	// EndpointScaledToZero means the endpoint is healthy but has no replica.
	// Requests still succeed; the first one pays cold-start latency.
	EndpointScaledToZero EndpointState = "SCALED_TO_ZERO"
)

// Terminal reports whether no further state change is expected without a new
// deployment. Only failure is terminal: a ready endpoint can scale to zero and
// back.
func (s EndpointState) Terminal() bool { return s == EndpointFailed }

// Endpoint is the status of one prediction endpoint.
type Endpoint struct {
	// Name is the endpoint's resource name on the owned runtime.
	Name string
	// State is the lifecycle state.
	State EndpointState
	// Reason carries the runtime's own explanation. It is passed through
	// verbatim rather than reworded: a paraphrased Kubernetes reason is
	// harder to search for than the original.
	Reason string
	// Message is the runtime's human-readable detail, if any.
	Message string
	// URI is where the endpoint serves, empty until it is ready.
	URI string
	// Routes are the contract routes the container was configured with, so a
	// console can show the predict path without guessing the default.
	Routes Routes
	// Replicas is the observed replica count.
	Replicas int
	// UpdatedAt is when the runtime last changed this status.
	UpdatedAt time.Time
}

// PredictURL returns the full URL to POST a prediction request to.
//
// It fails rather than returning a URL that cannot work, because a console
// showing an unusable endpoint URL is worse than one showing none.
func (e Endpoint) PredictURL() (string, error) {
	if e.URI == "" {
		return "", fmt.Errorf("endpoint %q has no URI (state %s)", e.Name, e.State)
	}
	route := e.Routes.Predict
	if route == "" {
		route = DefaultPredictRoute
	}
	return e.URI + route, nil
}

// ConditionSource is the subset of a runtime condition this package reads.
// Both Knative conditions and Cloud Run v2 conditions map onto it.
type ConditionSource struct {
	// Ready is the runtime's Ready condition: "True", "False" or "Unknown".
	Ready   string
	Reason  string
	Message string
	// Replicas is the observed replica count, if the runtime reports one.
	Replicas int
	// UpdatedAt is the condition's last transition time.
	UpdatedAt time.Time
	// URI is the address the runtime advertises.
	URI string
}

// StateFrom maps a runtime condition onto an endpoint state.
//
// "Unknown" deliberately maps to pending rather than failed: Knative reports
// Unknown while a revision is still coming up, and calling that a failure
// would make every healthy deployment look broken for its first few seconds.
func StateFrom(c ConditionSource) EndpointState {
	switch c.Ready {
	case "True":
		if c.Replicas == 0 {
			return EndpointScaledToZero
		}
		return EndpointReady
	case "False":
		return EndpointFailed
	default:
		return EndpointPending
	}
}

// NewEndpoint builds an endpoint status from a runtime condition.
func NewEndpoint(name string, routes Routes, c ConditionSource) Endpoint {
	return Endpoint{
		Name:      name,
		State:     StateFrom(c),
		Reason:    c.Reason,
		Message:   c.Message,
		URI:       c.URI,
		Routes:    routes,
		Replicas:  c.Replicas,
		UpdatedAt: c.UpdatedAt,
	}
}
