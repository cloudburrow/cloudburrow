package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// predictionSource lists the Vertex AI custom prediction endpoints this
// instance runs, for the console's Online prediction page (#869).
//
// A prediction endpoint is a Cloud Run service whose container is configured
// with the contract's AIP_* variables (docs/prediction.md), so the list is
// read through the Cloud Run API — the one an SDK would call — and filtered
// to those. Nothing here reads Knative directly.
type predictionSource struct {
	run runProvider
}

func (s predictionSource) PredictionEndpoints(ctx context.Context, project string) ([]prediction.Endpoint, error) {
	if s.run.runEndpoint() == "" {
		return nil, fmt.Errorf("the Cloud Run adapter is not running")
	}
	if project == "" {
		project = s.run.defaultProject
	}
	if project == "" {
		return nil, fmt.Errorf("choose a project")
	}
	c, err := s.run.servicesClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	var out []prediction.Endpoint
	it := c.ListServices(ctx, &runpb.ListServicesRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", project, s.run.location()),
	})
	for {
		svc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		if ep, ok := predictionEndpoint(svc); ok {
			out = append(out, ep)
		}
	}
	return out, nil
}

// predictionEndpoint reads a Cloud Run service as a prediction endpoint, or
// reports that it is not one: a service whose container sets no AIP_*
// variable does not claim the contract, and offering it a predict request
// would be sending a JSON body to whatever it happens to serve.
func predictionEndpoint(svc *runpb.Service) (prediction.Endpoint, bool) {
	containers := svc.GetTemplate().GetContainers()
	if len(containers) == 0 {
		return prediction.Endpoint{}, false
	}
	routes := prediction.DefaultRoutes()
	contract := false
	for _, e := range containers[0].GetEnv() {
		if !strings.HasPrefix(e.GetName(), "AIP_") {
			continue
		}
		contract = true
		v := e.GetValue()
		switch e.GetName() {
		case prediction.EnvPort:
			if n, err := strconv.Atoi(v); err == nil {
				routes.Port = n
			}
		case prediction.EnvHealthRoute:
			if v != "" {
				routes.Health = v
			}
		case prediction.EnvPredictRoute:
			if v != "" {
				routes.Predict = v
			}
		}
	}
	if !contract {
		return prediction.Endpoint{}, false
	}

	// The Cloud Run API reports readiness, not replicas, so a ready endpoint
	// is READY here whether or not it is scaled to zero; the first request to
	// one that is pays the cold start.
	cond := svc.GetTerminalCondition()
	src := prediction.ConditionSource{Message: cond.GetMessage(), URI: svc.GetUri(), Replicas: 1}
	switch cond.GetState() {
	case runpb.Condition_CONDITION_SUCCEEDED:
		src.Ready = "True"
	case runpb.Condition_CONDITION_FAILED:
		src.Ready = "False"
	default:
		src.Ready = "Unknown"
	}
	ep := prediction.NewEndpoint(svc.GetName(), routes, src)
	if ep.State != prediction.EndpointFailed && ep.State != prediction.EndpointPending {
		// A ready endpoint's condition message says nothing a caller needs.
		ep.Message = ""
	}
	return ep, true
}
