package main

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
)

func startTracedKMS(t *testing.T, tracing *telemetry.Tracing) *kmsapi.KeyManagementClient {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceKMS}
	cfg.Endpoints.KMS = 0
	svc := &kmsService{cfg: cfg, db: store.NewMemory(), tracing: tracing}
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(ctx) })
	c, err := kmsapi.NewKeyManagementClient(ctx, option.WithEndpoint(svc.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

const kmsTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// A KMS call is a server span continuing the caller's traceparent (#393).
func TestKMSCallsAreTraced(t *testing.T) {
	var mu sync.Mutex
	spans := map[string]string{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		if proto.Unmarshal(b, &req) == nil {
			mu.Lock()
			for _, rs := range req.GetResourceSpans() {
				for _, ss := range rs.GetScopeSpans() {
					for _, s := range ss.GetSpans() {
						spans[s.GetName()] = hex.EncodeToString(s.GetTraceId())
					}
				}
			}
			mu.Unlock()
		}
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		_, _ = w.Write(out)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	tracing, err := telemetry.Setup(context.Background(), os.Getenv, "test")
	if err != nil || !tracing.Enabled() {
		t.Fatalf("tracing = %v, %v", tracing.Enabled(), err)
	}
	c := startTracedKMS(t, tracing)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", "00-"+kmsTraceID+"-00f067aa0ba902b7-01")
	_, _ = c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: "projects/demo-project/locations/global/keyRings/absent"})
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tracing.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	const name = "google.cloud.kms.v1.KeyManagementService/GetKeyRing"
	if spans[name] != kmsTraceID {
		t.Errorf("span %q has trace %q, want %s; got %v", name, spans[name], kmsTraceID, spans)
	}
}

// With tracing off, a KMS instance creates no exporter and dials nothing.
func TestKMSWithoutAnEndpointDialsNothing(t *testing.T) {
	var mu sync.Mutex
	count := 0
	for _, port := range []string{"4317", "4318"} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Skipf("the default OTLP port %s is in use: %v", port, err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				count++
				mu.Unlock()
				_ = c.Close()
			}
		}()
	}
	for _, v := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Setenv(v, "")
	}
	tracing, err := telemetry.Setup(context.Background(), os.Getenv, "test")
	if err != nil || tracing.Enabled() {
		t.Fatalf("tracing on without an endpoint (%v)", err)
	}
	c := startTracedKMS(t, tracing)
	_, _ = c.GetKeyRing(context.Background(), &kmspb.GetKeyRingRequest{Name: "projects/demo-project/locations/global/keyRings/absent"})
	_ = tracing.Shutdown(context.Background())
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Errorf("%d connection(s) to a default OTLP port with tracing off", count)
	}
}
