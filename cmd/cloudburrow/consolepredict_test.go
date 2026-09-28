package main

import (
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// TestPredictionEndpointsAreTheContractServices (#869): the Online prediction
// page lists a Cloud Run service as an endpoint only when its container
// claims the Vertex contract with an AIP_* variable, takes its routes from
// those variables with the contract's defaults for the rest, and reads its
// state from the service's terminal condition.
func TestPredictionEndpointsAreTheContractServices(t *testing.T) {
	env := func(kv ...string) []*runpb.EnvVar {
		var out []*runpb.EnvVar
		for i := 0; i < len(kv); i += 2 {
			out = append(out, &runpb.EnvVar{Name: kv[i], Values: &runpb.EnvVar_Value{Value: kv[i+1]}})
		}
		return out
	}
	svc := func(state runpb.Condition_State, msg string, vars []*runpb.EnvVar) *runpb.Service {
		return &runpb.Service{
			Name: "projects/p/locations/us-central1/services/s",
			Uri:  "http://s.default.cloudburrow.localhost",
			Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
				Image: "dev.local/predictor:v1", Env: vars,
			}}},
			TerminalCondition: &runpb.Condition{State: state, Message: msg},
		}
	}

	for _, tc := range []struct {
		name    string
		svc     *runpb.Service
		want    bool
		routes  prediction.Routes
		state   prediction.EndpointState
		message string
	}{
		{name: "no AIP variable is not an endpoint",
			svc: svc(runpb.Condition_CONDITION_SUCCEEDED, "", env("PORT_NAME", "x", "GREETING", "hi"))},
		{name: "no container is not an endpoint",
			svc: &runpb.Service{Name: "projects/p/locations/us-central1/services/s", Template: &runpb.RevisionTemplate{}}},
		{name: "configured routes", want: true,
			svc:    svc(runpb.Condition_CONDITION_SUCCEEDED, "ready", env(prediction.EnvPort, "9000", prediction.EnvHealthRoute, "/healthz", prediction.EnvPredictRoute, "/v1/predict")),
			routes: prediction.Routes{Port: 9000, Health: "/healthz", Predict: "/v1/predict"},
			state:  prediction.EndpointReady},
		{name: "one variable claims the contract, defaults for the rest", want: true,
			svc:    svc(runpb.Condition_CONDITION_PENDING, "Revision is coming up", env(prediction.EnvStorageURI, "gs://b/m")),
			routes: prediction.DefaultRoutes(), state: prediction.EndpointPending, message: "Revision is coming up"},
		{name: "failed carries the runtime's message", want: true,
			svc:    svc(runpb.Condition_CONDITION_FAILED, "Container failed with: FAIL_STARTUP is set", env(prediction.EnvPredictRoute, "/p")),
			routes: prediction.Routes{Port: prediction.DefaultPort, Health: prediction.DefaultHealthRoute, Predict: "/p"},
			state:  prediction.EndpointFailed, message: "Container failed with: FAIL_STARTUP is set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep, ok := predictionEndpoint(tc.svc)
			if ok != tc.want {
				t.Fatalf("predictionEndpoint ok = %v, want %v (%+v)", ok, tc.want, ep)
			}
			if !ok {
				return
			}
			if ep.Routes != tc.routes || ep.State != tc.state || ep.Message != tc.message {
				t.Errorf("endpoint = %+v; want routes %+v, state %s, message %q", ep, tc.routes, tc.state, tc.message)
			}
			if ep.Name != tc.svc.GetName() || ep.URI != tc.svc.GetUri() {
				t.Errorf("endpoint names %q at %q", ep.Name, ep.URI)
			}
		})
	}
}
