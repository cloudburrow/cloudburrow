//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// deployGRPCModel deploys agnhost's gRPC health server as a model whose
// grpcPorts are 8080: the Cloud Run port named h2c (#1477). An AIP_*
// variable claims the prediction contract, as a Vertex AI model's container
// is given them.
func deployGRPCModel(t *testing.T, ctx context.Context, c *run.ServicesClient, h *Harness, id string) {
	t.Helper()
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: grpcServerImage, Args: []string{"grpc-health-checking", "--port=8080", "--http-port=8081"},
			Ports: []*runpb.ContainerPort{{Name: "h2c", ContainerPort: 8080}},
			Env:   []*runpb.EnvVar{{Name: "AIP_DEPLOYED_MODEL_ID", Values: &runpb.EnvVar_Value{Value: "health-v1"}}},
		}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer delCancel()
		_, _ = c.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the model: %v", err)
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/DirectRawPredict, google.cloud.aiplatform.v1.PredictionService/StreamDirectRawPredict, google.cloud.aiplatform.v1.PredictionService/StreamingRawPredict
//
// TestPredictionClientReachesAModelsGRPCServer (#1477): a model whose
// container serves gRPC — Kubernetes' agnhost, pinned by digest, serving
// grpc.health.v1.Health — is deployed through the official Cloud Run SDK
// with its grpcPorts as the port named h2c, and called through the official
// aiplatform PredictionClient as projects/{p}/locations/us-central1/endpoints/{service}.
// DirectRawPredict sends the input bytes to method_name and returns the
// method's response bytes; StreamDirectRawPredict and StreamingRawPredict do
// the same on a bidirectional stream; the container's own refusal of a
// method it does not serve comes back as its status; and a malformed
// method_name is INVALID_ARGUMENT.
func TestPredictionClientReachesAModelsGRPCServer(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvPrediction)
	c := runClient(t, h)
	// Pulling agnhost and the rollout outlast h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	id := "compat-grpc-model"
	deployGRPCModel(t, ctx, c, h, id)
	endpoint := runParent(h) + "/endpoints/" + id
	const check = "/grpc.health.v1.Health/Check"
	input, err := proto.Marshal(&healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	serving := func(what string, out []byte) {
		t.Helper()
		var resp healthpb.HealthCheckResponse
		if err := proto.Unmarshal(out, &resp); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("%s output %x = %v, %v; want SERVING", what, out, &resp, err)
		}
	}

	// The first call may meet the revision before its gRPC server listens.
	var direct *aiplatformpb.DirectRawPredictResponse
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
		direct, err = pc.DirectRawPredict(cctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: endpoint, MethodName: check, Input: input})
		ccancel()
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("DirectRawPredict: %v", err)
	}
	serving("DirectRawPredict", direct.GetOutput())

	// Vertex documents method_name as /namespace.Service/Method/.
	if direct, err = pc.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: endpoint, MethodName: check + "/", Input: input}); err != nil {
		t.Errorf("DirectRawPredict with the trailing slash: %v", err)
	} else {
		serving("DirectRawPredict with the trailing slash", direct.GetOutput())
	}

	_, err = pc.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: endpoint, MethodName: "/grpc.health.v1.Health/NotAMethod", Input: input})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("a method the container does not serve: %v; want its UNIMPLEMENTED", err)
	}
	_, err = pc.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: endpoint, MethodName: "Check", Input: input})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed method_name: %v; want INVALID_ARGUMENT", err)
	}

	sd, err := pc.StreamDirectRawPredict(ctx)
	if err != nil {
		t.Fatalf("StreamDirectRawPredict: %v", err)
	}
	if err := sd.Send(&aiplatformpb.StreamDirectRawPredictRequest{Endpoint: endpoint, MethodName: check, Input: input}); err != nil {
		t.Fatalf("StreamDirectRawPredict Send: %v", err)
	}
	if err := sd.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if got, err := sd.Recv(); err != nil {
		t.Errorf("StreamDirectRawPredict Recv: %v", err)
	} else {
		serving("StreamDirectRawPredict", got.GetOutput())
	}
	if _, err := sd.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("StreamDirectRawPredict after the answer: %v; want EOF", err)
	}

	sr, err := pc.StreamingRawPredict(ctx)
	if err != nil {
		t.Fatalf("StreamingRawPredict: %v", err)
	}
	if err := sr.Send(&aiplatformpb.StreamingRawPredictRequest{Endpoint: endpoint, MethodName: check, Input: input}); err != nil {
		t.Fatalf("StreamingRawPredict Send: %v", err)
	}
	if err := sr.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if got, err := sr.Recv(); err != nil {
		t.Errorf("StreamingRawPredict Recv: %v", err)
	} else {
		serving("StreamingRawPredict", got.GetOutput())
	}
	if _, err := sr.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("StreamingRawPredict after the answer: %v; want EOF", err)
	}
}

// TestConsoleDirectRawPredictThroughTheConsoleAPI (#1477): the console's
// Online prediction page lists a model that declares grpcPorts with its gRPC
// port, and its DirectRawPredict form's request reaches the container's gRPC
// server through the cluster ingress: grpc.health.v1.Health/Check answers
// SERVING, as bytes; a method the container does not serve comes back as its
// UNIMPLEMENTED.
func TestConsoleDirectRawPredictThroughTheConsoleAPI(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	id := "compat-console-grpc-model"
	deployGRPCModel(t, ctx, c, h, id)
	project := h.Project()

	code, body := consoleDo(t, addr, http.MethodGet, "/api/ai/predict?project="+project, "")
	if code != http.StatusOK || !strings.Contains(body, `"id":"`+id+`"`) || !strings.Contains(body, `"grpcPort":8080`) {
		t.Fatalf("GET /api/ai/predict = %d: %s; want %s with grpcPort 8080", code, body, id)
	}
	input, _ := proto.Marshal(&healthpb.HealthCheckRequest{})
	send := func(method string) (int, string, map[string]any) {
		req, _ := json.Marshal(map[string]string{"endpoint": id, "methodName": method, "input": base64.StdEncoding.EncodeToString(input)})
		code, got := consoleDo(t, addr, http.MethodPost, "/api/ai/predict/direct-raw?"+url.Values{"project": {project}}.Encode(), string(req))
		var r map[string]any
		_ = json.Unmarshal([]byte(got), &r)
		return code, got, r
	}
	var r map[string]any
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if code, body, r = send("/grpc.health.v1.Health/Check"); code == http.StatusOK && r["code"] == "OK" {
			break
		}
	}
	if code != http.StatusOK || r["code"] != "OK" {
		t.Fatalf("DirectRawPredict through the console = %d: %s", code, body)
	}
	out, _ := base64.StdEncoding.DecodeString(r["output"].(string))
	var resp healthpb.HealthCheckResponse
	if err := proto.Unmarshal(out, &resp); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("output %x = %v, %v; want SERVING", out, &resp, err)
	}
	if code, body, r = send("/grpc.health.v1.Health/NotAMethod"); code != http.StatusOK || r["code"] != "Unimplemented" {
		t.Errorf("a method the container does not serve = %d: %s; want its Unimplemented", code, body)
	}
}
