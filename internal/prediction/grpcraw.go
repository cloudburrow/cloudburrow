package prediction

import (
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// A model's gRPC server (#1477).
//
// Vertex AI's ModelContainerSpec.grpcPorts names the port a container serves
// gRPC on: Vertex sends gRPC prediction requests (DirectRawPredict,
// StreamDirectRawPredict, StreamingRawPredict) and its health checks there.
// A custom prediction container runs here as a Cloud Run service, and Cloud
// Run serves a container port end to end over HTTP/2 cleartext when the port
// is named "h2c" (#1475), so a model's gRPC port is its service's h2c port.
// Cloud Run has one container port, which therefore carries both.

// GRPCPortName is the Cloud Run container port name that makes the port a
// model's gRPC port.
const GRPCPortName = "h2c"

// RawCodec passes message bytes through unchanged, so a relay can call a
// method it has no descriptor for. Its name is "proto", which is the content
// subtype a gRPC server expects of a protobuf method.
type RawCodec struct{}

// Marshal returns the bytes of v, a *[]byte.
func (RawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("raw codec: cannot marshal %T", v)
	}
	return *b, nil
}

// Unmarshal stores a copy of data in v, a *[]byte.
func (RawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("raw codec: cannot unmarshal into %T", v)
	}
	*b = append([]byte(nil), data...)
	return nil
}

// Name is "proto".
func (RawCodec) Name() string { return "proto" }

// GRPCMethod checks a PredictionService method_name and returns the gRPC
// method path it names. Vertex documents the format as
// `/namespace.Service/Method/`, trailing slash included, with the example
// `/tensorflow.serving.PredictionService/Predict`; both spellings are read.
func GRPCMethod(name string) (string, error) {
	m := strings.TrimSuffix(name, "/")
	parts := strings.Split(m, "/")
	if len(parts) != 3 || parts[0] != "" || parts[1] == "" || parts[2] == "" || !strings.Contains(parts[1], ".") {
		return "", fmt.Errorf("method_name %q is not a fully qualified gRPC method: "+
			"expected /namespace.Service/Method, for example /tensorflow.serving.PredictionService/Predict", name)
	}
	return m, nil
}

// MaxGRPCMessageBytes bounds one relayed message each way.
const MaxGRPCMessageBytes = 32 << 20

// DialModel returns a client connection to the gRPC server of the service
// routed on host, through gateway, the cluster ingress: Knative routes on
// the :authority, as it routes HTTP/1 on Host.
func DialModel(gateway, host string) (*grpc.ClientConn, error) {
	return grpc.NewClient(gateway,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority(host),
		grpc.WithDefaultCallOptions(
			grpc.ForceCodec(RawCodec{}),
			grpc.MaxCallRecvMsgSize(MaxGRPCMessageBytes),
			grpc.MaxCallSendMsgSize(MaxGRPCMessageBytes)))
}
