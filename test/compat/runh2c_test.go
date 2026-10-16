//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// grpcServerImage is Kubernetes' own e2e agnhost 2.53, pinned by the index
// digest (linux/amd64 and linux/arm64), whose grpc-health-checking command
// serves grpc.health.v1.Health over HTTP/2 cleartext.
const grpcServerImage = "registry.k8s.io/e2e-test-images/agnhost:2.53@sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a85"

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestRunH2CPortServesGRPC (#1475), with the official run/apiv2 client: a
// service whose container port is named h2c, as Cloud Run documents for
// end-to-end HTTP/2, serves a gRPC server through the service URL, and the
// port reads back named h2c.
func TestRunH2CPortServesGRPC(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	c := runClient(t, h)
	// Pulling agnhost and the rollout outlast h.Context().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	id := "compat-h2c"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: grpcServerImage, Args: []string{"grpc-health-checking", "--port=8080", "--http-port=8081"},
			Ports: []*runpb.ContainerPort{{Name: "h2c", ContainerPort: 8080}},
		}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	if p := svc.GetTemplate().GetContainers()[0].GetPorts(); len(p) != 1 || p[0].GetName() != "h2c" || p[0].GetContainerPort() != 8080 {
		t.Errorf("ports read back as %v; want h2c 8080", p)
	}
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if p := got.GetTemplate().GetContainers()[0].GetPorts(); len(p) != 1 || p[0].GetName() != "h2c" {
		t.Errorf("GetService ports = %v; want h2c", p)
	}

	base, host := ingress(t), hostOf(t, svc.GetUri())
	conn, err := grpc.NewClient(strings.TrimPrefix(base, "http://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithAuthority(host))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var resp *healthpb.HealthCheckResponse
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err = healthpb.NewHealthClient(conn).Check(cctx, &healthpb.HealthCheckRequest{})
		ccancel()
		if err == nil {
			break
		}
	}
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("gRPC Health.Check through the service URL = %v, %v; want SERVING", resp, err)
	}
}

// covers: run.namespaces.services.create, run.namespaces.services.get
//
// TestGcloudRunDeployUseHTTP2 (#1475): `gcloud run deploy --port --use-http2`,
// through the configuration `cloudburrow gcloud-setup` writes, with egress
// to Google refused, deploys a gRPC server whose port is named h2c; the
// service becomes ready and describe reads the port name back.
func TestGcloudRunDeployUseHTTP2(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	id := "gcloud-h2c"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	g.must("run", "deploy", id, "--image="+grpcServerImage, region, "--no-allow-unauthenticated",
		"--port=8080", "--use-http2", "--args=grpc-health-checking,--port=8080,--http-port=8081", "--quiet")
	got := g.must("run", "services", "describe", id, region,
		"--format=value(status.latestReadyRevisionName,spec.template.spec.containers[0].ports[0].name,"+
			"spec.template.spec.containers[0].ports[0].containerPort)")
	if !strings.Contains(got, id+"-00001") || !strings.Contains(got, "h2c") || !strings.Contains(got, "8080") {
		t.Errorf("describe = %q; want the first revision ready and port h2c 8080", got)
	}
}
