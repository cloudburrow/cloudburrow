package vertexai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

const modelHost = "model.default.cloudburrow.localhost"

// grpcResolver is fakeResolver plus the model's gRPC host; an endpoint ID
// other than grpcID serves no gRPC.
type grpcResolver struct {
	fakeResolver
	grpcID string
}

func (g grpcResolver) GRPCHost(_ context.Context, _, _, endpoint string) (string, error) {
	switch endpoint {
	case g.grpcID:
		return modelHost, nil
	case g.id:
		return "", apierror.FailedPrecondition("endpoint %s serves no gRPC: its model declares no grpcPorts", endpoint)
	}
	return "", apierror.NotFound("no custom prediction endpoint %q is deployed", endpoint)
}

// modelGateway is the cluster ingress in front of a model's gRPC server: it
// routes on :authority as Knative does. /test.Echo/Upper answers each
// message upper-cased; /test.Echo/Refuse answers NOT_FOUND in its own words.
func modelGateway(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(prediction.RawCodec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			md, _ := metadata.FromIncomingContext(stream.Context())
			if a := md.Get(":authority"); len(a) == 0 || a[0] != modelHost {
				return status.Errorf(codes.Unavailable, "no route for authority %v", a)
			}
			method, _ := grpc.MethodFromServerStream(stream)
			if method == "/test.Echo/Refuse" {
				return status.Error(codes.NotFound, "no such tensor")
			}
			if method != "/test.Echo/Upper" {
				return status.Errorf(codes.Unimplemented, "unknown method %s", method)
			}
			for {
				var in []byte
				if err := stream.RecvMsg(&in); errors.Is(err, io.EOF) {
					return nil
				} else if err != nil {
					return err
				}
				out := bytes.ToUpper(in)
				if err := stream.SendMsg(&out); err != nil {
					return err
				}
			}
		}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func grpcEndpointServer(t *testing.T) aiplatformpb.PredictionServiceClient {
	t.Helper()
	gw := modelGateway(t)
	s := NewServer(nil)
	s.SetEndpoints(grpcResolver{fakeResolver: fakeResolver{id: "predictor", url: "http://x/predict"}, grpcID: "model"},
		func() string { return gw }, nil)
	c, _ := startMixed(t, s)
	return c
}

const modelEndpoint = "projects/p/locations/us-central1/endpoints/model"

func TestDirectRawPredictRelaysToTheModelsGRPCServer(t *testing.T) {
	c := grpcEndpointServer(t)
	ctx := context.Background()
	// Vertex documents method_name with a trailing slash; both spellings call the method.
	for _, m := range []string{"/test.Echo/Upper", "/test.Echo/Upper/"} {
		resp, err := c.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: modelEndpoint, MethodName: m, Input: []byte("abc")})
		if err != nil || string(resp.GetOutput()) != "ABC" {
			t.Errorf("DirectRawPredict(%s) = %q, %v; want ABC", m, resp.GetOutput(), err)
		}
	}
	_, err := c.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: modelEndpoint, MethodName: "/test.Echo/Refuse"})
	if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), "endpoint model: no such tensor") {
		t.Errorf("container refusal = %v; want NOT_FOUND in its words", err)
	}
}

func TestDirectRawPredictRefusals(t *testing.T) {
	c := grpcEndpointServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, endpoint, method string
		code                   codes.Code
		want                   string
	}{
		{"bad method", modelEndpoint, "Predict", codes.InvalidArgument, "fully qualified"},
		{"no method", modelEndpoint, "", codes.InvalidArgument, "method_name"},
		{"no grpcPorts", endpointName, "/test.Echo/Upper", codes.FailedPrecondition, "grpcPorts"},
		{"unknown endpoint", "projects/p/locations/us-central1/endpoints/nope", "/test.Echo/Upper", codes.NotFound, "nope"},
		{"publisher model", "projects/p/locations/us-central1/publishers/google/models/gemma", "/test.Echo/Upper", codes.Unimplemented, "publisher models"},
	} {
		_, err := c.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: tc.endpoint, MethodName: tc.method})
		if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v; want %s naming %q", tc.name, err, tc.code, tc.want)
		}
	}
}

func TestDirectRawPredictWithoutCloudRun(t *testing.T) {
	c, _ := startMixed(t, NewServer(nil))
	_, err := c.DirectRawPredict(context.Background(), &aiplatformpb.DirectRawPredictRequest{Endpoint: modelEndpoint, MethodName: "/test.Echo/Upper"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "--services") {
		t.Errorf("DirectRawPredict without Cloud Run = %v; want UNIMPLEMENTED naming --services", err)
	}
}

func TestStreamingRawMethodsRelayBothWays(t *testing.T) {
	c := grpcEndpointServer(t)
	ctx := context.Background()

	st, err := c.StreamingRawPredict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Each request is answered before the next is sent: the stream is live
	// both ways, not collected and replayed.
	for i, in := range []string{"one", "two", "three"} {
		req := &aiplatformpb.StreamingRawPredictRequest{Input: []byte(in)}
		if i == 0 {
			req.Endpoint, req.MethodName = modelEndpoint, "/test.Echo/Upper"
		}
		if err := st.Send(req); err != nil {
			t.Fatal(err)
		}
		got, err := st.Recv()
		if err != nil || string(got.GetOutput()) != strings.ToUpper(in) {
			t.Fatalf("StreamingRawPredict reply %d = %q, %v", i, got.GetOutput(), err)
		}
	}
	if err := st.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("after CloseSend: %v; want EOF", err)
	}

	sd, err := c.StreamDirectRawPredict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = sd.Send(&aiplatformpb.StreamDirectRawPredictRequest{Endpoint: modelEndpoint, MethodName: "/test.Echo/Upper", Input: []byte("x")})
	if got, err := sd.Recv(); err != nil || string(got.GetOutput()) != "X" {
		t.Fatalf("StreamDirectRawPredict = %q, %v", got.GetOutput(), err)
	}
	// One stream is one call: a later request may not change the method.
	_ = sd.Send(&aiplatformpb.StreamDirectRawPredictRequest{MethodName: "/test.Echo/Refuse", Input: []byte("y")})
	if _, err := sd.Recv(); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "one stream is one call") {
		t.Errorf("changed method_name = %v; want INVALID_ARGUMENT", err)
	}

	// The container's refusal ends the stream with its status.
	sr, _ := c.StreamDirectRawPredict(ctx)
	_ = sr.Send(&aiplatformpb.StreamDirectRawPredictRequest{Endpoint: modelEndpoint, MethodName: "/test.Echo/Refuse"})
	if _, err := sr.Recv(); status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), "no such tensor") {
		t.Errorf("refusal on a stream = %v; want NOT_FOUND", err)
	}

	// A stream that names no endpoint before ending.
	se, _ := c.StreamingRawPredict(ctx)
	_ = se.CloseSend()
	if _, err := se.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty stream = %v; want INVALID_ARGUMENT", err)
	}
}

func TestGRPCMethodNames(t *testing.T) {
	for in, want := range map[string]string{
		"/tensorflow.serving.PredictionService/Predict":  "/tensorflow.serving.PredictionService/Predict",
		"/tensorflow.serving.PredictionService/Predict/": "/tensorflow.serving.PredictionService/Predict",
	} {
		if got, err := prediction.GRPCMethod(in); err != nil || got != want {
			t.Errorf("GRPCMethod(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/", "Predict", "/Service/Predict", "/a.B/", "a.B/C", "/a.B/C/D"} {
		if _, err := prediction.GRPCMethod(bad); err == nil {
			t.Errorf("GRPCMethod(%q) accepted", bad)
		}
	}
}
