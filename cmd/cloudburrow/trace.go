package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/trace"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// traceService serves Cloud Trace (#2118) in the CLI process: the Cloud
// Trace API v1 and v2, gRPC and JSON on one port as cloudtrace.googleapis.com
// serves both, and the Telemetry API's OTLP trace ingestion
// (telemetry.googleapis.com) on the same port, OTLP/gRPC and POST
// /v1/traces. Spans are a file in the instance's directory in persistent
// mode.
type traceService struct {
	cfg       config.Config
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// wrapJSON wraps the JSON API (IAM enforcement); nil is none.
	wrapJSON func(http.Handler) http.Handler
	server   *grpctransport.Server
	api      *trace.Server
	// logging is the instance's Cloud Logging, or nil: the Observability
	// API's _Trace bucket, whose linked datasets take the spans (#2117).
	logging *loggingService
}

func newTraceService(cfg config.Config) *traceService {
	if !traceEnabled(cfg) {
		return nil
	}
	return &traceService{cfg: cfg}
}

// traceEnabled is whether `up` binds the Cloud Trace port.
func traceEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceTrace) && cfg.Endpoints.Trace >= 0
}

func (s *traceService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *traceService) Name() string { return "trace" }

// API is the server, available after Start.
func (s *traceService) API() *trace.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *traceService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *traceService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "trace.json")
	}
	api, err := trace.NewServer(state)
	if err != nil {
		return err
	}
	api.SetDefaultProject(s.cfg.DefaultProject())
	s.api = api
	if s.logging != nil {
		lg := s.logging
		api.OnWrite(func(spans []*trace.Span) { traceToObservability(lg.Admin(), spans) })
		if a := lg.Admin(); a != nil {
			// Reprovisioning _Trace loses its data, as Google's reference says.
			a.OnTraceBucketReprovision(func(project string) { _ = api.ResetProject(project) })
		}
	}
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Trace)))
	s.server.Observe(s.calls)
	// The process's own tracing is not attached here: tracing the server
	// that receives traces would feed it its own spans.
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = trace.NewRESTHandler(api)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *traceService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// traceResetter deletes the stored traces.
type traceResetter struct{ svc *traceService }

func (r *traceResetter) Name() string { return "trace" }

func (r *traceResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *traceResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Cloud Trace has not started")
	}
	return api.ResetProject(project)
}

// traceToObservability makes each project's _Trace bucket on its first
// spans, as Google's trace storage does, and streams the spans to the
// BigQuery datasets linked to its Spans dataset, in the columns of the
// documented span schema.
func traceToObservability(a *logging.Admin, spans []*trace.Span) {
	if a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	byProject := map[string][]*trace.Span{}
	var order []string
	for _, sp := range spans {
		if _, ok := byProject[sp.Project]; !ok {
			order = append(order, sp.Project)
		}
		byProject[sp.Project] = append(byProject[sp.Project], sp)
	}
	for _, p := range order {
		if err := a.EnsureTraceBucket(ctx, p); err != nil {
			continue
		}
		var rows []map[string]any
		var ids []string
		for _, sp := range byProject[p] {
			rows = append(rows, spanRow(sp))
			ids = append(ids, sp.TraceID+"/"+sp.SpanID+"/"+strconv.FormatInt(sp.Received.UnixNano(), 10))
		}
		_ = a.AppendSpans(ctx, p, rows, ids)
	}
}

// spanRow is a span in the Spans dataset's columns.
func spanRow(sp *trace.Span) map[string]any {
	v := sp.V2
	attrs := map[string]string{}
	for k, val := range v.GetAttributes().GetAttributeMap() {
		attrs[k] = trace.AttrString(val)
	}
	ab, _ := json.Marshal(attrs)
	start, end := sp.Start(), sp.End()
	row := map[string]any{
		"trace_id": sp.TraceID, "span_id": sp.SpanID, "parent_span_id": v.GetParentSpanId(), "name": sp.Name(),
		"kind":       int64(v.GetSpanKind()),
		"start_time": start.UTC().Format(time.RFC3339Nano), "start_time_unix_nano": start.UnixNano(),
		"end_time": end.UTC().Format(time.RFC3339Nano), "end_time_unix_nano": end.UnixNano(),
		"receive_time": sp.Received.UTC().Format(time.RFC3339Nano), "receive_time_unix_nano": sp.Received.UnixNano(),
		"duration_unix_nano": end.Sub(start).Nanoseconds(),
		"attributes":         string(ab), "dropped_attributes_count": int64(v.GetAttributes().GetDroppedAttributesCount()),
	}
	if st := v.GetStatus(); st != nil {
		row["status"] = map[string]any{"code": int64(st.GetCode()), "message": st.GetMessage()}
	}
	return row
}
