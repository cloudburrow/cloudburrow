package vertexai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// Raw prediction on a model's gRPC server (#1477).
//
// DirectRawPredict, StreamDirectRawPredict and StreamingRawPredict call
// method_name on the gRPC server in a model's container, with the request's
// input bytes as the method's request message and the method's response
// messages as output bytes: nothing is decoded on the way. Vertex AI sends
// them to the first of the model's containerSpec.grpcPorts; here a model is
// a Cloud Run service, and its gRPC port is the service's container port
// named h2c, which Cloud Run serves end to end over HTTP/2 (#1475). So the
// call goes to the cluster ingress with the service's host as :authority,
// and the container's own status, code and message, comes back unchanged.
//
// DirectRawPredict is one request and one response. The two streaming
// methods are bidirectional, both ways: each request's input is sent on one
// stream to method_name as it arrives, each message the container sends is
// returned as it arrives, and the caller closing its side closes the
// container's. The first request names the endpoint and method_name; a later
// one may repeat them but not change them, since one stream is one call.

// GRPCResolver is implemented by an EndpointResolver that can also find a
// model's gRPC server.
type GRPCResolver interface {
	// GRPCHost returns the host the model's gRPC server is routed on through
	// the cluster ingress, or an *apierror.Error saying why there is none.
	GRPCHost(ctx context.Context, project, location, endpoint string) (string, error)
}

// modelConn opens a client connection to t's gRPC model server.
func (s *Server) modelConn(ctx context.Context, method string, t target) (*grpc.ClientConn, error) {
	if t.endpoint == "" {
		return nil, apierror.Unimplemented("%s calls a gRPC model server, and publisher models have none: "+
			"they run in this process and answer Predict, RawPredict and their streaming forms; "+
			"name a custom endpoint whose model declares grpcPorts (docs/prediction.md)", method)
	}
	s.mu.RLock()
	res, ingress := s.endpoints, s.ingress
	s.mu.RUnlock()
	g, ok := res.(GRPCResolver)
	if !ok {
		return nil, apierror.Unimplemented(
			"custom prediction endpoints are not served on this instance: they are Cloud Run services, " +
				"and Cloud Run is not enabled (start with --services including run); see docs/prediction.md")
	}
	host, err := g.GRPCHost(ctx, t.project, t.location, t.endpoint)
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
	conn, err := prediction.DialModel(gateway, host)
	if err != nil {
		return nil, apierror.Internal(err, "dialling endpoint %s: %v", t.endpoint, err)
	}
	return conn, nil
}

// modelError renders what a call to endpoint's gRPC server returned: the
// container's own status keeps its code and words, naming the endpoint.
func modelError(endpoint string, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return grpcError(err)
	}
	if st.Code() == codes.Canceled || st.Code() == codes.DeadlineExceeded {
		return st.Err()
	}
	return status.Error(st.Code(), fmt.Sprintf("endpoint %s: %s", endpoint, st.Message()))
}

// methodOf checks method_name.
func methodOf(name string) (string, error) {
	m, err := prediction.GRPCMethod(name)
	if err != nil {
		return "", apierror.InvalidArgument("%v", err)
	}
	return m, nil
}

// DirectRawPredict calls method_name once on the model's gRPC server.
func (p *predictionService) DirectRawPredict(ctx context.Context, req *aiplatformpb.DirectRawPredictRequest) (*aiplatformpb.DirectRawPredictResponse, error) {
	t, err := parseTarget("endpoint", req.GetEndpoint())
	if err != nil {
		return nil, grpcError(err)
	}
	method, err := methodOf(req.GetMethodName())
	if err != nil {
		return nil, grpcError(err)
	}
	conn, err := p.s.modelConn(ctx, "DirectRawPredict", t)
	if err != nil {
		return nil, grpcError(err)
	}
	defer func() { _ = conn.Close() }()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, endpointPredictDeadline)
		defer cancel()
	}
	in, out := req.GetInput(), []byte(nil)
	if in == nil {
		in = []byte{}
	}
	if err := conn.Invoke(ctx, method, &in, &out); err != nil {
		return nil, modelError(t.endpoint, err)
	}
	return &aiplatformpb.DirectRawPredictResponse{Output: out}, nil
}

// rawRequest is the part of a streaming raw request the relay reads.
type rawRequest interface {
	GetEndpoint() string
	GetMethodName() string
	GetInput() []byte
}

// relayStream relays one bidirectional stream: recv reads the caller's next
// request, send returns one output to it.
func (s *Server) relayStream(ctx context.Context, rpc string, recv func() (rawRequest, error), send func([]byte) error) error {
	first, err := recv()
	if errors.Is(err, io.EOF) {
		return grpcError(apierror.InvalidArgument("%s: the stream ended before a request named the endpoint", rpc))
	}
	if err != nil {
		return err
	}
	t, err := parseTarget("endpoint", first.GetEndpoint())
	if err != nil {
		return grpcError(err)
	}
	method, err := methodOf(first.GetMethodName())
	if err != nil {
		return grpcError(err)
	}
	conn, err := s.modelConn(ctx, rpc, t)
	if err != nil {
		return grpcError(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method)
	if err != nil {
		return modelError(t.endpoint, err)
	}

	// The caller's side, on its own goroutine: a refusal of a later request
	// is recorded and ends the call.
	var (
		mu      sync.Mutex
		refused error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		upload := func(r rawRequest) error {
			if e := r.GetEndpoint(); e != "" && e != first.GetEndpoint() {
				return apierror.InvalidArgument("endpoint %q differs from the stream's first request %q: one stream is one call", e, first.GetEndpoint())
			}
			if m := r.GetMethodName(); m != "" && m != first.GetMethodName() {
				return apierror.InvalidArgument("method_name %q differs from the stream's first request %q: one stream is one call", m, first.GetMethodName())
			}
			in := r.GetInput()
			if in == nil {
				in = []byte{}
			}
			if err := cs.SendMsg(&in); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		}
		r := first
		for {
			if err := upload(r); err != nil {
				mu.Lock()
				refused = err
				mu.Unlock()
				cancel()
				return
			}
			next, err := recv()
			if errors.Is(err, io.EOF) {
				_ = cs.CloseSend()
				return
			}
			if err != nil {
				cancel()
				return
			}
			r = next
		}
	}()

	var out error
	for {
		var msg []byte
		if err := cs.RecvMsg(&msg); err != nil {
			out = modelError(t.endpoint, err)
			break
		}
		if err := send(msg); err != nil {
			out = err
			break
		}
	}
	if ctx.Err() != nil {
		// Cancelled by the caller's side, which has stopped reading: wait
		// for it, so a refusal it recorded is the answer. Otherwise the
		// container ended the call, and the caller's side may be waiting
		// for a request that will not come; it stops at its next one.
		<-done
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if refused != nil {
		return grpcError(refused)
	}
	return out
}

// StreamDirectRawPredict relays a bidirectional stream to method_name on the
// model's gRPC server.
func (p *predictionService) StreamDirectRawPredict(stream aiplatformpb.PredictionService_StreamDirectRawPredictServer) error {
	return p.s.relayStream(stream.Context(), "StreamDirectRawPredict",
		func() (rawRequest, error) { return stream.Recv() },
		func(b []byte) error { return stream.Send(&aiplatformpb.StreamDirectRawPredictResponse{Output: b}) })
}

// StreamingRawPredict relays a bidirectional stream to method_name on the
// model's gRPC server.
func (p *predictionService) StreamingRawPredict(stream aiplatformpb.PredictionService_StreamingRawPredictServer) error {
	return p.s.relayStream(stream.Context(), "StreamingRawPredict",
		func() (rawRequest, error) { return stream.Recv() },
		func(b []byte) error { return stream.Send(&aiplatformpb.StreamingRawPredictResponse{Output: b}) })
}
