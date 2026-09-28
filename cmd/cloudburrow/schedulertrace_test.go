package main

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging/logadmin"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	rmsvc "github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
)

// spanCollector is an in-memory OTLP/HTTP collector keeping every span it is
// sent, by name, with its hex trace ID.
func spanCollector(t *testing.T) (*sync.Mutex, map[string]string) {
	t.Helper()
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
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	return &mu, spans
}

func insecureClient(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// With OTEL_EXPORTER_OTLP_ENDPOINT set, Cloud Scheduler, Cloud Logging and
// Resource Manager calls are server spans continuing the caller's
// traceparent (#600), as Cloud Tasks and Cloud KMS calls are: each service is
// started the way up starts it, with the tracing up hands it.
func TestSchedulerLoggingAndResourceManagerCallsAreTraced(t *testing.T) {
	mu, spans := spanCollector(t)
	tracing, err := telemetry.Setup(context.Background(), os.Getenv, "test")
	if err != nil || !tracing.Enabled() {
		t.Fatalf("tracing = %v, %v", tracing.Enabled(), err)
	}
	ctx := context.Background()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceScheduler, config.ServiceLogging}
	cfg.Endpoints.Scheduler, cfg.Endpoints.Logging = 0, 0

	sched := newSchedulerService(cfg, func() *netfwd.Forwarder { return nil })
	sched.tracing = tracing
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sched.Stop(ctx) })
	logs := newLoggingService(cfg)
	logs.tracing = tracing
	if err := logs.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Stop(ctx) })
	rm := rmsvc.NewServer("127.0.0.1:0", rmsvc.New(store.NewMemory()))
	rm.ServerOptions(tracing.ServerOptions()...)
	if err := rm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Stop(ctx) })

	call := metadata.AppendToOutgoingContext(ctx, "traceparent", "00-"+kmsTraceID+"-00f067aa0ba902b7-01")
	sc, err := scheduler.NewCloudSchedulerClient(ctx, insecureClient(sched.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	parent := "projects/demo-project/locations/us-central1"
	if _, err := sc.CreateJob(call, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/traced", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:9/"}}}}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	lc, err := logadmin.NewClient(ctx, "projects/demo-project", insecureClient(logs.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	_, _ = lc.Logs(call).Next()
	rc, err := resourcemanager.NewProjectsClient(ctx, insecureClient(rm.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, _ = rc.GetProject(call, &rmpb.GetProjectRequest{Name: "projects/absent"})

	// A server span ends after its handler returns, which can be after the
	// client has its answer; a graceful stop waits for in-flight calls, so
	// every span has ended before the exporter is flushed.
	_ = sched.Stop(ctx)
	_ = logs.Stop(ctx)
	_ = rm.Stop(ctx)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tracing.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{
		"google.cloud.scheduler.v1.CloudScheduler/CreateJob",
		"google.logging.v2.LoggingServiceV2/ListLogs",
		"google.cloud.resourcemanager.v3.Projects/GetProject",
	} {
		if spans[name] != kmsTraceID {
			t.Errorf("span %q has trace %q, want %s; got %v", name, spans[name], kmsTraceID, spans)
		}
	}
}
