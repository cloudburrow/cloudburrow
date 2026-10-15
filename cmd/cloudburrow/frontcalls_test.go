package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// pubsubFront is a Pub/Sub front before an in-memory Pub/Sub, as in the
// pod, with an official client that reaches it.
func pubsubFront(t *testing.T) (string, *pubsub.Client) {
	t.Helper()
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	f, err := pubsubfront.New(fake.Addr, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Serve(ctx, l, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	c, err := pubsub.NewClient(context.Background(), "calls-test", option.WithEndpoint(l.Addr().String()),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return l.Addr().String(), c
}

// The calls the front served reach /metrics, the request log and a span,
// with their method, code and latency, and the span continues the client's
// trace (#1266).
func TestPubSubCallsAreMeasured(t *testing.T) {
	addr, client := pubsubFront(t)
	rec := admin.NewRecorder(100, time.Now)
	reg := metrics.New("pubsub")
	s := newFrontCallScraper("pubsub", nil, rec, reg, nil)
	if got := reg.Unmeasured(); len(got) != 0 {
		t.Fatalf("unmeasured = %v, want none once the scraper is built", got)
	}
	s.addr = func() string { return addr }
	spans := tracetest.NewSpanRecorder()
	s.tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	topic := "projects/calls-test/topics/measured"
	if _, err := client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("secret payload")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/calls-test/topics/missing"})

	s.scrape(context.Background())
	// A second scrape finds nothing new: every call is counted once.
	s.scrape(context.Background())

	var b strings.Builder
	_ = reg.Write(&b)
	fams := parseMetrics(t, b.String())
	for _, want := range []map[string]string{
		{"service": "pubsub", "method": "CreateTopic", "code": "OK"},
		{"service": "pubsub", "method": "Publish", "code": "OK"},
		{"service": "pubsub", "method": "GetTopic", "code": "NOT_FOUND"},
	} {
		if v, ok := counter(fams, "cloudburrow_requests_total", want); !ok || v != 1 {
			t.Errorf("%v = %v (found %v), want 1:\n%s", want, v, ok, b.String())
		}
	}
	if _, ok := fams["cloudburrow_request_duration_seconds"]; !ok {
		t.Error("no latency histogram")
	}

	events := rec.Events("pubsub", 10)
	targets := map[string]admin.Event{}
	for _, e := range events {
		targets[e.Target] = e
	}
	pub, ok := targets["/google.pubsub.v1.Publisher/Publish"]
	if !ok {
		t.Fatalf("no Publish in the request log: %+v", events)
	}
	if pub.Detail["code"] != "OK" || pub.Detail["resource"] != topic || pub.Detail["project"] != "calls-test" ||
		pub.Detail["transport"] != "grpc" || pub.Detail["duration_ms"] == "" {
		t.Errorf("Publish event detail = %v", pub.Detail)
	}
	for _, e := range events {
		for _, v := range e.Detail {
			if strings.Contains(v, "secret payload") {
				t.Errorf("a payload reached the request log: %+v", e)
			}
		}
	}

	ended := spans.Ended()
	if len(ended) != 3 {
		t.Fatalf("%d spans, want 3", len(ended))
	}
	for _, sp := range ended {
		// The publisher batches messages and sends them with a context of
		// its own, so only the admin calls carry the test's traceparent.
		if sp.Name() != "google.pubsub.v1.Publisher/Publish" && sp.SpanContext().TraceID().String() != traceID {
			t.Errorf("span %s is in trace %s, not the client's", sp.Name(), sp.SpanContext().TraceID())
		}
		if !sp.EndTime().After(sp.StartTime()) {
			t.Errorf("span %s has no duration", sp.Name())
		}
	}
}

// A front that restarted numbers its calls from 1 again; the scraper starts
// over rather than waiting for the old sequence to be passed.
func TestPubSubCallsAfterAFrontRestart(t *testing.T) {
	addr, client := pubsubFront(t)
	reg := metrics.New("pubsub")
	s := newFrontCallScraper("pubsub", nil, nil, reg, nil)
	s.addr = func() string { return addr }
	s.after = 1 << 20
	if _, err := client.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: "projects/calls-test/topics/again"}); err != nil {
		t.Fatal(err)
	}
	s.scrape(context.Background())
	var b strings.Builder
	_ = reg.Write(&b)
	if v, ok := counter(parseMetrics(t, b.String()), "cloudburrow_requests_total",
		map[string]string{"service": "pubsub", "method": "CreateTopic", "code": "OK"}); !ok || v != 1 {
		t.Errorf("CreateTopic after a restart = %v (found %v), want 1", v, ok)
	}
}

// A guarded tunnel's calls (#1267) are counted and recorded under their
// service as a front's are, gRPC by canonical code and REST by status.
func TestTunnelCallsAreMeasured(t *testing.T) {
	rec := admin.NewRecorder(100, time.Now)
	reg := metrics.New("firestore", "spanner")
	observe := tunnelCalls(rec, reg, nil, "firestore")
	if got := reg.Unmeasured(); len(got) != 1 || got[0] != "spanner" {
		t.Fatalf("unmeasured = %v, want spanner alone", got)
	}
	observe(netfwd.Call{Method: "/google.firestore.v1.Firestore/GetDocument", GRPC: true, Code: 5,
		Resource: "projects/fs/databases/(default)", Time: time.Now(), Duration: time.Millisecond})
	observe(netfwd.Call{Method: "GET /v1/projects/fs/databases/(default)/documents/c/d", Code: 200,
		Time: time.Now(), Duration: time.Millisecond})
	var b strings.Builder
	_ = reg.Write(&b)
	fams := parseMetrics(t, b.String())
	for _, want := range []map[string]string{
		{"service": "firestore", "method": "GetDocument", "code": "NOT_FOUND"},
		{"service": "firestore", "method": "JSON GET", "code": "200"},
	} {
		if v, ok := counter(fams, "cloudburrow_requests_total", want); !ok || v != 1 {
			t.Errorf("%v = %v (found %v), want 1", want, v, ok)
		}
	}
	events := rec.Events("firestore", 10)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2", events)
	}
	for _, e := range events {
		if e.Detail["project"] != "fs" {
			t.Errorf("event %s has project %q, want fs", e.Target, e.Detail["project"])
		}
	}
}
