package console

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// grpcGateway is the cluster ingress in front of a model's gRPC server: it
// routes on :authority; /test.Echo/Upper answers its input upper-cased, any
// other method is the container's UNIMPLEMENTED.
func grpcGateway(t *testing.T, host string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(prediction.RawCodec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			md, _ := metadata.FromIncomingContext(stream.Context())
			if a := md.Get(":authority"); len(a) == 0 || a[0] != host {
				return status.Errorf(codes.Unavailable, "no route for %v", a)
			}
			if m, _ := grpc.MethodFromServerStream(stream); m != "/test.Echo/Upper" {
				return status.Errorf(codes.Unimplemented, "unknown method %s", m)
			}
			var in []byte
			if err := stream.RecvMsg(&in); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			out := bytes.ToUpper(in)
			return stream.SendMsg(&out)
		}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestDirectRawPredictOnAModelWithGRPCPorts(t *testing.T) {
	model := readyPredictor(prediction.DefaultRoutes())
	model.GRPCPort = 8080
	httpOnly := prediction.NewEndpoint("projects/p1/locations/us-central1/services/plain", prediction.DefaultRoutes(),
		prediction.ConditionSource{Ready: "True", Replicas: 1, URI: "http://plain.default.cloudburrow.localhost"})
	gw := grpcGateway(t, "doubler.default.cloudburrow.localhost")
	srv := newPredictServer(t, &Prediction{
		Source:  &fakePredictionSource{endpoints: []prediction.Endpoint{model, httpOnly}},
		Ingress: func() string { return gw },
	})

	// The status lists the gRPC port, so the screen offers the form.
	_, body := getJSON(t, srv, "/api/ai/predict?project=p1")
	if !strings.Contains(body, `"grpcPort":8080`) {
		t.Errorf("status does not carry the gRPC port: %s", body)
	}

	send := func(req map[string]string) (int, DirectRawResult, string) {
		b, _ := json.Marshal(req)
		resp, got := postJSON(t, srv, "/api/ai/predict/direct-raw?project=p1", string(b))
		var r DirectRawResult
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal([]byte(got), &r); err != nil {
				t.Fatalf("decode: %v: %s", err, got)
			}
		}
		return resp.StatusCode, r, got
	}
	code, r, got := send(map[string]string{"endpoint": "doubler", "methodName": "/test.Echo/Upper/",
		"input": base64.StdEncoding.EncodeToString([]byte("abc"))})
	if code != http.StatusOK || r.Code != "OK" || r.Output != base64.StdEncoding.EncodeToString([]byte("ABC")) ||
		r.OutputHex != "414243" || r.Method != "/test.Echo/Upper" || r.Authority != "doubler.default.cloudburrow.localhost" {
		t.Errorf("DirectRawPredict = %d %+v %s", code, r, got)
	}
	// The container's refusal is an answer, with its status.
	code, r, _ = send(map[string]string{"endpoint": "doubler", "methodName": "/test.Echo/Nope"})
	if code != http.StatusOK || r.Code != "Unimplemented" || !strings.Contains(r.Message, "unknown method") {
		t.Errorf("container refusal = %d %+v", code, r)
	}
	for _, tc := range []struct {
		req  map[string]string
		code int
		want string
	}{
		{map[string]string{"endpoint": "plain", "methodName": "/test.Echo/Upper"}, http.StatusConflict, "grpcPorts"},
		{map[string]string{"endpoint": "doubler", "methodName": "Upper"}, http.StatusBadRequest, "fully qualified"},
		{map[string]string{"endpoint": "doubler", "methodName": "/test.Echo/Upper", "input": "!!"}, http.StatusBadRequest, "base64"},
		{map[string]string{"endpoint": "nope", "methodName": "/test.Echo/Upper"}, http.StatusNotFound, "nope"},
	} {
		if code, _, got := send(tc.req); code != tc.code || !strings.Contains(got, tc.want) {
			t.Errorf("%v = %d %s; want %d naming %q", tc.req, code, got, tc.code, tc.want)
		}
	}
}
