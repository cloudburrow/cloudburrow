package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// pubsubCallScraper feeds the calls the in-cluster Pub/Sub front served
// (internal/pubsubfront/calls.go, #1266) into /metrics, /admin/events (the
// console's request log) and, with tracing on, one span each, as the
// storage scraper does for the builtin storage server (#513): the front
// keeps its last calls at pubsubfront.CallsPath, and the CLI polls it through
// the tunnel, since a pod cannot reach the CLI, which binds loopback
// (ADR-0004). Every client's calls are seen, the host's, a workload's in the
// cluster and the storage server's notifications alike, since they all
// reach the emulator through the front.
type pubsubCallScraper struct {
	addr     func() string
	grpcCall grpctransport.Observer
	restCall func(rest.Request)
	tracer   trace.Tracer
	interval time.Duration

	mu     sync.Mutex
	after  uint64
	cancel context.CancelFunc
	done   chan struct{}
}

// newPubSubCallScraper reads the front through tunnel. Building it marks
// pubsub measured in reg.
func newPubSubCallScraper(tunnel *netfwd.Forwarder, rec *admin.Recorder, reg *metrics.Registry, tracing *telemetry.Tracing) *pubsubCallScraper {
	s := &pubsubCallScraper{
		addr: func() string {
			if tunnel == nil {
				return ""
			}
			return tunnel.HostAddr()
		},
		grpcCall: callEvents(rec, reg, "pubsub"),
		restCall: requestEvents(rec, reg, "pubsub"),
		interval: 2 * time.Second,
	}
	if tracing != nil && tracing.Enabled() {
		s.tracer = tracing.Provider().Tracer("github.com/cloudburrow/cloudburrow/pubsub")
	}
	return s
}

func (s *pubsubCallScraper) Name() string { return "pubsub-calls" }

func (s *pubsubCallScraper) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			s.scrape(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (s *pubsubCallScraper) Stop(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	// The calls since the last poll, so a stop loses none.
	s.scrape(ctx)
	return nil
}

// page reads the front's calls after a sequence number.
func (s *pubsubCallScraper) page(ctx context.Context, after uint64) ([]pubsubfront.Call, bool) {
	addr := s.addr()
	if addr == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+addr+pubsubfront.CallsPath+"?after="+strconv.FormatUint(after, 10), nil)
	if err != nil {
		return nil, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var page struct {
		Calls []pubsubfront.Call `json:"calls"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&page) != nil {
		return nil, false
	}
	return page.Calls, true
}

// scrape records the calls after the last one seen. A front that restarted
// numbers from 1 again, so a sequence that went back resets the cursor.
func (s *pubsubCallScraper) scrape(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls, ok := s.page(ctx, s.after)
	if !ok {
		return
	}
	if len(calls) == 0 && s.after > 0 {
		all, ok := s.page(ctx, 0)
		if !ok || len(all) == 0 || all[len(all)-1].Seq >= s.after {
			return
		}
		s.after, calls = 0, all
	}
	for _, c := range calls {
		s.record(c)
		s.after = c.Seq
	}
}

func (s *pubsubCallScraper) record(c pubsubfront.Call) {
	switch c.Transport {
	case "grpc":
		if s.grpcCall != nil {
			s.grpcCall(grpctransport.Call{Method: c.Method, Resource: c.Resource, Code: codes.Code(c.Code),
				Duration: c.Duration, Stream: c.Stream})
		}
	case "http":
		verb, path, _ := strings.Cut(c.Method, " ")
		if s.restCall != nil {
			s.restCall(rest.Request{Method: verb, Path: path, Status: c.Code, Duration: c.Duration})
		}
	default:
		return
	}
	s.span(c)
}

// span records the call as a server span, at the time the front served it,
// continuing the trace its traceparent named.
func (s *pubsubCallScraper) span(c pubsubfront.Call) {
	if s.tracer == nil {
		return
	}
	ctx := context.Background()
	if c.TraceParent != "" {
		ctx = telemetry.Propagator.Extract(ctx, propagation.MapCarrier{"traceparent": c.TraceParent})
	}
	name := strings.TrimPrefix(c.Method, "/")
	attrs := []attribute.KeyValue{attribute.String("cloudburrow.service", "pubsub")}
	failed := false
	if c.Transport == "grpc" {
		svc, method, _ := strings.Cut(name, "/")
		attrs = append(attrs, attribute.String("rpc.system", "grpc"), attribute.String("rpc.service", svc),
			attribute.String("rpc.method", method), attribute.Int("rpc.grpc.status_code", c.Code))
		failed = c.Code != int(codes.OK)
	} else {
		verb, path, _ := strings.Cut(c.Method, " ")
		name = verb
		attrs = append(attrs, attribute.String("http.request.method", verb), attribute.String("url.path", path),
			attribute.Int("http.response.status_code", c.Code))
		failed = c.Code >= 500
	}
	if c.Resource != "" {
		attrs = append(attrs, attribute.String("cloudburrow.resource", c.Resource))
	}
	_, sp := s.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(c.Time), trace.WithAttributes(attrs...))
	if failed {
		sp.SetStatus(otelcodes.Error, "")
	}
	sp.End(trace.WithTimestamp(c.Time.Add(c.Duration)))
}
