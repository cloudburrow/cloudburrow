package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// google.cloud.aiplatform.v1.PredictionService over gRPC (#1193).
//
// The official Go client, cloud.google.com/go/aiplatform's PredictionClient,
// speaks gRPC only, so it could not reach the JSON routes above. This serves
// the methods those routes implement — Predict, RawPredict and
// GenerateContent — on the same port, and every other method answers
// UNIMPLEMENTED.
//
// Each method converts its request to the JSON the matching route reads and
// calls the same function that route calls, so a request is validated,
// refused and answered identically whichever protocol carried it.

// predictionService implements aiplatformpb.PredictionServiceServer.
type predictionService struct {
	aiplatformpb.UnimplementedPredictionServiceServer
	s *Server
}

// newGRPCServer builds the gRPC server for this endpoint.
func (s *Server) newGRPCServer() *grpc.Server {
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(grpctransport.MaxMessageBytes),
		grpc.MaxSendMsgSize(grpctransport.MaxMessageBytes),
	)
	s.RegisterGRPC(srv)
	return srv
}

// RegisterGRPC registers PredictionService on srv.
func (s *Server) RegisterGRPC(srv *grpc.Server) {
	aiplatformpb.RegisterPredictionServiceServer(srv, &predictionService{s: s})
}

// grpcError renders err as a status: an *apierror.Error keeps its code, a
// context error becomes CANCELLED or DEADLINE_EXCEEDED.
func grpcError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	e := apierror.From(err)
	return status.Error(e.Code, e.Message)
}

// target is a parsed PredictionService resource name.
type target struct {
	project, location string
	// model is set for projects/{p}/locations/{l}/publishers/{pub}/models/{m}.
	publisher, model string
	// endpoint is set for projects/{p}/locations/{l}/endpoints/{e}.
	endpoint string
}

// parseTarget reads the two resource name shapes Vertex accepts.
func parseTarget(field, name string) (target, error) {
	parts := strings.Split(name, "/")
	if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "locations" && parts[1] != "" && parts[3] != "" {
		t := target{project: parts[1], location: parts[3]}
		switch {
		case len(parts) == 6 && parts[4] == "endpoints" && parts[5] != "":
			t.endpoint = parts[5]
			return t, nil
		case len(parts) == 8 && parts[4] == "publishers" && parts[6] == "models" && parts[5] != "" && parts[7] != "":
			t.publisher, t.model = parts[5], parts[7]
			return t, nil
		}
	}
	return target{}, apierror.InvalidArgument(
		"%s %q is not a resource name this endpoint serves: expected projects/{project}/locations/{location}/endpoints/{endpoint} "+
			"or projects/{project}/locations/{location}/publishers/google/models/{model}", field, name)
}

func (t target) checkPublisher() error {
	if t.publisher != "google" {
		return apierror.NotFound("publisher %q is not served; only \"google\" is", t.publisher)
	}
	return nil
}

// predictBody is the JSON :predict body for instances and parameters.
func predictBody(instances []*structpb.Value, parameters *structpb.Value) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"instances":[`)
	for i, v := range instances {
		if i > 0 {
			b.WriteByte(',')
		}
		j, err := protojson.Marshal(v)
		if err != nil {
			return nil, apierror.InvalidArgument("instances[%d]: %v", i, err)
		}
		b.Write(j)
	}
	b.WriteByte(']')
	if parameters != nil {
		j, err := protojson.Marshal(parameters)
		if err != nil {
			return nil, apierror.InvalidArgument("parameters: %v", err)
		}
		b.WriteString(`,"parameters":`)
		b.Write(j)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

var lenient = protojson.UnmarshalOptions{DiscardUnknown: true}

// Predict serves text embeddings on a publisher model and custom prediction
// on an endpoint.
func (p *predictionService) Predict(ctx context.Context, req *aiplatformpb.PredictRequest) (*aiplatformpb.PredictResponse, error) {
	t, err := parseTarget("endpoint", req.GetEndpoint())
	if err != nil {
		return nil, grpcError(err)
	}
	body, err := predictBody(req.GetInstances(), req.GetParameters())
	if err != nil {
		return nil, grpcError(err)
	}
	var out []byte
	if t.model != "" {
		if err := t.checkPublisher(); err != nil {
			return nil, grpcError(err)
		}
		resp, err := p.s.embedPredict(ctx, t.model, bytes.NewReader(body))
		if err != nil {
			return nil, grpcError(err)
		}
		if out, err = json.Marshal(resp); err != nil {
			return nil, grpcError(apierror.Internal(err, "encoding the response: %v", err))
		}
		// The JSON route's provenance header, as response metadata.
		_ = grpc.SetHeader(ctx, metadata.Pairs(strings.ToLower(ProvenanceHeader), provenanceValue))
	} else {
		if out, err = p.s.endpointPredict(ctx, t.project, t.location, t.endpoint, body); err != nil {
			return nil, grpcError(err)
		}
	}
	resp := &aiplatformpb.PredictResponse{}
	if err := lenient.Unmarshal(out, resp); err != nil {
		return nil, grpcError(apierror.Internal(err, "the answer does not fit PredictResponse: %v", err))
	}
	return resp, nil
}

// RawPredict relays an arbitrary body to a custom endpoint's predict route.
// Publisher models have no rawPredict route here, so they are UNIMPLEMENTED.
func (p *predictionService) RawPredict(ctx context.Context, req *aiplatformpb.RawPredictRequest) (*httpbody.HttpBody, error) {
	t, err := parseTarget("endpoint", req.GetEndpoint())
	if err != nil {
		return nil, grpcError(err)
	}
	if t.endpoint == "" {
		return nil, status.Errorf(codes.Unimplemented,
			"RawPredict is served only on custom prediction endpoints (projects/{p}/locations/{l}/endpoints/{e}), not on publisher models")
	}
	hb := req.GetHttpBody()
	a, err := p.s.relay(ctx, t.project, t.location, t.endpoint, hb.GetData(), hb.GetContentType())
	if err != nil {
		return nil, grpcError(err)
	}
	if a.status < 200 || a.status > 299 {
		return nil, grpcError(containerError(t.endpoint, a))
	}
	return &httpbody.HttpBody{ContentType: a.contentType, Data: a.body}, nil
}

// GenerateContent serves the same subset the JSON generateContent route does.
func (p *predictionService) GenerateContent(ctx context.Context, req *aiplatformpb.GenerateContentRequest) (*aiplatformpb.GenerateContentResponse, error) {
	t, err := parseTarget("model", req.GetModel())
	if err != nil {
		return nil, grpcError(err)
	}
	if t.model == "" {
		return nil, status.Errorf(codes.Unimplemented,
			"GenerateContent is served only on publisher models (projects/{p}/locations/{l}/publishers/google/models/{m}), not on endpoints")
	}
	if err := t.checkPublisher(); err != nil {
		return nil, grpcError(err)
	}
	// The request as the JSON route reads it: the proto's JSON mapping, less
	// the model, which the route takes from the path. Every field the client
	// set is kept, so one the subset refuses is refused here too.
	j, err := protojson.Marshal(req)
	if err != nil {
		return nil, grpcError(apierror.InvalidArgument("%v", err))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(j, &fields); err != nil {
		return nil, grpcError(apierror.Internal(err, "re-encoding the request: %v", err))
	}
	delete(fields, "model")
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, grpcError(apierror.Internal(err, "re-encoding the request: %v", err))
	}
	resp, err := p.s.generateUnary(ctx, t.model, bytes.NewReader(body))
	if err != nil {
		return nil, grpcError(err)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, grpcError(apierror.Internal(err, "encoding the response: %v", err))
	}
	pb := &aiplatformpb.GenerateContentResponse{}
	if err := lenient.Unmarshal(out, pb); err != nil {
		return nil, grpcError(apierror.Internal(err, "the answer does not fit GenerateContentResponse: %v", err))
	}
	return pb, nil
}
