//go:build compat

package compat

// Cloud Trace (#2118): the Cloud Trace API v2's writes and v1's reads
// through the official cloud.google.com/go/trace clients, OTLP ingestion
// (the Telemetry API's) through OpenTelemetry's own OTLP exporters, and
// Google's Cloud Trace exporter for OpenTelemetry
// (github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace).

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	trace "cloud.google.com/go/trace/apiv1"
	cloudtracepb "cloud.google.com/go/trace/apiv1/tracepb"
	tracev2 "cloud.google.com/go/trace/apiv2"
	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EnvTrace points at the local Cloud Trace endpoint (#2118): the Cloud
// Trace API v1 and v2, gRPC and JSON, and OTLP trace ingestion, on one port.
const EnvTrace = "CLOUDBURROW_TEST_TRACE"

func traceGRPCOpts(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// traceV1Clients are the v1 client over gRPC and over REST.
func traceV1Clients(t *testing.T, h *Harness, addr string) map[string]*trace.Client {
	t.Helper()
	g, err := trace.NewClient(h.Context(), traceGRPCOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := trace.NewRESTClient(h.Context(), option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*trace.Client{"grpc": g, "rest": r}
}

func traceCode(err error) codes.Code {
	var he *googleapi.Error
	if errors.As(err, &he) {
		switch he.Code {
		case 400:
			return codes.InvalidArgument
		case 404:
			return codes.NotFound
		}
		return codes.Unknown
	}
	return status.Code(err)
}

// newTraceID is a fresh random-looking trace ID, unique per run.
func newTraceID(seed string) string {
	b := []byte(fmt.Sprintf("%-8.8s%08x", seed, uint32(time.Now().UnixNano())))
	return hex.EncodeToString(b)
}

func v2Span(project, traceID, spanID, parent, name string, start time.Time, d time.Duration, attrs map[string]string) *cloudtracev2pb.Span {
	sp := &cloudtracev2pb.Span{Name: "projects/" + project + "/traces/" + traceID + "/spans/" + spanID, SpanId: spanID, ParentSpanId: parent,
		DisplayName: &cloudtracev2pb.TruncatableString{Value: name}, StartTime: timestamppb.New(start), EndTime: timestamppb.New(start.Add(d)),
		SpanKind: cloudtracev2pb.Span_SERVER, Attributes: &cloudtracev2pb.Span_Attributes{AttributeMap: map[string]*cloudtracev2pb.AttributeValue{}}}
	for k, v := range attrs {
		sp.Attributes.AttributeMap[k] = &cloudtracev2pb.AttributeValue{Value: &cloudtracev2pb.AttributeValue_StringValue{StringValue: &cloudtracev2pb.TruncatableString{Value: v}}}
	}
	return sp
}

func listTraceIDs(t *testing.T, h *Harness, c *trace.Client, req *cloudtracepb.ListTracesRequest) []*cloudtracepb.Trace {
	t.Helper()
	var out []*cloudtracepb.Trace
	it := c.ListTraces(h.Context(), req)
	for {
		tr, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("ListTraces(%q): %v", req.GetFilter(), err)
		}
		out = append(out, tr)
	}
}

// covers: google.devtools.cloudtrace.v2.TraceService/BatchWriteSpans
// covers: google.devtools.cloudtrace.v2.TraceService/CreateSpan
// covers: google.devtools.cloudtrace.v1.TraceService/ListTraces
// covers: google.devtools.cloudtrace.v1.TraceService/GetTrace
// covers: google.devtools.cloudtrace.v1.TraceService/PatchTraces
//
// TestCloudTraceWriteV2ReadV1 writes spans with BatchWriteSpans and
// CreateSpan and reads them back through v1's ListTraces (filters, views,
// ordering, interval, paging), GetTrace and PatchTraces, over gRPC and REST.
func TestCloudTraceWriteV2ReadV1(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvTrace)
	ctx := h.Context()
	w, err := tracev2.NewClient(ctx, traceGRPCOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wr, err := tracev2.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	p := h.Project()
	run := fmt.Sprintf("r%d", time.Now().UnixNano()%1e9)
	t0 := time.Now().Add(-2 * time.Minute).Truncate(time.Millisecond)
	orders, health := newTraceID("orders"), newTraceID("health")
	err = w.BatchWriteSpans(ctx, &cloudtracev2pb.BatchWriteSpansRequest{Name: "projects/" + p, Spans: []*cloudtracev2pb.Span{
		v2Span(p, orders, "00000000000000a1", "", "/"+run+"/orders", t0, 300*time.Millisecond, map[string]string{"/http/method": "GET", "run": run}),
		v2Span(p, orders, "00000000000000a2", "00000000000000a1", "db.query", t0.Add(10*time.Millisecond), 100*time.Millisecond, map[string]string{"db": "orders", "run": run}),
	}})
	if err != nil {
		t.Fatalf("BatchWriteSpans: %v", err)
	}
	// CreateSpan, over REST, returns the span.
	got, err := wr.CreateSpan(ctx, v2Span(p, health, "00000000000000b1", "", "/"+run+"/healthz", t0.Add(time.Second), 2*time.Millisecond, map[string]string{"/http/method": "POST", "run": run}))
	if err != nil || got.GetDisplayName().GetValue() != "/"+run+"/healthz" {
		t.Fatalf("CreateSpan: %v %v", got, err)
	}
	// The v2 rules: a zero trace ID, a missing displayName, a span of
	// another project.
	for name, sp := range map[string]*cloudtracev2pb.Span{
		"zero trace": v2Span(p, strings.Repeat("0", 32), "00000000000000a1", "", "x", t0, time.Second, nil),
		"no name":    v2Span(p, orders, "00000000000000a3", "", "", t0, time.Second, nil),
		"other":      v2Span("other-"+p, orders, "00000000000000a3", "", "x", t0, time.Second, nil),
	} {
		if err := w.BatchWriteSpans(ctx, &cloudtracev2pb.BatchWriteSpansRequest{Name: "projects/" + p, Spans: []*cloudtracev2pb.Span{sp}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("BatchWriteSpans(%s): %v, want INVALID_ARGUMENT", name, err)
		}
	}
	for transport, c := range traceV1Clients(t, h, addr) {
		t.Run(transport, func(t *testing.T) {
			base := "run:" + run
			count := func(filter string) int {
				return len(listTraceIDs(t, h, c, &cloudtracepb.ListTracesRequest{ProjectId: p, Filter: strings.TrimSpace(base + " " + filter)}))
			}
			for f, want := range map[string]int{"": 2, "/" + run + "/ord": 1, "+/" + run + "/ord": 0, "+root:/" + run + "/orders": 1,
				"span:db": 1, "+span:db.query": 1, "latency:100ms": 1, "latency:1s": 0, "method:GET": 1, "^db:orders": 0, "db:ord": 1, "label:db": 1} {
				if n := count(f); n != want {
					t.Errorf("filter %q: %d traces, want %d", f, n, want)
				}
			}
			ts := listTraceIDs(t, h, c, &cloudtracepb.ListTracesRequest{ProjectId: p, Filter: base, OrderBy: "duration desc",
				View: cloudtracepb.ListTracesRequest_ROOTSPAN, PageSize: 1})
			if len(ts) != 2 || ts[0].GetTraceId() != orders || len(ts[0].GetSpans()) != 1 || ts[0].GetSpans()[0].GetName() != "/"+run+"/orders" {
				t.Fatalf("ROOTSPAN by duration desc, one a page: %v", ts)
			}
			if ts := listTraceIDs(t, h, c, &cloudtracepb.ListTracesRequest{ProjectId: p, Filter: base}); len(ts[0].GetSpans()) != 0 {
				t.Errorf("MINIMAL returned spans: %v", ts[0])
			}
			complete := listTraceIDs(t, h, c, &cloudtracepb.ListTracesRequest{ProjectId: p, Filter: base, View: cloudtracepb.ListTracesRequest_COMPLETE, OrderBy: "start"})
			if len(complete) != 2 || complete[0].GetTraceId() != orders || len(complete[0].GetSpans()) != 2 {
				t.Errorf("COMPLETE by start: %v", complete)
			}
			// The interval: a window after both traces holds neither.
			late := listTraceIDs(t, h, c, &cloudtracepb.ListTracesRequest{ProjectId: p, Filter: base, StartTime: timestamppb.New(time.Now().Add(time.Hour))})
			if len(late) != 0 {
				t.Errorf("a later interval: %v", late)
			}
			if _, err := c.ListTraces(ctx, &cloudtracepb.ListTracesRequest{ProjectId: p, OrderBy: "name,start"}).Next(); traceCode(err) != codes.InvalidArgument {
				t.Errorf("two sort fields: %v", err)
			}
			tr, err := c.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: p, TraceId: orders})
			if err != nil || len(tr.GetSpans()) != 2 {
				t.Fatalf("GetTrace: %v %v", tr, err)
			}
			var child *cloudtracepb.TraceSpan
			for _, sp := range tr.GetSpans() {
				if sp.GetSpanId() == 0xa2 {
					child = sp
				}
			}
			if child == nil || child.GetParentSpanId() != 0xa1 || child.GetKind() != cloudtracepb.TraceSpan_RPC_SERVER || child.GetLabels()["db"] != "orders" {
				t.Errorf("child span: %v", child)
			}
			if _, err := c.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: p, TraceId: newTraceID("missing")}); traceCode(err) != codes.NotFound {
				t.Errorf("GetTrace of a missing trace: %v, want NOT_FOUND", err)
			}
		})
	}
	// PatchTraces: labels merge into the span, other fields stay.
	c1 := traceV1Clients(t, h, addr)["grpc"]
	err = c1.PatchTraces(ctx, &cloudtracepb.PatchTracesRequest{ProjectId: p, Traces: &cloudtracepb.Traces{Traces: []*cloudtracepb.Trace{{
		ProjectId: p, TraceId: orders, Spans: []*cloudtracepb.TraceSpan{{SpanId: 0xa2, Labels: map[string]string{"rows": "3"}}}}}}})
	if err != nil {
		t.Fatalf("PatchTraces: %v", err)
	}
	tr, err := c1.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: p, TraceId: orders})
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range tr.GetSpans() {
		if sp.GetSpanId() == 0xa2 && (sp.GetLabels()["rows"] != "3" || sp.GetLabels()["db"] != "orders" || sp.GetName() != "db.query") {
			t.Errorf("patched span: %v", sp)
		}
	}
}

// otlpTraceProvider exports one span through exp and returns its trace ID.
func exportOneSpan(t *testing.T, h *Harness, exp sdktrace.SpanExporter, res *resource.Resource, name string, attrs ...attribute.KeyValue) string {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithResource(res))
	ctx, parent := tp.Tracer("compat").Start(h.Context(), name, oteltrace.WithSpanKind(oteltrace.SpanKindServer), oteltrace.WithAttributes(attrs...))
	_, child := tp.Tracer("compat").Start(ctx, name+"/child")
	time.Sleep(5 * time.Millisecond)
	child.End()
	parent.End()
	if err := tp.Shutdown(h.Context()); err != nil {
		t.Fatalf("exporting %s: %v", name, err)
	}
	return parent.SpanContext().TraceID().String()
}

// waitTrace reads a trace back through v1 GetTrace.
func waitTrace(t *testing.T, h *Harness, c *trace.Client, project, traceID string, spans int) *cloudtracepb.Trace {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		tr, err := c.GetTrace(h.Context(), &cloudtracepb.GetTraceRequest{ProjectId: project, TraceId: traceID})
		if err == nil && len(tr.GetSpans()) == spans {
			return tr
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetTrace(%s/%s): %v %v, want %d spans", project, traceID, tr, err, spans)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestCloudTraceOTLPIngestion sends OpenTelemetry spans through OTLP/gRPC
// and OTLP/HTTP (OpenTelemetry's own exporters, as Google's migration guide
// configures them, with gcp.project_id naming the project) and reads them
// back through Cloud Trace v1; and OTLP/JSON by hand.
func TestCloudTraceOTLPIngestion(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvTrace)
	ctx := h.Context()
	p := h.Project()
	c := traceV1Clients(t, h, addr)["grpc"]
	res := resource.NewSchemaless(attribute.String("service.name", "checkout"), attribute.String("gcp.project_id", p))

	g, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(addr), otlptracegrpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	id := exportOneSpan(t, h, g, res, "otlp-grpc", attribute.String("http.method", "GET"))
	tr := waitTrace(t, h, c, p, id, 2)
	var root *cloudtracepb.TraceSpan
	for _, sp := range tr.GetSpans() {
		if sp.GetName() == "otlp-grpc" {
			root = sp
		}
	}
	if root == nil || root.GetKind() != cloudtracepb.TraceSpan_RPC_SERVER || root.GetLabels()["service.name"] != "checkout" || root.GetLabels()["http.method"] != "GET" {
		t.Errorf("OTLP/gRPC root span: %v", root)
	}

	hp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(addr), otlptracehttp.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	id = exportOneSpan(t, h, hp, res, "otlp-http")
	waitTrace(t, h, c, p, id, 2)

	// OTLP/JSON, IDs in hex, to POST /v1/traces.
	jsonID := newTraceID("otlpjson")
	now := time.Now().UnixNano()
	body := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"gcp.project_id","value":{"stringValue":%q}}]},
"scopeSpans":[{"spans":[{"traceId":%q,"spanId":"00000000000000c1","name":"otlp-json","kind":2,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"}]}]}]}`,
		p, jsonID, now, now+int64(time.Millisecond))
	resp, err := http.Post("http://"+addr+"/v1/traces", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OTLP/JSON: %d %s", resp.StatusCode, b)
	}
	waitTrace(t, h, c, p, jsonID, 1)
	// The project's first spans made its _Trace bucket, with the Spans
	// dataset and _AllSpans view (#2117), on the Logging port.
	if logAddr := os.Getenv(EnvLogging); logAddr != "" {
		oc := observabilityClient(t, h, h.Endpoint(EnvLogging))
		var found string
		it := oc.ListBuckets(ctx, &obspb.ListBucketsRequest{Parent: "projects/" + p + "/locations/-"})
		for {
			b, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("ListBuckets: %v", err)
			}
			if strings.HasSuffix(b.GetName(), "/buckets/_Trace") {
				found = b.GetName()
			}
		}
		if found == "" {
			t.Fatal("no _Trace bucket after the project's first spans")
		}
		if _, err := oc.GetView(ctx, &obspb.GetViewRequest{Name: found + "/datasets/Spans/views/_AllSpans"}); err != nil {
			t.Errorf("the _AllSpans view: %v", err)
		}
	}
	// A GET is not OTLP.
	if resp, err := http.Get("http://" + addr + "/v1/traces"); err != nil || resp.StatusCode == http.StatusOK {
		t.Errorf("GET /v1/traces: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
}

// TestCloudTraceOpenTelemetryExporter exports through Google's Cloud Trace
// exporter for OpenTelemetry, which writes with the v2 client, and reads
// its mapping back: http.method as /http/method, and g.co/agent.
func TestCloudTraceOpenTelemetryExporter(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvTrace)
	p := h.Project()
	exp, err := texporter.New(texporter.WithProjectID(p), texporter.WithContext(h.Context()), texporter.WithTraceClientOptions(traceGRPCOpts(addr)))
	if err != nil {
		t.Fatal(err)
	}
	id := exportOneSpan(t, h, exp, resource.NewSchemaless(attribute.String("service.name", "frontend")), "exporter-root", attribute.String("http.method", "PUT"))
	c := traceV1Clients(t, h, addr)["rest"]
	tr := waitTrace(t, h, c, p, id, 2)
	var root *cloudtracepb.TraceSpan
	for _, sp := range tr.GetSpans() {
		if sp.GetName() == "exporter-root" {
			root = sp
		}
	}
	if root == nil || root.GetLabels()["/http/method"] != "PUT" || !strings.Contains(root.GetLabels()["g.co/agent"], "opentelemetry") {
		t.Fatalf("exporter's root span: %v", root)
	}
	ts := listTraceIDs(t, h, c, &cloudtracepb.ListTracesRequest{ProjectId: p, Filter: "+exporter-root method:PUT"})
	if len(ts) != 1 || ts[0].GetTraceId() != id {
		t.Errorf("ListTraces +exporter-root method:PUT: %v", ts)
	}
}
