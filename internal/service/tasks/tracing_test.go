package tasks_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

const (
	callerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerParent  = "00-" + callerTraceID + "-00f067aa0ba902b7-01"
)

// otlpReceiver is an in-memory OTLP/HTTP collector: it keeps every span it
// is sent, by name, with its trace ID.
type otlpReceiver struct {
	mu    sync.Mutex
	spans map[string]string // name -> hex trace ID
}

func (o *otlpReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(b, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				o.spans[s.GetName()] = hexID(s.GetTraceId())
			}
		}
	}
	o.mu.Unlock()
	out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

func hexID(b []byte) string {
	const digits = "0123456789abcdef"
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(digits[c>>4])
		sb.WriteByte(digits[c&0xf])
	}
	return sb.String()
}

// createAndDispatch serves Cloud Tasks over the real gRPC transport with
// tracing's server options, creates a task from a caller that sends
// callerParent, dispatches it once, and returns the traceparent the target
// received.
func createAndDispatch(t *testing.T, tracing *telemetry.Tracing) string {
	t.Helper()
	ctx := context.Background()
	var got string
	var gotMu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMu.Lock()
		got = r.Header.Get("traceparent")
		gotMu.Unlock()
	}))
	defer target.Close()

	st := tasks.NewStore(store.NewMemory())
	srv := grpctransport.New("127.0.0.1:0")
	srv.ServerOptions(tracing.ServerOptions()...)
	// As in production: the JSON API shares the port, so gRPC is served
	// through the HTTP/2 handler rather than grpc.Server.Serve.
	srv.ServeHTTP(http.NotFoundHandler())
	if err := srv.Register(func(g *grpc.Server) { tasks.NewGRPCServer(st).Register(g) }); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := taskspb.NewCloudTasksClient(conn)

	parent := "projects/trace-proj/locations/us-central1"
	q, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: parent, Queue: &taskspb.Queue{Name: parent + "/queues/q"}})
	if err != nil {
		t.Fatal(err)
	}
	callCtx := metadata.AppendToOutgoingContext(ctx, "traceparent", callerParent)
	task, err := c.CreateTask(callCtx, &taskspb.CreateTaskRequest{Parent: q.GetName(), Task: &taskspb.Task{
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: target.URL, HttpMethod: taskspb.HttpMethod_POST}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTask(task.GetName())
	if err != nil {
		t.Fatal(err)
	}
	d := tasks.NewDispatcher(st, nil, sched.RealClock{})
	if tracing.Enabled() {
		d = d.WithTracing(tracing.Provider())
	}
	if err := d.Dispatch(ctx, stored); err != nil {
		t.Fatal(err)
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	return got
}

// With an OTLP endpoint set, a task created with a traceparent is dispatched
// carrying the same trace, and the CreateTask and dispatch spans are
// exported to that endpoint (#313).
func TestATracedTaskIsDispatchedInTheCallersTrace(t *testing.T) {
	recv := &otlpReceiver{spans: map[string]string{}}
	collector := httptest.NewServer(recv)
	defer collector.Close()
	// The exporter reads its endpoint from the process environment.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	tracing, err := telemetry.Setup(context.Background(), os.Getenv, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !tracing.Enabled() {
		t.Fatal("tracing is off with OTEL_EXPORTER_OTLP_ENDPOINT set")
	}

	header := createAndDispatch(t, tracing)
	if parts := strings.Split(header, "-"); len(parts) != 4 || parts[1] != callerTraceID {
		t.Errorf("the target received traceparent %q, want trace %s", header, callerTraceID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tracing.Shutdown(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	for _, name := range []string{"google.cloud.tasks.v2.CloudTasks/CreateTask", "CloudTasks dispatch"} {
		if id, ok := recv.spans[name]; !ok || id != callerTraceID {
			t.Errorf("span %q exported with trace %q (present %v), want %s; got %v", name, id, ok, callerTraceID, recv.spans)
		}
	}
}

// With no OTLP variable set, no exporter exists: nothing connects to the
// default OTLP ports, no stats handler is installed, and the target
// receives no traceparent from CloudBurrow.
func TestWithoutAnEndpointNothingIsExportedOrDialled(t *testing.T) {
	var count int
	var mu sync.Mutex
	for _, port := range []string{"4317", "4318"} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Skipf("the default OTLP port %s is in use, so a stray connection could not be told apart: %v", port, err)
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
	// Protocol alone does not turn tracing on.
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	tracing, err := telemetry.Setup(context.Background(), os.Getenv, "test")
	if err != nil {
		t.Fatal(err)
	}
	if tracing.Enabled() || len(tracing.ServerOptions()) != 0 {
		t.Fatal("tracing is on with no endpoint configured")
	}
	if header := createAndDispatch(t, tracing); header != "" {
		t.Errorf("the target received traceparent %q with tracing off", header)
	}
	_ = tracing.Shutdown(context.Background())
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Errorf("%d connection(s) reached a default OTLP port with tracing off", count)
	}
}
